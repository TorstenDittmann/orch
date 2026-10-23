package images

// SetFreeBytes replaces how the store measures free disk space.
func SetFreeBytes(s *Store, free func(string) (uint64, error)) { s.freeBytes = free }
