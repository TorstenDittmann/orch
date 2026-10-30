package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	pklschema "github.com/TorstenDittmann/orch/pkl"
	applepkl "github.com/apple/pkl-go/pkl"
)

const (
	MaxManifestBytes         = 1 << 20
	defaultEvaluationTimeout = 10 * time.Second
)

type LoadOptions struct {
	RootDir        string
	PklCommand     string
	Timeout        time.Duration
	caCertificates []byte
}

// Load evaluates one Pkl application module with restricted access and returns
// a resolved, validated manifest.
func Load(ctx context.Context, filename string, options LoadOptions) (*Manifest, error) {
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = defaultEvaluationTimeout
	}
	evaluationContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	entry, root, source, err := loadEntry(filename, options.RootDir)
	if err != nil {
		return nil, err
	}
	pklCommand, err := checkedPklCommand(evaluationContext, options.PklCommand)
	if err != nil {
		return nil, err
	}

	mirror, err := pklschema.StartMirror()
	if err != nil {
		return nil, err
	}
	defer mirror.Close()

	evaluator, err := applepkl.NewEvaluatorWithCommand(
		evaluationContext,
		[]string{pklCommand},
		func(evaluatorOptions *applepkl.EvaluatorOptions) {
			evaluatorOptions.Properties = map[string]string{}
			evaluatorOptions.Env = map[string]string{}
			evaluatorOptions.AllowedModules = []string{`^pkl:.*$`, `^file:.*$`, `^https://.*$`, `^orch:/v1alpha1/Application\.pkl$`, mirror.AllowedModule()}
			evaluatorOptions.AllowedResources = []string{`^$`}
			evaluatorOptions.ModuleReaders = []applepkl.ModuleReader{pklschema.ModuleReader{}}
			evaluatorOptions.CacheDir = ""
			evaluatorOptions.RootDir = root
			evaluatorOptions.Logger = applepkl.NoopLogger
			evaluatorOptions.Http = &applepkl.Http{CaCertificates: options.caCertificates, Rewrites: mirror.Rewrites()}
		},
	)
	if err != nil {
		return nil, evaluationFailure(evaluationContext, filename, "start Pkl evaluator", err)
	}
	if evaluator == nil {
		return nil, evaluationFailure(evaluationContext, filename, "start Pkl evaluator", errors.New("evaluator did not start"))
	}

	moduleSource := applepkl.FileSource(entry)
	moduleSource.Contents = string(source)
	var doc pklDocument
	evaluateErr := evaluator.EvaluateModule(evaluationContext, moduleSource, &doc)
	closeErr := evaluator.Close()
	if evaluateErr != nil {
		return nil, evaluationFailure(evaluationContext, filename, "evaluate Pkl manifest", evaluateErr)
	}
	if evaluationContext.Err() != nil {
		return nil, evaluationFailure(evaluationContext, filename, "evaluate Pkl manifest", evaluationContext.Err())
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close Pkl evaluator for %q: %w", filename, closeErr)
	}

	manifest := resolvePkl(doc)
	var outside []Problem
	for _, name := range slices.Sorted(maps.Keys(manifest.Services)) {
		service := manifest.Services[name]
		if service.Static == nil || service.Static.Dir == "" {
			continue
		}
		if !filepath.IsAbs(service.Static.Dir) {
			service.Static.Dir = filepath.Join(filepath.Dir(entry), filepath.FromSlash(service.Static.Dir))
		}
		// A deploy publishes the folder, so like module imports it must stay
		// inside the evaluation root; otherwise a manifest or a module it
		// imports could publish any folder on the deploying machine.
		if !insideRoot(root, service.Static.Dir) {
			outside = append(outside, Problem{Path: "services." + name + ".static.dir", Message: "must be a folder inside the evaluation root " + root})
		}
	}
	if len(outside) != 0 {
		return nil, &ValidationError{Problems: outside}
	}
	databases := resolvePklDatabases(doc)
	if err := connectDatabases(manifest, resolvePklConnections(doc), databases); err != nil {
		return nil, err
	}
	if err := expandDatabases(manifest, databases); err != nil {
		return nil, err
	}
	if err := Validate(manifest); err != nil {
		return nil, err
	}
	normalize(manifest)
	canonical, err := CanonicalJSON(manifest)
	if err != nil {
		return nil, err
	}
	if len(canonical) > MaxManifestBytes {
		return nil, fmt.Errorf("resolved manifest exceeds %d bytes", MaxManifestBytes)
	}
	return manifest, nil
}

