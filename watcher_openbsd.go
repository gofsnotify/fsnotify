//go:build openbsd

package fswatcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// O_EVTONLY opens a file for kqueue notification only; not in stdlib syscall.
const oEvtOnly = 0x8000

// Watcher monitors registered paths via kqueue. Directories are watched
// non-recursively: child entries are tracked so Create and Remove fire
// for files inside the directory, matching the Linux/Windows backends.
type Watcher struct {
	// Events delivers change notifications. Closed when Close returns.
	Events <-chan Event
	// Errors delivers non-fatal errors from the read loop. Closed when Close returns.
	Errors <-chan error

	events chan<- Event
	errors chan<- error

	mu     sync.Mutex
	kq     int
	roots  map[string]*kqWatch
	byFd   map[int]*kqWatch
	closed bool
	done   chan struct{}
	exited chan struct{}
	// pipe for signaling Close on platforms without EVFILT_USER
	closePipe [2]int
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
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}

	// Create a pipe for signaling Close
	var closePipe [2]int
	if err := unix.Pipe(closePipe[:]); err != nil {
		unix.Close(kq)
		return nil, err
	}

	// Register the read end of the pipe with kqueue
	var ev unix.Kevent_t
	unix.SetKevent(&ev, closePipe[0], unix.EVFILT_READ, unix.EV_ADD|unix.EV_CLEAR)
	if _, err := unix.Kevent(kq, []unix.Kevent_t{ev}, nil, nil); err != nil {
		unix.Close(closePipe[0])
		unix.Close(closePipe[1])
		unix.Close(kq)
		return nil, err
	}

	events := make(chan Event, 64)
	errors := make(chan error, 8)
	w := &Watcher{
		Events:    events,
		Errors:    errors,
		events:    events,
		errors:    errors,
		kq:        kq,
		roots:     make(map[string]*kqWatch),
		byFd:      make(map[int]*kqWatch),
		done:      make(chan struct{}),
		exited:    make(chan struct{}),
		closePipe: closePipe,
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
//
// When a directory is created underneath an AddRecursive root, the
// watcher attaches a watch to it and walks it for any pre-existing
// descendants (for example after mkdir -p or after a populated subtree
// is moved in) so that their Create events are not lost. If another
// process concurrently creates entries inside the new directory in the
// brief window between watch attachment and the walk, the same Create
// may be reported twice; consumers should handle duplicate Create
// events idempotently.
func (w *Watcher) AddRecursive(path string, op Op) error {
	return w.add(path, op, true)
}

func (w *Watcher) add(path string, op Op, recursive bool) error {
	if op == 0 {
		op = All
	}
	abs, err := canonicalize(path)
	if err != nil {
		return fmt.Errorf("fswatcher: add %s: %w", path, err)
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
		return fmt.Errorf("fswatcher: add %s: %w", abs, err)
	}
	root.recursive = recursive
	if root.isDir {
		// Pre-existing descendants at registration time are intentionally
		// silent — the user just asked us to start watching, so they are
		// not new from the user's point of view.
		_ = w.populateChildrenLocked(root, recursive)
	}
	w.roots[key] = root
	return nil
}

// populateChildrenLocked scans dir and registers a watch for every
// immediate child. When recursive, descends into each child directory
// so the entire subtree is covered. Returns the absolute paths of
// every entry (immediate children and descendants when recursive) that
// was newly added on this call so callers can synthesize Create events
// for them. Caller holds w.mu.
func (w *Watcher) populateChildrenLocked(dir *kqWatch, recursive bool) []string {
	entries, err := os.ReadDir(dir.path)
	if err != nil {
		return nil
	}
	var added []string
	for _, e := range entries {
		childPath := filepath.Join(dir.path, e.Name())
		child, err := w.openLocked(childPath, dir.op, dir)
		if err != nil {
			continue
		}
		dir.children[e.Name()] = child
		added = append(added, childPath)
		if recursive && child.isDir {
			added = append(added, w.populateChildrenLocked(child, true)...)
		}
	}
	return added
}

// Remove unregisters path. Returns ErrNotAdded if path is not registered.
func (w *Watcher) Remove(path string) error {
	abs, err := canonicalize(path)
	if err != nil {
		return fmt.Errorf("fswatcher: remove %s: %w", path, err)
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

// Close stops the watcher. Subsequent calls are no-ops. Close blocks
// until the read loop has fully exited so callers can rely on
// Events/Errors being closed by the time Close returns.
func (w *Watcher) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		<-w.exited
		return nil
	}
	w.closed = true
	close(w.done)
	for _, root := range w.roots {
		w.closeTreeLocked(root)
	}
	w.roots = nil
	kq := w.kq
	closePipeWrite := w.closePipe[1]
	w.mu.Unlock()

	// Signal readLoop by writing to the pipe
	unix.Write(closePipeWrite, []byte{0})
	unix.Close(closePipeWrite)

	<-w.exited

	unix.Close(w.closePipe[0])
	err := unix.Close(kq)
	return err
}

func (w *Watcher) openLocked(path string, op Op, parent *kqWatch) (*kqWatch, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|oEvtOnly, 0)
	if err != nil {
		return nil, err
	}
	stat, err := os.Lstat(path)
	if err != nil {
		unix.Close(fd)
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
		unix.Close(fd)
		return nil, err
	}
	w.byFd[fd] = ww
	return ww, nil
}

