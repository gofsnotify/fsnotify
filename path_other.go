//go:build !windows && !darwin

package fswatcher

// pathKey returns p unchanged on platforms with case-sensitive paths.
func pathKey(p string) string {
	return p
}

// canonicalizeOS is a no-op on platforms without 8.3 short-form aliases.
func canonicalizeOS(p string) string {
	return p
}
