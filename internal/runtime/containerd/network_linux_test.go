//go:build linux

package containerd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TorstenDittmann/orch/internal/model"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
)

func TestVolumeMountsWritesApplicationScopedReadableResolver(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	volumes := filepath.Join(root, "volumes")
	if err := os.Mkdir(volumes, 0o700); err != nil {
		t.Fatal(err)
	}
	baseResolver := filepath.Join(root, "network", "resolv.conf")
	client := &runtimeClient{
		options: orchruntime.Options{VolumesDir: volumes, ResolverPath: baseResolver},
		networkConfig: model.NetworkConfig{
			Generation: 2, Gateway: "10.96.0.1", DNSZone: model.ServiceDNSZone,
		},
	}
	mounts, err := client.volumeMounts(model.AllocationDesired{
		Application: "hello", NetworkGeneration: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(root, "network", "resolv-hello.conf")
	if len(mounts) != 1 || mounts[0].Destination != "/etc/resolv.conf" || mounts[0].Source != wantPath {
		t.Fatalf("resolver mount = %#v", mounts)
	}
	contents, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "nameserver 10.96.0.1\nsearch hello.svc.internal\noptions ndots:1 timeout:1 attempts:2\n"
	if string(contents) != want {
		t.Fatalf("resolver contents = %q, want %q", contents, want)
	}
	info, err := os.Lstat(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("resolver mode = %v", info.Mode().Perm())
	}
}
