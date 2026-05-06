package fsnotify

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const eventTimeout = 10 * time.Second

// tempDir returns t.TempDir() routed through canonicalize so paths
// emitted by the watcher (which canonicalizes Add input) compare cleanly
// against expected values regardless of platform-specific symlinks
// (/var → /private/var on macOS) or 8.3 short forms on Windows.
func tempDir(t *testing.T) string {
	t.Helper()
	d, err := canonicalize(t.TempDir())
	if err != nil {
		t.Fatalf("canonicalize TempDir: %v", err)
	}
	return d
}

func newWatcher(t *testing.T) *Watcher {
	t.Helper()
	w, err := NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func waitOp(t *testing.T, w *Watcher, want Op) Event {
	t.Helper()
	deadline := time.NewTimer(eventTimeout)
	defer deadline.Stop()
	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				t.Fatalf("Events channel closed while waiting for %s", want)
			}
			if ev.Op.Has(want) {
				return ev
			}
		case err := <-w.Errors:
			t.Fatalf("unexpected error: %v", err)
		case <-deadline.C:
			t.Fatalf("timeout waiting for %s", want)
		}
	}
}

func TestWatchCreate(t *testing.T) {
	dir := tempDir(t)
	w := newWatcher(t)
	if err := w.Add(dir, Create); err != nil {
		t.Fatalf("Add: %v", err)
	}

	target := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(target, []byte("hi"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ev := waitOp(t, w, Create)
	if ev.Name != target {
		t.Errorf("Name = %q, want %q", ev.Name, target)
	}
}

func TestWatchWrite(t *testing.T) {
	dir := tempDir(t)
	target := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	w := newWatcher(t)
	if err := w.Add(dir, Write); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := os.WriteFile(target, []byte("changed"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ev := waitOp(t, w, Write)
	if ev.Name != target {
		t.Errorf("Name = %q, want %q", ev.Name, target)
	}
}

func TestWatchRemove(t *testing.T) {
	dir := tempDir(t)
	target := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	w := newWatcher(t)
	if err := w.Add(dir, Remove); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := os.Remove(target); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	ev := waitOp(t, w, Remove)
	if ev.Name != target {
		t.Errorf("Name = %q, want %q", ev.Name, target)
	}
}

func TestAddDuplicate(t *testing.T) {
	dir := tempDir(t)
	w := newWatcher(t)
	if err := w.Add(dir, All); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := w.Add(dir, All); !errors.Is(err, ErrAlreadyAdded) {
		t.Fatalf("Add(dup) = %v, want ErrAlreadyAdded", err)
	}
}

func TestRemoveUnregistered(t *testing.T) {
	dir := tempDir(t)
	w := newWatcher(t)
	if err := w.Remove(dir); !errors.Is(err, ErrNotAdded) {
		t.Fatalf("Remove(missing) = %v, want ErrNotAdded", err)
	}
}

func TestClosedWatcher(t *testing.T) {
	w, err := NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close (idempotent): %v", err)
	}
	if err := w.Add(t.TempDir(), All); !errors.Is(err, ErrClosed) {
		t.Fatalf("Add after Close = %v, want ErrClosed", err)
	}
	if err := w.Remove("/tmp"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Remove after Close = %v, want ErrClosed", err)
	}

	deadline := time.NewTimer(eventTimeout)
	defer deadline.Stop()
	select {
	case _, ok := <-w.Events:
		if ok {
			t.Fatalf("Events channel should be closed")
		}
	case <-deadline.C:
		t.Fatalf("Events channel not closed after Close")
	}
}

func TestOpFilterIgnoresOthers(t *testing.T) {
	dir := tempDir(t)
	w := newWatcher(t)
	if err := w.Add(dir, Create); err != nil {
		t.Fatalf("Add: %v", err)
	}

	target := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	waitOp(t, w, Create)

	if err := os.WriteFile(target, []byte("y"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	timer := time.NewTimer(300 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case ev := <-w.Events:
			if ev.Op&^Create != 0 {
				t.Fatalf("got disallowed event %s", ev)
			}
		case <-timer.C:
			return
		}
	}
}

func TestWatchRename(t *testing.T) {
	dir := tempDir(t)
	oldPath := filepath.Join(dir, "old.txt")
	newPath := filepath.Join(dir, "new.txt")
	if err := os.WriteFile(oldPath, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	w := newWatcher(t)
	if err := w.Add(dir, Rename); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	ev := waitOp(t, w, Rename)
	if ev.Name != oldPath {
		t.Errorf("Name = %q, want %q", ev.Name, oldPath)
	}
}

func TestWatchChmod(t *testing.T) {
	dir := tempDir(t)
	target := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	w := newWatcher(t)
	if err := w.Add(dir, Chmod); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// 0o400 forces the read-only attribute on Windows so the change
	// is observable; on Unix it is a real permission flip.
	if err := os.Chmod(target, 0o400); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	ev := waitOp(t, w, Chmod)
	if ev.Name != target {
		t.Errorf("Name = %q, want %q", ev.Name, target)
	}
}

func TestMultiplePaths(t *testing.T) {
	dirA := tempDir(t)
	dirB := tempDir(t)

	w := newWatcher(t)
	if err := w.Add(dirA, Create); err != nil {
		t.Fatalf("Add A: %v", err)
	}
	if err := w.Add(dirB, Create); err != nil {
		t.Fatalf("Add B: %v", err)
	}

	fileA := filepath.Join(dirA, "a.txt")
	fileB := filepath.Join(dirB, "b.txt")
	if err := os.WriteFile(fileA, nil, 0o644); err != nil {
		t.Fatalf("WriteFile A: %v", err)
	}
	if err := os.WriteFile(fileB, nil, 0o644); err != nil {
		t.Fatalf("WriteFile B: %v", err)
	}

	got := map[string]bool{}
	deadline := time.NewTimer(eventTimeout)
	defer deadline.Stop()
	for len(got) < 2 {
		select {
		case ev := <-w.Events:
			if ev.Op.Has(Create) {
				got[ev.Name] = true
			}
		case <-deadline.C:
			t.Fatalf("timeout: got %v", got)
		}
	}
	if !got[fileA] || !got[fileB] {
		t.Errorf("missing events: got %v", got)
	}
}

func TestRemoveStopsEvents(t *testing.T) {
	dir := tempDir(t)
	w := newWatcher(t)
	if err := w.Add(dir, Create); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := w.Remove(dir); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	timer := time.NewTimer(300 * time.Millisecond)
	defer timer.Stop()
	select {
	case ev := <-w.Events:
		t.Fatalf("unexpected event after Remove: %s", ev)
	case <-timer.C:
	}
}

func TestCanonicalize(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}

	got, err := canonicalize(".")
	if err != nil {
		t.Fatalf("canonicalize(.): %v", err)
	}
	if got != cwd {
		t.Errorf("canonicalize(.) = %q, want %q", got, cwd)
	}

	got, err = canonicalize("foo/../bar")
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	want := filepath.Join(cwd, "bar")
	if got != want {
		t.Errorf("canonicalize(foo/../bar) = %q, want %q", got, want)
	}
}

func TestAddRelativePath(t *testing.T) {
	dir := tempDir(t)
	parent := filepath.Dir(dir)
	base := filepath.Base(dir)

	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(parent); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	w := newWatcher(t)
	if err := w.Add(base, Create); err != nil {
		t.Fatalf("Add(relative): %v", err)
	}

	target := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ev := waitOp(t, w, Create)
	if !filepath.IsAbs(ev.Name) {
		t.Errorf("Event.Name not absolute: %q", ev.Name)
	}
}

func TestAddDuplicateAcrossForms(t *testing.T) {
	dir := tempDir(t)
	w := newWatcher(t)
	if err := w.Add(dir, All); err != nil {
		t.Fatalf("Add: %v", err)
	}

	withSlash := dir + string(os.PathSeparator)
	if err := w.Add(withSlash, All); !errors.Is(err, ErrAlreadyAdded) {
		t.Errorf("Add(trailing-slash) = %v, want ErrAlreadyAdded", err)
	}

	viaDot := filepath.Join(dir, ".")
	if err := w.Add(viaDot, All); !errors.Is(err, ErrAlreadyAdded) {
		t.Errorf("Add(./) = %v, want ErrAlreadyAdded", err)
	}

	if err := w.Remove(viaDot); err != nil {
		t.Errorf("Remove(./) = %v, want nil", err)
	}
}

func TestWatchTargetDeletedThenRecreated(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows pins open handles to deleted directories")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "victim")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	w := newWatcher(t)
	if err := w.Add(dir, All); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	// Wait for the kernel notification that retires the watch.
	time.Sleep(200 * time.Millisecond)

	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("Mkdir (re-create): %v", err)
	}
	if err := w.Add(dir, All); err != nil {
		t.Errorf("Add after delete+recreate = %v, want nil", err)
	}
}

func TestSymlinkDeduplicates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}
	real := t.TempDir()
	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	w := newWatcher(t)
	if err := w.Add(real, All); err != nil {
		t.Fatalf("Add(real): %v", err)
	}
	if err := w.Add(link, All); !errors.Is(err, ErrAlreadyAdded) {
		t.Errorf("Add(symlink) = %v, want ErrAlreadyAdded", err)
	}
	if err := w.Remove(link); err != nil {
		t.Errorf("Remove(symlink) = %v, want nil", err)
	}
}

func TestAddCaseInsensitiveOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only")
	}
	dir := tempDir(t)
	w := newWatcher(t)
	if err := w.Add(dir, All); err != nil {
		t.Fatalf("Add: %v", err)
	}

	upper := strings.ToUpper(dir)
	if err := w.Add(upper, All); !errors.Is(err, ErrAlreadyAdded) {
		t.Errorf("Add(uppercased) = %v, want ErrAlreadyAdded", err)
	}
	if err := w.Remove(upper); err != nil {
		t.Errorf("Remove(uppercased) = %v, want nil", err)
	}
}

