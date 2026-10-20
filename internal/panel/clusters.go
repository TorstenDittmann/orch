package panel

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TorstenDittmann/orch/internal/security"
)

const (
	// ConnectCodeLifetime is how long a connect code from the panel works.
	ConnectCodeLifetime = 15 * time.Minute
	maxConnectCodes     = 20
	// retryInterval spaces sign-in attempts to a cluster that failed.
	retryInterval = 10 * time.Second
	clustersFile  = "clusters.json"
)

var clusterNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// storedCluster is what the panel keeps about a connected cluster: its name
// and the administrator access token the cluster gave the panel.
type storedCluster struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

type cluster struct {
	stored      storedCluster
	fingerprint string

	mu    sync.Mutex
	conn  *Connection
	err   error
	tried time.Time
}

func newCluster(stored storedCluster) *cluster {
	return &cluster{stored: stored, fingerprint: fingerprint(stored.Token)}
}

// fingerprint is the cluster CA pin an access token names, which identifies
// the cluster whatever address it is reached at.
func fingerprint(token string) string {
	address, _, err := security.ParseAccessToken(token)
	if err != nil {
		return ""
	}
	_, pin, _ := strings.Cut(address, "#sha256:")
	return pin
}

// endpoint is the controller URL an access token names.
func endpoint(token string) string {
	address, _, _ := security.ParseAccessToken(token)
	endpoint, _, _ := strings.Cut(address, "#")
	return endpoint
}

// ready returns the cluster's connection, signing in first if needed.
func (c *cluster) ready(ctx context.Context, connect func(context.Context, string) (*Connection, error), now time.Time) (*Connection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return c.conn, nil
	}
	if c.err != nil && now.Sub(c.tried) < retryInterval {
		return nil, c.err
	}
	c.tried = now
	c.conn, c.err = connect(ctx, c.stored.Token)
	return c.conn, c.err
}

func (c *cluster) lastError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		return ""
	}
	return c.err.Error()
}

func (c *cluster) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil && c.conn.Close != nil {
		c.conn.Close()
	}
	c.conn = nil
}

// store keeps the connected clusters in one private file.
type store struct{ dir string }

