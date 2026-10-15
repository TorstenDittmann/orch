package client

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/TorstenDittmann/orch/gen/orch/v1alpha1/orchv1alpha1connect"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/target"
)

// socketAuthority is the placeholder origin for requests on a unix socket.
const socketAuthority = "http://orch-admin"

// newSocketClient talks cleartext HTTP/2 to the controller's administrator
// socket, either on this host or forwarded through an SSH session.
func newSocketClient(endpoint Endpoint) (*Client, error) {
	path := endpoint.Transport.Socket
	if path == "" {
		path = target.DefaultAdminSocket
	}
	var tunnel *sshTunnel
	if endpoint.Transport.SSHHost != "" {
		var err error
		tunnel, err = startSSHSocketTunnel(endpoint.SSHBinary, endpoint.Transport.SSHHost, endpoint.Transport.SSHPort, path)
		if err != nil {
			return nil, err
		}
		path = tunnel.Address()
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			connection, err := dialer.DialContext(ctx, "unix", path)
			if err != nil {
				if endpoint.Transport.SSHHost != "" {
					return nil, fmt.Errorf("administrator socket via %s: %w", endpoint.Transport.SSHHost, err)
				}
				return nil, fmt.Errorf("administrator socket %s: %w (run as root on the controller host)", path, err)
			}
			return connection, nil
		},
		Protocols:       protocols,
		MaxIdleConns:    10,
		IdleConnTimeout: 60 * time.Second,
	}
	httpClient := &http.Client{Transport: transport}
	service := orchv1alpha1connect.NewControlServiceClient(
		httpClient, socketAuthority,
		connect.WithSendMaxBytes(maxMessageBytes),
		connect.WithReadMaxBytes(maxMessageBytes),
	)
	var local clusterctl.LocalContext
	local.Cluster.ID = endpoint.Cluster.ID
	local.Cluster.Name = endpoint.Cluster.Name
	return &Client{
		context: local, service: service, http: httpClient,
		target: endpoint.Target, mode: target.TransportSocket, tunnel: tunnel,
		controllerNotAfter: &certificateExpiry{},
	}, nil
}
