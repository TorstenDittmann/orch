//go:build linux

package control

import (
	"errors"
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func authorizeSocketPeer(connection net.Conn) error {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return errors.New("admin socket connection is not a unix socket")
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return err
	}
	var credentials *unix.Ucred
	var credentialErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, credentialErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if credentialErr != nil {
		return credentialErr
	}
	if credentials.Uid != 0 && int(credentials.Uid) != os.Getuid() {
		return fmt.Errorf("admin socket peer uid %d is not authorized", credentials.Uid)
	}
	return nil
}
