// Package fswatcher provides cross-platform file system change notifications.
package fswatcher

import (
	"errors"
	"fmt"
	"math/bits"
	"strings"
)

// Op describes a set of file system event types as a bitmask.
type Op uint32

const (
	// Create indicates a file or directory was created.
	Create Op = 1 << iota
	// Write indicates a file's contents were modified.
	Write
	// Remove indicates a file or directory was removed.
	Remove
	// Rename indicates a file or directory was renamed or moved.
	Rename
	// Chmod indicates permissions or attributes changed.
	Chmod
)

// All is the union of every supported Op bit.
const All = Create | Write | Remove | Rename | Chmod

// Has reports whether op contains any bit set in target.
func (op Op) Has(target Op) bool {
	return op&target != 0
}

// String returns a human-readable representation such as "CREATE|WRITE".
func (op Op) String() string {
	op &= All
	if op == 0 {
		return ""
	}

	// Fast path for single-bit ops, which are common in practice and avoid allocations.
	n := bits.OnesCount32(uint32(op))
	if n == 1 {
		switch op {
		case Create:
			return "CREATE"
		case Write:
			return "WRITE"
		case Remove:
			return "REMOVE"
		case Rename:
			return "RENAME"
		case Chmod:
			return "CHMOD"
		}
	}

	// Slow path for multiple bits, which allocates a string builder.
	var buf strings.Builder
	buf.Grow(n * 7) // "CREATE" is 6 chars + '|' separator
	if op.Has(Create) {
		buf.WriteString("|CREATE")
	}
	if op.Has(Write) {
		buf.WriteString("|WRITE")
	}
	if op.Has(Remove) {
		buf.WriteString("|REMOVE")
	}
	if op.Has(Rename) {
		buf.WriteString("|RENAME")
	}
	if op.Has(Chmod) {
		buf.WriteString("|CHMOD")
	}
	return buf.String()[1:]
}

// Event represents a single file system change.
type Event struct {
	// Name is the absolute or watcher-relative path of the affected entry.
	Name string
	// Op is the set of changes that occurred. A single notification may
	// carry more than one bit when the underlying OS coalesces events.
	Op Op
}

// String returns a human-readable representation such as "CREATE: /tmp/x".
func (e Event) String() string {
	return fmt.Sprintf("%s: %q", e.Op, e.Name)
}

// Sentinel errors returned by Watcher methods.
var (
	// ErrAlreadyAdded is returned by Add when path is already registered.
	ErrAlreadyAdded = errors.New("fswatcher: path already added")
	// ErrNotAdded is returned by Remove when path is not registered.
	ErrNotAdded = errors.New("fswatcher: path not added")
	// ErrClosed is returned by methods called on a closed Watcher.
	ErrClosed = errors.New("fswatcher: watcher closed")
	// ErrUnsupported is returned by NewWatcher on platforms without a backend.
	ErrUnsupported = errors.New("fswatcher: platform not supported")
)