func TestConcurrentAddClose(t *testing.T) {
	for i := 0; i < 50; i++ {
		w, err := NewWatcher()
		if err != nil {
			t.Fatalf("NewWatcher: %v", err)
		}
		dir := tempDir(t)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			err := w.Add(dir, All)
			if err != nil && !errors.Is(err, ErrClosed) {
				t.Errorf("Add: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := w.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
		wg.Wait()
	}
}

func TestAddRecursiveExistingTree(t *testing.T) {
	root := tempDir(t)
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	target := filepath.Join(nested, "deep.txt")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	w := newWatcher(t)
	if err := w.AddRecursive(root, Write); err != nil {
		t.Fatalf("AddRecursive: %v", err)
	}

	if err := os.WriteFile(target, []byte("changed"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ev := waitOp(t, w, Write)
	if ev.Name != target {
		t.Errorf("Name = %q, want %q", ev.Name, target)
	}
}

func TestAddRecursiveNewDir(t *testing.T) {
	root := tempDir(t)

	w := newWatcher(t)
	if err := w.AddRecursive(root, All); err != nil {
		t.Fatalf("AddRecursive: %v", err)
	}

	newDir := filepath.Join(root, "newsub")
	if err := os.Mkdir(newDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	// Give the auto-watch a moment to register before creating the file.
	time.Sleep(200 * time.Millisecond)

	target := filepath.Join(newDir, "f.txt")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	deadline := time.NewTimer(eventTimeout)
	defer deadline.Stop()
	for {
		select {
		case ev := <-w.Events:
			if ev.Name == target && ev.Op.Has(Create) {
				return
			}
		case err := <-w.Errors:
			t.Fatalf("unexpected error: %v", err)
		case <-deadline.C:
			t.Fatalf("timeout waiting for Create on file in auto-watched subdir")
		}
	}
}

func TestAddRecursiveRemoveDropsSubtree(t *testing.T) {
	root := tempDir(t)
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	w := newWatcher(t)
	if err := w.AddRecursive(root, All); err != nil {
		t.Fatalf("AddRecursive: %v", err)
	}
	if err := w.Remove(root); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Subdirectory should no longer be tracked.
	if err := w.Add(nested, All); err != nil {
		t.Errorf("Add(nested) after Remove(root) = %v, want nil", err)
	}
}

func TestConcurrentAddDistinct(t *testing.T) {
	w := newWatcher(t)
	const n = 16
	dirs := make([]string, n)
	for i := range dirs {
		dirs[i] = t.TempDir()
	}

	var wg sync.WaitGroup
	wg.Add(n)
	for _, d := range dirs {
		go func(d string) {
			defer wg.Done()
			if err := w.Add(d, All); err != nil {
				t.Errorf("Add(%q): %v", d, err)
			}
		}(d)
	}
	wg.Wait()
}
