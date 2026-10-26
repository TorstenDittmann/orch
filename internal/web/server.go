// Package web serves the local operator dashboard behind `orch web`.
//
// The server runs inside the CLI process on a loopback address and reuses the
// administrator client of the selected target. The browser never receives
// credentials: it authenticates to this process with a one-time token that is
// exchanged for an HttpOnly cookie, and every controller call stays mTLS.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/database"
	"github.com/TorstenDittmann/orch/internal/tui"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	cookieName     = "orch_web"
	requestTimeout = 15 * time.Second
	// Backups and restores copy a volume and, for dump engines, load the
	// dump, so they get far longer than reads.
	backupTimeout = 20 * time.Minute
	maxBodyBytes  = 16 << 10
)

// The dashboard is a Svelte single-page app in ui/, built into ui/dist with
// `make web` and embedded so the orch binary stays self-contained.
//
//go:embed all:ui/dist
var assets embed.FS

// Provider is the controller surface the dashboard reads and mutates.
type Provider interface {
	tui.Provider
	ListJobs(context.Context, string, string, uint32) (*orchv1alpha1.ListJobsResponse, error)
	CancelJob(context.Context, string) (*orchv1alpha1.OneOffJob, error)
	RetryJob(context.Context, string, string) (*orchv1alpha1.OneOffJob, error)
	ListCronRuns(context.Context, string, string, uint32) ([]*orchv1alpha1.JobRunStatus, error)
	FindDatabase(context.Context, string, string) (database.Database, error)
	ListVolumes(context.Context, string, string) ([]*orchv1alpha1.VolumeSummary, error)
	BackupVolume(context.Context, string, string) (*orchv1alpha1.VolumeBackupInfo, error)
	GetBackupDestination(context.Context) (*orchv1alpha1.GetBackupDestinationResponse, error)
	RestoreDatabase(context.Context, database.Database, string) (*orchv1alpha1.VolumeBackupInfo, error)
}

// ClientProvider adapts the administrator client used by the TUI.
type ClientProvider struct{ tui.ClientProvider }

func (p ClientProvider) FindDatabase(ctx context.Context, application, name string) (database.Database, error) {
	return database.Find(ctx, p.Client, application, name)
}

func (p ClientProvider) ListVolumes(ctx context.Context, application, volume string) ([]*orchv1alpha1.VolumeSummary, error) {
	return p.Client.ListVolumes(ctx, application, volume)
}

func (p ClientProvider) BackupVolume(ctx context.Context, application, volume string) (*orchv1alpha1.VolumeBackupInfo, error) {
	return p.Client.BackupVolume(ctx, application, volume)
}

func (p ClientProvider) GetBackupDestination(ctx context.Context) (*orchv1alpha1.GetBackupDestinationResponse, error) {
	return p.Client.GetBackupDestination(ctx)
}

func (p ClientProvider) RestoreDatabase(ctx context.Context, found database.Database, backup string) (*orchv1alpha1.VolumeBackupInfo, error) {
	return database.Restore(ctx, p.Client, found, backup, io.Discard)
}

type Server struct {
	provider  Provider
	token     string
	session   string
	mu        sync.Mutex
	exchanged bool
	target    string
	transport string
	mux       *http.ServeMux
	static    http.Handler
}

