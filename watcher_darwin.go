//go:build darwin

package fsnotify

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// O_EVTONLY opens a file for kqueue notification only; not in stdlib syscall.
const oEvtOnly = 0x8000

// Watcher monitors registered paths via kqueue. Directories are watched
// non-recursively: child entries are tracked so Create and Remove fire
// for files inside the directory, matching the Linux/Windows backends.
type Watcher struct {
	// Events delivers change notifications. Closed when Close returns.
	Events chan Event
	// Errors delivers non-fatal errors from the read loop. Closed when Close returns.
	Errors chan error

	mu     sync.Mutex
	kq     int
	roots  map[string]*kqWatch
	byFd   map[int]*kqWatch
	closed bool
	done   chan struct{}
}

type kqWatch struct {
	fd        int
	path      string
	op        Op
	isDir     bool
	recursive bool // true only on the user-Add'd root for AddRecursive
	parent    *kqWatch
	children  map[string]*kqWatch
}

// NewWatcher returns a Watcher backed by kqueue.
func NewWatcher() (*Watcher, error) {
	kq, err := syscall.Kqueue()
	if err != nil {
		return nil, err
	}
	w := &Watcher{
		Events: make(chan Event, 64),
		Errors: make(chan error, 8),
		kq:     kq,
		roots:  make(map[string]*kqWatch),
		byFd:   make(map[int]*kqWatch),
		done:   make(chan struct{}),
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
// subdirectories created inside path are watched automatically; removed
// subdirectories are dropped on NOTE_DELETE. Returns ErrAlreadyAdded
// if path is already registered.
func (w *Watcher) AddRecursive(path string, op Op) error {
	return w.add(path, op, true)
}

func (w *Watcher) add(path string, op Op, recursive bool) error {
	if op == 0 {
		op = All
	}
	abs, err := canonicalize(path)
	if err != nil {
		return fmt.Errorf("fsnotify: add %s: %w", path, err)
	}
	key := pathKey(abs)

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	if _, exists := w.roots[key]; exists {
		return ErrAlreadyAdded
	}
	root, err := w.openLocked(abs, op, nil)
	if err != nil {
		return fmt.Errorf("fsnotify: add %s: %w", abs, err)
	}
	root.recursive = recursive
	if root.isDir {
		w.populateChildrenLocked(root, recursive)
	}
	w.roots[key] = root
	return nil
}

// populateChildrenLocked scans dir and registers a watch for every
// immediate child. When recursive, descends into each child directory
// so the entire subtree is covered. Caller holds w.mu.
func (w *Watcher) populateChildrenLocked(dir *kqWatch, recursive bool) {
	entries, err := os.ReadDir(dir.path)
	if err != nil {
		return
	}
	for _, e := range entries {
		childPath := filepath.Join(dir.path, e.Name())
		child, err := w.openLocked(childPath, dir.op, dir)
		if err != nil {
			continue
		}
		dir.children[e.Name()] = child
		if recursive && child.isDir {
			w.populateChildrenLocked(child, true)
		}
	}
}

// Remove unregisters path. Returns ErrNotAdded if path is not registered.
func (w *Watcher) Remove(path string) error {
	abs, err := canonicalize(path)
	if err != nil {
		return fmt.Errorf("fsnotify: remove %s: %w", path, err)
	}
	key := pathKey(abs)

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	root, ok := w.roots[key]
	if !ok {
		return ErrNotAdded
	}
	delete(w.roots, key)
	w.closeTreeLocked(root)
	return nil
}

// Close stops the watcher. Subsequent calls are no-ops.
func (w *Watcher) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	close(w.done)
	for _, root := range w.roots {
		w.closeTreeLocked(root)
	}
	w.roots = nil
	kq := w.kq
	w.mu.Unlock()
	return syscall.Close(kq)
}

func (w *Watcher) openLocked(path string, op Op, parent *kqWatch) (*kqWatch, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|oEvtOnly, 0)
	if err != nil {
		return nil, err
	}
	stat, err := os.Lstat(path)
	if err != nil {
		syscall.Close(fd)
		return nil, err
	}
	ww := &kqWatch{
		fd:     fd,
		path:   path,
		op:     op,
		isDir:  stat.IsDir(),
		parent: parent,
	}
	if ww.isDir {
		ww.children = make(map[string]*kqWatch)
	}
	if err := w.registerLocked(ww); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	w.byFd[fd] = ww
	return ww, nil
}

