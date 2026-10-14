package host

import (
	"os"
	"path/filepath"
	"testing"

	"change.codes/torsten/better-swarm/internal/buildinfo"
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
