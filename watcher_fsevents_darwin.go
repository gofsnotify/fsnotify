//go:build darwin && cgo

package fsnotify

/*
#cgo LDFLAGS: -framework CoreServices
#include <CoreServices/CoreServices.h>
#include <dispatch/dispatch.h>

// Forward-declare the Go callback so the C trampoline can call it.
extern void goFSEventsCallback(
	uintptr_t info,
	size_t numEvents,
	char **paths,
	FSEventStreamEventFlags *flags,
	FSEventStreamEventId *ids);

// bridgeCallback is a static C function passed to FSEventStreamCreate.
// It casts the opaque arguments into typed pointers and forwards them to Go.
static void bridgeCallback(
	ConstFSEventStreamRef ref,
	void *info,
	size_t numEvents,
	void *eventPaths,
	const FSEventStreamEventFlags eventFlags[],
	const FSEventStreamEventId eventIds[])
{
	goFSEventsCallback(
		(uintptr_t)info,
		numEvents,
		(char **)eventPaths,
		(FSEventStreamEventFlags *)eventFlags,
		(FSEventStreamEventId *)eventIds);
}

// createStream wraps FSEventStreamCreate so Go doesn't need to build a
// C function pointer from an //export symbol (which cgo disallows).
static FSEventStreamRef createStream(
	uintptr_t info,
	CFArrayRef paths,
	FSEventStreamEventId sinceWhen,
	CFTimeInterval latency,
	FSEventStreamCreateFlags flags)
{
	FSEventStreamContext ctx = {0, (void *)info, NULL, NULL, NULL};
	return FSEventStreamCreate(
		NULL,
		bridgeCallback,
		&ctx,
		paths,
		sinceWhen,
		latency,
		flags);
}

// releaseQueue wraps dispatch_release so it can be called from Go
// without needing a cast to dispatch_object_t.
static void releaseQueue(dispatch_queue_t q) {
	dispatch_release(q);
}

// cfArrayAppendCFString appends a CFStringRef to a CFMutableArrayRef,
// avoiding an unsafe.Pointer cast that triggers go vet warnings.
static void cfArrayAppendCFString(CFMutableArrayRef a, CFStringRef s) {
	CFArrayAppendValue(a, s);
}
*/
import "C"

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"
)

// Stream-level event flags.
const (
	fseRootChanged  = 0x00000020
	fseMustScanSubs = 0x00000001
)

// File-level event flags (require kFSEventStreamCreateFlagFileEvents).
const (
	fseItemCreated      = 0x00000100
	fseItemRemoved      = 0x00000200
	fseItemInodeMetaMod = 0x00000400
	fseItemRenamed      = 0x00000800
	fseItemModified     = 0x00001000
	fseItemChangeOwner  = 0x00004000
	fseItemXattrMod     = 0x00008000
	fseItemIsFile       = 0x00010000
	fseItemIsDir        = 0x00020000
	fseItemIsSymlink    = 0x00040000
)

// Create flags.
const (
	fseCreateNoDefer    = 0x02
	fseCreateWatchRoot  = 0x04
	fseCreateFileEvents = 0x10
)

const defaultLatency = 0.01 // 10ms

// fseReg holds a snapshot of a registered stream's configuration, used
// inside the callback to match events without holding the watcher lock.
type fseReg struct {
	path      string
	op        Op
	recursive bool
}

// fsStream represents a single FSEventStream for one Add/AddRecursive call.
type fsStream struct {
	stream    C.FSEventStreamRef
	path      string
	op        Op
	recursive bool
}

// Watcher monitors registered paths via macOS FSEvents.
type Watcher struct {
	// Events delivers change notifications. Closed when Close returns.
	Events chan Event
	// Errors delivers non-fatal errors from the read loop. Closed when Close returns.
	Errors chan error

	mu       sync.Mutex
	id       uintptr
	queue    C.dispatch_queue_t
	streams  map[string]*fsStream // keyed by pathKey
	cleanupW sync.WaitGroup       // tracks async stream cleanup goroutines
	internal chan Event
	closed   bool
	done     chan struct{}
	exited   chan struct{}
}

// Global registry maps watcher IDs to watchers so the C callback
// can find the correct Go object. Access is serialised by registryMu.
var (
	registryMu sync.Mutex
	registry   = map[uintptr]*Watcher{}
	nextID     uintptr
)

func registerWatcher(w *Watcher) uintptr {
	registryMu.Lock()
	defer registryMu.Unlock()
	nextID++
	registry[nextID] = w
	return nextID
}

func unregisterWatcher(id uintptr) {
	registryMu.Lock()
	defer registryMu.Unlock()
	delete(registry, id)
}

