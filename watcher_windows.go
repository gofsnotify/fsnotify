//go:build windows

package fswatcher

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	fileListDirectory        = 1
	fileNotifyChangeSecurity = 0x00000100
	watchBufferSize          = 4096
	// FILE_NOTIFY_INFORMATION layout: 3 x uint32 then a flexible WCHAR[].
	fileNotifyHeaderSize = 12
	// ERROR_INVALID_HANDLE; not exposed by stdlib syscall.
	errorInvalidHandle = windows.Errno(6)
)

// fileNotifyInformation mirrors the Win32 FILE_NOTIFY_INFORMATION struct
// since stdlib syscall does not expose it.
type fileNotifyInformation struct {
	NextEntryOffset uint32
	Action          uint32
	FileNameLength  uint32
	FileName        [1]uint16
}

// Watcher monitors registered paths via ReadDirectoryChangesW.
type Watcher struct {
	// Events delivers change notifications. Closed when Close returns.
	Events <-chan Event
	// Errors delivers non-fatal errors from the read loop. Closed when Close returns.
	Errors <-chan error

	events chan<- Event
	errors chan<- error

	mu      sync.Mutex
	port    windows.Handle
	watches map[uintptr]*winWatch
	nextKey uintptr
	closed  bool
	done    chan struct{}
	exited  chan struct{}
}

type winWatch struct {
	handle windows.Handle
	path   string
	op     Op
	mask   uint32
	// buf must stay DWORD-aligned because ReadDirectoryChangesW dereferences
	// it with that constraint; placing other fields before it can shift the
	// offset on 64-bit Go and silently break the call. Keep buf early and
	// push optional fields below overlapped.
	buf        [watchBufferSize]byte
	overlapped windows.Overlapped
	recursive  bool
}

// NewWatcher returns a Watcher backed by ReadDirectoryChangesW.
func NewWatcher() (*Watcher, error) {
	port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 0)
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
		port:    port,
		watches: make(map[uintptr]*winWatch),
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

// AddRecursive registers path and every directory below it. Backed by
// ReadDirectoryChangesW with bWatchSubtree, so new and removed
// subdirectories are tracked by the kernel. Returns ErrAlreadyAdded
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
		return fmt.Errorf("fswatcher: add %s: %w", path, err)
	}
	cmpKey := pathKey(abs)

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	for _, ww := range w.watches {
		if pathKey(ww.path) == cmpKey {
			return ErrAlreadyAdded
		}
	}

	pathPtr, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return fmt.Errorf("fswatcher: add %s: %w", abs, err)
	}
	handle, err := windows.CreateFile(
		pathPtr,
		fileListDirectory,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED,
		0,
	)
	if err != nil {
		return fmt.Errorf("fswatcher: add %s: %w", abs, err)
	}
	w.nextKey++
	key := w.nextKey
	if _, err := windows.CreateIoCompletionPort(handle, w.port, key, 0); err != nil {
		windows.CloseHandle(handle)
		return fmt.Errorf("fswatcher: add %s: %w", abs, err)
	}

	ww := &winWatch{
		handle:    handle,
		path:      abs,
		op:        op,
		mask:      opToFilter(op),
		recursive: recursive,
	}
	if err := ww.startRead(); err != nil {
		windows.CloseHandle(handle)
		return fmt.Errorf("fswatcher: add %s: %w", abs, err)
	}
	w.watches[key] = ww
	return nil
}

// Remove unregisters path. Returns ErrNotAdded if path is not registered.
func (w *Watcher) Remove(path string) error {
	abs, err := canonicalize(path)
	if err != nil {
		return fmt.Errorf("fswatcher: remove %s: %w", path, err)
	}
	cmpKey := pathKey(abs)

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	for k, ww := range w.watches {
		if pathKey(ww.path) == cmpKey {
			delete(w.watches, k)
			if err := windows.CloseHandle(ww.handle); err != nil {
				return fmt.Errorf("fswatcher: remove %s: %w", abs, err)
			}
			return nil
		}
	}
	return ErrNotAdded
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
	for _, ww := range w.watches {
		windows.CloseHandle(ww.handle)
	}
	w.watches = nil
	port := w.port
	w.mu.Unlock()
	// Wake any GetQueuedCompletionStatus waiter so the loop can observe done.
	windows.PostQueuedCompletionStatus(port, 0, 0, nil)
	err := windows.CloseHandle(port)
	<-w.exited
	return err
}

func (ww *winWatch) startRead() error {
	return windows.ReadDirectoryChanges(
		ww.handle,
		&ww.buf[0],
		uint32(len(ww.buf)),
		ww.recursive,
		ww.mask,
		nil,
		&ww.overlapped,
		0,
	)
}

