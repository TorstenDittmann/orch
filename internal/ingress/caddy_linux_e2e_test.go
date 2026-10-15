//go:build linux

package ingress

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
	"golang.org/x/net/websocket"
)

func TestCaddyRealHTTPReloadRollbackAndAdoption(t *testing.T) {
	if os.Getenv("ORCH_CADDY_E2E") != "1" {
		t.Skip("use scripts/network-e2e.sh on a disposable Linux host")
	}
	if os.Geteuid() != 0 {
		t.Fatal("the real Caddy lifecycle test requires root to launch the dedicated identity")
	}
	binary := requiredCaddyEnvironment(t, "ORCH_CADDY_BINARY")
	userName := requiredCaddyEnvironment(t, "ORCH_CADDY_USER")
	backendAddress := requiredCaddyEnvironment(t, "ORCH_CADDY_BACKEND_ADDRESS")
	account, err := user.Lookup(userName)
	if err != nil {
		t.Fatal(err)
	}
	uid, uidErr := strconv.Atoi(account.Uid)
	gid, gidErr := strconv.Atoi(account.Gid)
	if uidErr != nil || gidErr != nil || uid == 0 || gid == 0 {
		t.Fatalf("Caddy identity %q must have non-root numeric UID/GID", userName)
	}

	root := t.TempDir()
	if err := os.Chmod(root, 0o711); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(root, "caddy-data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dataDir, uid, gid); err != nil {
		t.Fatal(err)
	}
	options := CaddyOptions{
		Binary: binary, DataDir: dataDir, StateDir: filepath.Join(root, "agent-state"), User: userName,
		AdminSocket: filepath.Join(dataDir, "admin.sock"),
	}
	caddy, err := NewCaddy(options)
	if err != nil {
		t.Fatal(err)
	}
	if version, err := caddy.Preflight(context.Background()); err != nil || version != RequiredCaddyVersion {
		t.Fatalf("Caddy preflight = %q, %v", version, err)
	}
	defer stopManagedCaddy(t, caddy)

	firstAddress, closeFirst := startPrivateBackend(t, backendAddress, "first")
	defer closeFirst()
	route := Route{Application: "e2e", Service: "web", Domain: "caddy-http.orch.invalid", Upstreams: []string{firstAddress}}
	assertAppliedRoute(t, caddy, route)
	if body := requestManagedRoute(t, route.Domain); body != "first" {
		t.Fatalf("first route response = %q", body)
	}

	secondAddress, closeSecond := startPrivateBackend(t, backendAddress, "second")
	defer closeSecond()
	route.Upstreams = []string{secondAddress}
	assertAppliedRoute(t, caddy, route)
	if body := requestManagedRoute(t, route.Domain); body != "second" {
		t.Fatalf("reloaded route response = %q", body)
	}

	route.Upstreams = []string{firstAddress, secondAddress}
	assertAppliedRoute(t, caddy, route)
	seen := map[string]bool{}
	for attempt := 0; attempt < 64 && len(seen) != 2; attempt++ {
		seen[requestManagedRoute(t, route.Domain)] = true
	}
	if !seen["first"] || !seen["second"] {
		t.Fatalf("balanced route reached %#v; want both upstreams", seen)
	}

	behaviorAddress, releaseStream, closeBehavior := startBehaviorBackend(t, backendAddress)
	defer closeBehavior()
	route.Upstreams = []string{behaviorAddress}
	assertAppliedRoute(t, caddy, route)
	assertManagedStreaming(t, route.Domain, releaseStream)
	assertManagedWebSocket(t, route.Domain)

	route.Upstreams = []string{secondAddress}
	assertAppliedRoute(t, caddy, route)

	failingAddress, closeFailing := startPrivateBackendWithStatus(t, backendAddress, "candidate", http.StatusServiceUnavailable)
	defer closeFailing()
	candidate := route
	candidate.Upstreams = []string{failingAddress}
	probeContext, cancelProbe := context.WithTimeout(context.Background(), 2*time.Second)
	statuses, err := caddy.Apply(probeContext, []Route{candidate}, time.Now())
	cancelProbe()
	if err != nil || len(statuses) != 1 || statuses[0].State != model.RouteFailed || statuses[0].Reason != "route_probe_failed" {
		t.Fatalf("failed candidate = %#v, %v", statuses, err)
	}
	// Caddy accepted the candidate, so it stays loaded and only this route
	// reports the failing upstream.
	failingContext, cancelFailing := context.WithTimeout(context.Background(), 2*time.Second)
	failingResponse, err := managedHTTPRequest(failingContext, route.Domain, "/")
	cancelFailing()
	if err != nil {
		t.Fatal(err)
	}
	_ = failingResponse.Body.Close()
	if failingResponse.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("kept candidate route response = %s", failingResponse.Status)
	}
	assertAppliedRoute(t, caddy, route)
	if body := requestManagedRoute(t, route.Domain); body != "second" {
		t.Fatalf("route was not reapplied after candidate failure: %q", body)
	}

	adopter, err := NewCaddy(options)
	if err != nil {
		t.Fatal(err)
	}
	defer adopter.Close()
	if _, err := adopter.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertAppliedRoute(t, adopter, route)
	if adopter.process != nil {
		t.Fatal("adoption started a duplicate Caddy process")
	}
	if body := requestManagedRoute(t, route.Domain); body != "second" {
		t.Fatalf("adopted route response = %q", body)
	}
}

