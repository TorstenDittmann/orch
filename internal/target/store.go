package target

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
)

const (
	Version         = "v1alpha1"
	ReferencePrefix = "target://"
	TransportDirect = "direct"
	TransportWG     = "wireguard"
	TransportSSH    = "ssh"
	// TransportSocket reaches the controller's local administrator socket,
	// directly on the controller host or forwarded over SSH. It needs no
	// administrator certificate: the SSH login (as root) is the credential.
	TransportSocket = "socket"
	// DefaultAdminSocket is where a managed controller serves its
	// administrator socket (host.ControllerDataDir + "/admin.sock").
	DefaultAdminSocket = "/var/lib/orch-controller/admin.sock"
)

var (
	namePattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	clusterIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

var ErrNotFound = errors.New("target was not found")

// ErrNoCurrent reports that no target has been selected.
var ErrNoCurrent = errors.New("no target is selected; run 'orch target add NAME ssh://root@CONTROLLER' or 'orch target use NAME'")

type Transport struct {
	Type           string `json:"type"`
	Address        string `json:"address,omitempty"`
	SSHHost        string `json:"ssh_host,omitempty"`
	SSHDestination string `json:"ssh_destination,omitempty"`
	SSHPort        int    `json:"ssh_port,omitempty"`
	Socket         string `json:"socket,omitempty"`
}

// Cluster records the identity a socket target connected to when it was
// added, so later connections detect a different cluster at the same host.
type Cluster struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Manifest struct {
	Version   string    `json:"version"`
	Name      string    `json:"name"`
	Context   string    `json:"context,omitempty"`
	Transport Transport `json:"transport"`
	Cluster   *Cluster  `json:"cluster,omitempty"`
}

type Resolved struct {
	Name        string
	ContextPath string
	Transport   Transport
	// Cluster is set for socket targets, which have no context file.
	Cluster Cluster
}

type Store struct {
	root string
}

func Default() (*Store, error) {
	root := os.Getenv("XDG_CONFIG_HOME")
	if root == "" {
		var err error
		root, err = os.UserConfigDir()
		if err != nil {
			return nil, fmt.Errorf("resolve user configuration directory: %w", err)
		}
	}
	absolute, err := filepath.Abs(filepath.Join(root, "orch"))
	if err != nil {
		return nil, fmt.Errorf("resolve target store: %w", err)
	}
	return &Store{root: absolute}, nil
}

func Open(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("target store path is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve target store: %w", err)
	}
	if err := ensurePrivateDirectory(absolute); err != nil {
		return nil, err
	}
	if err := ensurePrivateDirectory(filepath.Join(absolute, "targets")); err != nil {
		return nil, err
	}
	return &Store{root: absolute}, nil
}

func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("target name %q must match %s", name, namePattern.String())
	}
	return nil
}

func Reference(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	return ReferencePrefix + name, nil
}

func IsReference(value string) bool { return strings.HasPrefix(value, ReferencePrefix) }

func (s *Store) Import(name, sourceContext string) (Resolved, error) {
	return s.importContext(name, sourceContext, false)
}

// Replace swaps the credentials of an existing target for a renewed context
// of the same cluster and keeps its transport. The current credentials may
// already be expired; only the replacement is required to be valid.
func (s *Store) Replace(name, sourceContext string) (Resolved, error) {
	return s.importContext(name, sourceContext, true)
}

