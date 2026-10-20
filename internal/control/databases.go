package control

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/store"
)

// ensureDatabaseSecrets creates the passwords and connection URLs of the
// manifest's databases that do not exist yet. Existing values are kept, so a
// database keeps the password it was initialized with across deploys.
func (s *service) ensureDatabaseSecrets(ctx context.Context, manifest *config.Manifest) error {
	s.databaseSecrets.Lock()
	defer s.databaseSecrets.Unlock()
	for _, name := range sortedServiceNames(*manifest) {
		database := manifest.Services[name].Database
		if database == nil {
			continue
		}
		existing := map[string]string{}
		for _, secret := range config.DatabaseSecretNames(name, database.Engine) {
			resolved, err := s.state.ResolveApplicationSecret(ctx, s.secretBox, manifest.Name, secret)
			if errors.Is(err, store.ErrSecretNotFound) {
				continue
			}
			if err != nil {
				return connect.NewError(connect.CodeInternal, fmt.Errorf("read secret %s", secret))
			}
			existing[secret] = string(resolved.Value)
			wipeBytes(resolved.Value)
		}
		host := name + "." + manifest.Name + ".svc.internal"
		created, err := config.GenerateDatabaseSecrets(name, database.Engine, host, existing, s.random)
		if err != nil {
			return connect.NewError(connect.CodeInternal, fmt.Errorf("generate secrets for database %s", name))
		}
		for _, secret := range config.DatabaseSecretNames(name, database.Engine) {
			value, ok := created[secret]
			if !ok {
				continue
			}
			versionID, err := newOpaqueID("secv_", s.random)
			if err != nil {
				return connect.NewError(connect.CodeInternal, errors.New("generate secret version"))
			}
			idempotencyKey, err := newOpaqueID("dbsecret_", s.random)
			if err != nil {
				return connect.NewError(connect.CodeInternal, errors.New("generate secret version"))
			}
			if _, err := s.state.SetApplicationSecret(ctx, s.secretBox, manifest.Name, secret, versionID, []byte(value), idempotencyKey, s.now().UTC()); err != nil {
				return connect.NewError(connect.CodeInternal, fmt.Errorf("store secret %s", secret))
			}
		}
	}
	return nil
}
