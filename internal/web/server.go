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
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	"change.codes/torsten/better-swarm/internal/tui"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	cookieName     = "orch_web"
	requestTimeout = 15 * time.Second
	maxBodyBytes   = 16 << 10
)

//go:embed index.html
var assets embed.FS

// Provider is the controller surface the dashboard reads and mutates.
type Provider interface {
	tui.Provider
	ListJobs(context.Context, string, string, uint32) (*orchv1alpha1.ListJobsResponse, error)
}

type Server struct {
	provider  Provider
	token     string
	target    string
	transport string
	mux       *http.ServeMux
}

// NewToken returns a random URL-safe access token.
func NewToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("create web access token: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func New(provider Provider, target, transport, token string) *Server {
	s := &Server{provider: provider, token: token, target: target, transport: transport, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /{$}", s.index)
	s.mux.HandleFunc("GET /api/snapshot", s.snapshot)
	s.mux.HandleFunc("GET /api/apps/{name}", s.application)
	s.mux.HandleFunc("GET /api/logs", s.logs)
	s.mux.HandleFunc("GET /api/jobs", s.jobs)
	s.mux.HandleFunc("POST /api/nodes/{name}", s.updateNode)
	s.mux.HandleFunc("POST /api/apps/{name}/rollback", s.rollback)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Content-Security-Policy", "default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	if !loopbackHost(r.Host) {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/" && r.Method == http.MethodGet && r.URL.Query().Has("token") {
		if !s.validToken(r.URL.Query().Get("token")) {
			http.Error(w, "invalid token", http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	cookie, err := r.Cookie(cookieName)
	if err != nil || !s.validToken(cookie.Value) {
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

func (s *Server) validToken(value string) bool {
	return value != "" && subtle.ConstantTimeCompare([]byte(value), []byte(s.token)) == 1
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

func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	page, err := assets.ReadFile("index.html")
	if err != nil {
		http.Error(w, "missing dashboard asset", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
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
		value, err := s.provider.GetCluster(ctx)
		return message(value), err
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
		batch, err = s.provider.LoadLogs(ctx, application, query.Get("service"), uint32(replica), 500, cursor)
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
