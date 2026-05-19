package fswatcher

import "path/filepath"

// Canonicalize returns the same form that the watcher uses internally
// for [Watcher.Add], [Watcher.Remove], and [Event.Name]: an absolute,
// cleaned path with OS-specific normalization applied (on Windows, 8.3
// short forms are expanded and case is folded; on macOS, the
// `/private` prefix on `/var` and `/tmp` is preserved as returned by
// the system). When the target exists, symlinks are resolved so two
// paths reaching the same inode dedupe; when it does not exist, the
// cleaned absolute path is returned as-is.
//
// Callers that need to compute paths relative to a watched root
// (for example, to apply an ignore list to [Event.Name]) should pass
// the root through Canonicalize first so the prefix matches.
//
// The returned error comes from [filepath.Abs] and indicates the
// working directory could not be determined.
func Canonicalize(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	cleaned := filepath.Clean(abs)
	cleaned = canonicalizeOS(cleaned)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return resolved, nil
	}
	return cleaned, nil
}
