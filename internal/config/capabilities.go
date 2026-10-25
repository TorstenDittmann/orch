package config

import (
	"fmt"
	"slices"
	"strings"
)

// Linux capabilities through CAP_CHECKPOINT_RESTORE. Names are the canonical OCI
// names, not aliases or the Docker "ALL" shorthand.
var linuxCapabilities = strings.Fields(`
CAP_AUDIT_CONTROL CAP_AUDIT_READ CAP_AUDIT_WRITE CAP_BLOCK_SUSPEND CAP_BPF
CAP_CHECKPOINT_RESTORE CAP_CHOWN CAP_DAC_OVERRIDE CAP_DAC_READ_SEARCH CAP_FOWNER
CAP_FSETID CAP_IPC_LOCK CAP_IPC_OWNER CAP_KILL CAP_LEASE CAP_LINUX_IMMUTABLE
CAP_MAC_ADMIN CAP_MAC_OVERRIDE CAP_MKNOD CAP_NET_ADMIN CAP_NET_BIND_SERVICE
CAP_NET_BROADCAST CAP_NET_RAW CAP_PERFMON CAP_SETFCAP CAP_SETGID CAP_SETPCAP
CAP_SETUID CAP_SYS_ADMIN CAP_SYS_BOOT CAP_SYS_CHROOT CAP_SYS_MODULE CAP_SYS_NICE
CAP_SYS_PACCT CAP_SYS_PTRACE CAP_SYS_RAWIO CAP_SYS_RESOURCE CAP_SYS_TIME
CAP_SYS_TTY_CONFIG CAP_SYSLOG CAP_WAKE_ALARM
`)

// ValidateCapabilities checks both authoring input and agent assignments. The
// caller separately enforces canonical ordering on the runtime wire contract.
func ValidateCapabilities(capabilities []string) error {
	if len(capabilities) > len(linuxCapabilities) {
		return fmt.Errorf("must not contain more than %d Linux capabilities", len(linuxCapabilities))
	}
	seen := make(map[string]bool, len(capabilities))
	for _, capability := range capabilities {
		if len(capability) > 64 {
			return fmt.Errorf("Linux capability names must not exceed 64 bytes")
		}
		if !slices.Contains(linuxCapabilities, capability) {
			return fmt.Errorf("unknown Linux capability %q; use canonical CAP_* names", capability)
		}
		if seen[capability] {
			return fmt.Errorf("duplicates Linux capability %q", capability)
		}
		seen[capability] = true
	}
	return nil
}

// CapabilityServices returns sorted service names requiring explicit capability
// grants. Like privileged services, only administrators may deploy them.
func (m *Manifest) CapabilityServices() []string {
	var names []string
	for _, name := range sortedKeys(m.Services) {
		if len(m.Services[name].Capabilities) != 0 {
			names = append(names, name)
		}
	}
	return names
}
