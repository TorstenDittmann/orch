package hostports

import (
	"slices"
	"testing"
)

func TestParseSSHPortsReadsEveryPortLine(t *testing.T) {
	config := "port 2222\nport 22\naddressfamily any\nlistenaddress bogus\npermitrootlogin prohibit-password\nport 0\n"
	if got := parseSSH(config); !slices.Equal(got, []uint16{2222, 22}) {
		t.Fatalf("parseSSH = %v", got)
	}
}

func TestParseSSHPortsReadsListenAddresses(t *testing.T) {
	// sshd -T prints the default port even when ListenAddress names another,
	// and sshd does not listen on it then.
	config := "port 22\nlistenaddress 0.0.0.0:2222\nlistenaddress [::]:2200 rdomain blue\nlistenaddress [::]:2222\nlistenaddress bogus\n"
	if got := parseSSH(config); !slices.Equal(got, []uint16{2222, 2200}) {
		t.Fatalf("parseSSH = %v", got)
	}
}

func TestListeningCountsOnlyListeners(t *testing.T) {
	tcp := "  sl  local_address rem_address   st\n" +
		"   0: 00000000:08AE 00000000:0000 0A 00000000:00000000\n" + // 2222 LISTEN
		"   1: 0A000002:1F90 0A000001:01BB 01 00000000:00000000\n" + // 8080 as a source port
		"   2: 0A000002:1F91 0A000001:01BB 06 00000000:00000000\n" // 8081 in TIME_WAIT
	udp := "   sl  local_address rem_address   st\n" +
		"   0: 00000000:CA6C 00000000:0000 07 00000000:00000000\n" + // 51820 bound
		"   1: 0A000002:0035 08080808:0035 01 00000000:00000000\n" // 53 connected
	for _, tc := range []struct {
		table, protocol string
		port            uint16
		want            bool
	}{
		{tcp, "tcp", 2222, true}, {tcp, "tcp", 8080, false}, {tcp, "tcp", 8081, false}, {tcp, "tcp", 22, false},
		{udp, "udp", 51820, true}, {udp, "udp", 53, false},
	} {
		if got := listening(tc.table, tc.protocol, tc.port); got != tc.want {
			t.Errorf("listening(%s %d) = %v, want %v", tc.protocol, tc.port, got, tc.want)
		}
	}
}
