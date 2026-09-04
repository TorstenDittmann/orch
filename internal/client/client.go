package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	"change.codes/torsten/better-swarm/gen/orch/v1alpha1/orchv1alpha1connect"
	clusterctl "change.codes/torsten/better-swarm/internal/cluster"
	"change.codes/torsten/better-swarm/internal/security"
	"change.codes/torsten/better-swarm/internal/target"
	"connectrpc.com/connect"
)

const maxMessageBytes = 2 << 20

type Client struct {
	context clusterctl.LocalContext
	service orchv1alpha1connect.ControlServiceClient
	http    *http.Client
	target  string
	mode    string
	tunnel  *sshTunnel
	close   sync.Once
}

// New loads a protected administrator context and creates an mTLS client.
func New(contextPath string) (*Client, error) {
	endpoint := Endpoint{ContextPath: contextPath, Transport: target.Transport{Type: target.TransportDirect}}
	if target.IsReference(contextPath) {
		store, err := target.Default()
		if err != nil {
			return nil, err
		}
		resolved, err := store.ResolveReference(contextPath)
		if err != nil {
			return nil, err
		}
		endpoint = Endpoint{ContextPath: resolved.ContextPath, Target: resolved.Name, Transport: resolved.Transport}
	}
	return NewEndpoint(endpoint)
}

type Endpoint struct {
	ContextPath string
	Target      string
	Transport   target.Transport
	SSHBinary   string
}

// NewEndpoint creates an mTLS client whose route may differ from its canonical
// HTTPS authority. TLS always verifies the server name from the context.
func NewEndpoint(endpoint Endpoint) (*Client, error) {
	if endpoint.Transport.Type == "" {
		endpoint.Transport.Type = target.TransportDirect
	}
	if err := target.ValidateTransport(endpoint.Transport); err != nil {
		return nil, err
	}
	credentials, err := clusterctl.LoadAdministratorCredentials(endpoint.ContextPath, time.Now())
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		RootCAs:      credentials.RootCAs,
		Certificates: []tls.Certificate{credentials.Certificate},
		ServerName:   credentials.ServerName,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 || len(state.VerifiedChains) == 0 {
				return fmt.Errorf("controller certificate was not verified")
			}
			if err := security.VerifyPeerIdentity(state.PeerCertificates[0], credentials.Context.Cluster.ID, security.RoleController); err != nil {
				return fmt.Errorf("validate controller identity: %w", err)
			}
			return nil
		},
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	dialContext := dialer.DialContext
	proxy := http.ProxyFromEnvironment
	var tunnel *sshTunnel
	switch endpoint.Transport.Type {
	case target.TransportWG:
		address := endpoint.Transport.Address
		dialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			connection, err := dialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, fmt.Errorf("wireguard route %s: %w", address, err)
			}
			return connection, nil
		}
		proxy = nil
	case target.TransportSSH:
		destination := endpoint.Transport.SSHDestination
		if destination == "" {
			serverURL, parseErr := url.Parse(credentials.Context.Server)
			if parseErr != nil {
				return nil, fmt.Errorf("parse canonical controller URL: %w", parseErr)
			}
			port := serverURL.Port()
			if port == "" {
				port = "443"
			}
			destination = net.JoinHostPort(serverURL.Hostname(), port)
		}
		tunnel, err = startSSHTunnel(endpoint.SSHBinary, endpoint.Transport.SSHHost, destination)
		if err != nil {
			return nil, err
		}
		address := tunnel.Address()
		dialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			connection, dialErr := dialer.DialContext(ctx, network, address)
			if dialErr != nil {
				return nil, fmt.Errorf("SSH tunnel via %s: %w", endpoint.Transport.SSHHost, dialErr)
			}
			return connection, nil
		}
		proxy = nil
	}
	transport := &http.Transport{
		Proxy:                 proxy,
		DialContext:           dialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       tlsConfig,
	}
	httpClient := &http.Client{Transport: transport}
	service := orchv1alpha1connect.NewControlServiceClient(
		httpClient, credentials.Context.Server,
		connect.WithSendMaxBytes(maxMessageBytes),
		connect.WithReadMaxBytes(maxMessageBytes),
	)
	return &Client{
		context: credentials.Context, service: service, http: httpClient,
		target: endpoint.Target, mode: endpoint.Transport.Type, tunnel: tunnel,
	}, nil
}

func (client *Client) Context() clusterctl.LocalContext {
	if client == nil {
		return clusterctl.LocalContext{}
	}
	return client.context
}

func (client *Client) CloseIdleConnections() {
	_ = client.Close()
}

func (client *Client) Close() error {
	if client == nil {
		return nil
	}
	var closeErr error
	client.close.Do(func() {
		if client.http != nil {
			client.http.CloseIdleConnections()
		}
		if client.tunnel != nil {
			closeErr = client.tunnel.Close()
		}
	})
	return closeErr
}

func (client *Client) Target() (string, string) {
	if client == nil {
		return "", ""
	}
	return client.target, client.mode
}