func (w *Watcher) registerLocked(ww *kqWatch) error {
	var ev unix.Kevent_t
	unix.SetKevent(&ev, ww.fd, unix.EVFILT_VNODE, unix.EV_ADD|unix.EV_CLEAR)
	ev.Fflags = opToNoteFlags(ww.op, ww.isDir)
	_, err := unix.Kevent(w.kq, []unix.Kevent_t{ev}, nil, nil)
	return err
}

func (w *Watcher) closeTreeLocked(ww *kqWatch) {
	for _, c := range ww.children {
		w.closeTreeLocked(c)
	}
	delete(w.byFd, ww.fd)
	unix.Close(ww.fd)
}

func (w *Watcher) readLoop() {
	defer close(w.exited)
	defer close(w.events)
	defer close(w.errors)

	events := make([]unix.Kevent_t, 16)
	for {
		n, err := unix.Kevent(w.kq, nil, events, nil)
		select {
		case <-w.done:
			return
		default:
		}
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if errors.Is(err, unix.EBADF) {
				return
			}
			w.sendError(err)
			return
		}
		for i := 0; i < n; i++ {
			if int(events[i].Ident) == w.closePipe[0] {
				// Close signal via pipe
				return
			}
			w.handleEvent(&events[i])
		}
	}
}

func (w *Watcher) handleEvent(ev *unix.Kevent_t) {
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

	if fflags&unix.NOTE_DELETE != 0 && requested.Has(Remove) {
		w.sendEvent(Event{Name: path, Op: Remove})
	}
	if fflags&unix.NOTE_RENAME != 0 && requested.Has(Rename) {
		w.sendEvent(Event{Name: path, Op: Rename})
	}
	if fflags&unix.NOTE_ATTRIB != 0 && requested.Has(Chmod) {
		w.sendEvent(Event{Name: path, Op: Chmod})
	}
	if fflags&unix.NOTE_WRITE != 0 {
		if isDir {
			w.diffDir(ww, requested)
		} else if requested.Has(Write) {
			w.sendEvent(Event{Name: path, Op: Write})
		}
	}

	if fflags&(unix.NOTE_DELETE|unix.NOTE_RENAME) != 0 {
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
			// mkdir -p (or a populated subtree moved in) lands all the
			// nested entries before NOTE_WRITE reaches us; report them
			// instead of attaching watches silently.
			added = append(added, w.populateChildrenLocked(child, true)...)
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

func opToNoteFlags(op Op, isDir bool) uint32 {
	var f uint32
	if op.Has(Remove) {
		f |= unix.NOTE_DELETE
	}
	if op.Has(Rename) {
		f |= unix.NOTE_RENAME
	}
	if op.Has(Chmod) {
		f |= unix.NOTE_ATTRIB
	}
	// Directory watches always need NOTE_WRITE to detect Create/Remove of children.
	if isDir || op.Has(Write) {
		f |= unix.NOTE_WRITE
	}
	return f
}