func lookupWatcher(id uintptr) *Watcher {
	registryMu.Lock()
	defer registryMu.Unlock()
	return registry[id]
}

// NewWatcher returns a Watcher backed by macOS FSEvents.
func NewWatcher() (*Watcher, error) {
	label := C.CString("github.com/gofsnotify/fsnotify")
	defer C.free(unsafe.Pointer(label))
	queue := C.dispatch_queue_create(label, nil) // serial queue

	w := &Watcher{
		Events:   make(chan Event, 64),
		Errors:   make(chan error, 8),
		queue:    queue,
		streams:  make(map[string]*fsStream),
		internal: make(chan Event, 256),
		done:     make(chan struct{}),
		exited:   make(chan struct{}),
	}
	w.id = registerWatcher(w)
	go w.readLoop()
	return w, nil
}

// readLoop drains the internal channel and forwards events to the
// public Events channel. It is the sole goroutine that closes Events,
// Errors, and exited, matching the pattern of the other backends.
func (w *Watcher) readLoop() {
	defer close(w.exited)
	defer close(w.Events)
	defer close(w.Errors)

	for {
		select {
		case ev := <-w.internal:
			select {
			case w.Events <- ev:
			case <-w.done:
				return
			}
		case <-w.done:
			return
		}
	}
}

// Add registers path with the given event mask. Returns ErrAlreadyAdded
// if path is already registered, or ErrClosed if the watcher is closed.
func (w *Watcher) Add(path string, op Op) error {
	return w.add(path, op, false)
}

// AddRecursive registers path and every directory below it. FSEvents
// natively supports recursive monitoring so no manual walk is needed.
// Returns ErrAlreadyAdded if path is already registered.
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
	if _, exists := w.streams[key]; exists {
		return ErrAlreadyAdded
	}

	stream, err := w.createStreamLocked(abs)
	if err != nil {
		return fmt.Errorf("fsnotify: add %s: %w", abs, err)
	}
	w.streams[key] = &fsStream{
		stream:    stream,
		path:      abs,
		op:        op,
		recursive: recursive,
	}
	return nil
}

func (w *Watcher) createStreamLocked(path string) (C.FSEventStreamRef, error) {
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	cfPath := C.CFStringCreateWithCString(0, cPath, C.kCFStringEncodingUTF8)
	defer C.CFRelease(C.CFTypeRef(cfPath))

	pathArray := C.CFArrayCreateMutable(0, 1, &C.kCFTypeArrayCallBacks)
	defer C.CFRelease(C.CFTypeRef(pathArray))
	C.cfArrayAppendCFString(pathArray, cfPath)

	flags := C.FSEventStreamCreateFlags(
		fseCreateFileEvents | fseCreateNoDefer | fseCreateWatchRoot,
	)

	stream := C.createStream(
		C.uintptr_t(w.id),
		C.CFArrayRef(pathArray),
		C.FSEventStreamEventId(C.kFSEventStreamEventIdSinceNow),
		C.CFTimeInterval(defaultLatency),
		flags,
	)
	if stream == nil {
		return nil, fmt.Errorf("FSEventStreamCreate failed")
	}

	C.FSEventStreamSetDispatchQueue(stream, w.queue)
	if C.FSEventStreamStart(stream) == 0 {
		C.FSEventStreamInvalidate(stream)
		C.FSEventStreamRelease(stream)
		return nil, fmt.Errorf("FSEventStreamStart failed")
	}
	return stream, nil
}

// Remove unregisters path. Returns ErrNotAdded if path is not registered.
func (w *Watcher) Remove(path string) error {
	abs, err := canonicalize(path)
	if err != nil {
		return fmt.Errorf("fsnotify: remove %s: %w", path, err)
	}
	key := pathKey(abs)

	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return ErrClosed
	}
	fs, ok := w.streams[key]
	if !ok {
		w.mu.Unlock()
		return ErrNotAdded
	}
	delete(w.streams, key)
	stream := fs.stream
	w.mu.Unlock()

	// Stop outside the lock to avoid deadlocking with a callback that
	// needs w.mu.
	stopStream(stream)
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
	// Close done first so in-flight callbacks' sendEvent calls unblock.
	close(w.done)

	// Collect streams to stop; clear the map while holding the lock.
	streams := make([]C.FSEventStreamRef, 0, len(w.streams))
	for _, fs := range w.streams {
		streams = append(streams, fs.stream)
	}
	w.streams = nil
	queue := w.queue
	id := w.id
	w.mu.Unlock()

	// Stop streams outside the lock.
	for _, s := range streams {
		stopStream(s)
	}

	// Wait for any async root-change cleanup goroutines.
	w.cleanupW.Wait()

	<-w.exited
	C.releaseQueue(queue)
	unregisterWatcher(id)
	return nil
}