func (s *Store) importContext(name, sourceContext string, replace bool) (Resolved, error) {
	if err := ValidateName(name); err != nil {
		return Resolved{}, err
	}
	if strings.TrimSpace(sourceContext) == "" {
		return Resolved{}, fmt.Errorf("administrator context path is required")
	}
	if err := ensurePrivateDirectory(s.root); err != nil {
		return Resolved{}, err
	}
	if err := ensurePrivateDirectory(filepath.Join(s.root, "targets")); err != nil {
		return Resolved{}, err
	}
	absolute, err := filepath.Abs(sourceContext)
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve administrator context: %w", err)
	}
	credentials, err := clusterctl.LoadAdministratorCredentials(absolute, time.Now())
	if err != nil {
		return Resolved{}, err
	}
	if !clusterIDPattern.MatchString(credentials.Context.Cluster.ID) {
		return Resolved{}, fmt.Errorf("administrator context has an invalid cluster id")
	}
	paths := []string{
		credentials.Context.Credentials.CertificateAuthority,
		credentials.Context.Credentials.ClientCertificate,
		credentials.Context.Credentials.ClientKey,
	}
	contents := make([][]byte, len(paths))
	for i, reference := range paths {
		path, err := resolveReference(absolute, reference)
		if err != nil {
			return Resolved{}, err
		}
		contents[i], err = readPrivateRegular(path)
		if err != nil {
			return Resolved{}, fmt.Errorf("read imported credential: %w", err)
		}
	}

	finalPath := s.targetPath(name)
	transport := Transport{Type: TransportDirect}
	if replace {
		existing, err := s.existingTarget(name)
		if err != nil {
			return Resolved{}, err
		}
		if existing.clusterID != credentials.Context.Cluster.ID {
			return Resolved{}, fmt.Errorf("target %q belongs to cluster %s, not %s", name, existing.clusterID, credentials.Context.Cluster.ID)
		}
		transport = existing.transport
	} else if _, err := os.Lstat(finalPath); err == nil {
		return Resolved{}, fmt.Errorf("target %q already exists", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Resolved{}, fmt.Errorf("inspect target %q: %w", name, err)
	}
	temporary, err := os.MkdirTemp(filepath.Join(s.root, "targets"), ".import-")
	if err != nil {
		return Resolved{}, fmt.Errorf("stage target import: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(temporary)
		}
	}()
	if err := os.Chmod(temporary, 0o700); err != nil {
		return Resolved{}, fmt.Errorf("protect staged target: %w", err)
	}

	identityDirectory := filepath.Join(temporary, "identities", credentials.Context.Cluster.ID)
	if err := os.MkdirAll(identityDirectory, 0o700); err != nil {
		return Resolved{}, fmt.Errorf("create managed identity directory: %w", err)
	}
	if err := os.Chmod(filepath.Join(temporary, "identities"), 0o700); err != nil {
		return Resolved{}, fmt.Errorf("protect managed identity root: %w", err)
	}
	credentialNames := []string{
		filepath.ToSlash(filepath.Join("identities", credentials.Context.Cluster.ID, "ca.pem")),
		filepath.ToSlash(filepath.Join("identities", credentials.Context.Cluster.ID, "admin.pem")),
		filepath.ToSlash(filepath.Join("identities", credentials.Context.Cluster.ID, "admin-key.pem")),
	}
	for i, filename := range credentialNames {
		if err := writePrivateFile(filepath.Join(temporary, filename), contents[i]); err != nil {
			return Resolved{}, err
		}
	}
	managedContext := credentials.Context
	managedContext.Credentials = clusterctl.ContextCredentials{
		CertificateAuthority: credentialNames[0],
		ClientCertificate:    credentialNames[1],
		ClientKey:            credentialNames[2],
	}
	if err := writeJSONFile(filepath.Join(temporary, "context.json"), managedContext); err != nil {
		return Resolved{}, err
	}
	manifest := Manifest{
		Version: Version, Name: name, Context: "context.json",
		Transport: transport,
	}
	if err := writeJSONFile(filepath.Join(temporary, "target.json"), manifest); err != nil {
		return Resolved{}, err
	}
	if replace {
		if err := swapDirectory(temporary, finalPath); err != nil {
			return Resolved{}, fmt.Errorf("replace target %q: %w", name, err)
		}
		cleanup = false
		return s.Get(name)
	}
	if err := os.Rename(temporary, finalPath); err != nil {
		if _, statErr := os.Lstat(finalPath); statErr == nil {
			return Resolved{}, fmt.Errorf("target %q already exists", name)
		}
		return Resolved{}, fmt.Errorf("publish target %q: %w", name, err)
	}
	cleanup = false
	return s.Get(name)
}

type existingTarget struct {
	clusterID string
	transport Transport
}

