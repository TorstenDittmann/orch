package cluster

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/security"
	"change.codes/torsten/better-swarm/internal/store"
)

const (
	DatabaseFilename            = "control.db"
	controllerIdentityDirectory = "pki"
	administratorRootDirectory  = "identities"
)

var (
	ErrInvalidOptions = errors.New("invalid cluster options")
	clusterName       = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	hostnameLabel     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
)

type BootstrapOptions struct {
	Name        string
	Server      string
	DataDir     string
	ContextPath string
	Now         func() time.Time
	Random      io.Reader
}

type BootstrapResult struct {
	Cluster     model.Cluster
	DataDir     string
	ContextPath string
	Created     bool
}

type normalizedOptions struct {
	name        string
	server      string
	dataDir     string
	contextPath string
	now         func() time.Time
	random      io.Reader
}

// Bootstrap initializes one durable cluster or validates and returns an
// existing initialization. Existing credentials are never regenerated.
func Bootstrap(ctx context.Context, options BootstrapOptions) (result BootstrapResult, finalErr error) {
	normalized, err := normalizeOptions(options)
	if err != nil {
		return BootstrapResult{}, err
	}
	dataCreated, err := ensureDataDirectory(normalized.dataDir)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("prepare data directory: %w", err)
	}
	databasePath := filepath.Join(normalized.dataDir, DatabaseFilename)
	databaseExisted, err := pathExists(databasePath)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("inspect cluster database: %w", err)
	}
	initialized := false
	if !databaseExisted {
		file, err := os.OpenFile(databasePath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return BootstrapResult{}, fmt.Errorf("reserve cluster database: %w", err)
		}
		if err := file.Close(); err != nil {
			_ = os.Remove(databasePath)
			return BootstrapResult{}, fmt.Errorf("close new cluster database: %w", err)
		}
	}
	defer func() {
		if finalErr == nil || databaseExisted || initialized {
			return
		}
		for _, suffix := range []string{"-shm", "-wal", ""} {
			_ = os.Remove(databasePath + suffix)
		}
		if dataCreated {
			_ = os.Remove(normalized.dataDir)
		}
	}()

	var state *store.Store
	if databaseExisted {
		state, err = store.OpenInitialized(ctx, databasePath)
	} else {
		state, err = store.Open(ctx, databasePath)
	}
	if err != nil {
		if databaseExisted && errors.Is(err, store.ErrNotInitialized) {
			return BootstrapResult{}, fmt.Errorf("existing orch database is not initialized; refusing to overwrite it")
		}
		return BootstrapResult{}, fmt.Errorf("open cluster state: %w", err)
	}
	defer func() {
		if err := state.Close(); err != nil && finalErr == nil {
			finalErr = err
		}
	}()

	existing, err := state.Cluster(ctx)
	if err == nil {
		if err := validateExisting(normalized, existing); err != nil {
			return BootstrapResult{}, err
		}
		initialized = true
		return BootstrapResult{
			Cluster: existing, DataDir: normalized.dataDir,
			ContextPath: normalized.contextPath, Created: false,
		}, nil
	}
	if !errors.Is(err, store.ErrNotInitialized) {
		return BootstrapResult{}, fmt.Errorf("read cluster state: %w", err)
	}

	if err := requireAbsent(normalized.contextPath, "administrator context"); err != nil {
		return BootstrapResult{}, err
	}
	controllerDirectory := filepath.Join(normalized.dataDir, controllerIdentityDirectory)
	if err := requireAbsent(controllerDirectory, "controller identity directory"); err != nil {
		return BootstrapResult{}, err
	}

	clusterID, err := newClusterID(normalized.random)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("generate cluster id: %w", err)
	}
	createdAt := normalized.now().UTC()
	if createdAt.IsZero() {
		return BootstrapResult{}, fmt.Errorf("current time must not be zero")
	}
	identity, err := security.Generate(security.GenerateOptions{
		ClusterID: clusterID, ClusterName: normalized.name, Server: normalized.server,
		Now: createdAt, Random: normalized.random,
	})
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("generate cluster identity: %w", err)
	}

	contextDirectory := filepath.Dir(normalized.contextPath)
	contextDirectoryExisted, err := pathExists(contextDirectory)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("inspect context directory: %w", err)
	}
	if err := os.MkdirAll(contextDirectory, 0o700); err != nil {
		return BootstrapResult{}, fmt.Errorf("create context directory: %w", err)
	}
	if err := requireDirectory(contextDirectory); err != nil {
		return BootstrapResult{}, fmt.Errorf("validate context directory: %w", err)
	}
	keepContextDirectory := contextDirectoryExisted
	defer func() {
		if !keepContextDirectory {
			_ = os.Remove(contextDirectory)
		}
	}()
	identityRoot := filepath.Join(contextDirectory, administratorRootDirectory)
	identityRootCreated, err := ensurePrivateDirectory(identityRoot)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("prepare administrator identity root: %w", err)
	}
	keepIdentityRoot := !identityRootCreated
	defer func() {
		if !keepIdentityRoot {
			_ = os.Remove(identityRoot)
		}
	}()
	administratorDirectory := filepath.Join(identityRoot, clusterID)
	if err := requireAbsent(administratorDirectory, "administrator identity directory"); err != nil {
		return BootstrapResult{}, err
	}

	controllerFiles := map[string][]byte{
		"ca.pem":             identity.CACertificatePEM,
		"ca-key.pem":         identity.CAPrivateKeyPEM,
		"controller.pem":     identity.Controller.CertificatePEM,
		"controller-key.pem": identity.Controller.PrivateKeyPEM,
	}
	administratorFiles := map[string][]byte{
		"ca.pem":        identity.CACertificatePEM,
		"admin.pem":     identity.Administrator.CertificatePEM,
		"admin-key.pem": identity.Administrator.PrivateKeyPEM,
	}
	controllerStage, err := stageDirectory(normalized.dataDir, ".orch-pki-*", controllerFiles)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("stage controller identity: %w", err)
	}
	defer os.RemoveAll(controllerStage)
	administratorStage, err := stageDirectory(identityRoot, ".orch-admin-*", administratorFiles)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("stage administrator identity: %w", err)
	}
	defer os.RemoveAll(administratorStage)

	localContext := LocalContext{
		Version: ContextVersion,
		Cluster: ContextCluster{ID: clusterID, Name: normalized.name},
		Server:  normalized.server,
		Credentials: ContextCredentials{
			CertificateAuthority: relativeContextPath(contextDirectory, filepath.Join(administratorDirectory, "ca.pem")),
			ClientCertificate:    relativeContextPath(contextDirectory, filepath.Join(administratorDirectory, "admin.pem")),
			ClientKey:            relativeContextPath(contextDirectory, filepath.Join(administratorDirectory, "admin-key.pem")),
		},
	}
	contextContents, err := encodeContext(localContext)
	if err != nil {
		return BootstrapResult{}, err
	}
	contextStage, err := stageFile(contextDirectory, ".orch-context-*", contextContents)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("stage administrator context: %w", err)
	}
	defer os.Remove(contextStage)

	publishedController := false
	publishedAdministrator := false
	publishedContext := false
	committed := false
	defer func() {
		if committed {
			return
		}
		if publishedContext {
			_ = os.Remove(normalized.contextPath)
		}
		if publishedAdministrator {
			removePublishedDirectory(administratorDirectory, administratorFiles)
		}
		if publishedController {
			removePublishedDirectory(controllerDirectory, controllerFiles)
		}
	}()

	if err := publishDirectory(controllerStage, controllerDirectory, controllerFiles); err != nil {
		return BootstrapResult{}, fmt.Errorf("publish controller identity: %w", err)
	}
	publishedController = true
	if err := publishDirectory(administratorStage, administratorDirectory, administratorFiles); err != nil {
		return BootstrapResult{}, fmt.Errorf("publish administrator identity: %w", err)
	}
	publishedAdministrator = true
	if err := publishFile(contextStage, normalized.contextPath); err != nil {
		return BootstrapResult{}, fmt.Errorf("publish administrator context: %w", err)
	}
	publishedContext = true

	cluster := model.Cluster{
		ID: clusterID, Name: normalized.name, Endpoint: normalized.server,
		Generation: 1, CreatedAt: createdAt, WorkloadCIDR: model.DefaultWorkloadCIDR,
		NetworkGeneration: 1,
	}
	if err := state.InitializeCluster(ctx, cluster); err != nil {
		return BootstrapResult{}, fmt.Errorf("initialize cluster state: %w", err)
	}
	initialized = true
	committed = true
	keepIdentityRoot = true
	keepContextDirectory = true
	return BootstrapResult{
		Cluster: cluster, DataDir: normalized.dataDir,
		ContextPath: normalized.contextPath, Created: true,
	}, nil
}

