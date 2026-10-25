package hostports

import (
	"slices"
	"testing"
)

func TestParseSSHPortsReadsEveryPortLine(t *testing.T) {
	config := "port 2222\nport 22\naddressfamily any\nlistenaddress [::]:2222\npermitrootlogin prohibit-password\nport 0\n"
	if got := parseSSH(config); !slices.Equal(got, []uint16{2222, 22}) {
		t.Fatalf("parseSSH = %v", got)
	}
}

func TestParseSSHPortsReadsListenAddresses(t *testing.T) {
	// sshd -T prints the default port even when ListenAddress names another.
	config := "port 22\nlistenaddress 0.0.0.0:2222\nlistenaddress [::]:2200 rdomain blue\nlistenaddress bogus\n"
	if got := parseSSH(config); !slices.Equal(got, []uint16{22, 2222, 2200}) {
		t.Fatalf("parseSSH = %v", got)
	}
}
