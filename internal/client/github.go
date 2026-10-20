package client

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/gen/orch/v1alpha1/orchv1alpha1connect"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/security"
	"github.com/TorstenDittmann/orch/internal/target"
)

// GitHubReferencePrefix marks a context reference that signs in from a
// GitHub Actions run instead of reading stored credentials. The rest of the
// reference is a cluster address.
const GitHubReferencePrefix = "github-actions:"

// ClusterAddress is the non-secret "https://HOST:PORT#sha256:HEX" form that
// tells a workflow where the controller is and which CA to pin.
func ClusterAddress(endpoint string, caFingerprint []byte) string {
	return endpoint + "#sha256:" + hex.EncodeToString(caFingerprint)
}

// ParseClusterAddress splits a cluster address into its endpoint and pinned
// CA fingerprint.
func ParseClusterAddress(address string) (string, []byte, error) {
	endpoint, pin, found := strings.Cut(strings.TrimSpace(address), "#sha256:")
	server, err := url.Parse(endpoint)
	fingerprint, hexErr := hex.DecodeString(pin)
	if !found || err != nil || server.Scheme != "https" || server.Host == "" || server.Path != "" || server.User != nil ||
		hexErr != nil || len(fingerprint) != sha256.Size {
		return "", nil, errors.New("cluster address must look like https://HOST:PORT#sha256:FINGERPRINT; copy it from orch github trust")
	}
	return endpoint, fingerprint, nil
}

// NewGitHubActions signs in from a GitHub Actions run: it pins the cluster CA,
// exchanges the run's OIDC token for a short-lived deployer certificate, and
// connects with it. application may be empty when the repository is trusted
// for only one application.
func NewGitHubActions(ctx context.Context, address, application string) (*Client, error) {
	return newExchangedClient(ctx, address, "github-actions:", func(credentials orchv1alpha1connect.CredentialServiceClient, clusterID string, csrDER []byte) ([]byte, string, error) {
		token, err := githubActionsToken(ctx, os.Getenv, "orch:"+clusterID)
		if err != nil {
			return nil, "", err
		}
		response, err := credentials.ExchangeGitHubToken(ctx, connect.NewRequest(&orchv1alpha1.ExchangeGitHubTokenRequest{
			Token: token, Application: application, CertificateRequestDer: csrDER,
		}))
		if err != nil {
			return nil, "", fmt.Errorf("sign in from GitHub Actions: %w", err)
		}
		return response.Msg.GetCertificatePem(), response.Msg.GetApplication(), nil
	})
}

// NewAccessToken signs in with an ORCH_TOKEN value from orch token create:
// it pins the cluster CA the token names and exchanges the token for a
// short-lived certificate limited to the token's application, or an
// administrator certificate for an administrator token.
func NewAccessToken(ctx context.Context, value string) (*Client, error) {
	address, credential, err := security.ParseAccessToken(value)
	if err != nil {
		return nil, err
	}
	return newExchangedClient(ctx, address, "token:", func(credentials orchv1alpha1connect.CredentialServiceClient, _ string, csrDER []byte) ([]byte, string, error) {
		response, err := credentials.ExchangeAccessToken(ctx, connect.NewRequest(&orchv1alpha1.ExchangeAccessTokenRequest{
			Token: credential, CertificateRequestDer: csrDER,
		}))
		if err != nil {
			return nil, "", fmt.Errorf("sign in with ORCH_TOKEN: %w", err)
		}
		application := response.Msg.GetApplication()
		if response.Msg.GetAdministrator() {
			application = "administrator"
		}
		return response.Msg.GetCertificatePem(), application, nil
	})
}

// exchange trades proof of identity and a CSR for a certificate PEM and the
// application it is limited to.
type exchange func(credentials orchv1alpha1connect.CredentialServiceClient, clusterID string, csrDER []byte) ([]byte, string, error)

// newExchangedClient pins the CA of the cluster at address, obtains a
// short-lived certificate through exchange, and connects with it.
func newExchangedClient(ctx context.Context, address, label string, exchange exchange) (*Client, error) {
	endpoint, fingerprint, err := ParseClusterAddress(address)
	if err != nil {
		return nil, err
	}
	caPEM, clusterID, err := security.FetchPinnedCA(ctx, endpoint, fingerprint)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("controller CA is invalid")
	}
	server, _ := url.Parse(endpoint)
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: server.Hostname(),
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 || len(state.VerifiedChains) == 0 {
				return errors.New("controller certificate was not verified")
			}
			return security.VerifyPeerIdentity(state.PeerCertificates[0], clusterID, security.RoleController)
		},
	}
	httpClient := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true}}
	defer httpClient.CloseIdleConnections()
	csrDER, keyPEM, err := security.GenerateDeployerCertificateRequest(rand.Reader)
	if err != nil {
		return nil, err
	}
	certificatePEM, application, err := exchange(orchv1alpha1connect.NewCredentialServiceClient(httpClient, endpoint), clusterID, csrDER)
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("controller returned an invalid deployer certificate: %w", err)
	}
	if pair.Leaf == nil {
		if pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0]); err != nil {
			return nil, fmt.Errorf("controller returned an invalid deployer certificate: %w", err)
		}
	}
	credentials := clusterctl.AdministratorCredentials{
		Context:     clusterctl.LocalContext{Version: clusterctl.ContextVersion, Cluster: clusterctl.ContextCluster{ID: clusterID}, Server: endpoint},
		Certificate: pair, RootCAs: roots, ServerName: server.Hostname(), CACertificatePEM: caPEM,
	}
	return newCertificateClient(Endpoint{Target: label + application, Transport: target.Transport{Type: target.TransportDirect}}, credentials)
}

