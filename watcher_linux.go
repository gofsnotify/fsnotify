//go:build linux

package fsnotify

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// Watcher monitors registered paths for file system changes via inotify.
type Watcher struct {
	// Events delivers change notifications. Closed when Close returns.
	Events chan Event
	// Errors delivers non-fatal errors from the read loop. Closed when Close returns.
	Errors chan error

	mu      sync.Mutex
	fd      int
	watches map[string]*linuxWatch
	wdToKey map[int32]string
	closed  bool
	done    chan struct{}
}

type linuxWatch struct {
	abs string
	wd  int32
	op  Op
}

// NewWatcher returns a Watcher backed by Linux inotify.
func NewWatcher() (*Watcher, error) {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC)
	if err != nil {
		return nil, err
	}
	w := &Watcher{
		Events:  make(chan Event, 64),
		Errors:  make(chan error, 8),
		fd:      fd,
		watches: make(map[string]*linuxWatch),
		wdToKey: make(map[int32]string),
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
	if _, ok := w.watches[key]; ok {
		return ErrAlreadyAdded
	}
	wd, err := syscall.InotifyAddWatch(w.fd, abs, opToMask(op))
	if err != nil {
		return fmt.Errorf("fsnotify: add %s: %w", abs, err)
	}
	wd32 := int32(wd)
	w.watches[key] = &linuxWatch{abs: abs, wd: wd32, op: op}
	w.wdToKey[wd32] = key
	return nil
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
	lw, ok := w.watches[key]
	if !ok {
		return ErrNotAdded
	}
	if _, err := syscall.InotifyRmWatch(w.fd, uint32(lw.wd)); err != nil {
		return fmt.Errorf("fsnotify: remove %s: %w", abs, err)
	}
	delete(w.watches, key)
	delete(w.wdToKey, lw.wd)
	return nil
}

// Close stops the watcher. Subsequent calls are no-ops. The Events and
// Errors channels are closed once the read loop exits.
func (w *Watcher) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	close(w.done)
	fd := w.fd
	w.mu.Unlock()
	return syscall.Close(fd)
}

func (w *Watcher) readLoop() {
	defer close(w.Events)
	defer close(w.Errors)

	var buf [4096]byte
	for {
		n, err := syscall.Read(w.fd, buf[:])
		if err != nil {
			if errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.EINTR) {
				select {
				case <-w.done:
					return
				default:
				}
				if errors.Is(err, syscall.EINTR) {
					continue
				}
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
	if ok {
		lw = w.watches[key]
	}
	w.mu.Unlock()
	if lw == nil {
		return
	}

	op := maskToOp(mask) & lw.op
	if op == 0 {
		return
	}

	full := lw.abs
	if name != "" {
		full = filepath.Join(lw.abs, name)
	}
	select {
	case w.Events <- Event{Name: full, Op: op}:
	case <-w.done:
	}
}

func (w *Watcher) sendError(err error) {
	select {
	case w.Errors <- err:
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
