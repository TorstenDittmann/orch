package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	agentctl "change.codes/torsten/better-swarm/internal/agent"
	controlclient "change.codes/torsten/better-swarm/internal/client"
	clusterctl "change.codes/torsten/better-swarm/internal/cluster"
	"change.codes/torsten/better-swarm/internal/config"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func runNodeJoin(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: orch node join create|list|revoke")
		return ExitInvalid
	}
	switch args[0] {
	case "create":
		return runNodeJoinCreate(ctx, args[1:], stdout, stderr)
	case "list":
		return runNodeJoinList(ctx, args[1:], stdout, stderr)
	case "revoke":
		return runNodeJoinRevoke(ctx, args[1:], stdout, stderr)
	default:
		writeError(stderr, fmt.Errorf("unknown node join command %q", args[0]), false)
		return ExitInvalid
	}
}

func runNodeJoinCreate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("node join create", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath, output, idempotencyKey string
	var ttl time.Duration
	var jsonOutput bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.StringVar(&output, "output", "", "new private join artifact path")
	flags.StringVar(&idempotencyKey, "idempotency-key", "", "stable key for safe retries")
	flags.DurationVar(&ttl, "ttl", 15*time.Minute, "join lifetime from 1m through 24h")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable metadata")
	reordered := reorderInterspersed(args, map[string]bool{"-context": true, "--context": true, "-output": true, "--output": true, "-ttl": true, "--ttl": true, "-idempotency-key": true, "--idempotency-key": true})
	if err := flags.Parse(reordered); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 1 || contextPath == "" || output == "" || output == "-" || ttl < time.Minute || ttl > 24*time.Hour {
		writeError(stderr, errors.New("node join create requires NODE, --context, --output, and --ttl from 1m through 24h"), jsonOutput)
		return ExitInvalid
	}
	nodeName := flags.Arg(0)
	if err := config.ValidateApplicationName(nodeName); err != nil {
		writeError(stderr, fmt.Errorf("invalid node name: %w", err), jsonOutput)
		return ExitInvalid
	}
	if _, err := os.Lstat(output); err == nil || !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			err = errors.New("path already exists")
		}
		writeError(stderr, fmt.Errorf("refuse join artifact output %s: %w", output, err), jsonOutput)
		return ExitInvalid
	}
	credentials, err := clusterctl.LoadAdministratorCredentials(contextPath, time.Now())
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	pendingPath := output + ".pending"
	stage, err := loadJoinCreationStage(pendingPath)
	if errors.Is(err, os.ErrNotExist) {
		secret := make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, secret); err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
		joinIDBytes := make([]byte, 16)
		if _, err := io.ReadFull(rand.Reader, joinIDBytes); err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
		if idempotencyKey == "" {
			idempotencyKey, err = newIdempotencyKey(rand.Reader)
			if err != nil {
				writeError(stderr, err, jsonOutput)
				return ExitRuntime
			}
		}
		stage = joinCreationStage{Version: 1, TTL: ttl, IdempotencyKey: idempotencyKey, Artifact: agentctl.JoinArtifact{
			Version: agentctl.JoinArtifactVersion, JoinID: "join_" + hex.EncodeToString(joinIDBytes), Secret: secret,
			NodeName: nodeName, ClusterID: credentials.Context.Cluster.ID, ControllerEndpoint: credentials.Context.Server,
			CACertificatePEM: credentials.CACertificatePEM, ExpiresAt: time.Now().UTC().Add(ttl),
		}}
		encodedStage, encodeErr := json.MarshalIndent(stage, "", "  ")
		if encodeErr != nil {
			writeError(stderr, encodeErr, jsonOutput)
			return ExitRuntime
		}
		if err := publishJoinArtifact(pendingPath, append(encodedStage, '\n')); err != nil {
			writeError(stderr, fmt.Errorf("stage join artifact: %w", err), jsonOutput)
			return ExitRuntime
		}
	} else if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if stage.Version != 1 || stage.Artifact.NodeName != nodeName || stage.Artifact.ClusterID != credentials.Context.Cluster.ID ||
		stage.Artifact.ControllerEndpoint != credentials.Context.Server || stage.TTL != ttl ||
		(idempotencyKey != "" && idempotencyKey != stage.IdempotencyKey) {
		writeError(stderr, errors.New("pending join artifact belongs to different arguments or administrator context"), jsonOutput)
		return ExitInvalid
	}
	idempotencyKey = stage.IdempotencyKey
	if !cliIdempotencyKeyPattern.MatchString(idempotencyKey) {
		writeError(stderr, errors.New("invalid staged idempotency key"), jsonOutput)
		return ExitInvalid
	}
	secretHash := sha256.Sum256(stage.Artifact.Secret)
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	response, err := remote.CreateJoinToken(ctx, &orchv1alpha1.CreateJoinTokenRequest{Id: stage.Artifact.JoinID, NodeName: nodeName,
		SecretHash: secretHash[:], ExpiresAt: timestamppb.New(stage.Artifact.ExpiresAt), IdempotencyKey: idempotencyKey})
	if err != nil {
		writeError(stderr, fmt.Errorf("create join credential failed; retry requires the staged artifact: %w", err), jsonOutput)
		return ExitRuntime
	}
	encoded, err := agentctl.EncodeJoinArtifact(stage.Artifact)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if err := publishJoinArtifact(output, encoded); err != nil {
		writeError(stderr, fmt.Errorf("publish join artifact; retry with the same output path or revoke %s: %w", stage.Artifact.JoinID, err), jsonOutput)
		return ExitRuntime
	}
	if err := os.Remove(pendingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(stderr, fmt.Errorf("remove completed join stage: %w", err), jsonOutput)
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, struct {
			ID        string    `json:"id"`
			NodeName  string    `json:"node_name"`
			State     string    `json:"state"`
			Output    string    `json:"output"`
			ExpiresAt time.Time `json:"expires_at"`
		}{
			ID: response.GetToken().GetId(), NodeName: response.GetToken().GetNodeName(), State: response.GetToken().GetState(), Output: output, ExpiresAt: stage.Artifact.ExpiresAt})
	}
	fmt.Fprintf(stdout, "Created join credential %s for %s, expires %s\nArtifact: %s\n", stage.Artifact.JoinID, nodeName, stage.Artifact.ExpiresAt.Format(time.RFC3339Nano), output)
	return ExitOK
}