func (s *store) load() ([]storedCluster, error) {
	if s.dir == "" {
		return nil, nil
	}
	data, err := os.ReadFile(filepath.Join(s.dir, clustersFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read connected clusters: %w", err)
	}
	var document struct {
		Clusters []storedCluster `json:"clusters"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("read connected clusters: %w", err)
	}
	return document.Clusters, nil
}

func (s *store) save(clusters []storedCluster) error {
	if s.dir == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("save connected clusters: %w", err)
	}
	data, err := json.MarshalIndent(map[string]any{"clusters": clusters}, "", "  ")
	if err != nil {
		return err
	}
	temporary := filepath.Join(s.dir, clustersFile+".tmp")
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return fmt.Errorf("save connected clusters: %w", err)
	}
	if err := os.Rename(temporary, filepath.Join(s.dir, clustersFile)); err != nil {
		return fmt.Errorf("save connected clusters: %w", err)
	}
	return nil
}

// sortedNames lists the connected clusters by name; s.mu must be held.
func (s *Server) sortedNames() []string {
	names := make([]string, 0, len(s.clusters))
	for name := range s.clusters {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// storedClusters lists what to save; s.mu must be held.
func (s *Server) storedClusters() []storedCluster {
	stored := make([]storedCluster, 0, len(s.clusters))
	for _, name := range s.sortedNames() {
		stored = append(stored, s.clusters[name].stored)
	}
	return stored
}

func (s *Server) clusterCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clusters)
}

// clusterFor finds the connected cluster an access token address names.
func (s *Server) clusterFor(address string) *cluster {
	_, pin, _ := strings.Cut(address, "#sha256:")
	if pin == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cluster := range s.clusters {
		if cluster.fingerprint == pin {
			return cluster
		}
	}
	return nil
}

// codeValid reports whether code may connect a cluster: a code created in
// the panel, or the setup code while no cluster is connected. s.mu must be
// held.
func (s *Server) codeValid(code string) bool {
	if code == "" {
		return false
	}
	if len(s.clusters) == 0 && subtle.ConstantTimeCompare([]byte(code), []byte(s.setupCode)) == 1 {
		return true
	}
	expires, ok := s.codes[code]
	return ok && s.now().Before(expires)
}

// connectCluster adds a cluster from orch panel connect. The connect code
// proves the request was allowed by someone signed in to the panel, or by
// whoever can read its logs before the first cluster is connected.
func (s *Server) connectCluster(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, errors.New("send JSON"))
		return
	}
	if s.limited() {
		writeError(w, http.StatusTooManyRequests, errors.New("too many invalid attempts; wait a minute"))
		return
	}
	var body struct {
		Code  string `json:"code"`
		Name  string `json:"name"`
		Token string `json:"token"`
	}
	if !readBody(w, r, &body) {
		return
	}
	s.mu.Lock()
	valid := s.codeValid(body.Code)
	s.mu.Unlock()
	if !valid {
		s.recordFailure()
		writeError(w, http.StatusForbidden, errors.New("the connect code is invalid, expired or was already used; create a new one in the panel under Clusters"))
		return
	}
	pin := fingerprint(body.Token)
	if pin == "" {
		writeError(w, http.StatusBadRequest, errors.New("token is not an orch access token"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	connection, err := s.connect(ctx, body.Token)
	if err != nil {
		s.recordFailure()
		writeError(w, http.StatusBadRequest, fmt.Errorf("the panel cannot sign in to this cluster: %w", err))
		return
	}
	name := body.Name
	if name == "" {
		name = connection.Name
	}
	fail := func(status int, err error) {
		if connection.Close != nil {
			connection.Close()
		}
		writeError(w, status, err)
	}
	if !clusterNamePattern.MatchString(name) {
		fail(http.StatusBadRequest, fmt.Errorf("cluster name %q must be lowercase letters, digits and dashes; pass --name", name))
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// The code is used up only by a connect that succeeds, and only once.
	if !s.codeValid(body.Code) {
		fail(http.StatusForbidden, errors.New("the connect code was already used"))
		return
	}
	var replaced *cluster
	for _, existing := range s.clusters {
		if existing.fingerprint == pin {
			replaced = existing
		}
	}
	if existing := s.clusters[name]; existing != nil && existing != replaced {
		fail(http.StatusConflict, fmt.Errorf("another cluster is already connected as %s; pass --name", name))
		return
	}
	delete(s.codes, body.Code)
	if replaced != nil {
		delete(s.clusters, replaced.stored.Name)
	}
	added := newCluster(storedCluster{Name: name, Token: body.Token})
	added.conn = connection
	s.clusters[name] = added
	if err := s.store.save(s.storedClusters()); err != nil {
		delete(s.clusters, name)
		if replaced != nil {
			s.clusters[replaced.stored.Name] = replaced
		}
		fail(http.StatusInternalServerError, err)
		return
	}
	if replaced != nil {
		replaced.close()
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

// panelAPI serves the signed-in panel's own API: listing, connecting and
// disconnecting clusters.
func (s *Server) panelAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/panel/api/")
	switch {
	case path == "clusters" && r.Method == http.MethodGet:
		s.listClusters(w)
	case path == "clusters/code" && r.Method == http.MethodPost:
		s.createConnectCode(w, r)
	case strings.HasPrefix(path, "clusters/") && strings.HasSuffix(path, "/disconnect") && r.Method == http.MethodPost:
		s.disconnect(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "clusters/"), "/disconnect"))
	default:
		writeError(w, http.StatusNotFound, errors.New("unknown panel API route"))
	}
}

func (s *Server) listClusters(w http.ResponseWriter) {
	s.mu.Lock()
	clusters := make([]*cluster, 0, len(s.clusters))
	for _, name := range s.sortedNames() {
		clusters = append(clusters, s.clusters[name])
	}
	s.mu.Unlock()
	type entry struct {
		Name     string `json:"name"`
		Endpoint string `json:"endpoint"`
		Error    string `json:"error,omitempty"`
	}
	entries := make([]entry, 0, len(clusters))
	for _, cluster := range clusters {
		entries = append(entries, entry{Name: cluster.stored.Name, Endpoint: endpoint(cluster.stored.Token), Error: cluster.lastError()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"clusters": entries})
}

func (s *Server) createConnectCode(w http.ResponseWriter, r *http.Request) {
	code, err := randomHex(10)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	now := s.now()
	expires := now.Add(ConnectCodeLifetime)
	s.mu.Lock()
	for existing, at := range s.codes {
		if !now.Before(at) {
			delete(s.codes, existing)
		}
	}
	if len(s.codes) >= maxConnectCodes {
		s.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, errors.New("too many unused connect codes; wait for them to expire"))
		return
	}
	s.codes[code] = expires
	s.mu.Unlock()
	scheme := "http"
	if secure(r) {
		scheme = "https"
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"code":      code,
		"expiresAt": expires.UTC().Format(time.RFC3339),
		"command":   fmt.Sprintf("orch panel connect %s://%s --code %s", scheme, r.Host, code),
	})
}

// disconnect forgets a cluster and revokes the panel's token on it when the
// cluster can be reached.
func (s *Server) disconnect(w http.ResponseWriter, r *http.Request, name string) {
	s.mu.Lock()
	cluster := s.clusters[name]
	if cluster == nil {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, fmt.Errorf("cluster %s is not connected", name))
		return
	}
	delete(s.clusters, name)
	err := s.store.save(s.storedClusters())
	if err != nil {
		s.clusters[name] = cluster
	}
	s.mu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	revoked := false
	if connection, err := cluster.ready(ctx, s.connect, s.now()); err == nil {
		revoked, _ = connection.Controller.DeleteAccessToken(ctx, TokenName)
	}
	cluster.close()
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "revoked": revoked})
}