func normalizeOptions(options BootstrapOptions) (normalizedOptions, error) {
	name := strings.TrimSpace(options.Name)
	if !clusterName.MatchString(name) {
		return normalizedOptions{}, fmt.Errorf("%w: name must be a lowercase DNS label of at most 63 characters", ErrInvalidOptions)
	}
	server, err := normalizeServer(options.Server)
	if err != nil {
		return normalizedOptions{}, fmt.Errorf("%w: %v", ErrInvalidOptions, err)
	}
	if strings.TrimSpace(options.DataDir) == "" || strings.TrimSpace(options.ContextPath) == "" {
		return normalizedOptions{}, fmt.Errorf("%w: data directory and context path are required", ErrInvalidOptions)
	}
	dataDir, err := filepath.Abs(options.DataDir)
	if err != nil {
		return normalizedOptions{}, fmt.Errorf("%w: resolve data directory: %v", ErrInvalidOptions, err)
	}
	contextPath, err := filepath.Abs(options.ContextPath)
	if err != nil {
		return normalizedOptions{}, fmt.Errorf("%w: resolve context path: %v", ErrInvalidOptions, err)
	}
	dataDir = filepath.Clean(dataDir)
	contextPath = filepath.Clean(contextPath)
	if filepath.Dir(dataDir) == dataDir {
		return normalizedOptions{}, fmt.Errorf("%w: data directory cannot be a filesystem root", ErrInvalidOptions)
	}
	if filepath.Dir(filepath.Dir(contextPath)) == filepath.Dir(contextPath) {
		return normalizedOptions{}, fmt.Errorf("%w: context must be inside a non-root directory", ErrInvalidOptions)
	}
	identityRoot := filepath.Join(filepath.Dir(contextPath), administratorRootDirectory)
	if contextPath == identityRoot || pathWithin(contextPath, dataDir) || pathsOverlap(dataDir, identityRoot) {
		return normalizedOptions{}, fmt.Errorf("%w: data, context, and administrator identity paths must not overlap", ErrInvalidOptions)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	random := options.Random
	if random == nil {
		random = rand.Reader
	}
	return normalizedOptions{name: name, server: server, dataDir: dataDir, contextPath: contextPath, now: now, random: random}, nil
}

func normalizeServer(raw string) (string, error) {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil {
		return "", fmt.Errorf("server must be an absolute HTTPS URL")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", fmt.Errorf("server URL must not contain a path, query, or fragment")
	}
	if port := parsed.Port(); port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return "", fmt.Errorf("server URL has an invalid port")
		}
	}
	hostname := strings.ToLower(parsed.Hostname())
	if strings.Contains(hostname, "%") || net.ParseIP(hostname) == nil && !validHostname(hostname) {
		return "", fmt.Errorf("server URL has an invalid hostname")
	}
	parsed.Scheme = "https"
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = ""
	return parsed.String(), nil
}

