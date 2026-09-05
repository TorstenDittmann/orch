package host

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func fixtureProbe(distro string) Probe {
	files := map[string]string{"/etc/os-release": "ID=" + distro + "\nVERSION_ID=24.04", "/proc/1/comm": "systemd\n", "/sys/fs/cgroup/cgroup.controllers": "cpuset cpu io memory pids\n", "/proc/sys/net/ipv4/ip_forward": "0\n", "/sys/fs/selinux/enforce": "1"}
	if distro == "fedora" {
		files["/etc/os-release"] = "ID=fedora\nVERSION_ID=44"
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
	for _, distro := range []string{"ubuntu", "fedora"} {
		t.Run(distro, func(t *testing.T) {
			r := Inspect(context.Background(), "10.0.0.11", fixtureProbe(distro))
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
		"architecture":  func(p *Probe) { p.Arch = "arm64" },
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
		"selinux": func(p *Probe) {
			read := p.Read
			p.Read = func(s string) ([]byte, error) {
				if s == "/sys/fs/selinux/enforce" {
					return []byte("0"), nil
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
