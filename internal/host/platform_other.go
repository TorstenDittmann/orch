//go:build !linux && !darwin

package host

func freeSpace(string) uint64 { return 0 }