func validateExisting(options normalizedOptions, existing model.Cluster) error {
	if existing.Name != options.name || existing.Endpoint != options.server {
		return fmt.Errorf("existing cluster %q uses name %q and server %q; refusing to replace it", existing.ID, existing.Name, existing.Endpoint)
	}
	local, err := LoadContext(options.contextPath)
	if err != nil {
		return fmt.Errorf("validate existing administrator context: %w", err)
	}
	if local.Version != ContextVersion || local.Cluster.ID != existing.ID || local.Cluster.Name != existing.Name || local.Server != existing.Endpoint {
		return fmt.Errorf("administrator context does not match existing cluster %q", existing.ID)
	}
	expectedDirectory := filepath.Join(filepath.Dir(options.contextPath), administratorRootDirectory, existing.ID)
	expected := ContextCredentials{
		CertificateAuthority: relativeContextPath(filepath.Dir(options.contextPath), filepath.Join(expectedDirectory, "ca.pem")),
		ClientCertificate:    relativeContextPath(filepath.Dir(options.contextPath), filepath.Join(expectedDirectory, "admin.pem")),
		ClientKey:            relativeContextPath(filepath.Dir(options.contextPath), filepath.Join(expectedDirectory, "admin-key.pem")),
	}
	if local.Credentials != expected {
		return fmt.Errorf("administrator context has unexpected credential paths")
	}
	if err := requirePrivateDirectory(filepath.Join(filepath.Dir(options.contextPath), administratorRootDirectory)); err != nil {
		return fmt.Errorf("validate administrator identity root: %w", err)
	}
	if err := requirePrivateDirectory(expectedDirectory); err != nil {
		return fmt.Errorf("validate administrator identity directory: %w", err)
	}
	caPath, _ := resolveContextReference(options.contextPath, local.Credentials.CertificateAuthority)
	certificatePath, _ := resolveContextReference(options.contextPath, local.Credentials.ClientCertificate)
	keyPath, _ := resolveContextReference(options.contextPath, local.Credentials.ClientKey)
	caPEM, err := readPrivateFile(caPath)
	if err != nil {
		return fmt.Errorf("read administrator CA certificate: %w", err)
	}
	certificatePEM, err := readPrivateFile(certificatePath)
	if err != nil {
		return fmt.Errorf("read administrator certificate: %w", err)
	}
	keyPEM, err := readPrivateFile(keyPath)
	if err != nil {
		return fmt.Errorf("read administrator private key: %w", err)
	}
	now := options.now().UTC()
	if err := security.VerifyIdentity(caPEM, security.KeyPair{CertificatePEM: certificatePEM, PrivateKeyPEM: keyPEM}, existing.ID, security.RoleAdministrator, existing.Endpoint, now); err != nil {
		return fmt.Errorf("validate administrator identity: %w", err)
	}

	controllerDirectory := filepath.Join(options.dataDir, controllerIdentityDirectory)
	if err := requirePrivateDirectory(controllerDirectory); err != nil {
		return fmt.Errorf("validate controller identity directory: %w", err)
	}
	stateCA, err := readPrivateFile(filepath.Join(controllerDirectory, "ca.pem"))
	if err != nil {
		return fmt.Errorf("read controller CA certificate: %w", err)
	}
	if !bytes.Equal(stateCA, caPEM) {
		return fmt.Errorf("controller and administrator CA certificates differ")
	}
	caKey, err := readPrivateFile(filepath.Join(controllerDirectory, "ca-key.pem"))
	if err != nil {
		return fmt.Errorf("read controller CA private key: %w", err)
	}
	controllerCertificate, err := readPrivateFile(filepath.Join(controllerDirectory, "controller.pem"))
	if err != nil {
		return fmt.Errorf("read controller certificate: %w", err)
	}
	controllerKey, err := readPrivateFile(filepath.Join(controllerDirectory, "controller-key.pem"))
	if err != nil {
		return fmt.Errorf("read controller private key: %w", err)
	}
	if err := security.VerifyCertificateAuthority(stateCA, caKey, existing.Name, now); err != nil {
		return fmt.Errorf("validate cluster CA: %w", err)
	}
	if err := security.VerifyIdentity(stateCA, security.KeyPair{CertificatePEM: controllerCertificate, PrivateKeyPEM: controllerKey}, existing.ID, security.RoleController, existing.Endpoint, now); err != nil {
		return fmt.Errorf("validate controller identity: %w", err)
	}
	return nil
}