func (w *Watcher) registerLocked(ww *kqWatch) error {
	var ev syscall.Kevent_t
	syscall.SetKevent(&ev, ww.fd, syscall.EVFILT_VNODE, syscall.EV_ADD|syscall.EV_CLEAR)
	ev.Fflags = opToNoteFlags(ww.op, ww.isDir)
	_, err := syscall.Kevent(w.kq, []syscall.Kevent_t{ev}, nil, nil)
	return err
}

func (w *Watcher) closeTreeLocked(ww *kqWatch) {
	for _, c := range ww.children {
		w.closeTreeLocked(c)
	}
	delete(w.byFd, ww.fd)
	syscall.Close(ww.fd)
}

func (w *Watcher) readLoop() {
	defer close(w.Events)
	defer close(w.Errors)

	events := make([]syscall.Kevent_t, 16)
	for {
		n, err := syscall.Kevent(w.kq, nil, events, nil)
		select {
		case <-w.done:
			return
		default:
		}
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if errors.Is(err, syscall.EBADF) {
				return
			}
			w.sendError(err)
			return
		}
		for i := 0; i < n; i++ {
			w.handleEvent(&events[i])
		}
	}
}

func (w *Watcher) handleEvent(ev *syscall.Kevent_t) {
	fd := int(ev.Ident)
	fflags := ev.Fflags

	w.mu.Lock()
	ww, ok := w.byFd[fd]
	if !ok {
		w.mu.Unlock()
		return
	}
	root := ww
	for root.parent != nil {
		root = root.parent
	}
	requested := root.op
	path := ww.path
	isDir := ww.isDir
	parent := ww.parent
	w.mu.Unlock()

	if fflags&syscall.NOTE_DELETE != 0 && requested.Has(Remove) {
		w.sendEvent(Event{Name: path, Op: Remove})
	}
	if fflags&syscall.NOTE_RENAME != 0 && requested.Has(Rename) {
		w.sendEvent(Event{Name: path, Op: Rename})
	}
	if fflags&syscall.NOTE_ATTRIB != 0 && requested.Has(Chmod) {
		w.sendEvent(Event{Name: path, Op: Chmod})
	}
	if fflags&syscall.NOTE_WRITE != 0 {
		if isDir {
			w.diffDir(ww, requested)
		} else if requested.Has(Write) {
			w.sendEvent(Event{Name: path, Op: Write})
		}
	}

	if fflags&(syscall.NOTE_DELETE|syscall.NOTE_RENAME) != 0 {
		w.mu.Lock()
		if parent != nil {
			delete(parent.children, filepath.Base(path))
		} else {
			// Root watch went away; drop it from the user-facing map so
			// the path can be re-added.
			delete(w.roots, pathKey(path))
		}
		// Recursively close the dropped node and every descendant so deep
		// subtrees do not leak fds when an interior directory disappears.
		w.closeTreeLocked(ww)
		w.mu.Unlock()
	}
}

func (w *Watcher) diffDir(dir *kqWatch, requested Op) {
	entries, err := os.ReadDir(dir.path)
	if err != nil {
		w.sendError(err)
		return
	}
	current := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		current[e.Name()] = struct{}{}
	}

	var added []string

	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	root := dir
	for root.parent != nil {
		root = root.parent
	}
	recursive := root.recursive
	for name := range current {
		if _, ok := dir.children[name]; ok {
			continue
		}
		childPath := filepath.Join(dir.path, name)
		child, err := w.openLocked(childPath, requested, dir)
		if err != nil {
			continue
		}
		dir.children[name] = child
		added = append(added, childPath)
		if recursive && child.isDir {
			w.populateChildrenLocked(child, true)
		}
	}
	w.mu.Unlock()

	if requested.Has(Create) {
		for _, p := range added {
			w.sendEvent(Event{Name: p, Op: Create})
		}
	}
}

func (w *Watcher) sendEvent(e Event) {
	select {
	case w.Events <- e:
	case <-w.done:
	}
}

func (w *Watcher) sendError(err error) {
	select {
	case w.Errors <- err:
	case <-w.done:
	}
}

func opToNoteFlags(op Op, isDir bool) uint32 {
	var f uint32
	if op.Has(Remove) {
		f |= syscall.NOTE_DELETE
	}
	if op.Has(Rename) {
		f |= syscall.NOTE_RENAME
	}
	if op.Has(Chmod) {
		f |= syscall.NOTE_ATTRIB
	}
	// Directory watches always need NOTE_WRITE to detect Create/Remove of children.
	if isDir || op.Has(Write) {
		f |= syscall.NOTE_WRITE
	}
	return f
}