func (client *Client) GetCluster(ctx context.Context) (*orchv1alpha1.Cluster, error) {
	response, err := client.service.GetCluster(ctx, connect.NewRequest(&orchv1alpha1.GetClusterRequest{}))
	if err != nil {
		return nil, err
	}
	cluster := response.Msg.GetCluster()
	if cluster == nil || cluster.GetId() != client.context.Cluster.ID || cluster.GetName() != client.context.Cluster.Name {
		return nil, fmt.Errorf("controller cluster identity does not match administrator context")
	}
	return cluster, nil
}

func (client *Client) GetApplication(ctx context.Context, name string) (*orchv1alpha1.Application, error) {
	response, err := client.service.GetApplication(ctx, connect.NewRequest(&orchv1alpha1.GetApplicationRequest{Name: name}))
	if err != nil {
		return nil, err
	}
	if response.Msg.GetApplication() == nil {
		return nil, fmt.Errorf("controller returned an empty application")
	}
	return response.Msg.GetApplication(), nil
}

func (client *Client) ListApplications(ctx context.Context) ([]*orchv1alpha1.ApplicationSummary, error) {
	response, err := client.service.ListApplications(ctx, connect.NewRequest(&orchv1alpha1.ListApplicationsRequest{}))
	if err != nil {
		return nil, err
	}
	return response.Msg.GetApplications(), nil
}

func (client *Client) ListEvents(ctx context.Context, after uint64, limit uint32) (*orchv1alpha1.ListEventsResponse, error) {
	response, err := client.service.ListEvents(ctx, connect.NewRequest(&orchv1alpha1.ListEventsRequest{
		AfterSequence: after, Limit: limit,
	}))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}

func (client *Client) ListRecentEvents(ctx context.Context, limit uint32) (*orchv1alpha1.ListRecentEventsResponse, error) {
	response, err := client.service.ListRecentEvents(ctx, connect.NewRequest(&orchv1alpha1.ListRecentEventsRequest{Limit: limit}))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}

func (client *Client) ApplyApplication(ctx context.Context, canonicalManifest []byte, idempotencyKey string) (*orchv1alpha1.ApplyApplicationResponse, error) {
	response, err := client.service.ApplyApplication(ctx, connect.NewRequest(&orchv1alpha1.ApplyApplicationRequest{
		CanonicalManifestJson: append([]byte(nil), canonicalManifest...), IdempotencyKey: idempotencyKey,
	}))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}

func (client *Client) EnrollAgent(ctx context.Context, name string, csrDER []byte, idempotencyKey string) (*orchv1alpha1.EnrollAgentResponse, error) {
	response, err := client.service.EnrollAgent(ctx, connect.NewRequest(&orchv1alpha1.EnrollAgentRequest{
		Name: name, CertificateRequestDer: append([]byte(nil), csrDER...), IdempotencyKey: idempotencyKey,
	}))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}

func (client *Client) ListNodes(ctx context.Context) ([]*orchv1alpha1.Node, error) {
	response, err := client.service.ListNodes(ctx, connect.NewRequest(&orchv1alpha1.ListNodesRequest{}))
	if err != nil {
		return nil, err
	}
	return response.Msg.GetNodes(), nil
}

func (client *Client) UpdateNode(ctx context.Context, request *orchv1alpha1.UpdateNodeRequest) (*orchv1alpha1.UpdateNodeResponse, error) {
	response, err := client.service.UpdateNode(ctx, connect.NewRequest(request))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}

func (client *Client) RemoveNode(ctx context.Context, request *orchv1alpha1.RemoveNodeRequest) (*orchv1alpha1.RemoveNodeResponse, error) {
	response, err := client.service.RemoveNode(ctx, connect.NewRequest(request))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}

func (client *Client) CreateJoinToken(ctx context.Context, request *orchv1alpha1.CreateJoinTokenRequest) (*orchv1alpha1.CreateJoinTokenResponse, error) {
	response, err := client.service.CreateJoinToken(ctx, connect.NewRequest(request))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}

func (client *Client) ListJoinTokens(ctx context.Context) ([]*orchv1alpha1.JoinTokenMetadata, error) {
	response, err := client.service.ListJoinTokens(ctx, connect.NewRequest(&orchv1alpha1.ListJoinTokensRequest{}))
	if err != nil {
		return nil, err
	}
	return response.Msg.GetTokens(), nil
}

func (client *Client) RevokeJoinToken(ctx context.Context, id, idempotencyKey string) (*orchv1alpha1.RevokeJoinTokenResponse, error) {
	response, err := client.service.RevokeJoinToken(ctx, connect.NewRequest(&orchv1alpha1.RevokeJoinTokenRequest{Id: id, IdempotencyKey: idempotencyKey}))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}

func (client *Client) ConfigureNetwork(ctx context.Context, cidr, idempotencyKey string) (*orchv1alpha1.ConfigureNetworkResponse, error) {
	response, err := client.service.ConfigureNetwork(ctx, connect.NewRequest(&orchv1alpha1.ConfigureNetworkRequest{
		Cidr: cidr, IdempotencyKey: idempotencyKey,
	}))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}