func loadEntry(filename, configuredRoot string) (entry string, root string, contents []byte, err error) {
	if strings.TrimSpace(filename) == "" {
		return "", "", nil, errors.New("manifest path is required")
	}
	entry, err = realPath(filename)
	if err != nil {
		return "", "", nil, fmt.Errorf("resolve manifest %q: %w", filename, err)
	}
	root = configuredRoot
	if strings.TrimSpace(root) == "" {
		root = filepath.Dir(entry)
	} else {
		root, err = realPath(root)
		if err != nil {
			return "", "", nil, fmt.Errorf("resolve evaluation root %q: %w", configuredRoot, err)
		}
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return "", "", nil, fmt.Errorf("stat evaluation root %q: %w", root, err)
	}
	if !rootInfo.IsDir() {
		return "", "", nil, fmt.Errorf("evaluation root %q is not a directory", root)
	}
	relative, err := filepath.Rel(root, entry)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", "", nil, fmt.Errorf("manifest %q is outside evaluation root %q", entry, root)
	}

	file, err := os.Open(entry)
	if err != nil {
		return "", "", nil, fmt.Errorf("read manifest %q: %w", entry, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", "", nil, fmt.Errorf("stat manifest %q: %w", entry, err)
	}
	if !info.Mode().IsRegular() {
		return "", "", nil, fmt.Errorf("manifest %q is not a regular file", entry)
	}
	contents, err = io.ReadAll(io.LimitReader(file, MaxManifestBytes+1))
	if err != nil {
		return "", "", nil, fmt.Errorf("read manifest %q: %w", entry, err)
	}
	if len(contents) > MaxManifestBytes {
		return "", "", nil, fmt.Errorf("manifest exceeds %d bytes", MaxManifestBytes)
	}
	return entry, root, contents, nil
}

// insideRoot reports whether path, after resolving symlinks in the part of
// it that exists, is root or below it.
func insideRoot(root, path string) bool {
	resolved := filepath.Clean(path)
	for missing := ""; ; {
		if real, err := filepath.EvalSymlinks(resolved); err == nil {
			resolved = filepath.Join(real, missing)
			break
		}
		parent := filepath.Dir(resolved)
		if parent == resolved {
			return false
		}
		missing = filepath.Join(filepath.Base(resolved), missing)
		resolved = parent
	}
	relative, err := filepath.Rel(root, resolved)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func realPath(filename string) (string, error) {
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

func checkedPklCommand(ctx context.Context, configured string) (string, error) {
	command := configured
	if command == "" {
		var err error
		command, err = exec.LookPath("pkl")
		if err != nil {
			return "", fmt.Errorf("Pkl %s is required; install the repository toolchain with `mise install`", pklschema.RequiredPklVersion)
		}
	}
	output, err := exec.CommandContext(ctx, command, "--version").CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "", evaluationFailure(ctx, command, "check Pkl version", ctx.Err())
		}
		return "", fmt.Errorf("check Pkl version using %q: %w", command, err)
	}
	versionLine := strings.TrimSpace(string(output))
	wantPrefix := "Pkl " + pklschema.RequiredPklVersion
	if versionLine != wantPrefix && !strings.HasPrefix(versionLine, wantPrefix+" ") {
		return "", fmt.Errorf("Pkl %s is required, found %q; run `mise install`", pklschema.RequiredPklVersion, versionLine)
	}
	return command, nil
}

func evaluationFailure(ctx context.Context, filename, operation string, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%s %q: timed out: %w", operation, filename, context.DeadlineExceeded)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("%s %q: cancelled: %w", operation, filename, context.Canceled)
	}
	return fmt.Errorf("%s %q: %w", operation, filename, err)
}
