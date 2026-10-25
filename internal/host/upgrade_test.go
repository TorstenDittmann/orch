package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TorstenDittmann/orch/internal/buildinfo"
)

func TestUpgradeJournalPinsCandidateAndPreservesPreviousBinary(t *testing.T) {
	root := t.TempDir()
	installed := filepath.Join(root, "bin", "orch")
	if err := os.MkdirAll(filepath.Dir(installed), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, []byte("old-binary"), 0755); err != nil {
		t.Fatal(err)
	}
	digest, err := hashFile(installed)
	if err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(root, "state", "upgrade.json")
	journal, err := loadUpgradeJournal(journalPath, "node", "candidate-a", digest, installed)
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(root, "state", "upgrades", "candidate-a", "previous-orch")
	if got, err := os.ReadFile(backup); err != nil || string(got) != "old-binary" {
		t.Fatalf("backup = %q, %v", got, err)
	}
	if journal.Phase != "planned" || journal.PreviousHash != digest {
		t.Fatalf("journal = %#v", journal)
	}
	if _, err := loadUpgradeJournal(journalPath, "node", "candidate-b", digest, installed); err == nil {
		t.Fatal("expected a different candidate to be rejected")
	}
}

func TestCompatibleTransitionRejectsDowngradeAndBundleChange(t *testing.T) {
	installed := installedDescriptor(buildinfo.Descriptor{})
	candidate := installed
	if err := compatibleTransition(installed, candidate, true); err != nil {
		t.Fatal(err)
	}
	candidate.ReleaseGeneration = 0
	if err := compatibleTransition(installed, candidate, true); err == nil {
		t.Fatal("expected downgrade rejection")
	}
	candidate = installed
	candidate.DependencyGeneration = "other"
	if err := compatibleTransition(installed, candidate, false); err == nil {
		t.Fatal("expected dependency generation rejection")
	}
}

func TestCopyTreePrivateRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	if err := copyTreePrivate(source, filepath.Join(root, "copy")); err == nil {
		t.Fatal("expected symlink rejection")
	}
}

func TestApplyCandidateResumesAfterReplacement(t *testing.T) {
	root := t.TempDir()
	installed := filepath.Join(root, "installed")
	candidate := filepath.Join(root, "candidate")
	if err := os.WriteFile(installed, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, []byte("new"), 0755); err != nil {
		t.Fatal(err)
	}
	oldHash, _ := hashFile(installed)
	newHash, _ := hashFile(candidate)
	if err := applyCandidate(installed, candidate, oldHash, newHash); err != nil {
		t.Fatal(err)
	}
	if err := applyCandidate(installed, candidate, oldHash, newHash); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got, _ := os.ReadFile(installed); string(got) != "new" {
		t.Fatalf("installed=%q", got)
	}
}