// existingTarget reads a target without validating credential lifetimes so
// expired credentials can be replaced.
func (s *Store) existingTarget(name string) (existingTarget, error) {
	directory := s.targetPath(name)
	if err := requirePrivateDirectory(directory); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return existingTarget{}, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return existingTarget{}, fmt.Errorf("validate target %q: %w", name, err)
	}
	var manifest Manifest
	if err := readStrictJSON(filepath.Join(directory, "target.json"), &manifest); err != nil {
		return existingTarget{}, fmt.Errorf("read target %q: %w", name, err)
	}
	if manifest.Version != Version || manifest.Name != name || manifest.Context != "context.json" {
		return existingTarget{}, fmt.Errorf("target %q has an invalid or unsupported manifest", name)
	}
	if err := ValidateTransport(manifest.Transport); err != nil {
		return existingTarget{}, fmt.Errorf("target %q: %w", name, err)
	}
	local, err := clusterctl.LoadContext(filepath.Join(directory, manifest.Context))
	if err != nil {
		return existingTarget{}, fmt.Errorf("target %q context: %w", name, err)
	}
	return existingTarget{clusterID: local.Cluster.ID, transport: manifest.Transport}, nil
}

// swapDirectory moves the current directory aside, publishes the staged one,
// and restores the original if publication fails.
func swapDirectory(staged, destination string) error {
	previous, err := os.MkdirTemp(filepath.Dir(destination), ".replaced-")
	if err != nil {
		return err
	}
	if err := os.Remove(previous); err != nil {
		return err
	}
	if err := os.Rename(destination, previous); err != nil {
		return err
	}
	if err := os.Rename(staged, destination); err != nil {
		if restoreErr := os.Rename(previous, destination); restoreErr != nil {
			return fmt.Errorf("%w; previous target kept at %s", err, previous)
		}
		return err
	}
	return os.RemoveAll(previous)
}

func (s *Store) Get(name string) (Resolved, error) {
	if err := ValidateName(name); err != nil {
		return Resolved{}, err
	}
	directory := s.targetPath(name)
	if err := requirePrivateDirectory(directory); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Resolved{}, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return Resolved{}, fmt.Errorf("validate target %q: %w", name, err)
	}
	var manifest Manifest
	if err := readStrictJSON(filepath.Join(directory, "target.json"), &manifest); err != nil {
		return Resolved{}, fmt.Errorf("read target %q: %w", name, err)
	}
	if manifest.Version != Version || manifest.Name != name {
		return Resolved{}, fmt.Errorf("target %q has an invalid or unsupported manifest", name)
	}
	if err := ValidateTransport(manifest.Transport); err != nil {
		return Resolved{}, fmt.Errorf("target %q: %w", name, err)
	}
	if manifest.Transport.Type == TransportSocket {
		if manifest.Context != "" || manifest.Cluster == nil || !clusterIDPattern.MatchString(manifest.Cluster.ID) || manifest.Cluster.Name == "" {
			return Resolved{}, fmt.Errorf("target %q has an invalid or unsupported manifest", name)
		}
		return Resolved{Name: name, Transport: manifest.Transport, Cluster: *manifest.Cluster}, nil
	}
	if manifest.Context != "context.json" || manifest.Cluster != nil {
		return Resolved{}, fmt.Errorf("target %q has an invalid or unsupported manifest", name)
	}
	contextPath := filepath.Join(directory, manifest.Context)
	if _, err := clusterctl.LoadAdministratorCredentials(contextPath, time.Now()); err != nil {
		return Resolved{}, fmt.Errorf("target %q credentials: %w", name, err)
	}
	return Resolved{Name: name, ContextPath: contextPath, Transport: manifest.Transport}, nil
}

// Remove deletes a target and its credentials. Removing a missing target is
// not an error.
func (s *Store) Remove(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if err := os.RemoveAll(s.targetPath(name)); err != nil {
		return fmt.Errorf("remove target %q: %w", name, err)
	}
	return nil
}

