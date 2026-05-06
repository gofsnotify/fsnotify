package fsnotify

import "path/filepath"

// canonicalize returns the absolute, cleaned form of p so that paths
// passed to Add and Remove compare consistently regardless of the form
// the caller used (relative, with redundant separators, with `.`/`..`,
// or via symbolic links). When the target exists, symlinks are
// resolved so two paths reaching the same inode dedupe.
func canonicalize(p string) (string, error) {
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
