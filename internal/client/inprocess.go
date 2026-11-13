package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/gen/orch/v1alpha1/orchv1alpha1connect"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/target"
)

// InProcessReferencePrefix marks a context reference to a connection the
// controller's remote MCP gateway opened inside this process.
const InProcessReferencePrefix = "in-process://"

var inProcess = struct {
	sync.Mutex
	dialers map[string]inProcessDialer
}{dialers: map[string]inProcessDialer{}}

type inProcessDialer struct {
	label string
	dial  func(context.Context) (net.Conn, error)
}

// RegisterInProcess makes dial reachable as a --context reference until
// release is called. The reference is random, so only the caller can use it.
func RegisterInProcess(label string, dial func(context.Context) (net.Conn, error)) (string, func()) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		panic(err)
	}
	reference := InProcessReferencePrefix + hex.EncodeToString(random)
	inProcess.Lock()
	inProcess.dialers[reference] = inProcessDialer{label: label, dial: dial}
	inProcess.Unlock()
	return reference, func() {
		inProcess.Lock()
		delete(inProcess.dialers, reference)
		inProcess.Unlock()
	}
}

func newInProcessClient(reference string) (*Client, error) {
	inProcess.Lock()
	dialer, ok := inProcess.dialers[reference]
	inProcess.Unlock()
	if !ok || !strings.HasPrefix(reference, InProcessReferencePrefix) {
		return nil, errors.New("the in-process connection has ended")
	}
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{
		Proxy:       nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dialer.dial(ctx) },
		Protocols:   protocols, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second,
	}
	httpClient := &http.Client{Transport: transport}
	options := []connect.ClientOption{connect.WithSendMaxBytes(maxMessageBytes), connect.WithReadMaxBytes(maxMessageBytes)}
	return &Client{
		context: clusterctl.LocalContext{}, http: httpClient,
		service: orchv1alpha1connect.NewControlServiceClient(httpClient, socketAuthority, options...),
		uploads: orchv1alpha1connect.NewControlServiceClient(httpClient, socketAuthority, options...),
		target:  dialer.label, mode: target.TransportSocket,
		controllerNotAfter: &certificateExpiry{},
	}, nil
}

// SetMCPDomain serves the remote MCP server on domain, or turns it off when
// domain is empty.
func (client *Client) SetMCPDomain(ctx context.Context, domain string) (bool, error) {
	response, err := client.service.SetMCPDomain(ctx, connect.NewRequest(&orchv1alpha1.SetMCPDomainRequest{Domain: domain}))
	if err != nil {
		return false, err
	}
	return response.Msg.GetChanged(), nil
}

// MCPDomain returns the remote MCP server's domain, or "" when it is off.
func (client *Client) MCPDomain(ctx context.Context) (string, error) {
	response, err := client.service.GetMCPDomain(ctx, connect.NewRequest(&orchv1alpha1.GetMCPDomainRequest{}))
	if err != nil {
		return "", err
	}
	return response.Msg.GetDomain(), nil
}

// ApproveMCPConnection lets the assistant waiting with code connect.
func (client *Client) ApproveMCPConnection(ctx context.Context, code, access, application string, lifetime time.Duration) (*orchv1alpha1.ApproveMCPConnectionResponse, error) {
	response, err := client.service.ApproveMCPConnection(ctx, connect.NewRequest(&orchv1alpha1.ApproveMCPConnectionRequest{
		Code: code, Access: access, Application: application, TtlSeconds: uint32(lifetime / time.Second),
	}))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}
