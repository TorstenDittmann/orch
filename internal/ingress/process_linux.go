//go:build linux

package ingress

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func validateProcessOptions(options CaddyOptions) (processIdentity, error) {
	account, err := user.Lookup(options.User)
	if err != nil {
		return processIdentity{}, fmt.Errorf("look up dedicated Caddy user: %w", err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil || uid == 0 {
		return processIdentity{}, fmt.Errorf("Caddy user must be a dedicated non-root numeric identity")
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil || gid == 0 {
		return processIdentity{}, fmt.Errorf("Caddy group must be a dedicated non-root numeric identity")
	}
	binaryInfo, err := os.Lstat(options.Binary)
	if err != nil || !binaryInfo.Mode().IsRegular() || binaryInfo.Mode().Perm()&0o111 == 0 || binaryInfo.Mode().Perm()&0o022 != 0 {
		return processIdentity{}, fmt.Errorf("Caddy binary must be an executable regular file that is not group/world writable")
	}
	stat, ok := binaryInfo.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return processIdentity{}, fmt.Errorf("Caddy binary must be owned by root")
	}
	capabilities := make([]byte, 24)
	count, err := unix.Getxattr(options.Binary, "security.capability", capabilities)
	if err != nil || !hasOnlyNetBindServiceFileCapability(capabilities[:count]) {
		return processIdentity{}, fmt.Errorf("Caddy binary must have only cap_net_bind_service in its effective and permitted file capabilities")
	}
	data, err := os.Lstat(options.DataDir)
	if err != nil || !data.IsDir() || data.Mode().Perm()&0o077 != 0 {
		return processIdentity{}, fmt.Errorf("Caddy data directory must be a private non-symlink directory")
	}
	dataStat, ok := data.Sys().(*syscall.Stat_t)
	if !ok || int(dataStat.Uid) != uid || int(dataStat.Gid) != gid {
		return processIdentity{}, fmt.Errorf("Caddy data directory must be owned by the dedicated Caddy identity")
	}
	if filepath.Dir(options.AdminSocket) != options.DataDir {
		return processIdentity{}, fmt.Errorf("Caddy admin socket is outside its data directory")
	}
	state, err := os.Lstat(options.StateDir)
	if err != nil || !state.IsDir() || state.Mode().Perm()&0o077 != 0 {
		return processIdentity{}, fmt.Errorf("Caddy agent state directory must be a private non-symlink directory")
	}
	stateStat, ok := state.Sys().(*syscall.Stat_t)
	if !ok || stateStat.Uid != 0 {
		return processIdentity{}, fmt.Errorf("Caddy agent state directory must be owned by root")
	}
	if err := restrictCaddyBinary(options.Binary, binaryInfo, gid, capabilities[:count]); err != nil {
		return processIdentity{}, err
	}
	return processIdentity{uid: uid, gid: gid}, nil
}

// restrictCaddyBinary limits a world-executable Caddy, as older installs
// left it, to root and the ingress group: its file capability lets whoever
// runs it bind ports below 1024. chown drops file capabilities, so they are
// written back afterwards.
func restrictCaddyBinary(path string, info os.FileInfo, gid int, capabilities []byte) error {
	if info.Mode().Perm()&0o007 == 0 || os.Geteuid() != 0 {
		return nil
	}
	if err := os.Chown(path, 0, gid); err != nil {
		return fmt.Errorf("restrict Caddy binary: %w", err)
	}
	if err := os.Chmod(path, 0o750); err != nil {
		return fmt.Errorf("restrict Caddy binary: %w", err)
	}
	if err := unix.Setxattr(path, "security.capability", capabilities, 0); err != nil {
		return fmt.Errorf("restore Caddy file capability: %w", err)
	}
	return nil
}

func inspectRunningProcess(options CaddyOptions, identity processIdentity) (runningProcessIdentity, error) {
	pidPath := filepath.Join(options.DataDir, "caddy.pid")
	pidInfo, err := os.Lstat(pidPath)
	if err != nil || !pidInfo.Mode().IsRegular() || pidInfo.Mode().Perm()&0o022 != 0 || pidInfo.Size() > 32 {
		return runningProcessIdentity{}, fmt.Errorf("managed Caddy pid file is missing or unsafe")
	}
	pidStat, ok := pidInfo.Sys().(*syscall.Stat_t)
	if !ok || int(pidStat.Uid) != identity.uid {
		return runningProcessIdentity{}, fmt.Errorf("managed Caddy pid file has the wrong owner")
	}
	pidBytes, err := os.ReadFile(pidPath)
	if err != nil {
		return runningProcessIdentity{}, fmt.Errorf("read managed Caddy pid: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || pid < 2 {
		return runningProcessIdentity{}, fmt.Errorf("managed Caddy pid file is invalid")
	}
	running, err := inspectProcessPID(pid, options, identity)
	if err != nil {
		return runningProcessIdentity{}, err
	}
	socketDevice, socketInode, err := inspectAdminSocket(options, identity)
	if err != nil {
		return runningProcessIdentity{}, err
	}
	running.SocketDevice = socketDevice
	running.SocketInode = socketInode
	return running, nil
}

func inspectProcessPID(pid int, options CaddyOptions, identity processIdentity) (runningProcessIdentity, error) {
	if pid < 2 {
		return runningProcessIdentity{}, fmt.Errorf("managed Caddy pid is invalid")
	}
	executable, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return runningProcessIdentity{}, fmt.Errorf("inspect managed Caddy executable: %w", err)
	}
	wantExecutable, err := filepath.EvalSymlinks(options.Binary)
	if err != nil || executable != wantExecutable {
		return runningProcessIdentity{}, fmt.Errorf("managed Caddy executable does not match the pinned binary")
	}
	executableInfo, err := os.Stat(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return runningProcessIdentity{}, fmt.Errorf("stat managed Caddy executable: %w", err)
	}
	executableStat, ok := executableInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return runningProcessIdentity{}, fmt.Errorf("inspect managed Caddy executable identity")
	}
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return runningProcessIdentity{}, fmt.Errorf("inspect managed Caddy identity: %w", err)
	}
	wantUID := strconv.Itoa(identity.uid)
	wantGID := strconv.Itoa(identity.gid)
	uidMatches := false
	gidMatches := false
	for _, line := range strings.Split(string(status), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "Uid:" && fields[1] == wantUID && fields[2] == wantUID {
			uidMatches = true
		}
		if len(fields) >= 3 && fields[0] == "Gid:" && fields[1] == wantGID && fields[2] == wantGID {
			gidMatches = true
		}
	}
	if !uidMatches || !gidMatches {
		return runningProcessIdentity{}, fmt.Errorf("managed Caddy process does not run as the dedicated identity")
	}
	statContents, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return runningProcessIdentity{}, fmt.Errorf("inspect managed Caddy start identity: %w", err)
	}
	closing := strings.LastIndexByte(string(statContents), ')')
	fields := []string(nil)
	if closing >= 0 {
		fields = strings.Fields(string(statContents[closing+1:]))
	}
	if len(fields) < 20 {
		return runningProcessIdentity{}, fmt.Errorf("managed Caddy process stat is invalid")
	}
	startTime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || startTime == 0 {
		return runningProcessIdentity{}, fmt.Errorf("managed Caddy start identity is invalid")
	}
	return runningProcessIdentity{PID: pid, StartTime: startTime,
		Device: uint64(executableStat.Dev), Inode: executableStat.Ino}, nil
}

