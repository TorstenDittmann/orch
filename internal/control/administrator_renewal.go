package control

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/security"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// minimumRenewableAdministratorLifetime separates administrator context
// certificates, which last a year, from the 15-minute certificates access
// tokens exchange for. Only the former may renew themselves: a token holder
// must not be able to mint a credential that outlives the token's revocation.
const minimumRenewableAdministratorLifetime = 180 * 24 * time.Hour

// RenewAdministratorCertificate issues a fresh one-year administrator
// certificate to a caller that holds a valid one, so contexts on laptops and
// CI keep working without anyone running a renewal on the controller host.
// It does not retire the caller's current certificate; orch cluster
// certificates renew is still how every earlier one is locked out.
func (s *service) RenewAdministratorCertificate(ctx context.Context, request *connect.Request[orchv1alpha1.RenewAdministratorCertificateRequest]) (*connect.Response[orchv1alpha1.RenewAdministratorCertificateResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownMessage(request.Msg); err != nil {
		return nil, err
	}
	principal, _ := ctx.Value(principalKey{}).(authenticatedPrincipal)
	if principal.certificate == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("only an administrator certificate can be renewed; the local socket needs none"))
	}
	if principal.certificate.NotAfter.Sub(principal.certificate.NotBefore) < minimumRenewableAdministratorLifetime {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("access token certificates cannot be renewed; create a new token instead"))
	}
	if len(s.caPEM) == 0 || len(s.caKey) == 0 {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("controller cannot issue certificates"))
	}
	random := s.random
	if random == nil {
		random = rand.Reader
	}
	pair, err := security.IssueIdentity(s.caPEM, s.caKey, security.IssueOptions{
		ClusterID: s.cluster.ID, Role: security.RoleAdministrator, Now: s.now().UTC(), Random: random,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("issue administrator certificate"))
	}
	notAfter, _ := security.CertificateNotAfter(pair.CertificatePEM)
	return connect.NewResponse(&orchv1alpha1.RenewAdministratorCertificateResponse{
		CertificatePem: pair.CertificatePEM, PrivateKeyPem: pair.PrivateKeyPEM, ExpiresAt: timestamppb.New(notAfter),
	}), nil
}