func TestUpgradeJournalAllowsSequentialUpgrades(t *testing.T) {
	root := t.TempDir()
	installed := filepath.Join(root, "bin", "orch")
	if err := os.MkdirAll(filepath.Dir(installed), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, []byte("binary-1"), 0755); err != nil {
		t.Fatal(err)
	}
	first, _ := hashFile(installed)
	journalPath := filepath.Join(root, "state", "upgrade.json")
	journal, err := loadUpgradeJournal(journalPath, "node", "candidate-2", first, installed)
	if err != nil {
		t.Fatal(err)
	}
	// The first upgrade completes: the manifest now records candidate-2.
	journal.Phase = "replaced"
	if err := saveUpgradeJournal(journalPath, journal); err != nil {
		t.Fatal(err)
	}
	if err := finishUpgradeJournal(journalPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(journalPath); !os.IsNotExist(err) {
		t.Fatalf("journal still present after success: %v", err)
	}
	if err := os.WriteFile(installed, []byte("binary-2"), 0755); err != nil {
		t.Fatal(err)
	}
	second, _ := hashFile(installed)
	journal, err = loadUpgradeJournal(journalPath, "node", "candidate-3", second, installed)
	if err != nil {
		t.Fatalf("second upgrade: %v", err)
	}
	if journal.Phase != "planned" || journal.CandidateHash != "candidate-3" || journal.PreviousHash != second {
		t.Fatalf("journal = %#v", journal)
	}
}

func TestUpgradeJournalSupersedesFinishedJournals(t *testing.T) {
	root := t.TempDir()
	installed := filepath.Join(root, "orch")
	if err := os.WriteFile(installed, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	digest, _ := hashFile(installed)
	journalPath := filepath.Join(root, "upgrade.json")
	for name, stale := range map[string]upgradeJournal{
		"ready":            {Version: 1, Kind: "controller", CandidateHash: "older", PreviousHash: "oldest", Phase: "ready"},
		"manifest-records": {Version: 1, Kind: "controller", CandidateHash: digest, PreviousHash: "older", Phase: "replaced"},
	} {
		if err := saveUpgradeJournal(journalPath, stale); err != nil {
			t.Fatal(err)
		}
		journal, err := loadUpgradeJournal(journalPath, "controller", "next", digest, installed)
		if err != nil || journal.Phase != "planned" || journal.CandidateHash != "next" {
			t.Fatalf("%s: journal = %#v, %v", name, journal, err)
		}
	}
	incomplete := upgradeJournal{Version: 1, Kind: "controller", CandidateHash: "other", PreviousHash: digest, Phase: "replaced"}
	if err := saveUpgradeJournal(journalPath, incomplete); err != nil {
		t.Fatal(err)
	}
	if _, err := loadUpgradeJournal(journalPath, "controller", "next", digest, installed); err == nil {
		t.Fatal("expected an incomplete upgrade to block a different candidate")
	}
}

func TestVerifyInstalledBinaryAcceptsPinnedCandidateAfterReplacement(t *testing.T) {
	root := t.TempDir()
	installed := filepath.Join(root, "orch")
	if err := os.WriteFile(installed, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	oldHash, _ := hashFile(installed)
	if err := os.WriteFile(installed, []byte("new"), 0755); err != nil {
		t.Fatal(err)
	}
	newHash, _ := hashFile(installed)
	journalPath := filepath.Join(root, "upgrade.json")
	if err := verifyInstalledBinary(installed, oldHash, journalPath, "node", newHash); err == nil {
		t.Fatal("expected a replaced binary without a journal to be rejected")
	}
	journal := upgradeJournal{Version: 1, Kind: "node", CandidateHash: newHash, PreviousHash: oldHash, PreviousPath: installed, Phase: "replaced"}
	if err := saveUpgradeJournal(journalPath, journal); err != nil {
		t.Fatal(err)
	}
	if err := verifyInstalledBinary(installed, oldHash, journalPath, "node", newHash); err != nil {
		t.Fatalf("resume after replacement: %v", err)
	}
	if err := verifyInstalledBinary(installed, oldHash, journalPath, "node", "other-candidate"); err == nil {
		t.Fatal("expected a different candidate to be rejected")
	}
	if err := verifyInstalledBinary(installed, oldHash, journalPath, "controller", newHash); err == nil {
		t.Fatal("expected a journal of another kind to be rejected")
	}
	if err := os.WriteFile(installed, []byte("tampered"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := verifyInstalledBinary(installed, oldHash, journalPath, "node", newHash); err == nil {
		t.Fatal("expected an unknown binary to be rejected")
	}
	// The journal-pinned candidate resumes through the remaining phases.
	if err := os.WriteFile(installed, []byte("new"), 0755); err != nil {
		t.Fatal(err)
	}
	resumed, err := loadUpgradeJournal(journalPath, "node", newHash, oldHash, installed)
	if err != nil || resumed.Phase != "replaced" {
		t.Fatalf("resumed = %#v, %v", resumed, err)
	}
	if err := applyCandidate(installed, installed, resumed.PreviousHash, resumed.CandidateHash); err != nil {
		t.Fatalf("apply on resume: %v", err)
	}
}

type rollbackFixture struct {
	journalPath string
	agentDir    string
	installed   string
	journal     upgradeJournal
}

func newRollbackFixture(t *testing.T) rollbackFixture {
	t.Helper()
	root := t.TempDir()
	f := rollbackFixture{journalPath: filepath.Join(root, "state", "upgrade.json"), agentDir: filepath.Join(root, "agent"), installed: filepath.Join(root, "bin", "orch")}
	backup := filepath.Join(root, "state", "upgrades", "candidate", "agent-state")
	for _, base := range []string{f.agentDir, backup} {
		if err := os.MkdirAll(filepath.Join(base, "pki"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		filepath.Join(f.agentDir, "agent.db"): "candidate-state",
		filepath.Join(backup, "agent.db"):     "previous-state",
		f.installed:                           "candidate-binary",
		filepath.Join(root, "state", "upgrades", "candidate", "previous-orch"): "previous-binary",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	f.journal = upgradeJournal{Version: 1, Kind: "node", CandidateHash: "candidate", PreviousPath: f.installed, RecoveryPath: backup, Phase: "replaced"}
	if err := saveUpgradeJournal(f.journalPath, f.journal); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestRollbackNodeSurvivesCancelledContext(t *testing.T) {
	f := newRollbackFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls []string
	run := func(ctx context.Context, name string, args ...string) (string, error) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "show" {
			return "inactive\n", nil
		}
		return "", nil
	}
	err := rollbackNode(ctx, run, f.journalPath, f.agentDir, f.journal, errors.New("interrupted"))
	if err == nil || !strings.Contains(err.Error(), "previous agent was restored") {
		t.Fatalf("rollback = %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(f.agentDir, "agent.db")); string(got) != "previous-state" {
		t.Fatalf("agent state = %q", got)
	}
	if got, _ := os.ReadFile(f.installed); string(got) != "previous-binary" {
		t.Fatalf("binary = %q", got)
	}
	if len(calls) != 3 || calls[2] != "start orch-agent.service" {
		t.Fatalf("calls = %q", calls)
	}
}

func TestRollbackAbortsWhenServiceIsNotConfirmedStopped(t *testing.T) {
	f := newRollbackFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var started bool
	run := func(_ context.Context, name string, args ...string) (string, error) {
		switch args[0] {
		case "stop":
			return "", errors.New("stop failed")
		case "show":
			return "active\n", nil
		case "start":
			started = true
		}
		return "", nil
	}
	if err := rollbackNode(ctx, run, f.journalPath, f.agentDir, f.journal, errors.New("interrupted")); err == nil || !strings.Contains(err.Error(), "rollback was not attempted") {
		t.Fatalf("node rollback = %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(f.agentDir, "agent.db")); string(got) != "candidate-state" {
		t.Fatalf("agent state swapped under running candidate: %q", got)
	}
	if got, _ := os.ReadFile(f.installed); string(got) != "candidate-binary" {
		t.Fatalf("binary = %q", got)
	}
	if journal, err := loadUpgradeJournal(f.journalPath, "node", "candidate", "", f.installed); err != nil || journal.Phase != "replaced" {
		t.Fatalf("journal = %#v, %v", journal, err)
	}
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "control.db-wal"), []byte("candidate-wal"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := rollbackController(ctx, run, f.journalPath, dataDir, f.journal, errors.New("interrupted")); err == nil || !strings.Contains(err.Error(), "rollback was not attempted") {
		t.Fatalf("controller rollback = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "control.db-wal")); err != nil {
		t.Fatalf("controller WAL removed under running candidate: %v", err)
	}
	if started {
		t.Fatal("rollback restarted a service it could not stop")
	}
}

func inactiveUnits(calls *[]string) Command {
	return func(_ context.Context, name string, args ...string) (string, error) {
		*calls = append(*calls, strings.Join(args, " "))
		if args[0] == "show" {
			return "inactive\n", nil
		}
		return "", nil
	}
}

func TestRollbackRetryTakesFreshBackup(t *testing.T) {
	f := newRollbackFixture(t)
	var calls []string
	if err := rollbackNode(context.Background(), inactiveUnits(&calls), f.journalPath, f.agentDir, f.journal, errors.New("unhealthy")); err == nil || !strings.Contains(err.Error(), "previous agent was restored") {
		t.Fatalf("rollback = %v", err)
	}
	previousHash, err := hashFile(f.installed)
	if err != nil {
		t.Fatal(err)
	}
	// The retry resumes the same journal from planned, so it backs up the
	// state the restored agent has written since instead of the stale copy.
	journal, err := loadUpgradeJournal(f.journalPath, "node", "candidate", previousHash, f.installed)
	if err != nil || journal.Phase != "planned" {
		t.Fatalf("journal after rollback = %#v, %v", journal, err)
	}
	if err := verifyInstalledBinary(f.installed, previousHash, f.journalPath, "node", "candidate"); err != nil {
		t.Fatalf("restored binary rejected on retry: %v", err)
	}

	controller := newRollbackFixture(t)
	if err := os.WriteFile(filepath.Join(controller.journal.RecoveryPath, "control.db"), []byte("previous-db"), 0600); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := rollbackController(context.Background(), inactiveUnits(&calls), controller.journalPath, dataDir, controller.journal, errors.New("unhealthy")); err == nil || !strings.Contains(err.Error(), "previous controller was restored") {
		t.Fatalf("controller rollback = %v", err)
	}
	if journal, err := loadUpgradeJournal(controller.journalPath, "node", "candidate", "", controller.installed); err != nil || journal.Phase != "planned" {
		t.Fatalf("controller journal after rollback = %#v, %v", journal, err)
	}
}

func TestRollbackKeepsBackupPinnedWhenRestoreFails(t *testing.T) {
	f := newRollbackFixture(t)
	if err := os.Remove(filepath.Join(filepath.Dir(f.journalPath), "upgrades", "candidate", "previous-orch")); err != nil {
		t.Fatal(err)
	}
	var calls []string
	if err := rollbackNode(context.Background(), inactiveUnits(&calls), f.journalPath, f.agentDir, f.journal, errors.New("unhealthy")); err == nil || !strings.Contains(err.Error(), "also failed") {
		t.Fatalf("rollback = %v", err)
	}
	if journal, err := loadUpgradeJournal(f.journalPath, "node", "candidate", "", f.installed); err != nil || journal.Phase != "backed_up" {
		t.Fatalf("journal = %#v, %v", journal, err)
	}
}

func TestPrepareControllerRecoveryStopsFirstAndReplacesStaleSet(t *testing.T) {
	recovery := filepath.Join(t.TempDir(), "upgrades", "candidate", "recovery")
	if err := os.MkdirAll(recovery, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recovery, "control.db"), []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls []string
	err := prepareControllerRecovery(context.Background(), inactiveUnits(&calls), recovery, func(context.Context) error {
		if len(calls) == 0 || calls[0] != "stop orch-controller.service" {
			t.Fatalf("backup taken before the controller stopped: %q", calls)
		}
		if _, err := os.Stat(recovery); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale recovery set still present: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "start") {
			t.Fatalf("controller restarted after successful preparation: %q", calls)
		}
	}

	calls = nil
	err = prepareControllerRecovery(context.Background(), inactiveUnits(&calls), recovery, func(context.Context) error {
		return errors.New("backup failed")
	})
	if err == nil || err.Error() != "backup failed" || calls[len(calls)-1] != "start orch-controller.service" {
		t.Fatalf("failed preparation = %v, calls %q", err, calls)
	}
}
