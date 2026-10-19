package control

import (
	"context"
	"errors"
	"log"
	"strconv"
	"time"

	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/store"
)

// imageUpdateInterval is how often the controller re-resolves the image tags
// of applications that set deploy.autoUpdate.
const imageUpdateInterval = 5 * time.Minute

// imageUpdateActor names automatic deploys in history.
const imageUpdateActor = "auto-update"

func (s *service) runImageUpdates(ctx context.Context) {
	ticker := time.NewTicker(imageUpdateInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.updateImages(ctx)
		}
	}
}

// updateImages redeploys every auto-updating application whose image tags
// now resolve to different digests.
func (s *service) updateImages(ctx context.Context) {
	applications, err := s.state.Applications(ctx, false)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("controller: image updates: list applications: %v", err)
		}
		return
	}
	for _, application := range applications {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.updateApplicationImages(ctx, application.Name); err != nil && ctx.Err() == nil {
			log.Printf("controller: image updates: %s: %v", application.Name, err)
		}
	}
}

// updateApplicationImages re-resolves the current deployment's authored image
// references and rolls out a new revision when a digest changed, like a manual
// deploy would. It skips rollbacks, so it never undoes an operator's choice.
// A revision that failed is not retried until a tag moves again, because the
// failed revision stays the current one and still resolves to the same digest.
func (s *service) updateApplicationImages(ctx context.Context, name string) (bool, error) {
	deployments, _, err := s.state.Deployments(ctx, name, 0, 1)
	if errors.Is(err, store.ErrApplicationNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(deployments) == 0 {
		return false, nil
	}
	current := deployments[0]
	if !current.Current || current.RollbackFromRevisionID != "" {
		return false, nil
	}
	manifest, err := config.ParseCanonicalJSON(current.RequestedManifest)
	if err != nil || !manifest.Deploy.AutoUpdate {
		return false, nil
	}
	resolved, err := s.resolveImages(ctx, manifest)
	if err != nil {
		return false, err
	}
	digest := config.DigestCanonicalJSON(resolved)
	if digest == current.Digest {
		return false, nil
	}
	key := "auto-update-" + strconv.FormatUint(current.Generation, 10) + "-" + digest[len("sha256:"):len("sha256:")+32]
	_, err = s.state.ApplyApplication(ctx, model.ApplyApplication{
		Name: name, RevisionID: newRevisionID(s.cluster.ID, key, digest), Digest: digest,
		CanonicalManifest: resolved, RequestedManifest: current.RequestedManifest, IdempotencyKey: key,
		CreatedAt: s.now().UTC(), ExpectedGeneration: current.Generation,
		Source: &model.DeploymentSource{Actor: imageUpdateActor},
	})
	if errors.Is(err, store.ErrGenerationConflict) {
		// A deploy landed meanwhile; the next pass looks at it instead.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	log.Printf("controller: image updates: %s: deploying new image digests", name)
	if s.reconciler != nil {
		if err := s.reconciler.Reconcile(ctx); err != nil {
			return true, err
		}
	}
	return true, nil
}
