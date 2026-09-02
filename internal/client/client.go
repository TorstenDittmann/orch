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
	httpClient := &http.Client{Transport: transport, Timeout: 15 * time.Second}
	service := orchv1alpha1connect.NewControlServiceClient(
		httpClient, credentials.Context.Server,
		connect.WithSendMaxBytes(maxMessageBytes),
		connect.WithReadMaxBytes(maxMessageBytes),
	)
	return &Client{context: credentials.Context, service: service, http: httpClient}, nil
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

func (client *Client) ApplyApplication(ctx context.Context, manifest []byte, idempotencyKey string) (*orchv1alpha1.ApplyApplicationResponse, error) {
	response, err := client.service.ApplyApplication(ctx, connect.NewRequest(&orchv1alpha1.ApplyApplicationRequest{
		ManifestToml: manifest, IdempotencyKey: idempotencyKey,
	}))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}
