package fswatcher

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// BenchmarkAddRemove measures the overhead of registering and
// unregistering the same path repeatedly.
func BenchmarkAddRemove(b *testing.B) {
	dir := tempDir(b)
	w, err := NewWatcher()
	if err != nil {
		b.Fatalf("NewWatcher: %v", err)
	}
	b.Cleanup(func() { _ = w.Close() })

	b.ReportAllocs()
	for b.Loop() {
		if err := w.Add(dir, All); err != nil {
			b.Fatalf("Add: %v", err)
		}
		if err := w.Remove(dir); err != nil {
			b.Fatalf("Remove: %v", err)
		}
	}
}

// BenchmarkEventThroughput measures how fast the watcher delivers
// modification events. The reader goroutine drains Events as the test
// writes; the timer covers writes plus event delivery.
func BenchmarkEventThroughput(b *testing.B) {
	dir := tempDir(b)
	target := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		b.Fatalf("WriteFile: %v", err)
	}

	w, err := NewWatcher()
	if err != nil {
		b.Fatalf("NewWatcher: %v", err)
	}
	if err := w.Add(dir, Write); err != nil {
		b.Fatalf("Add: %v", err)
	}

	drained := make(chan struct{})
	go func() {
		for range w.Events {
		}
		close(drained)
	}()

	b.ReportAllocs()
	payload := []byte("x")
	for b.Loop() {
		if err := os.WriteFile(target, payload, 0o644); err != nil {
			b.Fatalf("WriteFile: %v", err)
		}
	}
	b.StopTimer()

	_ = w.Close()
	<-drained
}

// BenchmarkAddRecursive measures the cost of AddRecursive over a
// pre-built directory tree.
func BenchmarkAddRecursive(b *testing.B) {
	for _, dirs := range []int{16, 64, 256} {
		b.Run("dirs="+strconv.Itoa(dirs), func(b *testing.B) {
			root := tempDir(b)
			buildFlatTree(b, root, dirs)

			b.ReportAllocs()
			for b.Loop() {
				w, err := NewWatcher()
				if err != nil {
					b.Fatalf("NewWatcher: %v", err)
				}
				if err := w.AddRecursive(root, All); err != nil {
					b.Fatalf("AddRecursive: %v", err)
				}
				_ = w.Close()
			}
		})
	}
}

// buildFlatTree creates n empty subdirectories directly under root.
// This keeps the tree shallow enough to avoid per-OS limits on watch
// counts while still exercising the walk + register cost.
func buildFlatTree(tb testing.TB, root string, n int) {
	tb.Helper()
	for i := range n {
		if err := os.Mkdir(filepath.Join(root, "d"+strconv.Itoa(i)), 0o755); err != nil {
			tb.Fatalf("Mkdir: %v", err)
		}
	}
}
