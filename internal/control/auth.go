package control

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"

	"change.codes/torsten/better-swarm/internal/security"
	"connectrpc.com/connect"
)

type principalKey struct{}

type authenticatedPrincipal struct {
	certificate *x509.Certificate
	err         error
	code        connect.Code
}

func authenticateAdministrator(clusterID string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		principal := authenticatedPrincipal{}
		switch {
		case request.TLS == nil || len(request.TLS.PeerCertificates) == 0:
			principal.err = errors.New("administrator client certificate is required")
			principal.code = connect.CodeUnauthenticated
		case len(request.TLS.VerifiedChains) == 0:
			principal.err = errors.New("administrator client certificate was not verified")
			principal.code = connect.CodeUnauthenticated
		default:
			principal.certificate = request.TLS.PeerCertificates[0]
			if err := security.VerifyPeerIdentity(principal.certificate, clusterID, security.RoleAdministrator); err != nil {
				principal.err = err
				principal.code = connect.CodePermissionDenied
			}
		}
		ctx := context.WithValue(request.Context(), principalKey{}, principal)
		next.ServeHTTP(response, request.WithContext(ctx))
	})
}

func requireAdministrator(ctx context.Context) error {
	principal, ok := ctx.Value(principalKey{}).(authenticatedPrincipal)
	if !ok {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("administrator authentication was not evaluated"))
	}
	if principal.err != nil {
		return connect.NewError(principal.code, principal.err)
	}
	if principal.certificate == nil {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("administrator client certificate is required"))
	}
	return nil
}
