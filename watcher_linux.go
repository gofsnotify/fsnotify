//go:build linux

package fswatcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// Watcher monitors registered paths for file system changes via inotify.
type Watcher struct {
	// Events delivers change notifications. Closed when Close returns.
	Events <-chan Event
	// Errors delivers non-fatal errors from the read loop. Closed when Close returns.
	Errors <-chan error

	events chan<- Event
	errors chan<- error

	mu      sync.Mutex
	fd      int
	file    *os.File
	watches map[string]*linuxWatch
	wdToKey map[int32]string
	closed  bool
	done    chan struct{}
	exited  chan struct{}
}

type linuxWatch struct {
	abs       string
	wd        int32
	op        Op
	rootKey   string // pathKey of the user-Add'd root that owns this watch
	recursive bool   // true only on the root entry of an AddRecursive call
}

// NewWatcher returns a Watcher backed by Linux inotify.
func NewWatcher() (*Watcher, error) {
	// IN_NONBLOCK keeps the fd pollable through Go's runtime poller so
	// closing the *os.File wrapper reliably unblocks a pending Read.
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		return nil, err
	}

	events := make(chan Event, 64)
	errors := make(chan error, 8)
	w := &Watcher{
		Events:  events,
		Errors:  errors,
		events:  events,
		errors:  errors,
		fd:      fd,
		file:    os.NewFile(uintptr(fd), "inotify"),
		watches: make(map[string]*linuxWatch),
		wdToKey: make(map[int32]string),
		done:    make(chan struct{}),
		exited:  make(chan struct{}),
	}
	go w.readLoop()
	return w, nil
}

// Add registers path with the given event mask. Returns ErrAlreadyAdded
// if path is already registered, or ErrClosed if the watcher is closed.
func (w *Watcher) Add(path string, op Op) error {
	return w.add(path, op, false)
}

// AddRecursive registers path and every directory below it. New
// subdirectories created inside path are watched automatically; subtrees
// that disappear are dropped via the kernel's IN_IGNORED notification.
// Returns ErrAlreadyAdded if path is already registered.
//
// When a directory is created underneath an AddRecursive root, the
// watcher attaches an inotify watch to it and walks it for any
// pre-existing descendants (for example after mkdir -p or after a
// populated subtree is moved in) so that their Create events are not
// lost. If another process concurrently creates entries inside the new
// directory in the brief window between watch attachment and the walk,
// the same Create may be reported twice; consumers should handle
// duplicate Create events idempotently.
func (w *Watcher) AddRecursive(path string, op Op) error {
	return w.add(path, op, true)
}

func (w *Watcher) add(path string, op Op, recursive bool) error {
	if op == 0 {
		op = All
	}
	abs, err := Canonicalize(path)
	if err != nil {
		return fmt.Errorf("fswatcher: add %s: %w", path, err)
	}
	key := pathKey(abs)

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	if _, ok := w.watches[key]; ok {
		return ErrAlreadyAdded
	}
	if _, err := w.addWatchLocked(abs, op, key, recursive); err != nil {
		return fmt.Errorf("fswatcher: add %s: %w", abs, err)
	}
	if recursive {
		// Pre-existing descendants at registration time are intentionally
		// silent — the user just asked us to start watching, so they are
		// not new from the user's point of view.
		_ = w.walkAndAddLocked(abs, op, key)
	}
	return nil
}

// addWatchLocked registers a single inotify watch and stores it.
// Caller holds w.mu.
func (w *Watcher) addWatchLocked(abs string, op Op, rootKey string, recursive bool) (*linuxWatch, error) {
	wd, err := syscall.InotifyAddWatch(w.fd, abs, opToMask(op))
	if err != nil {
		return nil, err
	}
	wd32 := int32(wd)
	lw := &linuxWatch{abs: abs, wd: wd32, op: op, rootKey: rootKey, recursive: recursive}
	w.watches[pathKey(abs)] = lw
	w.wdToKey[wd32] = pathKey(abs)
	return lw, nil
}

// walkAndAddLocked walks root and adds an inotify watch for every
// subdirectory. Returns the absolute paths of subdirectories that were
// newly watched on this call (root itself excluded; entries that were
// already watched are skipped). Best-effort: unreadable subtrees are
// skipped silently. Caller holds w.mu.
func (w *Watcher) walkAndAddLocked(root string, op Op, rootKey string) []string {
	var added []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if pathKey(p) == rootKey {
			return nil
		}
		if _, dup := w.watches[pathKey(p)]; dup {
			return nil
		}
		if _, err := w.addWatchLocked(p, op, rootKey, false); err == nil {
			added = append(added, p)
		}
		return nil
	})
	return added
}

// Remove unregisters path. For an AddRecursive registration, every
// descendant watch added on its behalf is dropped too. Returns
// ErrNotAdded if path is not registered.
func (w *Watcher) Remove(path string) error {
	abs, err := Canonicalize(path)
	if err != nil {
		return fmt.Errorf("fswatcher: remove %s: %w", path, err)
	}
	key := pathKey(abs)

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	root, ok := w.watches[key]
	if !ok || root.rootKey != key {
		// Not a user-Add'd root; sub-watches added by AddRecursive
		// cannot be removed independently of their root.
		return ErrNotAdded
	}
	var toRemove []*linuxWatch
	for _, lw := range w.watches {
		if lw.rootKey == key {
			toRemove = append(toRemove, lw)
		}
	}
	for _, lw := range toRemove {
		_, _ = syscall.InotifyRmWatch(w.fd, uint32(lw.wd))
		delete(w.watches, pathKey(lw.abs))
		delete(w.wdToKey, lw.wd)
	}
	return nil
}