func (client *Client) RemoveApplication(ctx context.Context, name, idempotencyKey string) (*orchv1alpha1.RemoveApplicationResponse, error) {
	response, err := client.service.RemoveApplication(ctx, connect.NewRequest(&orchv1alpha1.RemoveApplicationRequest{
		Name: name, IdempotencyKey: idempotencyKey,
	}))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}

func (client *Client) TailLogs(ctx context.Context, application, service string, replica, tailLines uint32) (*connect.ServerStreamForClient[orchv1alpha1.TailLogsResponse], error) {
	return client.TailLogsAfter(ctx, application, service, replica, tailLines, "")
}

func (client *Client) TailLogsAfter(ctx context.Context, application, service string, replica, tailLines uint32, cursor string) (*connect.ServerStreamForClient[orchv1alpha1.TailLogsResponse], error) {
	return client.service.TailLogs(ctx, connect.NewRequest(&orchv1alpha1.TailLogsRequest{
		Application: application, Service: service, Replica: replica, TailLines: tailLines, Cursor: cursor,
	}))
}

func (client *Client) ListRuntimes(ctx context.Context, limit uint32) (*orchv1alpha1.ListRuntimesResponse, error) {
	response, err := client.service.ListRuntimes(ctx, connect.NewRequest(&orchv1alpha1.ListRuntimesRequest{Limit: limit}))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}

func (client *Client) Exec(ctx context.Context, application, service string, replica uint32, command []string, timeoutSeconds uint32) (*connect.ServerStreamForClient[orchv1alpha1.ExecResponse], error) {
	return client.service.Exec(ctx, connect.NewRequest(&orchv1alpha1.ExecRequest{
		Application: application, Service: service, Replica: replica,
		Command: append([]string(nil), command...), TimeoutSeconds: timeoutSeconds,
	}))
}

func (client *Client) ListCronRuns(ctx context.Context, application, job string, limit uint32) ([]*orchv1alpha1.JobRunStatus, error) {
	response, err := client.service.ListCronRuns(ctx, connect.NewRequest(&orchv1alpha1.ListCronRunsRequest{Application: application, Job: job, Limit: limit}))
	if err != nil {
		return nil, err
	}
	return response.Msg.GetRuns(), nil
}

func (client *Client) TailCronLogs(ctx context.Context, application, job, runID string, tailLines uint32) (*connect.ServerStreamForClient[orchv1alpha1.TailCronLogsResponse], error) {
	return client.TailCronLogsAfter(ctx, application, job, runID, tailLines, "")
}

func (client *Client) TailCronLogsAfter(ctx context.Context, application, job, runID string, tailLines uint32, cursor string) (*connect.ServerStreamForClient[orchv1alpha1.TailCronLogsResponse], error) {
	return client.service.TailCronLogs(ctx, connect.NewRequest(&orchv1alpha1.TailCronLogsRequest{
		Application: application, Job: job, RunId: runID, TailLines: tailLines, Cursor: cursor,
	}))
}

func (client *Client) SetSecret(ctx context.Context, application, name string, value []byte, idempotencyKey string) (*orchv1alpha1.SetSecretResponse, error) {
	response, err := client.service.SetSecret(ctx, connect.NewRequest(&orchv1alpha1.SetSecretRequest{
		Application: application, Name: name, Value: value, IdempotencyKey: idempotencyKey,
	}))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}

func (client *Client) ListSecrets(ctx context.Context, application string) ([]*orchv1alpha1.SecretMetadata, error) {
	response, err := client.service.ListSecrets(ctx, connect.NewRequest(&orchv1alpha1.ListSecretsRequest{Application: application}))
	if err != nil {
		return nil, err
	}
	return response.Msg.GetSecrets(), nil
}

func (client *Client) DeleteSecret(ctx context.Context, application, name, idempotencyKey string) (*orchv1alpha1.DeleteSecretResponse, error) {
	response, err := client.service.DeleteSecret(ctx, connect.NewRequest(&orchv1alpha1.DeleteSecretRequest{
		Application: application, Name: name, IdempotencyKey: idempotencyKey,
	}))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}

func (client *Client) LoginRegistry(ctx context.Context, host, username string, secret []byte, idempotencyKey string) (*orchv1alpha1.LoginRegistryResponse, error) {
	response, err := client.service.LoginRegistry(ctx, connect.NewRequest(&orchv1alpha1.LoginRegistryRequest{
		Host: host, Username: username, Secret: secret, IdempotencyKey: idempotencyKey,
	}))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}

func (client *Client) ListRegistries(ctx context.Context) ([]*orchv1alpha1.RegistryCredentialMetadata, error) {
	response, err := client.service.ListRegistries(ctx, connect.NewRequest(&orchv1alpha1.ListRegistriesRequest{}))
	if err != nil {
		return nil, err
	}
	return response.Msg.GetRegistries(), nil
}

func (client *Client) LogoutRegistry(ctx context.Context, host, idempotencyKey string) (*orchv1alpha1.LogoutRegistryResponse, error) {
	response, err := client.service.LogoutRegistry(ctx, connect.NewRequest(&orchv1alpha1.LogoutRegistryRequest{Host: host, IdempotencyKey: idempotencyKey}))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}