func TestCaddyRealHTTPSWithIsolatedPebble(t *testing.T) {
	if os.Getenv("ORCH_CADDY_E2E") != "1" {
		t.Skip("use scripts/network-e2e.sh on a disposable Linux host")
	}
	if os.Geteuid() != 0 {
		t.Fatal("the isolated ACME test requires root to launch the dedicated Caddy identity")
	}
	binary := requiredCaddyEnvironment(t, "ORCH_CADDY_BINARY")
	pebbleBinary := requiredCaddyEnvironment(t, "ORCH_PEBBLE_BINARY")
	userName := requiredCaddyEnvironment(t, "ORCH_CADDY_USER")
	backendAddress := requiredCaddyEnvironment(t, "ORCH_CADDY_BACKEND_ADDRESS")
	account, err := user.Lookup(userName)
	if err != nil {
		t.Fatal(err)
	}
	uid, uidErr := strconv.Atoi(account.Uid)
	gid, gidErr := strconv.Atoi(account.Gid)
	if uidErr != nil || gidErr != nil || uid == 0 || gid == 0 {
		t.Fatalf("Caddy identity %q must have non-root numeric UID/GID", userName)
	}

	root := t.TempDir()
	if err := os.Chmod(root, 0o711); err != nil {
		t.Fatal(err)
	}
	pebble := startIsolatedPebble(t, pebbleBinary, root)
	dataDir := filepath.Join(root, "caddy-data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dataDir, uid, gid); err != nil {
		t.Fatal(err)
	}
	options := CaddyOptions{
		Binary: binary, DataDir: dataDir, StateDir: filepath.Join(root, "agent-state"), User: userName,
		AdminSocket: filepath.Join(dataDir, "admin.sock"), Probe: trustedHTTPSProbe(pebble.issuanceRoots),
		testIssuer: &testACMEIssuer{directoryURL: pebble.directoryURL, trustedRootPEM: pebble.serverRootPEM},
	}
	caddy, err := NewCaddy(options)
	if err != nil {
		t.Fatal(err)
	}
	if version, err := caddy.Preflight(context.Background()); err != nil || version != RequiredCaddyVersion {
		t.Fatalf("Caddy preflight = %q, %v", version, err)
	}
	t.Cleanup(func() { stopManagedCaddy(t, caddy) })

	backend, closeBackend := startPrivateBackend(t, backendAddress, "issued-by-pebble")
	defer closeBackend()
	route := Route{Application: "e2e", Service: "web", Domain: "caddy-acme.example.com", HTTPS: true,
		Upstreams: []string{backend}}
	assertAppliedRoute(t, caddy, route)
	assertManagedHTTPSRedirect(t, route.Domain)
	body, firstCertificate := requestManagedHTTPSRoute(t, route.Domain, pebble.issuanceRoots)
	if body != "issued-by-pebble" {
		t.Fatalf("HTTPS route response = %q", body)
	}

	// A process restart must adopt the accepted config and durable certificate
	// storage rather than obtaining a replacement certificate.
	stopManagedCaddy(t, caddy)
	restarted, err := NewCaddy(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopManagedCaddy(t, restarted) })
	assertAppliedRoute(t, restarted, route)
	body, restartedCertificate := requestManagedHTTPSRoute(t, route.Domain, pebble.issuanceRoots)
	if body != "issued-by-pebble" {
		t.Fatalf("restarted HTTPS route response = %q", body)
	}
	if !bytes.Equal(firstCertificate, restartedCertificate) {
		t.Fatal("Caddy replaced the persisted certificate during process adoption")
	}
}

type isolatedPebble struct {
	directoryURL  string
	serverRootPEM string
	issuanceRoots *x509.CertPool
}

