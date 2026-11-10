// Package release holds the public keys that sign orch releases.
package release

import _ "embed"

// AllowedSigners is the allowed_signers file for release signatures.
//
//go:embed allowed_signers
var AllowedSigners []byte
