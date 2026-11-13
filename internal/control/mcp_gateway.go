package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/TorstenDittmann/orch/internal/ingress"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/security"
	"github.com/TorstenDittmann/orch/internal/store"
)

// The remote MCP server lets AI assistants that need a URL, such as Claude
// connectors, use orch. Ingress on the controller's host terminates HTTPS for
// the configured domain and forwards to this gateway on loopback. The gateway
// is its own OAuth 2.1 authorization server: an assistant registers itself,
// sends the person to /authorize, and an administrator approves the code
// shown there with orch mcp approve. Every MCP request then runs with exactly
// the approved access, enforced here on each API procedure.

// DefaultMCPListen is where the controller serves the remote MCP gateway for
// the ingress on its own host.
const DefaultMCPListen = ingress.MCPUpstream

const (
	mcpPendingLifetime     = 10 * time.Minute
	mcpCodeLifetime        = 5 * time.Minute
	mcpAccessTokenLifetime = time.Hour
	mcpDefaultGrantTTL     = 30 * 24 * time.Hour
	mcpMaxPending          = 50
	mcpMaxAccessTokens     = 10000
	mcpMaxRequestBytes     = 1 << 20
	mcpMaxClientIDBytes    = 2048
	mcpAccessTokenPrefix   = "orch_mcp_"
	mcpRefreshTokenPrefix  = "orch_mcpr_"
	mcpCodeAlphabet        = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

// MCPSession is one MCP request's authorized connection.
type MCPSession struct {
	// Name is the connection's name in orch token list.
	Name string
	// Access is model.MCPAccessRead, MCPAccessDeploy or MCPAccessDestructive.
	Access string
	// Application, when set, is the one application the connection may use.
	Application string
	// Dial connects to an in-process controller API that admits only this
	// connection's access, speaking HTTP/2 without TLS.
	Dial func(ctx context.Context) (net.Conn, error)
}

// MCPRunner answers MCP requests; the CLI implements it with its tools.
type MCPRunner interface {
	ServeMCP(response http.ResponseWriter, request *http.Request, session MCPSession)
}

type mcpClient struct {
	Name         string   `json:"n"`
	RedirectURIs []string `json:"r"`
}

type mcpPending struct {
	code        string
	poll        string
	clientID    string
	client      mcpClient
	redirectURI string
	challenge   string
	state       string
	expires     time.Time
	// Set once an administrator approves.
	grantID  string
	authCode string
	authHash string
}

type mcpAccessToken struct {
	grantID string
	expires time.Time
}

type mcpGateway struct {
	state  *store.Store
	now    func() time.Time
	random io.Reader
	runner MCPRunner
	// api is the control API handler, before authentication.
	api http.Handler

	mu        sync.Mutex
	pending   map[string]*mcpPending // by code
	byPoll    map[string]*mcpPending
	byAuth    map[string]*mcpPending // by authorization code hash
	tokens    map[string]mcpAccessToken
	lastTouch map[string]time.Time
}

func newMCPGateway(state *store.Store, now func() time.Time, runner MCPRunner, api http.Handler) *mcpGateway {
	return &mcpGateway{
		state: state, now: now, random: rand.Reader, runner: runner, api: api,
		pending: map[string]*mcpPending{}, byPoll: map[string]*mcpPending{}, byAuth: map[string]*mcpPending{},
		tokens: map[string]mcpAccessToken{}, lastTouch: map[string]time.Time{},
	}
}

func (g *mcpGateway) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("X-Content-Type-Options", "nosniff")
	response.Header().Set("Referrer-Policy", "no-referrer")
	domain, err := g.state.MCPDomain(request.Context())
	if err != nil {
		http.Error(response, "remote MCP is unavailable", http.StatusServiceUnavailable)
		return
	}
	host := request.Host
	if name, _, splitErr := net.SplitHostPort(host); splitErr == nil {
		host = name
	}
	if domain == "" || !strings.EqualFold(host, domain) {
		http.NotFound(response, request)
		return
	}
	base := "https://" + domain
	request.Body = http.MaxBytesReader(response, request.Body, mcpMaxRequestBytes)
	switch request.URL.Path {
	case "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp":
		g.protectedResource(response, request, base)
	case "/.well-known/oauth-authorization-server", "/.well-known/oauth-authorization-server/mcp":
		g.authorizationServer(response, request, base)
	case "/register":
		g.register(response, request)
	case "/authorize":
		g.authorize(response, request, base)
	case "/authorize/status":
		g.authorizeStatus(response, request, base)
	case "/token":
		g.token(response, request)
	case "/mcp":
		g.mcp(response, request, base)
	case "/":
		response.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(response, "orch MCP server. Add %s/mcp to your assistant as a remote MCP server.\n", base)
	default:
		http.NotFound(response, request)
	}
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func oauthError(response http.ResponseWriter, status int, code, description string) {
	writeJSON(response, status, map[string]string{"error": code, "error_description": description})
}

func (g *mcpGateway) protectedResource(response http.ResponseWriter, request *http.Request, base string) {
	if request.Method != http.MethodGet {
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"resource": base + "/mcp", "authorization_servers": []string{base},
		"bearer_methods_supported": []string{"header"}, "resource_name": "orch",
	})
}