func inspectAdminSocket(options CaddyOptions, identity processIdentity) (uint64, uint64, error) {
	socket, err := os.Lstat(options.AdminSocket)
	if err != nil || socket.Mode()&os.ModeSocket == 0 || socket.Mode().Perm()&0o007 != 0 {
		return 0, 0, fmt.Errorf("managed Caddy admin socket is missing or accessible to other users")
	}
	socketStat, ok := socket.Sys().(*syscall.Stat_t)
	if !ok || int(socketStat.Uid) != identity.uid {
		return 0, 0, fmt.Errorf("managed Caddy admin socket has the wrong owner")
	}
	return uint64(socketStat.Dev), socketStat.Ino, nil
}

func removeOwnedStaleSocket(options CaddyOptions, identity processIdentity, metadata runningProcessIdentity) error {
	if _, err := os.Lstat(options.AdminSocket); os.IsNotExist(err) {
		return nil
	}
	device, inode, err := inspectAdminSocket(options, identity)
	if err != nil {
		return fmt.Errorf("refuse unsafe stale Caddy admin socket removal: %w", err)
	}
	if metadata.SocketInode != 0 && (metadata.SocketDevice != device || metadata.SocketInode != inode) {
		return fmt.Errorf("refuse substituted stale Caddy admin socket removal")
	}
	return os.Remove(options.AdminSocket)
}

func prepareProcessCommand(command *exec.Cmd, identity processIdentity) error {
	if identity.uid == 0 || identity.gid == 0 {
		return fmt.Errorf("refusing to run Caddy as root")
	}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: uint32(identity.uid), Gid: uint32(identity.gid), Groups: []uint32{uint32(identity.gid)},
	}}
	return nil
}

func protectProcessFile(path string, identity processIdentity, mode os.FileMode) error {
	if err := os.Chown(path, identity.uid, identity.gid); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func publishProcessFile(path string, contents []byte, identity processIdentity, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".publish-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := protectProcessFile(temporaryPath, identity, mode); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func openProcessLog(path string, identity processIdentity) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_APPEND|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = file.Close()
		return nil, fmt.Errorf("Caddy bootstrap log must be a regular file")
	}
	if err := unix.Fchown(fd, identity.uid, identity.gid); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	if stat.Size > 1<<20 {
		if err := unix.Ftruncate(fd, 0); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	return file, nil
}
