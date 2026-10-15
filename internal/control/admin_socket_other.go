//go:build !linux

package control

import "net"

// Outside Linux the owner-only socket mode is the access control.
func authorizeSocketPeer(net.Conn) error { return nil }
