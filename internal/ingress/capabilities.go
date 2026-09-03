package ingress

import "encoding/binary"

const (
	fileCapabilityRevisionMask = uint32(0xff000000)
	fileCapabilityRevision1    = uint32(0x01000000)
	fileCapabilityRevision2    = uint32(0x02000000)
	fileCapabilityRevision3    = uint32(0x03000000)
	fileCapabilityEffective    = uint32(0x00000001)
	netBindServiceCapability   = uint32(1 << 10)
)

// hasOnlyNetBindServiceFileCapability validates the complete Linux file
// capability payload. Merely finding CAP_NET_BIND_SERVICE is insufficient:
// an otherwise privileged Caddy binary would violate the process boundary.
func hasOnlyNetBindServiceFileCapability(value []byte) bool {
	if len(value) < 4 {
		return false
	}
	magic := binary.LittleEndian.Uint32(value[:4])
	revision := magic & fileCapabilityRevisionMask
	words := 0
	expectedLength := 0
	switch revision {
	case fileCapabilityRevision1:
		words, expectedLength = 1, 12
	case fileCapabilityRevision2:
		words, expectedLength = 2, 20
	case fileCapabilityRevision3:
		words, expectedLength = 2, 24
	default:
		return false
	}
	if len(value) != expectedLength || magic != revision|fileCapabilityEffective {
		return false
	}
	for index := 0; index < words; index++ {
		permitted := binary.LittleEndian.Uint32(value[4+index*8 : 8+index*8])
		inheritable := binary.LittleEndian.Uint32(value[8+index*8 : 12+index*8])
		wantPermitted := uint32(0)
		if index == 0 {
			wantPermitted = netBindServiceCapability
		}
		if permitted != wantPermitted || inheritable != 0 {
			return false
		}
	}
	// Revision 3 records the root ID of the user namespace that created the
	// capability. Only the initial namespace's root identity is accepted.
	return revision != fileCapabilityRevision3 || binary.LittleEndian.Uint32(value[20:24]) == 0
}
