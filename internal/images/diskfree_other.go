//go:build !unix

package images

import "math"

// Controllers run on Linux; other platforms only build the package.
func freeBytes(string) (uint64, error) { return math.MaxUint64, nil }
