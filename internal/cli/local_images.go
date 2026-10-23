package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/distribution/reference"
)

// imageUploader is the part of the controller client that uploads images.
type imageUploader interface {
	LookupImage(ctx context.Context, image, localID string) (*orchv1alpha1.LookupImageResponse, error)
	UploadImage(ctx context.Context, image, localID string, archive io.Reader) (*orchv1alpha1.UploadImageResponse, error)
	ListNodes(ctx context.Context) ([]*orchv1alpha1.Node, error)
}

// localEngine reads images from the container engine on this machine.
type localEngine interface {
	inspect(ctx context.Context, image string) (localImage, bool)
	save(ctx context.Context, image string) io.ReadCloser
}

type localImage struct {
	ID           string
	OS           string
	Architecture string
	Size         int64
	// RepoDigests is empty for an image that was built here and never pushed
	// or pulled.
	RepoDigests []string
}

// uploadLocalImages pins every image that only exists in the local container
// engine to a copy uploaded to the controller, so locally built images deploy
// without a registry. An image its registry has is left for the controller to
// resolve as usual; an image the engine does not have is left alone.
func uploadLocalImages(ctx context.Context, remote imageUploader, engine localEngine, manifest *config.Manifest, progress io.Writer) error {
	if engine == nil {
		return nil
	}
	seen := map[string]bool{}
	var references []string
	for _, service := range manifest.Services {
		references = append(references, service.Image)
	}
	for _, job := range manifest.CronJobs {
		references = append(references, job.Image)
	}
	sort.Strings(references)
	pinned := map[string]string{}
	var architectures []string
	for _, image := range references {
		if seen[image] {
			continue
		}
		seen[image] = true
		named, err := reference.ParseNormalizedNamed(image)
		if err != nil {
			continue
		}
		if _, ok := named.(reference.Digested); ok {
			continue
		}
		local, ok := engine.inspect(ctx, image)
		if !ok {
			continue
		}
		// An image that was never pushed or pulled cannot be in a registry,
		// so the controller only checks the registry for the others.
		registryImage := ""
		if len(local.RepoDigests) != 0 {
			registryImage = image
		}
		found, err := remote.LookupImage(ctx, registryImage, local.ID)
		if connect.CodeOf(err) == connect.CodeUnimplemented {
			return nil
		}
		if err != nil {
			return fmt.Errorf("check local image %s: %w", image, err)
		}
		if found.GetInRegistry() {
			continue
		}
		if architectures == nil {
			if architectures, err = nodeArchitectures(ctx, remote); err != nil {
				return err
			}
		}
		if err := checkLocalPlatform(image, local, architectures); err != nil {
			return err
		}
		digest := found.GetDigest()
		if digest == "" {
			fmt.Fprintf(progress, "Uploading local image %s (%s)\n", image, formatImageSize(local.Size))
			archive := engine.save(ctx, image)
			uploaded, err := remote.UploadImage(ctx, image, local.ID, archive)
			_ = archive.Close()
			if err != nil {
				return fmt.Errorf("upload local image %s: %w", image, err)
			}
			digest = uploaded.GetDigest()
		} else {
			fmt.Fprintf(progress, "Using local image %s, uploaded before\n", image)
		}
		if !strings.HasPrefix(digest, "sha256:") {
			return fmt.Errorf("upload local image %s: the controller did not report its digest", image)
		}
		pinned[image] = named.Name() + "@" + digest
	}
	for name, service := range manifest.Services {
		if value, ok := pinned[service.Image]; ok {
			service.Image = value
			manifest.Services[name] = service
		}
	}
	for name, job := range manifest.CronJobs {
		if value, ok := pinned[job.Image]; ok {
			job.Image = value
			manifest.CronJobs[name] = job
		}
	}
	return nil
}

func nodeArchitectures(ctx context.Context, remote imageUploader) ([]string, error) {
	nodes, err := remote.ListNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	set := map[string]bool{}
	for _, node := range nodes {
		if architecture := node.GetArchitecture(); architecture != "" {
			set[architecture] = true
		}
	}
	architectures := []string{}
	for architecture := range set {
		architectures = append(architectures, architecture)
	}
	sort.Strings(architectures)
	return architectures, nil
}

// checkLocalPlatform catches the common mistake of building on an arm64
// laptop for amd64 servers before uploading anything.
func checkLocalPlatform(image string, local localImage, architectures []string) error {
	if len(architectures) == 0 || local.Architecture == "" {
		return nil
	}
	for _, architecture := range architectures {
		if architecture == local.Architecture && (local.OS == "" || local.OS == "linux") {
			return nil
		}
	}
	return fmt.Errorf("local image %s is built for %s/%s, but the cluster's nodes run linux/%s; rebuild it with docker build --platform linux/%s",
		image, local.OS, local.Architecture, strings.Join(architectures, ", linux/"), architectures[0])
}

func formatImageSize(size int64) string {
	switch {
	case size >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(size)/(1<<30))
	case size >= 1<<20:
		return fmt.Sprintf("%d MB", size>>20)
	default:
		return fmt.Sprintf("%d KB", size>>10)
	}
}

// commandEngine runs the docker or podman command line.
type commandEngine struct{ binary string }

// detectLocalEngine finds docker or podman, or ORCH_DEV_ENGINE when set. It
// returns nil when neither is installed, since deploying then needs no
// local images.
func detectLocalEngine() localEngine {
	candidates := []string{"docker", "podman"}
	if configured := os.Getenv("ORCH_DEV_ENGINE"); configured != "" {
		candidates = []string{configured}
	}
	for _, candidate := range candidates {
		if binary, err := exec.LookPath(candidate); err == nil {
			return commandEngine{binary: binary}
		}
	}
	return nil
}

func (e commandEngine) inspect(ctx context.Context, image string) (localImage, bool) {
	command := exec.CommandContext(ctx, e.binary, "image", "inspect", "--format",
		`{{.Id}}|{{.Os}}|{{.Architecture}}|{{.Size}}|{{json .RepoDigests}}`, image)
	output, err := command.Output()
	if err != nil {
		return localImage{}, false
	}
	fields := strings.SplitN(strings.TrimSpace(string(output)), "|", 5)
	if len(fields) != 5 {
		return localImage{}, false
	}
	size, _ := strconv.ParseInt(fields[3], 10, 64)
	local := localImage{ID: fields[0], OS: fields[1], Architecture: fields[2], Size: size}
	_ = json.Unmarshal([]byte(fields[4]), &local.RepoDigests)
	return local, local.ID != ""
}

func (e commandEngine) save(ctx context.Context, image string) io.ReadCloser {
	reader, writer := io.Pipe()
	command := exec.CommandContext(ctx, e.binary, "image", "save", image)
	var errorOutput bytes.Buffer
	command.Stdout, command.Stderr = writer, &errorOutput
	go func() {
		err := command.Run()
		if err != nil {
			message := strings.TrimSpace(errorOutput.String())
			if message == "" {
				message = err.Error()
			}
			err = errors.New(filepath.Base(e.binary) + " image save: " + message)
		}
		writer.CloseWithError(err)
	}()
	return reader
}