// Close stops the watcher. Subsequent calls are no-ops. Close blocks
// until the read loop has fully exited so the kernel cannot reuse the
// inotify fd while a stale goroutine is still reading from it.
func (w *Watcher) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		<-w.exited
		return nil
	}
	w.closed = true
	close(w.done)
	file := w.file
	w.mu.Unlock()
	// Closing the *os.File wakes the goroutine's blocking Read via the
	// runtime poller, unlike syscall.Close which can leave it stuck.
	err := file.Close()
	<-w.exited
	return err
}

func (w *Watcher) readLoop() {
	defer close(w.exited)
	defer close(w.events)
	defer close(w.errors)

	var buf [4096]byte
	for {
		n, err := w.file.Read(buf[:])
		if err != nil {
			select {
			case <-w.done:
				return
			default:
			}
			// os.ErrClosed surfaces when Close calls file.Close().
			if errors.Is(err, os.ErrClosed) {
				return
			}
			w.sendError(err)
			return
		}
		if n <= 0 {
			return
		}

		off := 0
		for off+syscall.SizeofInotifyEvent <= n {
			raw := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[off]))
			nameLen := int(raw.Len)
			nameStart := off + syscall.SizeofInotifyEvent
			nameEnd := nameStart + nameLen
			if nameEnd > n {
				break
			}
			name := ""
			if nameLen > 0 {
				name = strings.TrimRight(string(buf[nameStart:nameEnd]), "\x00")
			}
			off = nameEnd

			w.dispatch(raw.Wd, raw.Mask, name)
		}
	}
}

func (w *Watcher) dispatch(wd int32, mask uint32, name string) {
	// IN_IGNORED arrives when the kernel drops the watch (target deleted,
	// filesystem unmounted, or after explicit InotifyRmWatch). Clean up
	// our maps so the path can be re-added.
	if mask&syscall.IN_IGNORED != 0 {
		w.mu.Lock()
		if key, ok := w.wdToKey[wd]; ok {
			delete(w.watches, key)
			delete(w.wdToKey, wd)
		}
		w.mu.Unlock()
		return
	}

	w.mu.Lock()
	key, ok := w.wdToKey[wd]
	var lw *linuxWatch
	var recursive bool
	var rootKey string
	if ok {
		lw = w.watches[key]
		if lw != nil {
			rootKey = lw.rootKey
			if root := w.watches[lw.rootKey]; root != nil {
				recursive = root.recursive
			}
		}
	}
	w.mu.Unlock()
	if lw == nil {
		return
	}

	full := lw.abs
	if name != "" {
		full = filepath.Join(lw.abs, name)
	}

	// A new directory under a recursive root needs its own watch so we
	// keep seeing events as the tree grows. Walk into it in case it
	// already contains pre-existing children (e.g. mkdir -p, or a
	// populated subtree renamed in) and synthesize Create for each so
	// they are not silently lost — inotify only reports the outermost
	// directory in that situation. A concurrent writer creating entries
	// in the brief window between watch attachment and the walk may
	// cause the same Create to be reported twice; this is documented on
	// AddRecursive.
	var synth []string
	if recursive && mask&syscall.IN_CREATE != 0 && mask&syscall.IN_ISDIR != 0 {
		var addErr error
		w.mu.Lock()
		if !w.closed {
			if _, exists := w.watches[pathKey(full)]; !exists {
				if _, err := w.addWatchLocked(full, lw.op, rootKey, false); err != nil {
					addErr = err
				} else {
					synth = w.walkAndAddLocked(full, lw.op, rootKey)
				}
			}
		}
		w.mu.Unlock()
		if addErr != nil {
			w.sendError(fmt.Errorf("fswatcher: auto-watch %s: %w", full, addErr))
		}
	}

	op := maskToOp(mask) & lw.op
	if op != 0 {
		w.sendEvent(Event{Name: full, Op: op})
	}
	if lw.op.Has(Create) {
		for _, p := range synth {
			w.sendEvent(Event{Name: p, Op: Create})
		}
	}
}

func (w *Watcher) sendEvent(e Event) {
	select {
	case w.events <- e:
	case <-w.done:
	}
}

func (w *Watcher) sendError(err error) {
	select {
	case w.errors <- err:
	case <-w.done:
	}
}

func opToMask(op Op) uint32 {
	var m uint32
	if op.Has(Create) {
		m |= syscall.IN_CREATE | syscall.IN_MOVED_TO
	}
	if op.Has(Write) {
		m |= syscall.IN_MODIFY
	}
	if op.Has(Remove) {
		m |= syscall.IN_DELETE | syscall.IN_DELETE_SELF
	}
	if op.Has(Rename) {
		m |= syscall.IN_MOVED_FROM | syscall.IN_MOVE_SELF
	}
	if op.Has(Chmod) {
		m |= syscall.IN_ATTRIB
	}
	return m
}

func maskToOp(mask uint32) Op {
	var op Op
	if mask&(syscall.IN_CREATE|syscall.IN_MOVED_TO) != 0 {
		op |= Create
	}
	if mask&syscall.IN_MODIFY != 0 {
		op |= Write
	}
	if mask&(syscall.IN_DELETE|syscall.IN_DELETE_SELF) != 0 {
		op |= Remove
	}
	if mask&(syscall.IN_MOVED_FROM|syscall.IN_MOVE_SELF) != 0 {
		op |= Rename
	}
	if mask&syscall.IN_ATTRIB != 0 {
		op |= Chmod
	}
	return op
}
