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