func (s *Store) List() ([]Resolved, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "targets"))
	if errors.Is(err, os.ErrNotExist) {
		return []Resolved{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list targets: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && namePattern.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	result := make([]Resolved, 0, len(names))
	for _, name := range names {
		resolved, err := s.Get(name)
		if err != nil {
			return nil, err
		}
		result = append(result, resolved)
	}
	return result, nil
}

func (s *Store) Use(name string) error {
	if _, err := s.Get(name); err != nil {
		return err
	}
	return writeJSONAtomic(filepath.Join(s.root, "current.json"), struct {
		Version string `json:"version"`
		Target  string `json:"target"`
	}{Version: Version, Target: name})
}

func (s *Store) Current() (Resolved, error) {
	var current struct {
		Version string `json:"version"`
		Target  string `json:"target"`
	}
	if err := readStrictJSON(filepath.Join(s.root, "current.json"), &current); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Resolved{}, ErrNoCurrent
		}
		return Resolved{}, fmt.Errorf("read current target: %w", err)
	}
	if current.Version != Version {
		return Resolved{}, fmt.Errorf("current target uses unsupported version %q", current.Version)
	}
	return s.Get(current.Target)
}

func (s *Store) SetTransport(name string, transport Transport) error {
	if err := ValidateTransport(transport); err != nil {
		return err
	}
	resolved, err := s.Get(name)
	if err != nil {
		return err
	}
	if (resolved.Transport.Type == TransportSocket) != (transport.Type == TransportSocket) {
		return fmt.Errorf("target %q cannot switch between socket and certificate transports; add a new target instead", name)
	}
	manifest := Manifest{Version: Version, Name: name, Transport: transport}
	if transport.Type == TransportSocket {
		cluster := resolved.Cluster
		manifest.Cluster = &cluster
	} else {
		manifest.Context = filepath.Base(resolved.ContextPath)
	}
	return writeJSONAtomic(filepath.Join(s.targetPath(name), "target.json"), manifest)
}

func (s *Store) ResolveReference(reference string) (Resolved, error) {
	if !IsReference(reference) {
		return Resolved{}, fmt.Errorf("invalid target reference")
	}
	return s.Get(strings.TrimPrefix(reference, ReferencePrefix))
}

func ValidateTransport(transport Transport) error {
	if transport.Type == TransportSocket {
		if transport.Address != "" || transport.SSHDestination != "" {
			return fmt.Errorf("socket transport does not accept a wireguard address or SSH destination")
		}
		// A colon would be misread by ssh -L forwarding syntax.
		if !strings.HasPrefix(transport.Socket, "/") || strings.ContainsAny(transport.Socket, "\x00\r\n:") {
			return fmt.Errorf("socket path must be absolute and must not contain a colon")
		}
		if transport.SSHHost == "" {
			if transport.SSHPort != 0 {
				return fmt.Errorf("SSH port requires an SSH host")
			}
		} else if err := validateSSHHost(transport.SSHHost); err != nil {
			return err
		}
		if transport.SSHPort < 0 || transport.SSHPort > 65535 {
			return fmt.Errorf("SSH port must be from 1 through 65535")
		}
		return nil
	}
	if transport.Socket != "" || transport.SSHPort != 0 {
		return fmt.Errorf("only socket transport accepts a socket path or SSH port")
	}
	switch transport.Type {
	case TransportDirect:
		if transport.Address != "" || transport.SSHHost != "" || transport.SSHDestination != "" {
			return fmt.Errorf("direct transport does not accept routing options")
		}
	case TransportWG:
		if err := validateAddress(transport.Address); err != nil {
			return fmt.Errorf("wireguard address: %w", err)
		}
		if transport.SSHHost != "" || transport.SSHDestination != "" {
			return fmt.Errorf("wireguard transport does not accept SSH options")
		}
	case TransportSSH:
		if err := validateSSHHost(transport.SSHHost); err != nil {
			return err
		}
		if transport.Address != "" {
			return fmt.Errorf("SSH transport does not accept a wireguard address")
		}
		if transport.SSHDestination != "" {
			if err := validateAddress(transport.SSHDestination); err != nil {
				return fmt.Errorf("SSH destination: %w", err)
			}
		}
	default:
		return fmt.Errorf("transport must be direct, wireguard, ssh, or socket")
	}
	return nil
}

func validateSSHHost(host string) error {
	if strings.TrimSpace(host) == "" || strings.HasPrefix(host, "-") || strings.ContainsAny(host, "\x00\r\n") {
		return fmt.Errorf("SSH host must be a safe OpenSSH host or alias")
	}
	return nil
}

