package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	"change.codes/torsten/better-swarm/gen/orch/v1alpha1/orchv1alpha1connect"
	clusterctl "change.codes/torsten/better-swarm/internal/cluster"
	"change.codes/torsten/better-swarm/internal/security"
	"connectrpc.com/connect"
)

const maxMessageBytes = 2 << 20

type Client struct {
	context clusterctl.LocalContext
	service orchv1alpha1connect.ControlServiceClient
	http    *http.Client
}

// New loads a protected administrator context and creates an mTLS client.
func New(contextPath string) (*Client, error) {
	credentials, err := clusterctl.LoadAdministratorCredentials(contextPath, time.Now())
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
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout: 5 * time.Second, KeepAlive: 30 * time.Second,
		}).DialContext,
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
	return &Client{context: credentials.Context, service: service, http: httpClient}, nil
}

func (client *Client) Context() clusterctl.LocalContext {
	if client == nil {
		return clusterctl.LocalContext{}
	}
	return client.context
}

func (client *Client) CloseIdleConnections() {
	if client != nil && client.http != nil {
		client.http.CloseIdleConnections()
	}
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

func (client *Client) ListEvents(ctx context.Context, after uint64, limit uint32) (*orchv1alpha1.ListEventsResponse, error) {
	response, err := client.service.ListEvents(ctx, connect.NewRequest(&orchv1alpha1.ListEventsRequest{
		AfterSequence: after, Limit: limit,
	}))
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
	return client.service.TailLogs(ctx, connect.NewRequest(&orchv1alpha1.TailLogsRequest{
		Application: application, Service: service, Replica: replica, TailLines: tailLines,
	}))
}

func (client *Client) Exec(ctx context.Context, application, service string, replica uint32, command []string, timeoutSeconds uint32) (*connect.ServerStreamForClient[orchv1alpha1.ExecResponse], error) {
	return client.service.Exec(ctx, connect.NewRequest(&orchv1alpha1.ExecRequest{
		Application: application, Service: service, Replica: replica,
		Command: append([]string(nil), command...), TimeoutSeconds: timeoutSeconds,
	}))
}
