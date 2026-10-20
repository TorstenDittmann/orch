package control

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/pem"
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

	"connectrpc.com/connect"
	"github.com/TorstenDittmann/orch/gen/orch/v1alpha1/orchv1alpha1connect"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/security"
	"github.com/TorstenDittmann/orch/internal/store"
)

const maxHTTPBodyBytes = maxManifestBytes + 64<<10
const maxAgentMessageBytes = 20 << 20

type Options struct {
	DataDir string
	Listen  string
	// AdminSocket, when set, also serves the administrator API on a local
	// unix socket authorized by filesystem permissions instead of mTLS.
	AdminSocket   string
	Now           func() time.Time
	ImageResolver ImageResolver
	// BackupKeep is how many daily backups to keep in DATA_DIR/backups. Zero
	// keeps DefaultBackupKeep; a negative value turns automatic backups off.
	BackupKeep int

	// GitHubKeysURL replaces GitHub's Actions signing key set; tests only.
	GitHubKeysURL string
}

type Server struct {
	listener        net.Listener
	http            *http.Server
	admin           *http.Server
	adminDone       chan struct{}
	state           *store.Store
	sessions        *sessionManager
	lock            *dataLock
	serveDone       chan struct{}
	serveErr        error
	shutdownOnce    sync.Once
	shutdownDone    chan struct{}
	shutdownErr     error
	reconcileCancel context.CancelFunc
	reconcileDone   chan struct{}
	backupDone      chan struct{}
	upgradesDone    chan struct{}
	imagesDone      chan struct{}
	autoscaleDone   chan struct{}
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
	secretKeyPath := filepath.Join(dataDir, clusterctl.SecretKeyFilename)
	storedSecretFingerprint, err := state.SecretKeyFingerprint(ctx)
	if err != nil {
		return fail(fmt.Errorf("read controller secret key metadata: %w", err), state, nil)
	}
	var secretBox *security.SecretBox
	if storedSecretFingerprint != "" {
		secretBox, err = security.LoadSecretBox(secretKeyPath, nil)
	} else {
		secretBox, _, err = security.LoadOrCreateSecretBox(secretKeyPath, nil)
	}
	if err != nil {
		return fail(fmt.Errorf("load controller secret key: %w", err), state, nil)
	}
	if err := state.EnsureSecretKeyFingerprint(ctx, secretBox.Fingerprint(), now()); err != nil {
		return fail(fmt.Errorf("validate controller secret key: %w", err), state, nil)
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", options.Listen)
	if err != nil {
		return fail(fmt.Errorf("listen on %s: %w", options.Listen, err), state, nil)
	}
	sessions := newSessionManager(now)
	implementation := newService(state, cluster, credentials.CACertificatePEM, credentials.CAPrivateKeyPEM, secretBox, sessions, now)
	implementation.imageResolver = options.ImageResolver
	keep := options.BackupKeep
	if keep == 0 {
		keep = DefaultBackupKeep
	}
	if keep > 0 {
		implementation.backups = newBackupScheduler(state, dataDir, keep, now)
	}
	reconciler := NewReconciler(state, cluster, now, sessions.PublishAssignment)
	reconciler.secretBox = secretBox
	reconciler.sessions = sessions
	implementation.reconciler = reconciler
	implementation.autoscaler = newAutoscaler(state, reconciler.Reconcile, sessions.Connected, now)
	enrollmentImplementation := newEnrollmentService(state, cluster, credentials.CACertificatePEM, credentials.CAPrivateKeyPEM, now)
	enrollmentImplementation.reconciler = reconciler
	upgrades := newAgentUpgrades(sessions, dataDir, now)
	implementation.upgrades = upgrades
	agentImplementation := &agentService{state: state, sessions: sessions, reconciler: reconciler, now: now,
		caPEM: append([]byte(nil), credentials.CACertificatePEM...), caKey: append([]byte(nil), credentials.CAPrivateKeyPEM...), random: rand.Reader,
		upgrades: upgrades, autoscaler: implementation.autoscaler}
	path, handler := orchv1alpha1connect.NewControlServiceHandler(
		implementation,
		connect.WithReadMaxBytes(maxHTTPBodyBytes),
		connect.WithSendMaxBytes(maxHTTPBodyBytes),
	)
	mux := http.NewServeMux()
	mux.Handle(path, authenticateAdministrator(cluster.ID, http.MaxBytesHandler(handler, maxHTTPBodyBytes)))
	adminMux := http.NewServeMux()
	adminMux.Handle(path, authenticateLocalAdministrator(http.MaxBytesHandler(handler, maxHTTPBodyBytes)))
	adminMux.HandleFunc("/healthz", healthHandler)
	// /metrics needs the same credentials as the administrator API: an
	// administrator certificate on the HTTPS port, or access to the socket.
	metrics := implementation.metricsHandler(credentials.Certificate.Leaf.NotAfter)
	mux.Handle("/metrics", authenticateAdministrator(cluster.ID, metrics))
	adminMux.Handle("/metrics", authenticateLocalAdministrator(metrics))
	enrollmentPath, enrollmentHandler := orchv1alpha1connect.NewEnrollmentServiceHandler(
		enrollmentImplementation,
		connect.WithReadMaxBytes(64<<10),
		connect.WithSendMaxBytes(64<<10),
	)
	mux.Handle(enrollmentPath, http.MaxBytesHandler(enrollmentHandler, 64<<10))
	credentialImplementation := newCredentialService(state, cluster, credentials.CACertificatePEM, credentials.CAPrivateKeyPEM, now)
	if options.GitHubKeysURL != "" {
		credentialImplementation.github.jwksURL = options.GitHubKeysURL
	}
	credentialPath, credentialHandler := orchv1alpha1connect.NewCredentialServiceHandler(
		credentialImplementation,
		connect.WithReadMaxBytes(64<<10),
		connect.WithSendMaxBytes(64<<10),
	)
	mux.Handle(credentialPath, http.MaxBytesHandler(credentialHandler, 64<<10))
	agentPath, agentHandler := orchv1alpha1connect.NewAgentServiceHandler(
		agentImplementation,
		connect.WithReadMaxBytes(maxAgentMessageBytes),
		connect.WithSendMaxBytes(maxAgentMessageBytes),
	)
	mux.Handle(agentPath, authenticateAgent(cluster.ID, state, agentHandler))
	mux.HandleFunc("/healthz", healthHandler)
	if err := reconciler.Reconcile(ctx); err != nil {
		return fail(fmt.Errorf("reconcile initial assignment: %w", err), state, listener)
	}
	var adminListener net.Listener
	if options.AdminSocket != "" {
		adminListener, err = listenAdminSocket(ctx, options.AdminSocket)
		if err != nil {
			return fail(err, state, listener)
		}
	}
	// Serve the CA after the leaf so a joining node can pin it by
	// fingerprint from a join token before it has the CA certificate.
	served := credentials.Certificate
	if block, _ := pem.Decode(credentials.CACertificatePEM); block != nil {
		served.Certificate = append(append([][]byte(nil), served.Certificate...), block.Bytes)
	}
	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{served},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    credentials.ClientCAs,
		NextProtos:   []string{"h2", "http/1.1"},
	}
	httpServer := &http.Server{
		Handler: mux, TLSConfig: tlsConfig,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	reconcileContext, reconcileCancel := context.WithCancel(ctx)
	server := &Server{
		listener: listener, http: httpServer, state: state, sessions: sessions, lock: lock,
		serveDone: make(chan struct{}), shutdownDone: make(chan struct{}),
		reconcileCancel: reconcileCancel, reconcileDone: make(chan struct{}), backupDone: make(chan struct{}),
		upgradesDone: make(chan struct{}), imagesDone: make(chan struct{}), autoscaleDone: make(chan struct{}),
	}
	go func() {
		defer close(server.upgradesDone)
		upgrades.run(reconcileContext)
	}()
	go func() {
		defer close(server.imagesDone)
		implementation.runImageUpdates(reconcileContext)
	}()
	go func() {
		defer close(server.autoscaleDone)
		implementation.autoscaler.run(reconcileContext)
	}()
	go func() {
		defer close(server.backupDone)
		if backups := implementation.backups; backups != nil {
			backups.run(reconcileContext)
		}
	}()
	if adminListener != nil {
		protocols := new(http.Protocols)
		protocols.SetHTTP1(true)
		protocols.SetUnencryptedHTTP2(true)
		server.admin = &http.Server{
			Handler: adminMux, Protocols: protocols,
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    16 << 10,
			ErrorLog:          log.New(io.Discard, "", 0),
		}
		server.adminDone = make(chan struct{})
		go func() {
			defer close(server.adminDone)
			_ = server.admin.Serve(adminListener)
		}()
	}
	go func() {
		defer close(server.reconcileDone)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-reconcileContext.Done():
				return
			case <-ticker.C:
				_ = reconciler.Reconcile(reconcileContext)
			}
		}
	}()
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
		server.reconcileCancel()
		<-server.reconcileDone
		<-server.backupDone
		<-server.upgradesDone
		<-server.imagesDone
		<-server.autoscaleDone
		server.sessions.Close()
		shutdownErr := server.http.Shutdown(ctx)
		if shutdownErr != nil {
			_ = server.http.Close()
		}
		if server.admin != nil {
			if err := server.admin.Shutdown(ctx); err != nil {
				_ = server.admin.Close()
			}
			<-server.adminDone
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