func (g *mcpGateway) authorizationServer(response http.ResponseWriter, request *http.Request, base string) {
	if request.Method != http.MethodGet {
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"issuer": base, "authorization_endpoint": base + "/authorize", "token_endpoint": base + "/token",
		"registration_endpoint": base + "/register", "response_types_supported": []string{"code"},
		"grant_types_supported": []string{"authorization_code", "refresh_token"}, "code_challenge_methods_supported": []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"}, "authorization_response_iss_parameter_supported": true,
	})
}

// validRedirectURI admits HTTPS callbacks and loopback HTTP callbacks for
// assistants running on the person's own computer.
func validRedirectURI(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Fragment != "" || parsed.User != nil || parsed.Host == "" || len(raw) > 512 {
		return false
	}
	switch parsed.Scheme {
	case "https":
		return true
	case "http":
		host := parsed.Hostname()
		return host == "localhost" || host == "127.0.0.1" || host == "::1"
	}
	return false
}

// register implements dynamic client registration. Registering grants
// nothing: an administrator still approves each connection. The client ID
// carries the registration itself, so the gateway keeps no state for it.
func (g *mcpGateway) register(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		oauthError(response, http.StatusBadRequest, "invalid_client_metadata", "request body must be JSON client metadata")
		return
	}
	if len(body.RedirectURIs) == 0 || len(body.RedirectURIs) > 5 {
		oauthError(response, http.StatusBadRequest, "invalid_redirect_uri", "between 1 and 5 redirect_uris are required")
		return
	}
	for _, uri := range body.RedirectURIs {
		if !validRedirectURI(uri) {
			oauthError(response, http.StatusBadRequest, "invalid_redirect_uri", "redirect URIs must be https, or http on localhost")
			return
		}
	}
	client := mcpClient{Name: cleanClientName(body.ClientName), RedirectURIs: body.RedirectURIs}
	encoded, _ := json.Marshal(client)
	clientID := base64.RawURLEncoding.EncodeToString(encoded)
	if len(clientID) > mcpMaxClientIDBytes {
		oauthError(response, http.StatusBadRequest, "invalid_client_metadata", "client metadata is too large")
		return
	}
	writeJSON(response, http.StatusCreated, map[string]any{
		"client_id": clientID, "client_id_issued_at": g.now().Unix(), "client_name": client.Name,
		"redirect_uris": client.RedirectURIs, "grant_types": []string{"authorization_code", "refresh_token"},
		"response_types": []string{"code"}, "token_endpoint_auth_method": "none",
	})
}

func cleanClientName(name string) string {
	var builder strings.Builder
	for _, r := range strings.TrimSpace(name) {
		if r >= 0x20 && r != 0x7f && builder.Len() < 60 {
			builder.WriteRune(r)
		}
	}
	if builder.Len() == 0 {
		return "AI assistant"
	}
	return builder.String()
}