// githubTokenClient fetches OIDC tokens from the Actions runtime.
var githubTokenClient = &http.Client{Timeout: 30 * time.Second}

// githubActionsToken requests the run's OIDC token for one audience. GitHub
// only provides it to jobs with the id-token: write permission.
func githubActionsToken(ctx context.Context, getenv func(string) string, audience string) (string, error) {
	requestURL, bearer := getenv("ACTIONS_ID_TOKEN_REQUEST_URL"), getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if requestURL == "" || bearer == "" {
		return "", errors.New("GitHub Actions OIDC is unavailable; give the job the permission id-token: write")
	}
	parsed, err := url.Parse(requestURL)
	if err != nil || parsed.Scheme != "https" {
		return "", errors.New("ACTIONS_ID_TOKEN_REQUEST_URL must be an HTTPS URL")
	}
	query := parsed.Query()
	query.Set("audience", audience)
	parsed.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Accept", "application/json")
	response, err := githubTokenClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("request GitHub OIDC token: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("request GitHub OIDC token: %s", response.Status)
	}
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&body); err != nil || body.Value == "" {
		return "", errors.New("GitHub returned no OIDC token")
	}
	return body.Value, nil
}

func (client *Client) CreateAccessToken(ctx context.Context, name, application string, readOnly bool, lifetime time.Duration) (*orchv1alpha1.CreateAccessTokenResponse, error) {
	response, err := client.service.CreateAccessToken(ctx, connect.NewRequest(&orchv1alpha1.CreateAccessTokenRequest{
		Name: name, Application: application, ReadOnly: readOnly, TtlSeconds: uint32(lifetime / time.Second),
	}))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}

// CreateAdministratorAccessToken creates a token that grants full
// administrator access, for tools that manage the whole cluster.
func (client *Client) CreateAdministratorAccessToken(ctx context.Context, name string, lifetime time.Duration) (*orchv1alpha1.CreateAccessTokenResponse, error) {
	response, err := client.service.CreateAccessToken(ctx, connect.NewRequest(&orchv1alpha1.CreateAccessTokenRequest{
		Name: name, Administrator: true, TtlSeconds: uint32(lifetime / time.Second),
	}))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}

func (client *Client) ListAccessTokens(ctx context.Context) ([]*orchv1alpha1.AccessToken, error) {
	response, err := client.service.ListAccessTokens(ctx, connect.NewRequest(&orchv1alpha1.ListAccessTokensRequest{}))
	if err != nil {
		return nil, err
	}
	return response.Msg.GetTokens(), nil
}

func (client *Client) DeleteAccessToken(ctx context.Context, name string) (bool, error) {
	response, err := client.service.DeleteAccessToken(ctx, connect.NewRequest(&orchv1alpha1.DeleteAccessTokenRequest{Name: name}))
	if err != nil {
		return false, err
	}
	return response.Msg.GetChanged(), nil
}

func (client *Client) SetGitHubTrust(ctx context.Context, repository, application, branch, environment string) (*orchv1alpha1.SetGitHubTrustResponse, error) {
	response, err := client.service.SetGitHubTrust(ctx, connect.NewRequest(&orchv1alpha1.SetGitHubTrustRequest{
		Repository: repository, Application: application, Branch: branch, Environment: environment,
	}))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}

func (client *Client) ListGitHubTrusts(ctx context.Context) ([]*orchv1alpha1.GitHubTrust, error) {
	response, err := client.service.ListGitHubTrusts(ctx, connect.NewRequest(&orchv1alpha1.ListGitHubTrustsRequest{}))
	if err != nil {
		return nil, err
	}
	return response.Msg.GetTrusts(), nil
}

func (client *Client) DeleteGitHubTrust(ctx context.Context, repository, application string) (bool, error) {
	response, err := client.service.DeleteGitHubTrust(ctx, connect.NewRequest(&orchv1alpha1.DeleteGitHubTrustRequest{
		Repository: repository, Application: application,
	}))
	if err != nil {
		return false, err
	}
	return response.Msg.GetChanged(), nil
}
