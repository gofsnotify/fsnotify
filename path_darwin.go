//go:build darwin

package fsnotify

import (
	"golang.org/x/text/cases"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// pathKey returns a comparison key for p. APFS is case-insensitive,
// so fold the case before using a path as a map key.
// APFS is also Unicode-normalization-insensitive, so normalize to NFD as well.
func pathKey(p string) string {
	t := transform.Chain(norm.NFD, cases.Fold())
	p, _, _ = transform.String(t, p)
	return p
}

// canonicalizeOS is a no-op on platforms without 8.3 short-form aliases.
func canonicalizeOS(p string) string {
	return p
}
