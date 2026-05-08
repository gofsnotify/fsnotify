//go:build windows

package fsnotify

import (
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCanonicalizeExpandsShortPath(t *testing.T) {
	// t.TempDir() may already mix 8.3 and long components on CI runners,
	// so fully canonicalize first to get the expected long form.
	long, err := canonicalize(t.TempDir())
	if err != nil {
		t.Fatalf("canonicalize(TempDir): %v", err)
	}

	longPtr, err := syscall.UTF16PtrFromString(long)
	if err != nil {
		t.Fatalf("UTF16PtrFromString: %v", err)
	}
	var buf [syscall.MAX_PATH]uint16
	n, err := syscall.GetShortPathName(longPtr, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 {
		t.Skipf("GetShortPathName unavailable: %v", err)
	}
	short := syscall.UTF16ToString(buf[:n])
	if strings.EqualFold(short, long) {
		t.Skip("temp dir has no distinct 8.3 short form")
	}

	got, err := canonicalize(short)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if !strings.EqualFold(got, long) {
		t.Errorf("canonicalize(%q) = %q, want %q", short, got, long)
	}
}

// TestDropWatchReleasesEntry exercises the readLoop's recovery path for
// an aborted ReadDirectoryChangesW completion (e.g. a watched drive is
// ejected or the directory is force-deleted out from under the
// watcher). Calling dropWatch must remove the map entry, close the
// handle, and emit a Remove event so callers learn the root is gone.
func TestDropWatchReleasesEntry(t *testing.T) {
	dir := tempDir(t)
	w := newWatcher(t)
	if err := w.Add(dir, Remove); err != nil {
		t.Fatalf("Add: %v", err)
	}

	var key uintptr
	w.mu.Lock()
	for k := range w.watches {
		key = k
	}
	w.mu.Unlock()
	if key == 0 {
		t.Fatalf("no watch registered")
	}

	w.dropWatch(key)

	w.mu.Lock()
	_, present := w.watches[key]
	w.mu.Unlock()
	if present {
		t.Fatalf("watch entry not removed after dropWatch")
	}

	select {
	case ev, ok := <-w.Events:
		if !ok {
			t.Fatalf("Events closed before Remove arrived")
		}
		if ev.Op != Remove || ev.Name != dir {
			t.Errorf("event = %+v, want Remove %q", ev, dir)
		}
	case <-time.After(eventTimeout):
		t.Fatalf("timeout waiting for Remove event")
	}
}
