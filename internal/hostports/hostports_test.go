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