// AddSocket remembers a controller reached through its administrator socket.
// cluster is the identity the caller verified over that transport.
func (s *Store) AddSocket(name string, transport Transport, cluster Cluster) (Resolved, error) {
	if err := ValidateName(name); err != nil {
		return Resolved{}, err
	}
	if transport.Type != TransportSocket {
		return Resolved{}, fmt.Errorf("AddSocket requires socket transport")
	}
	if err := ValidateTransport(transport); err != nil {
		return Resolved{}, err
	}
	if !clusterIDPattern.MatchString(cluster.ID) || strings.TrimSpace(cluster.Name) == "" {
		return Resolved{}, fmt.Errorf("controller returned an invalid cluster identity")
	}
	if err := ensurePrivateDirectory(s.root); err != nil {
		return Resolved{}, err
	}
	if err := ensurePrivateDirectory(filepath.Join(s.root, "targets")); err != nil {
		return Resolved{}, err
	}
	finalPath := s.targetPath(name)
	if _, err := os.Lstat(finalPath); err == nil {
		return Resolved{}, fmt.Errorf("target %q already exists", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Resolved{}, fmt.Errorf("inspect target %q: %w", name, err)
	}
	temporary, err := os.MkdirTemp(filepath.Join(s.root, "targets"), ".add-")
	if err != nil {
		return Resolved{}, fmt.Errorf("stage target: %w", err)
	}
	defer os.RemoveAll(temporary)
	if err := os.Chmod(temporary, 0o700); err != nil {
		return Resolved{}, fmt.Errorf("protect staged target: %w", err)
	}
	manifest := Manifest{Version: Version, Name: name, Transport: transport, Cluster: &cluster}
	if err := writeJSONFile(filepath.Join(temporary, "target.json"), manifest); err != nil {
		return Resolved{}, err
	}
	if err := os.Rename(temporary, finalPath); err != nil {
		if _, statErr := os.Lstat(finalPath); statErr == nil {
			return Resolved{}, fmt.Errorf("target %q already exists", name)
		}
		return Resolved{}, fmt.Errorf("publish target %q: %w", name, err)
	}
	return s.Get(name)
}

func validateAddress(value string) error {
	if strings.TrimSpace(value) != value || strings.ContainsAny(value, "/@?#\x00\r\n") {
		return fmt.Errorf("must be a host:port value")
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("must be a host:port value")
	}
	if strings.TrimSpace(host) != host || strings.ContainsAny(host, " \t") {
		return fmt.Errorf("host must not contain whitespace")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("port must be a number from 1 through 65535")
	}
	return nil
}

func (s *Store) targetPath(name string) string { return filepath.Join(s.root, "targets", name) }

func ensurePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create target directory: %w", err)
		}
		return os.Chmod(path, 0o700)
	}
	if err != nil {
		return fmt.Errorf("inspect target directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("target store %s is not a directory", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("protect target directory: %w", err)
		}
	}
	return nil
}

func requirePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a directory", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s permissions %04o allow group or world access", path, info.Mode().Perm())
	}
	return nil
}

func resolveReference(contextPath, reference string) (string, error) {
	native := filepath.FromSlash(reference)
	if strings.TrimSpace(reference) == "" || filepath.IsAbs(native) || filepath.Clean(native) != native || native == "." || native == ".." || strings.HasPrefix(native, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid imported credential path %q", reference)
	}
	return filepath.Join(filepath.Dir(contextPath), native), nil
}

func readPrivateRegular(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s permissions %04o allow group or world access", path, info.Mode().Perm())
	}
	return os.ReadFile(path)
}

func writePrivateFile(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync %s: %w", filepath.Base(path), err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	return nil
}

func writeJSONFile(path string, value any) error {
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	return writePrivateFile(path, contents)
}

func writeJSONAtomic(path string, value any) error {
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".target-")
	if err != nil {
		return fmt.Errorf("stage %s: %w", filepath.Base(path), err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
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
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish %s: %w", filepath.Base(path), err)
	}
	return nil
}

func readStrictJSON(path string, destination any) error {
	contents, err := readPrivateRegular(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("contains more than one JSON value")
		}
		return err
	}
	return nil
}