// NewToken returns a random URL-safe access token.
func NewToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("create web access token: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func New(provider Provider, target, transport, token string) (*Server, error) {
	session, err := NewToken()
	if err != nil {
		return nil, err
	}
	s := &Server{provider: provider, token: token, session: session, target: target, transport: transport, mux: http.NewServeMux()}
	dist, err := fs.Sub(assets, "ui/dist")
	if err != nil {
		return nil, fmt.Errorf("load embedded dashboard: %w", err)
	}
	s.static = http.FileServerFS(dist)
	s.mux.HandleFunc("GET /", s.index)
	s.mux.HandleFunc("GET /api/snapshot", s.snapshot)
	s.mux.HandleFunc("GET /api/apps/{name}", s.application)
	s.mux.HandleFunc("GET /api/apps/{name}/compare", s.compare)
	s.mux.HandleFunc("GET /api/apps/{name}/cron/{job}/runs", s.cronRuns)
	s.mux.HandleFunc("GET /api/logs", s.logs)
	s.mux.HandleFunc("GET /api/jobs", s.jobs)
	s.mux.HandleFunc("POST /api/jobs/{id}/{action}", s.jobAction)
	s.mux.HandleFunc("GET /api/nodes/{name}/usage", s.nodeUsage)
	s.mux.HandleFunc("POST /api/nodes/{name}", s.updateNode)
	s.mux.HandleFunc("POST /api/apps/{name}/rollback", s.rollback)
	s.mux.HandleFunc("GET /api/apps/{name}/databases/{database}/backups", s.databaseBackups)
	s.mux.HandleFunc("POST /api/apps/{name}/databases/{database}/backup", s.backupDatabase)
	s.mux.HandleFunc("POST /api/apps/{name}/databases/{database}/restore", s.restoreDatabase)
	s.mux.HandleFunc("GET /api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, errors.New("unknown API route"))
	})
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; font-src 'self'; connect-src 'self'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	if !loopbackHost(r.Host) {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/" && r.Method == http.MethodGet && r.URL.Query().Has("token") {
		if !s.exchange(r.URL.Query().Get("token")) {
			http.Error(w, "this link was already used or is invalid; restart orch web for a new one", http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.session, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	cookie, err := r.Cookie(cookieName)
	if err != nil || !equal(cookie.Value, s.session) {
		http.Error(w, "open the URL printed by orch web", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if r.Header.Get("Origin") != "http://"+r.Host || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

// exchange accepts the printed URL token once, so a URL recovered later from
// shell or browser history cannot open a new session.
func (s *Server) exchange(value string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exchanged || !equal(value, s.token) {
		return false
	}
	s.exchanged = true
	return true
}

func equal(value, expected string) bool {
	return value != "" && subtle.ConstantTimeCompare([]byte(value), []byte(expected)) == 1
}

// loopbackHost rejects DNS-rebound names: only literal loopback hosts are served.
func loopbackHost(host string) bool {
	name, _, err := net.SplitHostPort(host)
	if err != nil {
		name = host
	}
	if name == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(name, "[]"))
	return ip != nil && ip.IsLoopback()
}

// index serves built assets and falls back to the app shell for client routes.
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name != "" {
		if info, err := fs.Stat(assets, "ui/dist/"+name); err == nil && !info.IsDir() {
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
			}
			s.static.ServeHTTP(w, r)
			return
		}
	}
	page, err := assets.ReadFile("ui/dist/index.html")
	if err != nil {
		http.Error(w, "dashboard assets are missing; run make web", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

func (s *Server) compare(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	query := r.URL.Query()
	if query.Get("from") == "" || query.Get("to") == "" {
		writeError(w, http.StatusBadRequest, errors.New("from and to are required"))
		return
	}
	value, err := s.provider.CompareDeployments(ctx, r.PathValue("name"), query.Get("from"), query.Get("to"))
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, message(value))
}

func (s *Server) cronRuns(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	runs, err := s.provider.ListCronRuns(ctx, r.PathValue("name"), r.PathValue("job"), 50)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": messages(runs)})
}

func (s *Server) jobAction(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	var job *orchv1alpha1.OneOffJob
	var err error
	switch r.PathValue("action") {
	case "cancel":
		job, err = s.provider.CancelJob(ctx, r.PathValue("id"))
	case "retry":
		var key string
		if key, err = NewToken(); err == nil {
			job, err = s.provider.RetryJob(ctx, r.PathValue("id"), key[:32])
		}
	default:
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown job action %q", r.PathValue("action")))
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, message(job))
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	var (
		mu       sync.Mutex
		wait     sync.WaitGroup
		errs     []string
		response = map[string]any{"target": s.target, "transport": s.transport, "refreshedAt": time.Now().UTC().Format(time.RFC3339)}
	)
	run := func(label string, operation func() (any, error)) {
		wait.Add(1)
		go func() {
			defer wait.Done()
			value, err := operation()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, label+": "+err.Error())
				return
			}
			response[label] = value
		}()
	}
	run("cluster", func() (any, error) {
		cluster, backup, err := s.provider.GetClusterStatus(ctx)
		if err == nil && backup != nil {
			mu.Lock()
			response["backup"] = message(backup)
			mu.Unlock()
		}
		return message(cluster), err
	})
	run("applications", func() (any, error) {
		value, err := s.provider.ListApplications(ctx)
		return messages(value), err
	})
	run("runtimes", func() (any, error) {
		value, err := s.provider.ListRuntimes(ctx, 2000)
		return message(value), err
	})
	run("nodes", func() (any, error) {
		value, err := s.provider.ListNodes(ctx)
		return messages(value), err
	})
	run("events", func() (any, error) {
		value, err := s.provider.ListRecentEvents(ctx, 200)
		return message(value), err
	})
	wait.Wait()
	response["errors"] = errs
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) application(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	name := r.PathValue("name")
	application, err := s.provider.GetApplication(ctx, name)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	deployments, err := s.provider.ListDeployments(ctx, name, 0, 20)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"application": message(application), "deployments": message(deployments)})
}

func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	query := r.URL.Query()
	value, err := s.provider.ListJobs(ctx, query.Get("app"), query.Get("before"), 50)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, message(value))
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	query := r.URL.Query()
	application, cursor := query.Get("app"), query.Get("cursor")
	if application == "" {
		writeError(w, http.StatusBadRequest, errors.New("app is required"))
		return
	}
	var batch tui.LogBatch
	var err error
	if job := query.Get("job"); job != "" {
		batch, err = s.provider.LoadCronLogs(ctx, application, job, query.Get("run"), 500, cursor)
	} else {
		replica, parseErr := strconv.ParseUint(query.Get("replica"), 10, 32)
		if query.Get("service") == "" || parseErr != nil {
			writeError(w, http.StatusBadRequest, errors.New("service and replica are required"))
			return
		}
		if query.Get("previous") == "1" {
			batch, err = s.provider.LoadPreviousLogs(ctx, application, query.Get("service"), uint32(replica), 500)
		} else {
			batch, err = s.provider.LoadLogs(ctx, application, query.Get("service"), uint32(replica), 500, cursor)
		}
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	lines := make([]map[string]string, 0, len(batch.Lines))
	for _, line := range batch.Lines {
		lines = append(lines, map[string]string{"stream": line.Stream, "text": line.Text})
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "nextCursor": batch.NextCursor, "cursorReset": batch.CursorReset, "exited": batch.Exited})
}