func stopStream(stream C.FSEventStreamRef) {
	C.FSEventStreamStop(stream)
	C.FSEventStreamInvalidate(stream)
	C.FSEventStreamRelease(stream)
}

//export goFSEventsCallback
func goFSEventsCallback(
	info C.uintptr_t,
	numEvents C.size_t,
	cpaths **C.char,
	cflags *C.FSEventStreamEventFlags,
	cids *C.FSEventStreamEventId,
) {
	w := lookupWatcher(uintptr(info))
	if w == nil {
		return
	}

	n := int(numEvents)
	paths := unsafe.Slice(cpaths, n)
	flags := unsafe.Slice(cflags, n)
	_ = unsafe.Slice(cids, n) // ids unused for now

	w.mu.Lock()
	closed := w.closed
	regs := make([]fseReg, 0, len(w.streams))
	for _, fs := range w.streams {
		regs = append(regs, fseReg{
			path:      fs.path,
			op:        fs.op,
			recursive: fs.recursive,
		})
	}
	w.mu.Unlock()
	if closed {
		return
	}

	for i := 0; i < n; i++ {
		p := C.GoString(paths[i])
		f := uint32(flags[i])

		// Canonicalize the event path for consistent matching.
		if abs, err := canonicalize(p); err == nil {
			p = abs
		}

		// Handle MustScanSubDirs: events were coalesced/dropped.
		if f&fseMustScanSubs != 0 {
			w.sendError(fmt.Errorf("fsnotify: events may have been dropped for %s", p))
		}

		// Find the most specific registration covering this event.
		r, ok := matchRegistration(p, regs)
		if !ok {
			continue
		}

		// Handle RootChanged: the watched path was deleted/renamed.
		// Must be checked before the depth filter since RootChanged events
		// always target the watched root itself (p == r.path).
		if f&fseRootChanged != 0 {
			w.mu.Lock()
			if !w.closed {
				key := pathKey(r.path)
				if fs, exists := w.streams[key]; exists {
					delete(w.streams, key)
					// Cannot call stopStream here (would deadlock on the
					// serial dispatch queue). Schedule cleanup async.
					w.cleanupW.Add(1)
					go func() {
						defer w.cleanupW.Done()
						stopStream(fs.stream)
					}()
				}
			}
			w.mu.Unlock()

			// Derive op from item-level flags when available.
			op := fseventFlagsToOp(f) & r.op
			if op == 0 && r.op.Has(Remove) {
				op = Remove
			}
			if op != 0 {
				w.sendEvent(Event{Name: r.path, Op: op})
			}
			continue
		}

		// For non-recursive watches, only emit events for direct children
		// of the watched directory (not the directory itself — its own
		// metadata changes are noise, matching kqueue behaviour).
		// For recursive watches, suppress events for the root path itself
		// for the same reason; child directories still pass through.
		if p == r.path {
			continue
		}
		if !r.recursive {
			rel, err := filepath.Rel(r.path, p)
			if err != nil || strings.ContainsRune(rel, filepath.Separator) {
				continue
			}
		}

		op := fseventFlagsToOp(f) & r.op
		if op == 0 {
			continue
		}
		w.sendEvent(Event{Name: p, Op: op})
	}
}

// matchRegistration finds the most specific (longest path) registration
// that covers the event path p. This ensures that nested registrations
// (e.g. /root and /root/sub) are resolved correctly.
func matchRegistration(p string, regs []fseReg) (fseReg, bool) {
	pk := pathKey(p)
	var best fseReg
	found := false
	for _, r := range regs {
		rk := pathKey(r.path)
		if pk == rk || isUnder(pk, rk) {
			if !found || len(rk) > len(pathKey(best.path)) {
				best = r
				found = true
			}
		}
	}
	return best, found
}

// isUnder reports whether child is a path under parent.
func isUnder(child, parent string) bool {
	if parent == "/" {
		return true
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}

func (w *Watcher) sendEvent(e Event) {
	select {
	case w.internal <- e:
	case <-w.done:
	}
}

func (w *Watcher) sendError(err error) {
	select {
	case w.Errors <- err:
	case <-w.done:
	}
}

func fseventFlagsToOp(f uint32) Op {
	var op Op
	if f&fseItemCreated != 0 {
		op |= Create
	}
	if f&fseItemModified != 0 {
		op |= Write
	}
	if f&fseItemRemoved != 0 {
		op |= Remove
	}
	if f&fseItemRenamed != 0 {
		op |= Rename
	}
	if f&(fseItemChangeOwner|fseItemInodeMetaMod|fseItemXattrMod) != 0 {
		op |= Chmod
	}
	return op
}
