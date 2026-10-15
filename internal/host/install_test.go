package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	nodebundle "change.codes/torsten/better-swarm/packaging/node"
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

func TestVethLinkPolicyIsPlannedOnSupportedDistributions(t *testing.T) {
	for _, distro := range []string{"ubuntu", "fedora"} {
		t.Run(distro, func(t *testing.T) {
			plan := buildPlan(Report{OS: distro}, "10.0.0.11", nil)
			if !slices.Contains(plan.Files, VethLinkPath) {
				t.Fatal("veth link policy is not owned by the installation plan")
			}
			if !strings.Contains(strings.Join(plan.Actions, "\n"), "preserving CNI MAC addresses") {
				t.Fatal("veth link policy is missing from the installation actions")
			}
			dependency := "udev"
			if distro == "fedora" {
				dependency = "systemd-udev"
			}
			if !slices.Contains(plan.Packages, dependency) {
				t.Fatalf("missing udev dependency %s", dependency)
			}
		})
	}
}

func TestVethLinkPolicyPublishesBeforeReloadAndCanResume(t *testing.T) {
	for _, distro := range []string{"ubuntu", "fedora"} {
		t.Run(distro, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "network", "10-orch-veth.link")
			want, err := nodebundle.Files.ReadFile("10-orch-veth.link")
			if err != nil {
				t.Fatal(err)
			}
			for _, setting := range []string{"OriginalName=veth*", "Driver=veth", "MACAddressPolicy=none"} {
				if !strings.Contains(string(want), setting) {
					t.Fatalf("veth-only MAC protection missing %q", setting)
				}
			}
			reloadErr := errors.New("udev unavailable")
			i := installer{journalPath: filepath.Join(dir, "manifest.json"), state: manifest{Distro: distro, Files: map[string]string{}}}
			calls := 0
			i.run = func(_ context.Context, name string, args ...string) (string, error) {
				calls++
				if name != "udevadm" || strings.Join(args, " ") != "control --reload" {
					t.Fatalf("must only reload udev, not trigger/reconfigure interfaces: %s %v", name, args)
				}
				got, err := os.ReadFile(path)
				if err != nil || string(got) != string(want) || i.state.Files[path] != digest(want) {
					t.Fatalf("policy must be published and journaled before reload: %q, %v", got, err)
				}
				return "", reloadErr
			}
			if err := i.configureVethLink(context.Background(), path); !errors.Is(err, reloadErr) {
				t.Fatalf("udev reload failure was not propagated: %v", err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			reloadErr = nil
			if err := i.configureVethLink(context.Background(), path); err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 2 || !os.SameFile(before, after) {
				t.Fatal("retry must reload udev without replacing the owned policy")
			}
		})
	}
}

func TestVethLinkPolicyRefusesForeignFileWithoutReload(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "10-orch-veth.link")
	if err := os.WriteFile(path, []byte("foreign policy"), 0644); err != nil {
		t.Fatal(err)
	}
	i := installer{journalPath: filepath.Join(dir, "manifest.json"), state: manifest{Files: map[string]string{}}, run: func(context.Context, string, ...string) (string, error) {
		t.Fatal("udev must not reload after refusing a foreign file")
		return "", nil
	}}
	if err := i.configureVethLink(context.Background(), path); err == nil {
		t.Fatal("foreign policy was adopted")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "foreign policy" || len(i.state.Files) != 0 {
		t.Fatalf("foreign policy was modified or claimed: %q, %v", got, err)
	}
}
