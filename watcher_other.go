//go:build !linux && !windows && !darwin && !freebsd

package fsnotify

// Watcher is a no-op placeholder on platforms without a backend.
type Watcher struct {
	Events chan Event
	Errors chan error
}

// NewWatcher returns ErrUnsupported on platforms without a backend.
func NewWatcher() (*Watcher, error) {
	return nil, ErrUnsupported
}

// Add returns ErrUnsupported.
func (w *Watcher) Add(string, Op) error { return ErrUnsupported }

// AddRecursive returns ErrUnsupported.
func (w *Watcher) AddRecursive(string, Op) error { return ErrUnsupported }

// Remove returns ErrUnsupported.
func (w *Watcher) Remove(string) error { return ErrUnsupported }

// Close is a no-op.
func (w *Watcher) Close() error { return nil }