func (w *Watcher) readLoop() {
	defer close(w.exited)
	defer close(w.events)
	defer close(w.errors)

	for {
		var (
			bytesRead  uint32
			key        uintptr
			overlapped *windows.Overlapped
		)
		err := windows.GetQueuedCompletionStatus(w.port, &bytesRead, &key, &overlapped, windows.INFINITE)
		select {
		case <-w.done:
			return
		default:
		}
		if err != nil {
			// Aborted/invalid-handle completions come from either an
			// explicit Remove (already cleaned up) or the watched
			// directory being deleted out from under us. In the latter
			// case the entry is still in the map, so drop it to release
			// the handle and surface a Remove event for the root.
			if errors.Is(err, windows.ERROR_OPERATION_ABORTED) || errors.Is(err, errorInvalidHandle) {
				w.dropWatch(key)
				continue
			}
			w.sendError(err)
			continue
		}
		if bytesRead == 0 {
			continue
		}
		w.handleCompletion(key, bytesRead)
	}
}

func (w *Watcher) handleCompletion(key uintptr, n uint32) {
	w.mu.Lock()
	ww, ok := w.watches[key]
	w.mu.Unlock()
	if !ok {
		return
	}

	off := uint32(0)
	for off+fileNotifyHeaderSize <= n {
		raw := (*fileNotifyInformation)(unsafe.Pointer(&ww.buf[off]))
		nameStart := off + fileNotifyHeaderSize
		nameEnd := nameStart + raw.FileNameLength
		if nameEnd > uint32(len(ww.buf)) || nameEnd > n {
			break
		}
		nameLen := int(raw.FileNameLength / 2)
		name := ""
		if nameLen > 0 {
			ptr := (*uint16)(unsafe.Pointer(&ww.buf[nameStart]))
			name = windows.UTF16ToString(unsafe.Slice(ptr, nameLen))
		}

		op := actionToOp(raw.Action, ww.op)
		if op != 0 {
			full := ww.path
			if name != "" {
				full = filepath.Join(ww.path, name)
			}
			w.sendEvent(Event{Name: full, Op: op})
		}

		if raw.NextEntryOffset == 0 {
			break
		}
		off += raw.NextEntryOffset
	}

	if err := ww.startRead(); err != nil {
		if !errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
			w.sendError(err)
		}
	}
}

// dropWatch removes the watch entry for key if it is still present and
// closes its handle. Called when ReadDirectoryChangesW completes with
// an aborted/invalid-handle status so a deleted watched directory does
// not leak its winWatch (handle, buffer, overlapped). Also surfaces a
// Remove event for the root path when the user requested Remove and
// the entry was still tracked, matching IN_DELETE_SELF on Linux.
func (w *Watcher) dropWatch(key uintptr) {
	w.mu.Lock()
	ww, ok := w.watches[key]
	if ok {
		delete(w.watches, key)
	}
	w.mu.Unlock()
	if !ok {
		return
	}
	windows.CloseHandle(ww.handle)
	if ww.op.Has(Remove) {
		w.sendEvent(Event{Name: ww.path, Op: Remove})
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

func opToFilter(op Op) uint32 {
	var f uint32
	if op.Has(Create) || op.Has(Remove) || op.Has(Rename) {
		f |= windows.FILE_NOTIFY_CHANGE_FILE_NAME | windows.FILE_NOTIFY_CHANGE_DIR_NAME
	}
	if op.Has(Write) {
		f |= windows.FILE_NOTIFY_CHANGE_LAST_WRITE | windows.FILE_NOTIFY_CHANGE_SIZE
	}
	if op.Has(Chmod) {
		f |= windows.FILE_NOTIFY_CHANGE_ATTRIBUTES | fileNotifyChangeSecurity
	}
	return f
}

// actionToOp maps a Win32 action code to an Op, biased by the user's
// requested mask. FILE_ACTION_MODIFIED is ambiguous (size, last-write,
// or attribute change), so the user's request decides whether to
// surface it as Write or Chmod.
func actionToOp(action uint32, requested Op) Op {
	switch action {
	case windows.FILE_ACTION_ADDED, windows.FILE_ACTION_RENAMED_NEW_NAME:
		if requested.Has(Create) {
			return Create
		}
	case windows.FILE_ACTION_REMOVED:
		if requested.Has(Remove) {
			return Remove
		}
	case windows.FILE_ACTION_MODIFIED:
		if requested.Has(Write) {
			return Write
		}
		if requested.Has(Chmod) {
			return Chmod
		}
	case windows.FILE_ACTION_RENAMED_OLD_NAME:
		if requested.Has(Rename) {
			return Rename
		}
	}
	return 0
}
