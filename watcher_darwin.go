//go:build darwin

package fsnotify

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
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
)

// Create flags.
const (
	fseCreateNoDefer    = 0x02
	fseCreateWatchRoot  = 0x04
	fseCreateFileEvents = 0x10
)

const (
	kCFStringEncodingUTF8          = 0x08000100
	kFSEventStreamEventIdSinceNow  = ^uint64(0)
	defaultLatency                 = 0.01 // 10ms
)

// fsEventStreamContext mirrors the C FSEventStreamContext struct layout.
type fsEventStreamContext struct {
	Version         int64   // CFIndex = long on 64-bit
	Info            uintptr // void* — our watcher ID
	Retain          uintptr // NULL
	Release         uintptr // NULL
	CopyDescription uintptr // NULL
}

// CoreFoundation / CoreServices / libdispatch functions loaded via purego.
var (
	_cfStringCreateWithCString func(alloc uintptr, cStr string, encoding uint32) uintptr
	_cfArrayCreateMutable      func(alloc uintptr, capacity int64, callbacks uintptr) uintptr
	_cfArrayAppendValue        func(arr uintptr, value uintptr)
	_cfRelease                 func(ref uintptr)

	_fseStreamCreate          func(alloc uintptr, callback uintptr, ctx *fsEventStreamContext, paths uintptr, sinceWhen uint64, latency float64, flags uint32) uintptr
	_fseStreamSetDispatchQueue func(stream uintptr, queue uintptr)
	_fseStreamStart            func(stream uintptr) uintptr
	_fseStreamStop             func(stream uintptr)
	_fseStreamInvalidate       func(stream uintptr)
	_fseStreamRelease          func(stream uintptr)

	_dispatchQueueCreate func(label string, attr uintptr) uintptr
	_dispatchRelease     func(queue uintptr)

	// kCFTypeArrayCallBacks symbol address
	cfTypeArrayCallBacks uintptr
)

var (
	fseInitOnce sync.Once
	fseInitErr  error
)

func initFSEvents() error {
	fseInitOnce.Do(func() {
		fseInitErr = doInitFSEvents()
	})
	return fseInitErr
}

func doInitFSEvents() error {
	cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		return fmt.Errorf("fsnotify: load CoreFoundation: %w", err)
	}
	cs, err := purego.Dlopen("/System/Library/Frameworks/CoreServices.framework/CoreServices", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		return fmt.Errorf("fsnotify: load CoreServices: %w", err)
	}
	ls, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		return fmt.Errorf("fsnotify: load libSystem: %w", err)
	}

	purego.RegisterLibFunc(&_cfStringCreateWithCString, cf, "CFStringCreateWithCString")
	purego.RegisterLibFunc(&_cfArrayCreateMutable, cf, "CFArrayCreateMutable")
	purego.RegisterLibFunc(&_cfArrayAppendValue, cf, "CFArrayAppendValue")
	purego.RegisterLibFunc(&_cfRelease, cf, "CFRelease")

	purego.RegisterLibFunc(&_fseStreamCreate, cs, "FSEventStreamCreate")
	purego.RegisterLibFunc(&_fseStreamSetDispatchQueue, cs, "FSEventStreamSetDispatchQueue")
	purego.RegisterLibFunc(&_fseStreamStart, cs, "FSEventStreamStart")
	purego.RegisterLibFunc(&_fseStreamStop, cs, "FSEventStreamStop")
	purego.RegisterLibFunc(&_fseStreamInvalidate, cs, "FSEventStreamInvalidate")
	purego.RegisterLibFunc(&_fseStreamRelease, cs, "FSEventStreamRelease")

	purego.RegisterLibFunc(&_dispatchQueueCreate, ls, "dispatch_queue_create")
	purego.RegisterLibFunc(&_dispatchRelease, ls, "dispatch_release")

	sym, err := purego.Dlsym(cf, "kCFTypeArrayCallBacks")
	if err != nil {
		return fmt.Errorf("fsnotify: lookup kCFTypeArrayCallBacks: %w", err)
	}
	cfTypeArrayCallBacks = sym

	return nil
}

// fseReg holds a snapshot of a registered stream's configuration, used
// inside the callback to match events without holding the watcher lock.
type fseReg struct {
	path      string
	op        Op
	recursive bool
}

// fsStream represents a single FSEventStream for one Add/AddRecursive call.
type fsStream struct {
	stream    uintptr // FSEventStreamRef
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
	queue    uintptr // dispatch_queue_t
	streams  map[string]*fsStream
	cleanupW sync.WaitGroup
	internal chan Event
	closed   bool
	done     chan struct{}
	exited   chan struct{}
}

// Global registry maps watcher IDs to watchers so the callback
// can find the correct Go object.
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

