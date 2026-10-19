//go:build linux

package agent

import "testing"

func TestParseHostUsageFiles(t *testing.T) {
	t.Parallel()
	times, err := parseProcStat([]byte("cpu  100 5 50 800 20 3 2 0 7 1\ncpu0 1 2 3 4\n"))
	if err != nil || times.total != 980 || times.idle != 820 {
		t.Fatalf("times = %#v, %v", times, err)
	}
	if _, err := parseProcStat([]byte("intr 1 2 3\n")); err == nil {
		t.Fatal("parsed a /proc/stat without a cpu line")
	}
	total, available, err := parseMeminfo([]byte("MemTotal:       16000 kB\nMemFree:         1000 kB\nMemAvailable:    9000 kB\n"))
	if err != nil || total != 16000<<10 || available != 9000<<10 {
		t.Fatalf("meminfo = %d %d %v", total, available, err)
	}
	if _, _, err := parseMeminfo([]byte("MemTotal: 1 kB\n")); err == nil {
		t.Fatal("parsed meminfo without MemAvailable")
	}
	if _, err := readHost(t.TempDir()); err != nil {
		t.Fatalf("read this host: %v", err)
	}
}
