package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
)

// AdminSocketFilename is the local administrator socket inside the controller
// data directory. Access is governed by filesystem permissions: the data
// directory and socket are private to the controller account, so only that
// account and root can connect, and the peer check rejects anyone else.
const AdminSocketFilename = "admin.sock"

// MaxAdminSocketPath is the portable sun_path limit (104 on BSD/macOS).
const MaxAdminSocketPath = 104

// listenAdminSocket replaces a stale socket left by a previous controller
// process (the data lock guarantees none is still serving) and listens with
// owner-only permissions.
func listenAdminSocket(ctx context.Context, path string) (net.Listener, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("admin socket path must be absolute")
	}
	if len(path) >= MaxAdminSocketPath {
		return nil, fmt.Errorf("admin socket path %s is longer than %d bytes", path, MaxAdminSocketPath-1)
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("admin socket path %s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale admin socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect admin socket: %w", err)
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on admin socket %s: %w", path, err)
	}
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(true)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("protect admin socket: %w", err)
	}
	return &peerCheckedListener{Listener: listener}, nil
}

// peerCheckedListener drops connections from processes that are neither root
// nor the controller account. File permissions already enforce this; the
// check keeps a misconfigured mode from silently granting administrator access.
type peerCheckedListener struct {
	net.Listener
}

func (listener *peerCheckedListener) Accept() (net.Conn, error) {
	for {
		connection, err := listener.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if err := authorizeSocketPeer(connection); err != nil {
			_ = connection.Close()
			continue
		}
		return connection, nil
	}
}

// authenticateLocalAdministrator marks every request on the admin socket as
// a local administrator. It must only wrap handlers served by the socket.
func authenticateLocalAdministrator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		ctx := context.WithValue(request.Context(), principalKey{}, authenticatedPrincipal{local: true})
		next.ServeHTTP(response, request.WithContext(ctx))
	})
}
