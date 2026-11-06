//go:build linux

package containerd

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/errdefs"
)

const (
	// keepUnusedImages is how many unused images per repository stay on a
	// node, so a rollback to a recent revision does not have to pull again.
	keepUnusedImages = 2
	// imagePruneInterval bounds how often unused images are pruned.
	imagePruneInterval = 10 * time.Minute
)

// pinImage marks an image as in use while Ensure pulls it and creates its
// container, so a concurrent prune cannot remove it in between.
func (c *runtimeClient) pinImage(reference string) func() {
	c.imageMu.Lock()
	if c.pinnedImages == nil {
		c.pinnedImages = make(map[string]int)
	}
	c.pinnedImages[reference]++
	c.imageMu.Unlock()
	return func() {
		c.imageMu.Lock()
		if c.pinnedImages[reference]--; c.pinnedImages[reference] <= 0 {
			delete(c.pinnedImages, reference)
		}
		c.imageMu.Unlock()
	}
}

// maybePruneImages prunes unused images at most once per imagePruneInterval.
func (c *runtimeClient) maybePruneImages(ctx context.Context) {
	c.imageMu.Lock()
	due := time.Since(c.lastImagePrune) >= imagePruneInterval
	if due {
		c.lastImagePrune = time.Now()
	}
	c.imageMu.Unlock()
	if due {
		// Pruning is best effort: a failure is retried on the next interval
		// and must not fail reconciliation.
		_, _ = c.pruneImages(ctx, keepUnusedImages)
	}
}

// pruneImages removes image records that no container in the namespace uses
// and that are not being pulled, keeping the keep most recent unused images
// of each repository. containerd then garbage collects their content and
// snapshots. Every image in the namespace was pulled by orch, and containers
// of every node sharing it count as users, so nothing in use is removed.
func (c *runtimeClient) pruneImages(ctx context.Context, keep int) (int, error) {
	// Holding imageMu throughout means an image pinned after the containers
	// were listed waits until the prune is done before it is pulled.
	c.imageMu.Lock()
	defer c.imageMu.Unlock()
	containers, err := c.client.Containers(ctx)
	if err != nil {
		return 0, fmt.Errorf("list containers: %w", err)
	}
	inUse := make(map[string]bool, len(containers))
	for _, container := range containers {
		info, err := container.Info(ctx)
		if err != nil {
			if errdefs.IsNotFound(err) {
				continue
			}
			return 0, fmt.Errorf("inspect container %q: %w", container.ID(), err)
		}
		inUse[info.Image] = true
	}
	service := c.client.ImageService()
	stored, err := service.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("list images: %w", err)
	}
	for reference := range c.pinnedImages {
		inUse[reference] = true
	}
	removable := imagesToPrune(stored, inUse, keep)
	removed := 0
	for index, name := range removable {
		var options []images.DeleteOpt
		if index == len(removable)-1 {
			// Wait for garbage collection once, so the space is free when
			// a pull that ran out of space is retried.
			options = append(options, images.SynchronousDelete())
		}
		if err := service.Delete(ctx, name, options...); err != nil && !errdefs.IsNotFound(err) {
			return removed, fmt.Errorf("remove unused image %q: %w", name, err)
		}
		removed++
	}
	return removed, nil
}

// imagesToPrune returns the names of the unused images to remove, keeping the
// keep most recently updated unused images of each repository.
func imagesToPrune(stored []images.Image, inUse map[string]bool, keep int) []string {
	unused := make(map[string][]images.Image)
	for _, image := range stored {
		if inUse[image.Name] {
			continue
		}
		repository := imageRepository(image.Name)
		unused[repository] = append(unused[repository], image)
	}
	var result []string
	for _, candidates := range unused {
		sort.Slice(candidates, func(i, j int) bool {
			if !candidates[i].UpdatedAt.Equal(candidates[j].UpdatedAt) {
				return candidates[i].UpdatedAt.After(candidates[j].UpdatedAt)
			}
			return candidates[i].Name > candidates[j].Name
		})
		for index, image := range candidates {
			if index >= keep {
				result = append(result, image.Name)
			}
		}
	}
	sort.Strings(result)
	return result
}

// imageRepository strips the digest and tag from an image reference.
func imageRepository(name string) string {
	if at := strings.IndexByte(name, '@'); at >= 0 {
		name = name[:at]
	}
	if colon := strings.LastIndexByte(name, ':'); colon > strings.LastIndexByte(name, '/') {
		name = name[:colon]
	}
	return name
}

// isNoSpace reports whether a containerd error says the disk is full. The
// error crosses gRPC, so it no longer wraps syscall.ENOSPC.
func isNoSpace(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no space left on device")
}
