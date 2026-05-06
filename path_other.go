//go:build !windows

package fsnotify

// pathKey returns p unchanged on platforms with case-sensitive paths.
func pathKey(p string) string {
	return p
}