func (s *Server) nodeUsage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	value, err := s.provider.GetNodeUsage(ctx, r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, message(value))
}

func (s *Server) updateNode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string `json:"action"`
	}
	if !readBody(w, r, &body) {
		return
	}
	key, err := NewToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	request := &orchv1alpha1.UpdateNodeRequest{Name: r.PathValue("name"), IdempotencyKey: key[:32]}
	switch body.Action {
	case "cordon":
		request.Lifecycle = orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_CORDONED
	case "uncordon":
		request.Lifecycle = orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_ACTIVE
	case "drain":
		request.Lifecycle = orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_DRAINING
	case "ingress-on", "ingress-off":
		request.SetIngress = true
		request.Ingress = body.Action == "ingress-on"
	default:
		writeError(w, http.StatusBadRequest, fmt.Errorf("unknown node action %q", body.Action))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	response, err := s.provider.UpdateNode(ctx, request)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, message(response))
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RevisionID         string `json:"revisionId"`
		ExpectedGeneration string `json:"expectedGeneration"`
	}
	if !readBody(w, r, &body) {
		return
	}
	generation, err := strconv.ParseUint(body.ExpectedGeneration, 10, 64)
	if body.RevisionID == "" || err != nil {
		writeError(w, http.StatusBadRequest, errors.New("revisionId and expectedGeneration are required"))
		return
	}
	key, err := NewToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	response, err := s.provider.RollbackApplication(ctx, &orchv1alpha1.RollbackApplicationRequest{
		Application: r.PathValue("name"), RevisionId: body.RevisionID, ExpectedGeneration: generation, IdempotencyKey: key[:32],
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, message(response))
}

// databaseBackups lists a database's backups with where they are kept. A
// database without backups answers with no volume rather than an error.
func (s *Server) databaseBackups(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	found, err := s.provider.FindDatabase(ctx, r.PathValue("name"), r.PathValue("database"))
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	response := map[string]any{"database": databaseJSON(found), "volume": nil, "destination": ""}
	if found.BackupVolume == "" {
		writeJSON(w, http.StatusOK, response)
		return
	}
	volumes, err := s.provider.ListVolumes(ctx, found.Application, found.BackupVolume)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if len(volumes) == 1 {
		response["volume"] = message(volumes[0])
	}
	// Only the bucket's URL is shown; the destination's keys stay with the controller.
	if destination, err := s.provider.GetBackupDestination(ctx); err == nil {
		response["destination"] = destination.GetDestination().GetUrl()
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) backupDatabase(w http.ResponseWriter, r *http.Request) {
	var body struct{}
	if !readBody(w, r, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), backupTimeout)
	defer cancel()
	found, err := s.provider.FindDatabase(ctx, r.PathValue("name"), r.PathValue("database"))
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	volume, err := found.Volume()
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	backup, err := s.provider.BackupVolume(ctx, found.Application, volume)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, message(backup))
}

func (s *Server) restoreDatabase(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Backup string `json:"backup"`
	}
	if !readBody(w, r, &body) {
		return
	}
	if body.Backup == "" {
		writeError(w, http.StatusBadRequest, errors.New("backup is required"))
		return
	}
	// A restore that stops halfway leaves the dump unloaded, so it runs to
	// the end even when the browser tab closes.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), backupTimeout)
	defer cancel()
	found, err := s.provider.FindDatabase(ctx, r.PathValue("name"), r.PathValue("database"))
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if _, err := found.Volume(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	previous, err := s.provider.RestoreDatabase(ctx, found, body.Backup)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	response := map[string]any{"restored": body.Backup, "previous": nil}
	if previous.GetName() != "" {
		response["previous"] = message(previous)
	}
	writeJSON(w, http.StatusOK, response)
}

func databaseJSON(found database.Database) map[string]any {
	return map[string]any{
		"name": found.Database, "engine": found.Engine, "version": found.Version,
		"backupVolume": found.BackupVolume, "backupSchedule": found.BackupSchedule, "backupKeep": found.BackupKeep,
		"keepsPreviousData": found.KeepsPreviousData(),
	}
}

func readBody(w http.ResponseWriter, r *http.Request, value any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode request: %w", err))
		return false
	}
	return true
}

var marshal = protojson.MarshalOptions{EmitUnpopulated: true}

func message(value proto.Message) json.RawMessage {
	if value == nil {
		return json.RawMessage("null")
	}
	encoded, err := marshal.Marshal(value)
	if err != nil {
		return json.RawMessage("null")
	}
	return encoded
}

func messages[T proto.Message](values []T) []json.RawMessage {
	result := make([]json.RawMessage, 0, len(values))
	for _, value := range values {
		result = append(result, message(value))
	}
	return result
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
