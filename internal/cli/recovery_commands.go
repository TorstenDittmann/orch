package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/objectstore"
)

func runClusterRecovery(args []string, restore bool, stdout, stderr io.Writer) int {
	name := "cluster backup inspect"
	if restore {
		name = "cluster restore"
	}
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var options clusterctl.RecoveryOptions
	var jsonOutput bool
	var from, endpoint, region string
	flags.StringVar(&options.Backup, "backup", "", "standalone database backup, or with --from the bucket backup's name")
	flags.StringVar(&from, "from", "", "restore the newest backup from s3://BUCKET[/PREFIX]")
	flags.StringVar(&endpoint, "endpoint", "", "S3 endpoint URL for --from; leave out for AWS")
	flags.StringVar(&region, "region", "", "signing region for --from; detected when left out")
	flags.StringVar(&options.PKIDir, "pki-dir", "", "saved controller PKI directory")
	flags.StringVar(&options.SecretKey, "secret-key", "", "saved secret master key")
	if restore {
		flags.StringVar(&options.DataDir, "data-dir", "", "new controller directory (must not exist)")
	}
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitInvalid
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, name+" accepts flags only")
		return ExitInvalid
	}
	if from != "" {
		directory, cleanup, err := downloadControllerBackup(context.Background(), from, endpoint, region, options.Backup, stderr)
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
		defer cleanup()
		options.Backup = directory
	}
	if strings.TrimSpace(options.Backup) == "" || restore && strings.TrimSpace(options.DataDir) == "" {
		required := "--backup or --from"
		if restore {
			required += " and --data-dir"
		}
		writeError(stderr, fmt.Errorf("%s requires %s", name, required), jsonOutput)
		return ExitInvalid
	}
	var result clusterctl.RecoveryResult
	var err error
	if restore {
		result, err = clusterctl.Restore(context.Background(), options)
	} else {
		result, err = clusterctl.InspectBackup(context.Background(), options)
	}
	if err != nil {
		writeError(stderr, err, jsonOutput)
		if errors.Is(err, clusterctl.ErrInvalidOptions) {
			return ExitInvalid
		}
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, result)
	}
	if restore {
		fmt.Fprintf(stdout, "Restored cluster %s (%s)\nData directory: %s\n", result.Cluster.Name, result.Cluster.ID, result.DataDir)
	} else {
		fmt.Fprintf(stdout, "Valid backup for cluster %s (%s)\nSchema: %d (this binary: %d)\n", result.Cluster.Name, result.Cluster.ID, result.SchemaVersion, result.TargetSchemaVersion)
	}
	fmt.Fprintf(stdout, "Controller endpoint: %s\nNodes: %d · Applications: %d · Deployments: %d · Jobs: %d (%d unfinished)\n", result.Cluster.Endpoint, result.Nodes, result.Applications, result.Deployments, result.Jobs, result.UnfinishedJobs)
	if !result.Ready {
		fmt.Fprintf(stdout, "Recovery material needed: %s\n", strings.Join(result.Missing, ", "))
	} else if !restore {
		fmt.Fprintln(stdout, "Recovery material verified.")
	}
	if result.ControllerCertificateExpired {
		renewDataDir := "DIR"
		if restore {
			renewDataDir = result.DataDir
		}
		fmt.Fprintf(stdout, "Controller certificate has expired; run 'orch cluster certificates renew --data-dir %s --admin-dir NEW_DIR' before starting the controller.\n", renewDataDir)
	}
	if restore {
		fmt.Fprintln(stdout, "Keep the old controller stopped. Restore endpoint routing before starting the recovered controller; keep existing agent caches.")
	}
	return ExitOK
}

// downloadControllerBackup fetches a controller backup set from the bucket
// into a private temporary directory: the newest one, or the one named.
func downloadControllerBackup(ctx context.Context, from, endpoint, region, name string, stderr io.Writer) (string, func(), error) {
	bucket, prefix, err := objectstore.ParseURL(from)
	if err != nil {
		return "", nil, err
	}
	if endpoint == "" {
		endpoint = firstEnv("AWS_ENDPOINT_URL_S3", "AWS_ENDPOINT_URL")
	}
	if endpoint, err = objectstore.NormalizeEndpoint(endpoint); err != nil {
		return "", nil, err
	}
	if region == "" {
		region = objectstore.RegionFromEndpoint(endpoint)
	}
	accessKeyID, secretAccessKey, err := backupCredentials(ctx, stderr)
	if err != nil {
		return "", nil, err
	}
	client := objectstore.New(objectstore.Destination{Bucket: bucket, Prefix: prefix, Endpoint: endpoint, Region: region,
		AccessKeyID: accessKeyID, SecretAccessKey: secretAccessKey}, nil)
	if region == "" {
		if _, err := client.DetectRegion(ctx); err != nil {
			return "", nil, fmt.Errorf("reach %s: %w", from, err)
		}
	}
	listPrefix := client.Destination().Key(objectstore.ControllerBackups)
	objects, err := client.List(ctx, listPrefix)
	if err != nil {
		return "", nil, fmt.Errorf("list controller backups in %s: %w", from, err)
	}
	key := ""
	newest := ""
	for candidate, object := range objectstore.Archives(objects, listPrefix) {
		// Names are UTC timestamps, so the greatest is the newest.
		if (name == "" && candidate > newest) || candidate == name {
			newest, key = candidate, object.Key
		}
	}
	if key == "" {
		if name != "" {
			return "", nil, fmt.Errorf("%s has no controller backup %s", from, name)
		}
		return "", nil, fmt.Errorf("%s has no controller backups", from)
	}
	stage, err := os.MkdirTemp("", ".orch-bucket-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(stage) }
	body, _, err := client.Get(ctx, key)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("download %s: %w", key, err)
	}
	defer body.Close()
	directory := filepath.Join(stage, newest)
	if err := clusterctl.ExtractBackupSet(body, directory); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("unpack %s: %w", key, err)
	}
	fmt.Fprintf(stderr, "Downloaded controller backup %s from %s\n", filepath.Base(directory), from)
	return directory, cleanup, nil
}
