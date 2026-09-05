// Package node contains the reviewed, embedded host installation artifacts.
package node

import "embed"

//go:embed artifacts.json *.service *.toml
var Files embed.FS
