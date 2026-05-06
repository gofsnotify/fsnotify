//go:build windows

package fsnotify

import "strings"

// pathKey returns a comparison key for p. NTFS is case-insensitive, so
// fold to lowercase before using a path as a map key.
func pathKey(p string) string {
	return strings.ToLower(p)
}
