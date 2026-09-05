// Package controller embeds the reviewed controller host bundle.
package controller

import "embed"

// Files contains the controller systemd unit template.
//
//go:embed orch-controller.service
var Files embed.FS
