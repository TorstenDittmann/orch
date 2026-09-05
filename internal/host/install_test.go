package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicationCanResumeJournaledMissingFileAndRefusesChangedContent(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "source")
	dest := filepath.Join(dir, "bin", "runtime")
	if err := os.WriteFile(source, []byte("original"), 0700); err != nil {
		t.Fatal(err)
	}
	i := installer{journalPath: filepath.Join(dir, "manifest.json"), state: manifest{Files: map[string]string{dest: digest([]byte("original"))}}}
	if err := i.publish(dest, source, 0755); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(dest)
	if err := i.publish(dest, source, 0755); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(dest)
	if !os.SameFile(before, after) {
		t.Fatal("identical retry replaced executable")
	}
	info, _ := os.Stat(source)
	if info.Mode().Perm() != 0700 {
		t.Fatal("installer changed its source executable permissions")
	}
	os.WriteFile(dest, []byte("tampered"), 0755)
	if err := i.publish(dest, source, 0755); err == nil {
		t.Fatal("silently replaced tampered owned file")
	}
	contents, _ := os.ReadFile(dest)
	if string(contents) != "tampered" {
		t.Fatal("modified conflicting file")
	}
}
func TestPublicationRefusesUnownedMatchingFile(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "source")
	dest := filepath.Join(dir, "dest")
	os.WriteFile(source, []byte("same"), 0600)
	os.WriteFile(dest, []byte("same"), 0600)
	i := installer{journalPath: filepath.Join(dir, "manifest.json"), state: manifest{Files: map[string]string{}}}
	if err := i.publish(dest, source, 0755); err == nil {
		t.Fatal("adopted foreign file")
	}
	if len(i.state.Files) != 0 {
		t.Fatal("claimed ownership after refusal")
	}
}
func TestPackageManagersInstallOnlyDeclaredDependencies(t *testing.T) {
	for _, distro := range []string{"ubuntu", "fedora"} {
		t.Run(distro, func(t *testing.T) {
			var calls []string
			i := installer{state: manifest{Distro: distro}, plan: Plan{Packages: packages(distro)}, run: func(_ context.Context, name string, args ...string) (string, error) {
				calls = append(calls, name+" "+strings.Join(args, " "))
				return "", nil
			}}
			if err := i.installPackages(context.Background()); err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(calls, "\n")
			if strings.Contains(joined, "upgrade") || strings.Contains(joined, "nogpgcheck") {
				t.Fatal(joined)
			}
			for _, p := range packages(distro) {
				if !strings.Contains(joined, p) {
					t.Fatalf("missing %s", p)
				}
			}
			if distro == "fedora" && !strings.Contains(joined, "gpgcheck=1") {
				t.Fatal("signature checking not enforced")
			}
			i.run = func(context.Context, string, ...string) (string, error) { return "", errors.New("lock unavailable") }
			if err := i.installPackages(context.Background()); err == nil {
				t.Fatal("package failure ignored")
			}
		})
	}
}
