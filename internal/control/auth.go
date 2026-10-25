package control

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/TorstenDittmann/orch/internal/security"
	"github.com/TorstenDittmann/orch/internal/store"
)

type principalKey struct{}
type agentPrincipalKey struct{}

type authenticatedAgent struct {
	nodeID string
	serial string
	slot   string
	err    error
	code   connect.Code
}

type authenticatedPrincipal struct {
	certificate *x509.Certificate
	// local is set only for requests on the controller's admin socket.
	local bool
	// application is set for deployer certificates, which may only read and
	// change this one application.
	application string
	// readOnly is set for viewer certificates, which may only read it.
	readOnly bool
	// domains limits the domains a deployer may route; empty means any.
	domains []string
	err     error
	code    connect.Code
}

func authenticateAgent(clusterID string, state *store.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		principal := authenticatedAgent{}
		if request.TLS == nil || len(request.TLS.PeerCertificates) == 0 || len(request.TLS.VerifiedChains) == 0 {
			principal.err = errors.New("verified agent client certificate is required")
			principal.code = connect.CodeUnauthenticated
		} else {
			certificate := request.TLS.PeerCertificates[0]
			nodeID, identityErr := security.AgentNodeID(certificate, clusterID)
			if identityErr != nil {
				principal.err = identityErr
				principal.code = connect.CodePermissionDenied
				ctx := context.WithValue(request.Context(), agentPrincipalKey{}, principal)
				next.ServeHTTP(response, request.WithContext(ctx))
				return
			}
			node, err := state.NodeByID(request.Context(), nodeID)
			if err != nil {
				principal.err = errors.New("no agent is enrolled")
				principal.code = connect.CodePermissionDenied
			} else if node.Lifecycle == "REMOVED" {
				principal.err = errors.New("agent node was removed")
				principal.code = connect.CodePermissionDenied
			} else {
				if err := security.VerifyAgentPeerIdentity(certificate, clusterID, node.ID); err != nil {
					principal.err = err
					principal.code = connect.CodePermissionDenied
				} else {
					fingerprint, err := security.CertificatePublicKeyHash(certificate)
					slot, slotErr := state.AgentCertificateSlot(request.Context(), nodeID, certificate.SerialNumber.String(), fingerprint)
					if err != nil || slotErr != nil {
						principal.err = errors.New("agent certificate does not match enrolled identity")
						principal.code = connect.CodePermissionDenied
					} else {
						principal.nodeID = node.ID
						principal.serial = certificate.SerialNumber.String()
						principal.slot = slot
					}
				}
			}
		}
		ctx := context.WithValue(request.Context(), agentPrincipalKey{}, principal)
		next.ServeHTTP(response, request.WithContext(ctx))
	})
}

func requireAgentCredential(ctx context.Context) (authenticatedAgent, error) {
	principal, ok := ctx.Value(agentPrincipalKey{}).(authenticatedAgent)
	if !ok {
		return authenticatedAgent{}, connect.NewError(connect.CodeUnauthenticated, errors.New("agent authentication was not evaluated"))
	}
	if principal.err != nil {
		return authenticatedAgent{}, connect.NewError(principal.code, principal.err)
	}
	if principal.nodeID == "" || principal.serial == "" || principal.slot == "" {
		return authenticatedAgent{}, connect.NewError(connect.CodeUnauthenticated, errors.New("agent identity is empty"))
	}
	return principal, nil
}

func requireAgent(ctx context.Context) (string, error) {
	principal, err := requireAgentCredential(ctx)
	return principal.nodeID, err
}

// authenticateAdministrator refuses administrator certificates that became
// valid before notBefore, which orch cluster certificates renew moves forward
// to retire every earlier context.
func authenticateAdministrator(clusterID string, notBefore time.Time, next http.Handler) http.Handler {
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
				application, readOnly, deployerErr := security.ApplicationCredential(principal.certificate, clusterID)
				if deployerErr != nil {
					principal.err = err
					principal.code = connect.CodePermissionDenied
				} else {
					principal.application, principal.readOnly = application, readOnly
					principal.domains = security.ApplicationDomains(principal.certificate)
				}
			} else if principal.certificate.NotBefore.Before(notBefore) {
				principal.err = errors.New("administrator certificate was retired by orch cluster certificates renew; import the new context")
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
	if principal.certificate == nil && !principal.local {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("administrator client certificate is required"))
	}
	if principal.application != "" {
		return connect.NewError(connect.CodePermissionDenied, errors.New("deployer credentials may only deploy their own application"))
	}
	return nil
}

// requireDeployerOrAdministrator authenticates either role. It returns the
// application a deployer is limited to, or "" for administrators.
func requireDeployerOrAdministrator(ctx context.Context) (string, error) {
	principal, ok := ctx.Value(principalKey{}).(authenticatedPrincipal)
	if ok && principal.err == nil && principal.certificate != nil && principal.application != "" {
		return principal.application, nil
	}
	return "", requireAdministrator(ctx)
}

// requireApplicationAccess admits administrators and deployers whose
// certificate names this application.
func requireApplicationAccess(ctx context.Context, application string) error {
	allowed, err := requireDeployerOrAdministrator(ctx)
	if err != nil {
		return err
	}
	if allowed != "" && allowed != application {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("deployer credentials are limited to application %s", allowed))
	}
	return nil
}

// requireChange rejects read-only credentials on requests that change an
// application. Call it after authenticating the request.
func requireChange(ctx context.Context) error {
	if principal, ok := ctx.Value(principalKey{}).(authenticatedPrincipal); ok && principal.readOnly {
		return connect.NewError(connect.CodePermissionDenied, errors.New("this access token is read-only"))
	}
	return nil
}

// requireAgentHTTP rejects plain HTTP requests that authenticateAgent did not
// admit, for handlers outside Connect.
func requireAgentHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if _, err := requireAgent(request.Context()); err != nil {
			http.Error(response, "agent credentials are required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(response, request)
	})
}
