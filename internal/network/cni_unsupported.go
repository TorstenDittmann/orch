//go:build !linux

package network

func New(Options) (Manager, error) { return nil, ErrUnsupported }
