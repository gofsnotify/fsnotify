# fsnotify

[![CI](https://github.com/gofsnotify/fsnotify/actions/workflows/ci.yml/badge.svg)](https://github.com/gofsnotify/fsnotify/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/gofsnotify/fsnotify.svg)](https://pkg.go.dev/github.com/gofsnotify/fsnotify)

Cross-platform file system notifications for Go.

## Install

```
go get github.com/gofsnotify/fsnotify
```

## Usage

```go
package main

import (
	"log"

	"github.com/gofsnotify/fsnotify"
)

func main() {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close()

	if err := w.Add("/path/to/dir", fsnotify.Create|fsnotify.Write|fsnotify.Remove); err != nil {
		log.Fatal(err)
	}

	for {
		select {
		case ev := <-w.Events:
			log.Println(ev)
		case err := <-w.Errors:
			log.Println("error:", err)
		}
	}
}
```

## API

- `NewWatcher() (*Watcher, error)` — creates a watcher.
- `(*Watcher).Add(path string, op Op) error` — registers `path` with the given event mask. Returns `ErrAlreadyAdded` if `path` is already registered.
- `(*Watcher).AddRecursive(path string, op Op) error` — registers `path` and every directory under it. New subdirectories created inside are watched automatically; removed subdirectories are dropped. `Remove` may only be called on the original recursive root.
- `(*Watcher).Remove(path string) error` — unregisters `path`. For an `AddRecursive` registration this drops the entire subtree.
- `(*Watcher).Close() error` — stops the watcher and closes the channels.
- `(*Watcher).Events <-chan Event` — receives change notifications.
- `(*Watcher).Errors <-chan error` — receives non-fatal errors.

Paths are canonicalized (absolute, cleaned, with symlinks resolved when the target exists; on Windows 8.3 short forms are expanded and case is folded), so two spellings of the same path dedupe and `Event.Name` is always returned in canonical form.

## Events

| Op     | Description                                 |
|--------|---------------------------------------------|
| Create | A file or directory was created.            |
| Write  | A file's contents were modified.            |
| Remove | A file or directory was removed.            |
| Rename | A file or directory was renamed or moved.   |
| Chmod  | Permissions or attributes changed.          |

`Op` is a bitmask; combine values with `|` when calling `Add`.

## Guarantees

- Thread-safe: methods may be called from multiple goroutines.
- Event ordering is preserved as far as the underlying OS allows.
- Behavior is normalized across supported platforms.

## Platform Support

| OS      | Backend                | Status    |
|---------|------------------------|-----------|
| Linux   | inotify                | Supported |
| Windows | ReadDirectoryChangesW  | Supported |
| macOS   | kqueue                 | Supported |

## License

MIT