func parseClientID(clientID string) (mcpClient, bool) {
	var client mcpClient
	if clientID == "" || len(clientID) > mcpMaxClientIDBytes {
		return client, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(clientID)
	if err != nil || json.Unmarshal(decoded, &client) != nil || len(client.RedirectURIs) == 0 || len(client.RedirectURIs) > 5 {
		return client, false
	}
	for _, uri := range client.RedirectURIs {
		if !validRedirectURI(uri) {
			return client, false
		}
	}
	client.Name = cleanClientName(client.Name)
	return client, true
}

func (g *mcpGateway) randomString(bytes int) string {
	value := make([]byte, bytes)
	if _, err := io.ReadFull(g.random, value); err != nil {
		panic(fmt.Sprintf("read random bytes: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(value)
}

func (g *mcpGateway) randomHex(bytes int) string {
	value := make([]byte, bytes)
	if _, err := io.ReadFull(g.random, value); err != nil {
		panic(fmt.Sprintf("read random bytes: %v", err))
	}
	return hex.EncodeToString(value)
}

func (g *mcpGateway) newCode() string {
	value := make([]byte, 8)
	if _, err := io.ReadFull(g.random, value); err != nil {
		panic(fmt.Sprintf("read random bytes: %v", err))
	}
	code := make([]byte, 0, 9)
	for i, b := range value {
		if i == 4 {
			code = append(code, '-')
		}
		code = append(code, mcpCodeAlphabet[int(b)%len(mcpCodeAlphabet)])
	}
	return string(code)
}

// normalizeMCPCode accepts a code typed with or without its dash, in any case.
func normalizeMCPCode(code string) string {
	code = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))
	if len(code) != 8 {
		return ""
	}
	return code[:4] + "-" + code[4:]
}

func hashSecret(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// sweep drops expired pending authorizations and access tokens. Call with
// g.mu held.
func (g *mcpGateway) sweep(now time.Time) {
	for code, pending := range g.pending {
		if !now.Before(pending.expires) {
			delete(g.pending, code)
			delete(g.byPoll, pending.poll)
			if pending.authHash != "" {
				delete(g.byAuth, pending.authHash)
			}
		}
	}
	for hash, token := range g.tokens {
		if !now.Before(token.expires) {
			delete(g.tokens, hash)
		}
	}
}

func (g *mcpGateway) authorize(response http.ResponseWriter, request *http.Request, base string) {
	if request.Method != http.MethodGet {
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	query := request.URL.Query()
	client, ok := parseClientID(query.Get("client_id"))
	redirectURI := query.Get("redirect_uri")
	if !ok {
		g.page(response, http.StatusBadRequest, authorizePage{Error: "This assistant is not registered. Remove the connector and add it again."})
		return
	}
	known := false
	for _, uri := range client.RedirectURIs {
		known = known || uri == redirectURI
	}
	if !known {
		g.page(response, http.StatusBadRequest, authorizePage{Error: "The assistant asked to return to an address it did not register."})
		return
	}
	if query.Get("response_type") != "code" || query.Get("code_challenge_method") != "S256" || len(query.Get("code_challenge")) < 43 {
		g.page(response, http.StatusBadRequest, authorizePage{Error: "The assistant must use the authorization code flow with PKCE (S256)."})
		return
	}
	if resource := query.Get("resource"); resource != "" && resource != base+"/mcp" && resource != base {
		g.page(response, http.StatusBadRequest, authorizePage{Error: "The assistant asked for access to another server."})
		return
	}
	now := g.now()
	g.mu.Lock()
	g.sweep(now)
	if len(g.pending) >= mcpMaxPending {
		g.mu.Unlock()
		g.page(response, http.StatusTooManyRequests, authorizePage{Error: "Too many connections are waiting for approval. Try again in a few minutes."})
		return
	}
	pending := &mcpPending{
		code: g.newCode(), poll: g.randomString(32), clientID: query.Get("client_id"), client: client,
		redirectURI: redirectURI, challenge: query.Get("code_challenge"), state: query.Get("state"),
		expires: now.Add(mcpPendingLifetime),
	}
	for g.pending[pending.code] != nil {
		pending.code = g.newCode()
	}
	g.pending[pending.code] = pending
	g.byPoll[pending.poll] = pending
	g.mu.Unlock()
	redirectHost := redirectURI
	if parsed, err := url.Parse(redirectURI); err == nil {
		redirectHost = parsed.Host
	}
	g.page(response, http.StatusOK, authorizePage{Client: client.Name, RedirectHost: redirectHost, Code: pending.code, Poll: pending.poll, Domain: strings.TrimPrefix(base, "https://")})
}

func (g *mcpGateway) authorizeStatus(response http.ResponseWriter, request *http.Request, base string) {
	if request.Method != http.MethodGet {
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	now := g.now()
	g.mu.Lock()
	g.sweep(now)
	pending := g.byPoll[request.URL.Query().Get("poll")]
	var authCode, redirectURI, state string
	if pending != nil {
		authCode, redirectURI, state = pending.authCode, pending.redirectURI, pending.state
	}
	g.mu.Unlock()
	if pending == nil {
		writeJSON(response, http.StatusOK, map[string]string{"status": "expired"})
		return
	}
	if authCode == "" {
		writeJSON(response, http.StatusOK, map[string]string{"status": "pending"})
		return
	}
	target, _ := url.Parse(redirectURI)
	values := target.Query()
	values.Set("code", authCode)
	if state != "" {
		values.Set("state", state)
	}
	values.Set("iss", base)
	target.RawQuery = values.Encode()
	writeJSON(response, http.StatusOK, map[string]string{"status": "approved", "redirect": target.String()})
}

// approve turns the pending authorization with code into a stored
// connection and an authorization code for the waiting page.
func (g *mcpGateway) approve(ctx context.Context, code, access, application string, lifetime time.Duration) (model.MCPGrant, string, error) {
	code = normalizeMCPCode(code)
	now := g.now()
	g.mu.Lock()
	g.sweep(now)
	pending := g.pending[code]
	if pending == nil || pending.grantID != "" {
		g.mu.Unlock()
		return model.MCPGrant{}, "", connect.NewError(connect.CodeNotFound, errors.New("no assistant is waiting with this code; codes expire after 10 minutes, so start connecting again"))
	}
	// Claim it before storing, so one code approves once.
	pending.grantID = "claimed"
	client, clientID, redirectURI := pending.client, pending.clientID, pending.redirectURI
	g.mu.Unlock()
	release := func() {
		g.mu.Lock()
		pending.grantID = ""
		g.mu.Unlock()
	}
	grant := model.MCPGrant{
		ID: g.randomHex(8), ClientName: client.Name, ClientID: clientID,
		Application: application, Access: access, CreatedAt: now, ExpiresAt: now.Add(lifetime),
	}
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		grant.Name = mcpGrantName(client.Name, g.randomHex(2))
		if err = g.state.CreateMCPGrant(ctx, grant); !errors.Is(err, store.ErrAccessTokenExists) {
			break
		}
	}
	if err != nil {
		release()
		if strings.Contains(err.Error(), "at most") {
			return model.MCPGrant{}, "", connect.NewError(connect.CodeResourceExhausted, err)
		}
		return model.MCPGrant{}, "", connect.NewError(connect.CodeInternal, errors.New("store MCP connection"))
	}
	authCode := g.randomString(32)
	g.mu.Lock()
	pending.grantID = grant.ID
	// The waiting page reads the code to send the browser back.
	pending.authCode = authCode
	pending.authHash = hashSecret(authCode)
	pending.expires = now.Add(mcpCodeLifetime)
	g.byAuth[pending.authHash] = pending
	g.mu.Unlock()
	redirectHost := redirectURI
	if parsed, parseErr := url.Parse(redirectURI); parseErr == nil {
		redirectHost = parsed.Host
	}
	return grant, redirectHost, nil
}

// mcpGrantName derives a token-list name such as mcp-claude-ab12 from the
// assistant's name.
func mcpGrantName(clientName, suffix string) string {
	var slug strings.Builder
	for _, r := range strings.ToLower(clientName) {
		if slug.Len() >= 20 {
			break
		}
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			slug.WriteRune(r)
		case slug.Len() > 0 && !strings.HasSuffix(slug.String(), "-"):
			slug.WriteRune('-')
		}
	}
	name := strings.Trim(slug.String(), "-")
	if name == "" {
		return "mcp-" + suffix
	}
	return "mcp-" + name + "-" + suffix
}

func (g *mcpGateway) token(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := request.ParseForm(); err != nil {
		oauthError(response, http.StatusBadRequest, "invalid_request", "the request must be form encoded")
		return
	}
	form := request.PostForm
	switch form.Get("grant_type") {
	case "authorization_code":
		g.redeemCode(response, request.Context(), form)
	case "refresh_token":
		g.refresh(response, request.Context(), form)
	default:
		oauthError(response, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
	}
}

func pkceMatches(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	digest := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(digest[:])), []byte(challenge)) == 1
}

func (g *mcpGateway) redeemCode(response http.ResponseWriter, ctx context.Context, form url.Values) {
	now := g.now()
	hash := hashSecret(form.Get("code"))
	g.mu.Lock()
	g.sweep(now)
	pending := g.byAuth[hash]
	if pending != nil {
		// A code works once, whatever happens next.
		delete(g.byAuth, hash)
		delete(g.pending, pending.code)
		delete(g.byPoll, pending.poll)
	}
	g.mu.Unlock()
	if pending == nil || pending.clientID != form.Get("client_id") || pending.redirectURI != form.Get("redirect_uri") ||
		!pkceMatches(form.Get("code_verifier"), pending.challenge) {
		oauthError(response, http.StatusBadRequest, "invalid_grant", "the authorization code is invalid, expired or already used")
		return
	}
	grant, err := g.state.MCPGrant(ctx, pending.grantID)
	if err != nil || !now.Before(grant.ExpiresAt) || grant.RefreshHash != "" {
		oauthError(response, http.StatusBadRequest, "invalid_grant", "the connection was revoked")
		return
	}
	refresh := mcpRefreshTokenPrefix + grant.ID + "." + g.randomString(32)
	if ok, err := g.state.RotateMCPRefresh(ctx, grant.ID, "", hashSecret(refresh)); err != nil || !ok {
		oauthError(response, http.StatusBadRequest, "invalid_grant", "the authorization code was already used")
		return
	}
	g.issue(response, grant, refresh, now)
}

func (g *mcpGateway) refresh(response http.ResponseWriter, ctx context.Context, form url.Values) {
	now := g.now()
	presented := form.Get("refresh_token")
	rejected := func() {
		oauthError(response, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid or the connection was revoked")
	}
	id, _, ok := strings.Cut(strings.TrimPrefix(presented, mcpRefreshTokenPrefix), ".")
	if !ok || !strings.HasPrefix(presented, mcpRefreshTokenPrefix) || len(id) != 16 {
		rejected()
		return
	}
	grant, err := g.state.MCPGrant(ctx, id)
	if err != nil || !now.Before(grant.ExpiresAt) || grant.ClientID != form.Get("client_id") ||
		subtle.ConstantTimeCompare([]byte(grant.RefreshHash), []byte(hashSecret(presented))) != 1 {
		rejected()
		return
	}
	next := mcpRefreshTokenPrefix + grant.ID + "." + g.randomString(32)
	if ok, err := g.state.RotateMCPRefresh(ctx, grant.ID, grant.RefreshHash, hashSecret(next)); err != nil || !ok {
		rejected()
		return
	}
	g.issue(response, grant, next, now)
}

func (g *mcpGateway) issue(response http.ResponseWriter, grant model.MCPGrant, refresh string, now time.Time) {
	access := mcpAccessTokenPrefix + g.randomString(32)
	expires := now.Add(mcpAccessTokenLifetime)
	if grant.ExpiresAt.Before(expires) {
		expires = grant.ExpiresAt
	}
	g.mu.Lock()
	g.sweep(now)
	if len(g.tokens) >= mcpMaxAccessTokens {
		g.mu.Unlock()
		oauthError(response, http.StatusServiceUnavailable, "temporarily_unavailable", "too many active MCP sessions")
		return
	}
	g.tokens[hashSecret(access)] = mcpAccessToken{grantID: grant.ID, expires: expires}
	g.mu.Unlock()
	writeJSON(response, http.StatusOK, map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": int(expires.Sub(now).Seconds()),
		"refresh_token": refresh, "scope": grant.Access,
	})
}

// revokeGrant drops access tokens of a deleted connection.
func (g *mcpGateway) revokeGrant(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for hash, token := range g.tokens {
		if token.grantID == id {
			delete(g.tokens, hash)
		}
	}
}

// authenticate resolves a bearer token to a session: an access token the
// gateway issued, or an ORCH_TOKEN access token.
func (g *mcpGateway) authenticate(ctx context.Context, bearer string) (MCPSession, authenticatedPrincipal, bool) {
	now := g.now()
	if strings.HasPrefix(bearer, mcpAccessTokenPrefix) {
		g.mu.Lock()
		token, ok := g.tokens[hashSecret(bearer)]
		g.mu.Unlock()
		if !ok || !now.Before(token.expires) {
			return MCPSession{}, authenticatedPrincipal{}, false
		}
		// Read the connection on every request, so orch token revoke takes
		// effect at once.
		grant, err := g.state.MCPGrant(ctx, token.grantID)
		if err != nil || !now.Before(grant.ExpiresAt) || grant.RefreshHash == "" {
			return MCPSession{}, authenticatedPrincipal{}, false
		}
		g.mu.Lock()
		touched := now.Sub(g.lastTouch[grant.ID]) > time.Minute
		if touched {
			g.lastTouch[grant.ID] = now
		}
		g.mu.Unlock()
		if touched {
			_ = g.state.TouchMCPGrant(ctx, grant.ID, now)
		}
		principal := authenticatedPrincipal{inProcess: true, application: grant.Application, readOnly: grant.Access == model.MCPAccessRead}
		return MCPSession{Name: grant.Name, Access: grant.Access, Application: grant.Application}, principal, true
	}
	_, credential, err := security.ParseAccessToken(bearer)
	if err != nil {
		return MCPSession{}, authenticatedPrincipal{}, false
	}
	id, secret, ok := security.SplitAccessTokenCredential(credential)
	if !ok {
		return MCPSession{}, authenticatedPrincipal{}, false
	}
	token, err := g.state.AccessToken(ctx, id)
	if err != nil || !now.Before(token.ExpiresAt) ||
		subtle.ConstantTimeCompare([]byte(token.SecretHash), []byte(security.AccessTokenSecretHash(secret))) != 1 {
		return MCPSession{}, authenticatedPrincipal{}, false
	}
	_ = g.state.TouchAccessToken(ctx, token.ID, now)
	// An access token gets what it gets on the CLI, without the tools that
	// delete things or run commands.
	access := model.MCPAccessDeploy
	if token.ReadOnly {
		access = model.MCPAccessRead
	}
	principal := authenticatedPrincipal{inProcess: true, application: token.Application, readOnly: token.ReadOnly, domains: token.Domains}
	return MCPSession{Name: token.Name, Access: access, Application: token.Application}, principal, true
}

func (g *mcpGateway) mcp(response http.ResponseWriter, request *http.Request, base string) {
	if origin := request.Header.Get("Origin"); origin != "" && origin != base {
		// Browsers may not drive the MCP endpoint from other sites.
		http.Error(response, "cross-origin requests are not allowed", http.StatusForbidden)
		return
	}
	bearer, found := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	metadata := fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource"`, base)
	if !found || bearer == "" {
		response.Header().Set("WWW-Authenticate", metadata)
		http.Error(response, "authorization is required", http.StatusUnauthorized)
		return
	}
	session, principal, ok := g.authenticate(request.Context(), strings.TrimSpace(bearer))
	if !ok {
		response.Header().Set("WWW-Authenticate", metadata+`, error="invalid_token"`)
		http.Error(response, "the access token is invalid, expired or revoked", http.StatusUnauthorized)
		return
	}
	if g.runner == nil {
		http.Error(response, "this controller does not serve MCP tools", http.StatusNotImplemented)
		return
	}
	listener := newPipeListener()
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	server := &http.Server{
		Handler:   http.MaxBytesHandler(restrictMCPAccess(principal, session.Access, g.api), maxHTTPBodyBytes),
		Protocols: protocols, ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	session.Dial = listener.Dial
	g.runner.ServeMCP(response, request, session)
}

// restrictMCPAccess admits only the API procedures the access level needs
// and authenticates them as principal. Procedures not listed, including any
// added later, are refused.
func restrictMCPAccess(principal authenticatedPrincipal, access string, next http.Handler) http.Handler {
	allowed := mcpProcedures(access)
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !allowed[request.URL.Path] {
			_ = connect.NewErrorWriter().Write(response, request, connect.NewError(connect.CodePermissionDenied,
				fmt.Errorf("this MCP connection has %s access, which does not allow this", access)))
			return
		}
		ctx := context.WithValue(request.Context(), principalKey{}, principal)
		next.ServeHTTP(response, request.WithContext(ctx))
	})
}

// pipeListener hands out in-memory connections, so the MCP tools reach the
// control API without a socket anyone else could open.
type pipeListener struct {
	connections chan net.Conn
	done        chan struct{}
	once        sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{connections: make(chan net.Conn), done: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case connection := <-l.connections:
		return connection, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

func (l *pipeListener) Dial(ctx context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.connections <- server:
		return client, nil
	case <-l.done:
		_, _ = client.Close(), server.Close()
		return nil, net.ErrClosed
	case <-ctx.Done():
		_, _ = client.Close(), server.Close()
		return nil, ctx.Err()
	}
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "mcp" }

type authorizePage struct {
	Error        string
	Client       string
	RedirectHost string
	Code         string
	Poll         string
	Domain       string
	Nonce        string
}

var authorizeTemplate = template.Must(template.New("authorize").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Connect to orch</title>
<style nonce="{{.Nonce}}">
:root{color-scheme:light dark;--bg:#fafaf9;--fg:#1c1917;--muted:#57534e;--card:#fff;--line:#e7e5e4;--code:#f5f5f4}
@media (prefers-color-scheme:dark){:root{--bg:#0c0a09;--fg:#fafaf9;--muted:#a8a29e;--card:#1c1917;--line:#292524;--code:#292524}}
body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,sans-serif;display:grid;place-items:center;min-height:100vh}
main{max-width:30rem;margin:1rem;padding:2rem;background:var(--card);border:1px solid var(--line);border-radius:12px}
h1{font-size:1.25rem;margin:0 0 1rem}p{margin:.75rem 0;color:var(--muted)}strong{color:var(--fg)}
.code{font:600 2rem/1.2 ui-monospace,monospace;letter-spacing:.1em;text-align:center;margin:1.25rem 0}
pre{background:var(--code);padding:.75rem 1rem;border-radius:8px;overflow-x:auto;font-size:.9rem}
</style></head><body><main>
{{if .Error}}<h1>Cannot connect</h1><p>{{.Error}}</p>
{{else}}<h1>Connect {{.Client}} to orch</h1>
<p><strong>{{.Client}}</strong> wants to use the orch cluster at <strong>{{.Domain}}</strong>, and will be sent back to <strong>{{.RedirectHost}}</strong>.</p>
<div class="code">{{.Code}}</div>
<p>To allow it, run this on a computer with orch administrator access:</p>
<pre>orch mcp approve {{.Code}}</pre>
<p>It can then only read the cluster. Add <code>--deploy</code> to also let it roll back, restart and back up, <code>--allow-destructive</code> for everything, and <code>--app NAME</code> to limit it to one application.</p>
<p id="status">Waiting for approval. This page continues by itself.</p>
<script nonce="{{.Nonce}}">
(function(){var poll={{.Poll}};function check(){fetch("/authorize/status?poll="+encodeURIComponent(poll),{cache:"no-store"}).then(function(r){return r.json()}).then(function(s){
if(s.status==="approved"){document.getElementById("status").textContent="Approved. Returning to the assistant.";window.location.replace(s.redirect);return}
if(s.status==="expired"){document.getElementById("status").textContent="This request expired. Start connecting again from the assistant.";return}
setTimeout(check,2000)}).catch(function(){setTimeout(check,4000)})}setTimeout(check,2000)})();
</script>{{end}}
</main></body></html>`))

func (g *mcpGateway) page(response http.ResponseWriter, status int, page authorizePage) {
	page.Nonce = g.randomString(16)
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("X-Frame-Options", "DENY")
	response.Header().Set("Content-Security-Policy", fmt.Sprintf("default-src 'none'; script-src 'nonce-%[1]s'; style-src 'nonce-%[1]s'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'", page.Nonce))
	response.WriteHeader(status)
	_ = authorizeTemplate.Execute(response, page)
}