// fseCallback is the single global FSEvents callback function pointer.
// All watchers share it; the clientInfo parameter identifies the watcher.
var fseCallback = purego.NewCallback(func(
	streamRef uintptr,
	clientInfo uintptr,
	numEvents uintptr,
	pathsPtr unsafe.Pointer,
	flagsPtr unsafe.Pointer,
	idsPtr unsafe.Pointer,
) {
	handleFSEventsCallback(clientInfo, int(numEvents), pathsPtr, flagsPtr)
})

// NewWatcher returns a Watcher backed by macOS FSEvents.
func NewWatcher() (*Watcher, error) {
	if err := initFSEvents(); err != nil {
		return nil, err
	}

	queue := _dispatchQueueCreate("github.com/gofsnotify/fsnotify\x00", 0)

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

func (w *Watcher) createStreamLocked(path string) (uintptr, error) {
	cfPath := _cfStringCreateWithCString(0, path, kCFStringEncodingUTF8)
	defer _cfRelease(cfPath)

	pathArray := _cfArrayCreateMutable(0, 1, cfTypeArrayCallBacks)
	defer _cfRelease(pathArray)
	_cfArrayAppendValue(pathArray, cfPath)

	ctx := fsEventStreamContext{Info: w.id}
	flags := uint32(fseCreateFileEvents | fseCreateNoDefer | fseCreateWatchRoot)

	stream := _fseStreamCreate(
		0,
		fseCallback,
		&ctx,
		pathArray,
		kFSEventStreamEventIdSinceNow,
		defaultLatency,
		flags,
	)
	if stream == 0 {
		return 0, fmt.Errorf("FSEventStreamCreate failed")
	}

	_fseStreamSetDispatchQueue(stream, w.queue)
	if _fseStreamStart(stream) == 0 {
		_fseStreamInvalidate(stream)
		_fseStreamRelease(stream)
		return 0, fmt.Errorf("FSEventStreamStart failed")
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
	close(w.done)

	streams := make([]uintptr, 0, len(w.streams))
	for _, fs := range w.streams {
		streams = append(streams, fs.stream)
	}
	w.streams = nil
	queue := w.queue
	id := w.id
	w.mu.Unlock()

	for _, s := range streams {
		stopStream(s)
	}

	w.cleanupW.Wait()
	<-w.exited
	_dispatchRelease(queue)
	unregisterWatcher(id)
	return nil
}

func stopStream(stream uintptr) {
	_fseStreamStop(stream)
	_fseStreamInvalidate(stream)
	_fseStreamRelease(stream)
}

// handleFSEventsCallback processes a batch of FSEvents notifications.
func handleFSEventsCallback(clientInfo uintptr, n int, pathsPtr, flagsPtr unsafe.Pointer) {
	w := lookupWatcher(clientInfo)
	if w == nil {
		return
	}

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

	ptrSize := unsafe.Sizeof(uintptr(0))

	for i := 0; i < n; i++ {
		// Read char* from the paths array (char**) without uintptr→unsafe.Pointer.
		cStr := *(*unsafe.Pointer)(unsafe.Add(pathsPtr, uintptr(i)*ptrSize))
		p := goString(cStr)
		f := *(*uint32)(unsafe.Add(flagsPtr, uintptr(i)*4))

		if abs, err := canonicalize(p); err == nil {
			p = abs
		}

		if f&fseMustScanSubs != 0 {
			w.sendError(fmt.Errorf("fsnotify: events may have been dropped for %s", p))
		}

		r, ok := matchRegistration(p, regs)
		if !ok {
			continue
		}

		// Handle RootChanged before the depth filter since RootChanged
		// events always target the watched root itself (p == r.path).
		if f&fseRootChanged != 0 {
			w.mu.Lock()
			if !w.closed {
				key := pathKey(r.path)
				if fs, exists := w.streams[key]; exists {
					delete(w.streams, key)
					w.cleanupW.Add(1)
					go func() {
						defer w.cleanupW.Done()
						stopStream(fs.stream)
					}()
				}
			}
			w.mu.Unlock()

			op := fseventFlagsToOp(f) & r.op
			if op == 0 && r.op.Has(Remove) {
				op = Remove
			}
			if op != 0 {
				w.sendEvent(Event{Name: r.path, Op: op})
			}
			continue
		}

		// Suppress events for the watched root — its metadata changes
		// are noise. Child events pass through.
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
// that covers the event path p.
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

// goString reads a null-terminated C string from ptr without cgo.
func goString(p unsafe.Pointer) string {
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(p, n)) != 0 {
		n++
	}
	b := make([]byte, n)
	copy(b, unsafe.Slice((*byte)(p), n))
	return string(b)
}