func startIsolatedPebble(t *testing.T, binary, root string) isolatedPebble {
	t.Helper()
	pebbleDir := filepath.Join(root, "pebble")
	if err := os.Mkdir(pebbleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	serverRootPEM, serverCertificate, serverKey := writePebbleServerPKI(t, pebbleDir)
	acmeAddress, managementAddress := reserveLoopbackAddresses(t)
	configuration := map[string]any{"pebble": map[string]any{
		"listenAddress": acmeAddress, "managementListenAddress": managementAddress,
		"certificate": serverCertificate, "privateKey": serverKey,
		"httpPort": 5002, "tlsPort": 5001, "ocspResponderURL": "",
		"externalAccountBindingRequired": false, "keyAlgorithm": "ecdsa",
		"retryAfter": map[string]int{"authz": 0, "order": 0},
	}}
	encoded, err := json.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(pebbleDir, "pebble-config.json")
	if err := os.WriteFile(configPath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(pebbleDir, "pebble.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "-config", configPath, "-strict=false")
	command.Stdout = logFile
	command.Stderr = logFile
	command.Env = append(os.Environ(), "PEBBLE_VA_ALWAYS_VALID=1", "PEBBLE_VA_NOSLEEP=1", "PEBBLE_WFE_NONCEREJECT=0")
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("start isolated Pebble: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			if command.Process != nil {
				_ = command.Process.Kill()
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Errorf("isolated Pebble did not stop; inspect %s", logPath)
			}
			_ = logFile.Close()
		})
	}
	t.Cleanup(stop)

	serverRoots := x509.NewCertPool()
	serverPEM, err := os.ReadFile(serverRootPEM)
	if err != nil || !serverRoots.AppendCertsFromPEM(serverPEM) {
		t.Fatalf("load isolated Pebble server root: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12, RootCAs: serverRoots,
	}}, Timeout: time.Second}
	directoryURL := "https://" + acmeAddress + "/dir"
	if err := waitForPebble(client, directoryURL, 10*time.Second); err != nil {
		stop()
		contents, _ := os.ReadFile(logPath)
		t.Fatalf("isolated Pebble did not become ready: %v\n%s", err, contents)
	}
	response, err := client.Get("https://" + managementAddress + "/roots/0")
	if err != nil {
		t.Fatal(err)
	}
	issuancePEM, readErr := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || len(issuancePEM) > 1<<20 {
		t.Fatalf("read isolated Pebble issuance root: status=%s err=%v", response.Status, readErr)
	}
	issuanceRoots := x509.NewCertPool()
	if !issuanceRoots.AppendCertsFromPEM(issuancePEM) {
		t.Fatal("isolated Pebble returned an invalid issuance root")
	}
	return isolatedPebble{directoryURL: directoryURL, serverRootPEM: serverRootPEM, issuanceRoots: issuanceRoots}
}

func writePebbleServerPKI(t *testing.T, directory string) (string, string, string) {
	t.Helper()
	now := time.Now().UTC()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "orch isolated Pebble server root"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), DNSNames: []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, rootTemplate, &leafKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(directory, "server-root.pem")
	certificatePath := filepath.Join(directory, "server.pem")
	keyPath := filepath.Join(directory, "server-key.pem")
	writePEMFile(t, rootPath, "CERTIFICATE", rootDER, 0o444)
	writePEMFile(t, certificatePath, "CERTIFICATE", leafDER, 0o444)
	writePEMFile(t, keyPath, "EC PRIVATE KEY", leafKeyDER, 0o600)
	return rootPath, certificatePath, keyPath
}

func writePEMFile(t *testing.T, path, blockType string, contents []byte, mode os.FileMode) {
	t.Helper()
	encoded := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: contents})
	if err := os.WriteFile(path, encoded, mode); err != nil {
		t.Fatal(err)
	}
}

func reserveLoopbackAddresses(t *testing.T) (string, string) {
	t.Helper()
	first, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	return first.Addr().String(), second.Addr().String()
}

func waitForPebble(client *http.Client, endpoint string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		response, err := client.Get(endpoint)
		if err == nil {
			_, _ = io.CopyN(io.Discard, response.Body, 4<<10)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("directory returned %s", response.Status)
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return lastErr
}

func trustedHTTPSProbe(roots *x509.CertPool) func(context.Context, Route) error {
	return func(ctx context.Context, route Route) error {
		response, err := managedHTTPSRequest(ctx, route.Domain, roots)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		_, _ = io.CopyN(io.Discard, response.Body, 4<<10)
		if response.StatusCode >= 500 {
			return &routeStatusError{domain: route.Domain, status: response.StatusCode}
		}
		return nil
	}
}

func assertManagedHTTPSRedirect(t *testing.T, domain string) {
	t.Helper()
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp4", "127.0.0.1:80")
	}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get("http://" + domain + "/redirect-check")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusPermanentRedirect || response.Header.Get("Location") != "https://"+domain+"/redirect-check" {
		t.Fatalf("HTTPS redirect = %s location=%q", response.Status, response.Header.Get("Location"))
	}
}

