package fsnotify

import "path/filepath"

// canonicalize returns the absolute, cleaned form of p so that paths
// passed to Add and Remove compare consistently regardless of the form
// the caller used (relative, with redundant separators, or with `.`/`..`).
func canonicalize(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}