type joinCreationStage struct {
	Version        int                   `json:"version"`
	TTL            time.Duration         `json:"ttl"`
	Artifact       agentctl.JoinArtifact `json:"artifact"`
	IdempotencyKey string                `json:"idempotency_key"`
}

func loadJoinCreationStage(path string) (joinCreationStage, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return joinCreationStage{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return joinCreationStage{}, errors.New("pending join artifact is not a private regular file")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return joinCreationStage{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var stage joinCreationStage
	if err := decoder.Decode(&stage); err != nil {
		return joinCreationStage{}, fmt.Errorf("decode pending join artifact: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return joinCreationStage{}, errors.New("pending join artifact contains trailing data")
	}
	if stage.Version != 1 || stage.TTL < time.Minute || stage.TTL > 24*time.Hour || stage.IdempotencyKey == "" || stage.Artifact.JoinID == "" || len(stage.Artifact.Secret) != 32 {
		return joinCreationStage{}, errors.New("pending join artifact is incomplete")
	}
	return stage, nil
}

func runNodeJoinList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("node join list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath string
	var jsonOutput bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 || contextPath == "" {
		writeError(stderr, errors.New("node join list requires --context"), jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	tokens, err := remote.ListJoinTokens(ctx)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, tokens)
	}
	if len(tokens) == 0 {
		fmt.Fprintln(stdout, "No join credentials.")
		return ExitOK
	}
	for _, token := range tokens {
		fmt.Fprintf(stdout, "%s  %s  %s  expires %s\n", token.GetId(), token.GetNodeName(), token.GetState(), token.GetExpiresAt().AsTime().Format(time.RFC3339Nano))
	}
	return ExitOK
}

func runNodeJoinRevoke(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("node join revoke", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath, idempotencyKey string
	var jsonOutput bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.StringVar(&idempotencyKey, "idempotency-key", "", "stable key for safe retries")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	reordered := reorderInterspersed(args, map[string]bool{"-context": true, "--context": true, "-idempotency-key": true, "--idempotency-key": true})
	if err := flags.Parse(reordered); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 1 || contextPath == "" {
		writeError(stderr, errors.New("node join revoke requires JOIN_ID and --context"), jsonOutput)
		return ExitInvalid
	}
	var err error
	if idempotencyKey == "" {
		idempotencyKey, err = newIdempotencyKey(rand.Reader)
	}
	if err != nil || !cliIdempotencyKeyPattern.MatchString(idempotencyKey) {
		writeError(stderr, errors.New("invalid --idempotency-key"), jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	response, err := remote.RevokeJoinToken(ctx, flags.Arg(0), idempotencyKey)
	if err != nil {
		writeError(stderr, fmt.Errorf("revoke join credential failed; retry with --idempotency-key %s: %w", idempotencyKey, err), jsonOutput)
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, response)
	}
	fmt.Fprintf(stdout, "Join credential %s state=%s changed=%t\n", response.GetToken().GetId(), response.GetToken().GetState(), response.GetChanged())
	return ExitOK
}

func publishJoinArtifact(path string, contents []byte) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	if err := directory.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}