func ensureDataDirectory(path string) (bool, error) {
	created, err := ensurePrivateDirectory(path)
	if err != nil {
		return false, err
	}
	return created, nil
}

func ensurePrivateDirectory(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return false, err
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return false, err
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%s is not a directory", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return false, fmt.Errorf("%s permissions %04o allow group or world access", path, info.Mode().Perm())
	}
	return false, nil
}

func requireDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	return nil
}

func requireAbsent(path, description string) error {
	_, err := os.Lstat(path)
	if err == nil {
		return fmt.Errorf("%s already exists at %s; refusing to overwrite it", description, path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", description, err)
	}
	return nil
}

func stageDirectory(parent, pattern string, files map[string][]byte) (string, error) {
	directory, err := os.MkdirTemp(parent, pattern)
	if err != nil {
		return "", err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		_ = os.RemoveAll(directory)
		return "", err
	}
	for name, contents := range files {
		if err := writePrivateFile(filepath.Join(directory, name), contents); err != nil {
			_ = os.RemoveAll(directory)
			return "", err
		}
	}
	if err := syncDirectory(directory); err != nil {
		_ = os.RemoveAll(directory)
		return "", err
	}
	return directory, nil
}

func writePrivateFile(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func stageFile(parent, pattern string, contents []byte) (string, error) {
	file, err := os.CreateTemp(parent, pattern)
	if err != nil {
		return "", err
	}
	path := file.Name()
	failed := true
	defer func() {
		_ = file.Close()
		if failed {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := file.Write(contents); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	failed = false
	return path, nil
}

func publishDirectory(staging, destination string, files map[string][]byte) error {
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			removePublishedDirectory(destination, files)
		}
	}()
	for name := range files {
		if err := os.Link(filepath.Join(staging, name), filepath.Join(destination, name)); err != nil {
			return err
		}
	}
	if err := syncDirectory(destination); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return err
	}
	published = true
	return nil
}

func publishFile(staging, destination string) error {
	if err := os.Link(staging, destination); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		_ = os.Remove(destination)
		return err
	}
	return nil
}

func removePublishedDirectory(directory string, files map[string][]byte) {
	for name := range files {
		_ = os.Remove(filepath.Join(directory, name))
	}
	_ = os.Remove(directory)
}

func syncDirectory(directory string) error {
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func encodeContext(local LocalContext) ([]byte, error) {
	contents, err := json.MarshalIndent(local, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode administrator context: %w", err)
	}
	return append(contents, '\n'), nil
}

func relativeContextPath(contextDirectory, target string) string {
	relative, err := filepath.Rel(contextDirectory, target)
	if err != nil {
		panic(err)
	}
	return filepath.ToSlash(relative)
}

func newClusterID(random io.Reader) (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(random, value); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}

func pathsOverlap(first, second string) bool {
	return pathWithin(first, second) || pathWithin(second, first)
}

func pathWithin(path, parent string) bool {
	relative, err := filepath.Rel(parent, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func validHostname(hostname string) bool {
	if hostname == "" || len(hostname) > 253 || strings.HasSuffix(hostname, ".") {
		return false
	}
	for _, label := range strings.Split(hostname, ".") {
		if !hostnameLabel.MatchString(label) {
			return false
		}
	}
	return true
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}
