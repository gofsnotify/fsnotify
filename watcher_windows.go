//go:build windows

package fsnotify

import (
	"errors"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"
)

const (
	fileListDirectory        = 1
	fileNotifyChangeSecurity = 0x00000100
	watchBufferSize          = 4096
	// FILE_NOTIFY_INFORMATION layout: 3 x uint32 then a flexible WCHAR[].
	fileNotifyHeaderSize = 12
	// ERROR_INVALID_HANDLE; not exposed by stdlib syscall.
	errorInvalidHandle = syscall.Errno(6)
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
	Events chan Event
	// Errors delivers non-fatal errors from the read loop. Closed when Close returns.
	Errors chan error

	mu      sync.Mutex
	port    syscall.Handle
	watches map[uint32]*winWatch
	nextKey uint32
	closed  bool
	done    chan struct{}
}

type winWatch struct {
	handle     syscall.Handle
	path       string
	op         Op
	mask       uint32
	buf        [watchBufferSize]byte
	overlapped syscall.Overlapped
}

// NewWatcher returns a Watcher backed by ReadDirectoryChangesW.
func NewWatcher() (*Watcher, error) {
	port, err := syscall.CreateIoCompletionPort(syscall.InvalidHandle, 0, 0, 0)
	if err != nil {
		return nil, err
	}
	w := &Watcher{
		Events:  make(chan Event, 64),
		Errors:  make(chan error, 8),
		port:    port,
		watches: make(map[uint32]*winWatch),
		done:    make(chan struct{}),
	}
	go w.readLoop()
	return w, nil
}

// Add registers path with the given event mask. Returns ErrAlreadyAdded
// if path is already registered, or ErrClosed if the watcher is closed.
func (w *Watcher) Add(path string, op Op) error {
	if op == 0 {
		op = All
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	for _, ww := range w.watches {
		if ww.path == abs {
			return ErrAlreadyAdded
		}
	}

	pathPtr, err := syscall.UTF16PtrFromString(abs)
	if err != nil {
		return err
	}
	handle, err := syscall.CreateFile(
		pathPtr,
		fileListDirectory,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OVERLAPPED,
		0,
	)
	if err != nil {
		return err
	}
	w.nextKey++
	key := w.nextKey
	if _, err := syscall.CreateIoCompletionPort(handle, w.port, key, 0); err != nil {
		syscall.CloseHandle(handle)
		return err
	}

	ww := &winWatch{
		handle: handle,
		path:   abs,
		op:     op,
		mask:   opToFilter(op),
	}
	if err := ww.startRead(); err != nil {
		syscall.CloseHandle(handle)
		return err
	}
	w.watches[key] = ww
	return nil
}

// Remove unregisters path. Returns ErrNotAdded if path is not registered.
func (w *Watcher) Remove(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	for k, ww := range w.watches {
		if ww.path == abs {
			delete(w.watches, k)
			return syscall.CloseHandle(ww.handle)
		}
	}
	return ErrNotAdded
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
	for _, ww := range w.watches {
		syscall.CloseHandle(ww.handle)
	}
	w.watches = nil
	port := w.port
	w.mu.Unlock()
	// Wake any GetQueuedCompletionStatus waiter so the loop can observe done.
	syscall.PostQueuedCompletionStatus(port, 0, 0, nil)
	return syscall.CloseHandle(port)
}

func (ww *winWatch) startRead() error {
	return syscall.ReadDirectoryChanges(
		ww.handle,
		&ww.buf[0],
		uint32(len(ww.buf)),
		false,
		ww.mask,
		nil,
		&ww.overlapped,
		0,
	)
}

func (w *Watcher) readLoop() {
	defer close(w.Events)
	defer close(w.Errors)

	for {
		var (
			bytesRead  uint32
			key        uint32
			overlapped *syscall.Overlapped
		)
		err := syscall.GetQueuedCompletionStatus(w.port, &bytesRead, &key, &overlapped, syscall.INFINITE)
		select {
		case <-w.done:
			return
		default:
		}
		if err != nil {
			// Aborted reads come from CloseHandle on a watched directory; not fatal.
			if errors.Is(err, syscall.ERROR_OPERATION_ABORTED) || errors.Is(err, errorInvalidHandle) {
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

func (w *Watcher) handleCompletion(key uint32, n uint32) {
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
			name = syscall.UTF16ToString(unsafe.Slice(ptr, nameLen))
		}

		op := actionToOp(raw.Action, ww.op)
		if op != 0 {
			full := ww.path
			if name != "" {
				full = filepath.Join(ww.path, name)
			}
			select {
			case w.Events <- Event{Name: full, Op: op}:
			case <-w.done:
				return
			}
		}

		if raw.NextEntryOffset == 0 {
			break
		}
		off += raw.NextEntryOffset
	}

	if err := ww.startRead(); err != nil {
		if !errors.Is(err, syscall.ERROR_OPERATION_ABORTED) {
			w.sendError(err)
		}
	}
}

func (w *Watcher) sendError(err error) {
	select {
	case w.Errors <- err:
	case <-w.done:
	}
}

func opToFilter(op Op) uint32 {
	var f uint32
	if op.Has(Create) || op.Has(Remove) || op.Has(Rename) {
		f |= syscall.FILE_NOTIFY_CHANGE_FILE_NAME | syscall.FILE_NOTIFY_CHANGE_DIR_NAME
	}
	if op.Has(Write) {
		f |= syscall.FILE_NOTIFY_CHANGE_LAST_WRITE | syscall.FILE_NOTIFY_CHANGE_SIZE
	}
	if op.Has(Chmod) {
		f |= syscall.FILE_NOTIFY_CHANGE_ATTRIBUTES | fileNotifyChangeSecurity
	}
	return f
}

// actionToOp maps a Win32 action code to an Op, biased by the user's
// requested mask. FILE_ACTION_MODIFIED is ambiguous (size, last-write,
// or attribute change), so the user's request decides whether to
// surface it as Write or Chmod.
func actionToOp(action uint32, requested Op) Op {
	switch action {
	case syscall.FILE_ACTION_ADDED, syscall.FILE_ACTION_RENAMED_NEW_NAME:
		if requested.Has(Create) {
			return Create
		}
	case syscall.FILE_ACTION_REMOVED:
		if requested.Has(Remove) {
			return Remove
		}
	case syscall.FILE_ACTION_MODIFIED:
		if requested.Has(Write) {
			return Write
		}
		if requested.Has(Chmod) {
			return Chmod
		}
	case syscall.FILE_ACTION_RENAMED_OLD_NAME:
		if requested.Has(Rename) {
			return Rename
		}
	}
	return 0
}