func requestManagedHTTPSRoute(t *testing.T, domain string, roots *x509.CertPool) (string, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := managedHTTPSRequest(ctx, domain, roots)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024))
	if err != nil || response.StatusCode != http.StatusOK || response.TLS == nil || len(response.TLS.PeerCertificates) == 0 {
		t.Fatalf("HTTPS route response = %s %q, TLS=%v, err=%v", response.Status, body, response.TLS != nil, err)
	}
	return string(body), append([]byte(nil), response.TLS.PeerCertificates[0].Raw...)
}

func managedHTTPSRequest(ctx context.Context, domain string, roots *x509.CertPool) (*http.Response, error) {
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12, ServerName: domain, RootCAs: roots,
	}}
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp4", "127.0.0.1:443")
	}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+domain+"/", nil)
	return client.Do(request)
}

func requiredCaddyEnvironment(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("%s is required", name)
	}
	return value
}

func startPrivateBackend(t *testing.T, address, body string) (string, func()) {
	t.Helper()
	return startPrivateBackendWithStatus(t, address, body, http.StatusOK)
}

func startPrivateBackendWithStatus(t *testing.T, address, body string, status int) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp4", net.JoinHostPort(address, "0"))
	if err != nil {
		t.Fatalf("listen on private test address: %v", err)
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(status)
		_, _ = io.WriteString(writer, body)
	})}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	return listener.Addr().String(), func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownContext)
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("backend exit: %v", err)
		}
	}
}

func startBehaviorBackend(t *testing.T, address string) (string, chan<- struct{}, func()) {
	t.Helper()
	listener, err := net.Listen("tcp4", net.JoinHostPort(address, "0"))
	if err != nil {
		t.Fatalf("listen on private behavior address: %v", err)
	}
	release := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "behavior")
	})
	mux.HandleFunc("/stream", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "first\n")
		writer.(http.Flusher).Flush()
		select {
		case <-release:
		case <-request.Context().Done():
			return
		}
		_, _ = io.WriteString(writer, "second\n")
		writer.(http.Flusher).Flush()
	})
	mux.Handle("/socket", websocket.Handler(func(connection *websocket.Conn) {
		var input string
		if websocket.Message.Receive(connection, &input) == nil {
			_ = websocket.Message.Send(connection, "echo:"+input)
		}
	}))
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: mux}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	return listener.Addr().String(), release, func() {
		select {
		case release <- struct{}{}:
		default:
		}
		shutdownContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownContext)
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("behavior backend exit: %v", err)
		}
	}
}

func assertManagedStreaming(t *testing.T, domain string, release chan<- struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response, err := managedHTTPRequest(ctx, domain, "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadString('\n')
	if err != nil || first != "first\n" {
		t.Fatalf("first streamed chunk = %q, %v", first, err)
	}
	release <- struct{}{}
	second, err := reader.ReadString('\n')
	if err != nil || second != "second\n" {
		t.Fatalf("second streamed chunk = %q, %v", second, err)
	}
}

func assertManagedWebSocket(t *testing.T, domain string) {
	t.Helper()
	configuration, err := websocket.NewConfig("ws://"+domain+"/socket", "http://"+domain)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.DialTimeout("tcp4", "127.0.0.1:80", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	socket, err := websocket.NewClient(configuration, connection)
	if err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	defer socket.Close()
	if err := websocket.Message.Send(socket, "hello"); err != nil {
		t.Fatal(err)
	}
	var reply string
	if err := websocket.Message.Receive(socket, &reply); err != nil || reply != "echo:hello" {
		t.Fatalf("WebSocket reply = %q, %v", reply, err)
	}
}

func assertAppliedRoute(t *testing.T, caddy *Caddy, route Route) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	statuses, err := caddy.Apply(ctx, []Route{route}, time.Now())
	if err != nil || len(statuses) != 1 || statuses[0].State != model.RouteActive ||
		statuses[0].UpstreamCount != uint32(len(route.Upstreams)) {
		t.Fatalf("apply route = %#v, %v", statuses, err)
	}
}

func requestManagedRoute(t *testing.T, domain string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, err := managedHTTPRequest(ctx, domain, "/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024))
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("route response = %s %q, %v", response.Status, body, err)
	}
	return string(body)
}

func managedHTTPRequest(ctx context.Context, domain, path string) (*http.Response, error) {
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp4", "127.0.0.1:80")
	}
	client := &http.Client{Transport: transport}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+domain+path, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(request)
}

func stopManagedCaddy(t *testing.T, caddy *Caddy) {
	t.Helper()
	_ = caddy.Close()
	if caddy.process == nil || caddy.process.Process == nil {
		return
	}
	pid := caddy.process.Process.Pid
	if err := caddy.process.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("kill managed Caddy: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		process, err := os.FindProcess(pid)
		if err != nil || process.Signal(syscall.Signal(0)) != nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Errorf("managed Caddy process %d did not exit", pid)
}
