package host

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func fixtureProbe(distro string) Probe {
	files := map[string]string{"/etc/os-release": "ID=" + distro + "\nVERSION_ID=24.04", "/proc/1/comm": "systemd\n", "/sys/fs/cgroup/cgroup.controllers": "cpuset cpu io memory pids\n", "/proc/sys/net/ipv4/ip_forward": "0\n", "/sys/fs/selinux/enforce": "1"}
	switch distro {
	case "fedora":
		files["/etc/os-release"] = "ID=fedora\nVERSION_ID=44\nVARIANT_ID=server"
	case "fedora-42":
		files["/etc/os-release"] = "ID=fedora\nVERSION_ID=42"
	case "ubuntu-26.04":
		files["/etc/os-release"] = "ID=ubuntu\nVERSION_ID=\"26.04\""
	}
	return Probe{GOOS: "linux", Arch: "amd64", UID: 0, Addresses: []string{"10.0.0.11"}, FreeBytes: 4 << 30,
		Read: func(p string) ([]byte, error) {
			if s, ok := files[p]; ok {
				return []byte(s), nil
			}
			return nil, os.ErrNotExist
		},
		Exists: func(string) bool { return false },
		Run: func(_ context.Context, name string, args ...string) (string, error) {
			switch name {
			case "modinfo", "which", "ss", "ip":
				return "", nil
			case "systemctl":
				return "not-found", nil
			}
			return "", errors.New("unexpected probe command: " + name)
		}}
}
func TestFreshSupportedHostsHaveOnlyInstallableFailures(t *testing.T) {
	for _, distro := range []string{"ubuntu", "ubuntu-26.04", "fedora", "fedora-42", "ubuntu/arm64", "fedora/arm64"} {
		t.Run(distro, func(t *testing.T) {
			name, arch, _ := strings.Cut(distro, "/")
			probe := fixtureProbe(name)
			if arch != "" {
				probe.Arch = arch
			}
			r := Inspect(context.Background(), "10.0.0.11", probe)
			if !r.Installable || r.Ready {
				t.Fatalf("unexpected report: %+v", r)
			}
			for _, c := range r.Checks {
				if (c.State == "fail" || c.State == "unknown") && !c.Installable {
					t.Fatalf("unexpected blocker: %+v", c)
				}
			}
		})
	}
}
func TestDoctorRejectsUnsafeHosts(t *testing.T) {
	cases := map[string]func(*Probe){
		"architecture": func(p *Probe) { p.Arch = "riscv64" },
		"ubuntu 22.04": func(p *Probe) {
			read := p.Read
			p.Read = func(s string) ([]byte, error) {
				if s == "/etc/os-release" {
					return []byte("ID=ubuntu\nVERSION_ID=22.04"), nil
				}
				return read(s)
			}
		},
		"nonroot":       func(p *Probe) { p.UID = 1000 },
		"address":       func(p *Probe) { p.Addresses = nil },
		"disk":          func(p *Probe) { p.FreeBytes = 1024 },
		"foreign state": func(p *Probe) { p.Exists = func(s string) bool { return s == AgentDir } },
		"foreign service": func(p *Probe) {
			p.Run = func(context.Context, string, ...string) (string, error) { return "loaded", nil }
		},
		"cgroups": func(p *Probe) {
			read := p.Read
			p.Read = func(s string) ([]byte, error) {
				if strings.Contains(s, "cgroup.controllers") {
					return []byte("cpuset memory pids"), nil
				}
				return read(s)
			}
		},
		"ports": func(p *Probe) {
			run := p.Run
			p.Run = func(c context.Context, n string, a ...string) (string, error) {
				if n == "ss" {
					return "tcp LISTEN 0 4096 0.0.0.0:80 0.0.0.0:*", nil
				}
				return run(c, n, a...)
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			p := fixtureProbe("fedora")
			change(&p)
			if r := Inspect(context.Background(), "10.0.0.11", p); r.Installable {
				t.Fatalf("unsafe host accepted: %+v", r)
			}
		})
	}
}
func TestDoctorNeverExecutesUntrustedInstalledFiles(t *testing.T) {
	p := fixtureProbe("ubuntu")
	p.Exists = func(string) bool { return true }
	p.Trusted = func(string) error { return errors.New("tampered") }
	p.Run = func(_ context.Context, name string, _ ...string) (string, error) {
		if strings.HasPrefix(name, Prefix) {
			t.Fatalf("executed untrusted %s", name)
		}
		return "", nil
	}
	if Inspect(context.Background(), "10.0.0.11", p).Installable {
		t.Fatal("tampered installation accepted")
	}
}

func TestDoctorUsesBoundedReadOnlyCommands(t *testing.T) {
	p := fixtureProbe("fedora")
	run := p.Run
	p.Run = func(ctx context.Context, name string, args ...string) (string, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 10*time.Second {
			t.Fatalf("unbounded probe %s", name)
		}
		switch name {
		case "getent", "modinfo", "ss":
		case "systemctl":
			if args[0] != "show" && args[0] != "is-active" {
				t.Fatalf("mutating systemctl call: %v", args)
			}
		case "ip":
			if strings.Join(args, " ") != "-4 route show" {
				t.Fatalf("mutating ip call: %v", args)
			}
		default:
			t.Fatalf("unexpected probe command %s", name)
		}
		return run(ctx, name, args...)
	}
	Inspect(context.Background(), "10.0.0.11", p)
}

