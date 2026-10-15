package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
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
	flags.StringVar(&options.Backup, "backup", "", "standalone database backup")
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
	if strings.TrimSpace(options.Backup) == "" || restore && strings.TrimSpace(options.DataDir) == "" {
		required := "--backup"
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
