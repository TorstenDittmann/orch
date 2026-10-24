package control

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"sync"

	"github.com/TorstenDittmann/orch/internal/model"
	"golang.org/x/crypto/argon2"
)

// Caddy's argon2id defaults, so ingress verifies at the cost it would choose.
const (
	basicAuthTime    = 1
	basicAuthMemory  = 46 * 1024
	basicAuthThreads = 1
	basicAuthKeyLen  = 32
)

var basicAuthHashes sync.Map

// basicAuthHash returns the argon2id PHC string ingress checks a basic auth
// password against. The salt is derived from the secret version, so the same
// version always hashes to the same string and replanning does not change the
// allocation spec digest. Each version is hashed once per controller process.
func basicAuthHash(secret model.ResolvedSecret) string {
	key := secret.Application + "\x00" + secret.Name + "\x00" + secret.VersionID
	if cached, ok := basicAuthHashes.Load(key); ok {
		return cached.(string)
	}
	digest := sha256.Sum256([]byte("orch basic auth salt\x00" + key))
	salt := digest[:16]
	derived := argon2.IDKey(secret.Value, salt, basicAuthTime, basicAuthMemory, basicAuthThreads, basicAuthKeyLen)
	hash := fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, basicAuthMemory, basicAuthTime, basicAuthThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(derived))
	basicAuthHashes.Store(key, hash)
	return hash
}
