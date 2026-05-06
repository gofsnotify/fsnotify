//go:build windows

package fsnotify

import (
	"strings"
	"syscall"
)

// pathKey returns a comparison key for p. NTFS is case-insensitive, so
// fold to lowercase before using a path as a map key.
func pathKey(p string) string {
	return strings.ToLower(p)
}

// canonicalizeOS expands an 8.3 short-form path (e.g. C:\PROGRA~1) to
// its long form via GetLongPathName so two spellings of the same path
// dedupe. Returns p unchanged when expansion fails.
func canonicalizeOS(p string) string {
	in, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return p
	}
	const initial = syscall.MAX_PATH
	buf := make([]uint16, initial)
	n, err := syscall.GetLongPathName(in, &buf[0], uint32(len(buf)))
	if err != nil {
		return p
	}
	if int(n) > len(buf) {
		buf = make([]uint16, n)
		n, err = syscall.GetLongPathName(in, &buf[0], uint32(len(buf)))
		if err != nil {
			return p
		}
	}
	return syscall.UTF16ToString(buf[:n])
}
