package control

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"change.codes/torsten/better-swarm/gen/orch/v1alpha1/orchv1alpha1connect"
	clusterctl "change.codes/torsten/better-swarm/internal/cluster"
	"change.codes/torsten/better-swarm/internal/store"
	"connectrpc.com/connect"
)

const maxHTTPBodyBytes = maxManifestBytes + 64<<10

type Options struct {
	DataDir string
	Listen  string
	Now     func() time.Time
}

type Server struct {
	listener     net.Listener
	http         *http.Server
	state        *store.Store
	lock         *dataLock
	serveDone    chan struct{}
	serveErr     error
	shutdownOnce sync.Once
	shutdownDone chan struct{}
	shutdownErr  error
}

// Start validates initialized state and identity, acquires controller
// ownership, and starts serving HTTPS.
func Start(ctx context.Context, options Options) (*Server, error) {
	dataDir, err := normalizeDataDir(options.DataDir)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(options.Listen) == "" {
		return nil, fmt.Errorf("listen address is required")
	}
	lock, err := acquireDataLock(dataDir)
	if err != nil {
		return nil, err
	}
	fail := func(openErr error, state *store.Store, listener net.Listener) (*Server, error) {
		if listener != nil {
			_ = listener.Close()
		}
		if state != nil {
			_ = state.Close()
		}
		_ = lock.Close()
		return nil, openErr
	}
	state, err := store.OpenInitialized(ctx, filepath.Join(dataDir, clusterctl.DatabaseFilename))
	if err != nil {
		return fail(fmt.Errorf("open initialized controller state: %w", err), nil, nil)
	}
	cluster, err := state.Cluster(ctx)
	if err != nil {
		return fail(fmt.Errorf("read controller cluster: %w", err), state, nil)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	credentials, err := clusterctl.LoadControllerCredentials(dataDir, cluster, now())
	if err != nil {
		return fail(err, state, nil)
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", options.Listen)
	if err != nil {
		return fail(fmt.Errorf("listen on %s: %w", options.Listen, err), state, nil)
	}
	implementation := newService(state, cluster, now)
	path, handler := orchv1alpha1connect.NewControlServiceHandler(
		implementation,
		connect.WithReadMaxBytes(maxHTTPBodyBytes),
		connect.WithSendMaxBytes(maxHTTPBodyBytes),
	)
	mux := http.NewServeMux()
	mux.Handle(path, authenticateAdministrator(cluster.ID, http.MaxBytesHandler(handler, maxHTTPBodyBytes)))
	mux.HandleFunc("/healthz", healthHandler)
	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{credentials.Certificate},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    credentials.ClientCAs,
		NextProtos:   []string{"h2", "http/1.1"},
	}
	httpServer := &http.Server{
		Handler: mux, TLSConfig: tlsConfig,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	server := &Server{
		listener: listener, http: httpServer, state: state, lock: lock,
		serveDone: make(chan struct{}), shutdownDone: make(chan struct{}),
	}
	go func() {
		defer close(server.serveDone)
		err := httpServer.ServeTLS(listener, "", "")
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			server.serveErr = err
		}
	}()
	return server, nil
}

// Serve starts a controller and blocks until cancellation or a serving error.
func Serve(ctx context.Context, options Options, ready func(net.Addr)) error {
	server, err := Start(ctx, options)
	if err != nil {
		return err
	}
	if ready != nil {
		ready(server.Addr())
	}
	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownContext)
	case <-server.serveDone:
		serveErr := server.serveErr
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return errors.Join(serveErr, server.Shutdown(shutdownContext))
	}
}

func (server *Server) Addr() net.Addr {
	if server == nil || server.listener == nil {
		return nil
	}
	return server.listener.Addr()
}

func (server *Server) Shutdown(ctx context.Context) error {
	if server == nil {
		return nil
	}
	server.shutdownOnce.Do(func() {
		defer close(server.shutdownDone)
		shutdownErr := server.http.Shutdown(ctx)
		if shutdownErr != nil {
			_ = server.http.Close()
		}
		<-server.serveDone
		server.shutdownErr = errors.Join(shutdownErr, server.serveErr, server.state.Close(), server.lock.Close())
	})
	<-server.shutdownDone
	return server.shutdownErr
}

func normalizeDataDir(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("data directory is required")
	}
	absolute, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("resolve data directory: %w", err)
	}
	absolute = filepath.Clean(absolute)
	if filepath.Dir(absolute) == absolute {
		return "", fmt.Errorf("data directory cannot be a filesystem root")
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect data directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("data path is not a directory: %s", absolute)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("data directory permissions %04o allow group or world access", info.Mode().Perm())
	}
	databasePath := filepath.Join(absolute, clusterctl.DatabaseFilename)
	database, err := os.Lstat(databasePath)
	if err != nil {
		return "", fmt.Errorf("inspect controller database: %w", err)
	}
	if !database.Mode().IsRegular() {
		return "", fmt.Errorf("controller database is not a regular file: %s", databasePath)
	}
	if database.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("controller database permissions %04o allow group or world access", database.Mode().Perm())
	}
	return absolute, nil
}

func healthHandler(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		response.Header().Set("Allow", "GET, HEAD")
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(http.StatusOK)
	if request.Method == http.MethodGet {
		_, _ = response.Write([]byte("ok\n"))
	}
}