func TestOwnedManifestDoesNotHideForeignListeners(t *testing.T) {
	for _, matches := range []bool{false, true} {
		p := fixtureProbe("ubuntu")
		p.Exists = func(path string) bool { return path == ManifestPath }
		run := p.Run
		p.Run = func(ctx context.Context, name string, args ...string) (string, error) {
			if name == "ss" {
				return "tcp LISTEN 0 4096 0.0.0.0:80 0.0.0.0:* users:((\"caddy\",pid=123,fd=3))", nil
			}
			return run(ctx, name, args...)
		}
		p.OwnListener = func(port, line string) bool { return matches }
		if got := Inspect(context.Background(), "10.0.0.11", p).Installable; got != matches {
			t.Fatalf("matching owned listener=%t installable=%t", matches, got)
		}
	}
}

func TestAnyFedoraEditionExceptOstreeIsSupported(t *testing.T) {
	for _, variant := range []string{"server", "cloud", "workstation", ""} {
		p := fixtureProbe("fedora")
		read := p.Read
		p.Read = func(path string) ([]byte, error) {
			if path == "/etc/os-release" {
				return []byte("ID=fedora\nVERSION_ID=44\nVARIANT_ID=" + variant), nil
			}
			return read(path)
		}
		if !Inspect(context.Background(), "10.0.0.11", p).Installable {
			t.Fatalf("Fedora 44 edition %q rejected", variant)
		}
	}
	p := fixtureProbe("fedora")
	exists := p.Exists
	p.Exists = func(path string) bool { return path == "/run/ostree-booted" || exists(path) }
	if Inspect(context.Background(), "10.0.0.11", p).Installable {
		t.Fatal("ostree-based Fedora accepted")
	}
}

func TestSELinuxModeWarnsInsteadOfBlocking(t *testing.T) {
	for mode, contents := range map[string]*string{"permissive": ptr("0"), "disabled": nil} {
		p := fixtureProbe("fedora")
		read := p.Read
		p.Read = func(path string) ([]byte, error) {
			if path == "/sys/fs/selinux/enforce" {
				if contents == nil {
					return nil, os.ErrNotExist
				}
				return []byte(*contents), nil
			}
			return read(path)
		}
		report := Inspect(context.Background(), "10.0.0.11", p)
		if !report.Installable {
			t.Fatalf("SELinux %s blocked installation: %+v", mode, report.Checks)
		}
		found := false
		for _, check := range report.Checks {
			if check.ID == "selinux" {
				found = check.State == "warn" && strings.Contains(check.Detail, mode)
			}
		}
		if !found {
			t.Fatalf("SELinux %s did not produce a warning: %+v", mode, report.Checks)
		}
	}
}

func ptr(value string) *string { return &value }

func TestPublicAdvertiseAddressWarnsAboutWireGuardFirewall(t *testing.T) {
	private := Inspect(context.Background(), "10.0.0.11", fixtureProbe("ubuntu"))
	for _, check := range private.Checks {
		if check.ID == "public_wireguard_endpoint" {
			t.Fatalf("private address produced %+v", check)
		}
	}
	probe := fixtureProbe("ubuntu")
	probe.Addresses = []string{"203.0.113.10"}
	public := Inspect(context.Background(), "203.0.113.10", probe)
	var advertise, warning *Check
	for index := range public.Checks {
		switch public.Checks[index].ID {
		case "advertise_address":
			advertise = &public.Checks[index]
		case "public_wireguard_endpoint":
			warning = &public.Checks[index]
		}
	}
	if advertise == nil || advertise.State != "pass" {
		t.Fatalf("public advertise address check = %+v", advertise)
	}
	if warning == nil || warning.State != "warn" || !strings.Contains(warning.Remedy, "UDP 51820") {
		t.Fatalf("public WireGuard warning = %+v", warning)
	}
	loopback := fixtureProbe("ubuntu")
	loopback.Addresses = []string{"127.0.0.1"}
	for _, check := range Inspect(context.Background(), "127.0.0.1", loopback).Checks {
		if check.ID == "advertise_address" && check.State != "fail" || check.ID == "public_wireguard_endpoint" {
			t.Fatalf("loopback advertise address produced %+v", check)
		}
	}
	noRoutes := fixtureProbe("ubuntu")
	noRoutes.Addresses = []string{"203.0.113.10"}
	run := noRoutes.Run
	noRoutes.Run = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "ip" {
			return "", errors.New("route inspection failed")
		}
		return run(ctx, name, args...)
	}
	warned := false
	for _, check := range Inspect(context.Background(), "203.0.113.10", noRoutes).Checks {
		warned = warned || check.ID == "public_wireguard_endpoint"
	}
	if !warned {
		t.Fatal("public address lost its WireGuard warning when route inspection failed")
	}
}
func TestSupportedDistributionsAreMinimums(t *testing.T) {
	for _, tc := range []struct {
		os, version, arch string
		want              bool
	}{
		{"ubuntu", "24.04", "amd64", true},
		{"ubuntu", "24.10", "amd64", true},
		{"ubuntu", "26.04", "amd64", true},
		{"ubuntu", "28.04", "amd64", true},
		{"ubuntu", "22.04", "amd64", false},
		{"ubuntu", "24.04", "arm64", true},
		{"fedora", "42", "arm64", true},
		{"ubuntu", "24.04", "riscv64", false},
		{"fedora", "42", "amd64", true},
		{"fedora", "44", "amd64", true},
		{"fedora", "45", "amd64", true},
		{"fedora", "41", "amd64", false},
		{"ubuntu", "", "amd64", false},
		{"ubuntu", "lts", "amd64", false},
		{"debian", "13", "amd64", false},
	} {
		if got := supported(tc.os, tc.version, tc.arch); got != tc.want {
			t.Errorf("supported(%q, %q, %q) = %v, want %v", tc.os, tc.version, tc.arch, got, tc.want)
		}
	}
}
