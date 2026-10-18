package control

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	githubActionsIssuer = "https://token.actions.githubusercontent.com"
	githubJWKSURL       = githubActionsIssuer + "/.well-known/jwks"
	maxGitHubTokenBytes = 16 << 10
	githubClockSkew     = time.Minute
)

// GitHubAudience is the OIDC audience a workflow requests for this cluster,
// so a token minted for another service or cluster is never accepted.
func GitHubAudience(clusterID string) string {
	return "orch:" + clusterID
}

// githubClaims are the verified GitHub Actions token claims orch relies on.
type githubClaims struct {
	Repository  string `json:"repository"`
	Ref         string `json:"ref"`
	Environment string `json:"environment"`
	SHA         string `json:"sha"`
	Actor       string `json:"actor"`
	RunID       string `json:"run_id"`
	EventName   string `json:"event_name"`
}

// githubVerifier checks RS256 tokens issued by GitHub Actions against
// GitHub's published keys, which it caches and refreshes on unknown key IDs.
type githubVerifier struct {
	issuer  string
	jwksURL string
	client  *http.Client
	now     func() time.Time

	mutex     sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

func newGitHubVerifier(now func() time.Time) *githubVerifier {
	return &githubVerifier{issuer: githubActionsIssuer, jwksURL: githubJWKSURL, client: &http.Client{Timeout: 10 * time.Second}, now: now}
}

func (v *githubVerifier) verify(ctx context.Context, token, audience string) (githubClaims, error) {
	if len(token) > maxGitHubTokenBytes {
		return githubClaims{}, errors.New("token is too large")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return githubClaims{}, errors.New("token is not a JWT")
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	if err := decodeJWTPart(parts[0], &header); err != nil || header.Algorithm != "RS256" || header.KeyID == "" {
		return githubClaims{}, errors.New("token must be signed with RS256 and name its key")
	}
	key, err := v.key(ctx, header.KeyID)
	if err != nil {
		return githubClaims{}, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return githubClaims{}, errors.New("token signature is malformed")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return githubClaims{}, errors.New("token signature is invalid")
	}
	var registered struct {
		Issuer    string          `json:"iss"`
		Audience  json.RawMessage `json:"aud"`
		ExpiresAt int64           `json:"exp"`
		NotBefore int64           `json:"nbf"`
	}
	var claims githubClaims
	if err := decodeJWTPart(parts[1], &registered); err != nil || decodeJWTPart(parts[1], &claims) != nil {
		return githubClaims{}, errors.New("token claims are malformed")
	}
	now := v.now()
	switch {
	case registered.Issuer != v.issuer:
		return githubClaims{}, errors.New("token was not issued by GitHub Actions")
	case !audienceContains(registered.Audience, audience):
		return githubClaims{}, fmt.Errorf("token audience must be %s", audience)
	case registered.ExpiresAt == 0 || !now.Before(time.Unix(registered.ExpiresAt, 0).Add(githubClockSkew)):
		return githubClaims{}, errors.New("token has expired")
	case registered.NotBefore != 0 && now.Add(githubClockSkew).Before(time.Unix(registered.NotBefore, 0)):
		return githubClaims{}, errors.New("token is not valid yet")
	case claims.Repository == "" || claims.Ref == "":
		return githubClaims{}, errors.New("token does not name a repository and ref")
	}
	return claims, nil
}

func decodeJWTPart(part string, value any) error {
	data, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func audienceContains(raw json.RawMessage, audience string) bool {
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return single == audience
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, value := range many {
			if value == audience {
				return true
			}
		}
	}
	return false
}

// key returns a cached signing key, refetching GitHub's key set when the key
// is unknown (at most once a minute) or the cache is older than an hour.
func (v *githubVerifier) key(ctx context.Context, keyID string) (*rsa.PublicKey, error) {
	v.mutex.Lock()
	defer v.mutex.Unlock()
	now := v.now()
	key, ok := v.keys[keyID]
	stale := now.Sub(v.fetchedAt) > time.Hour
	if ok && !stale {
		return key, nil
	}
	if !ok && !stale && now.Sub(v.fetchedAt) < time.Minute {
		return nil, errors.New("token signing key is unknown")
	}
	keys, err := v.fetchKeys(ctx)
	if err != nil {
		if ok {
			return key, nil
		}
		return nil, fmt.Errorf("fetch GitHub signing keys: %w", err)
	}
	v.keys, v.fetchedAt = keys, now
	if key, ok = keys[keyID]; !ok {
		return nil, errors.New("token signing key is unknown")
	}
	return key, nil
}

func (v *githubVerifier) fetchKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := v.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", response.Status)
	}
	var set struct {
		Keys []struct {
			KeyType string `json:"kty"`
			KeyID   string `json:"kid"`
			N       string `json:"n"`
			E       string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("decode key set: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, jwk := range set.Keys {
		if jwk.KeyType != "RSA" || jwk.KeyID == "" {
			continue
		}
		n, nErr := base64.RawURLEncoding.DecodeString(jwk.N)
		e, eErr := base64.RawURLEncoding.DecodeString(jwk.E)
		if nErr != nil || eErr != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		key := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if key.N.BitLen() < 2048 {
			continue
		}
		keys[jwk.KeyID] = key
	}
	if len(keys) == 0 {
		return nil, errors.New("key set contains no usable RSA keys")
	}
	return keys, nil
}
