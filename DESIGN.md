# Design

## Origin

This project is independent of `fsnotify/fsnotify`; its source must
not be consulted while building this one. The public API may end up
looking similar simply because the problem shape is similar.

## Core Watcher

`NewWatcher` returns a `*Watcher`. A caller registers a path together
with the set of `Op` bits it cares about. Change notifications are
delivered on a buffered `Events` channel; non-fatal errors flow on
`Errors`. Both channels are closed when the read loop exits.

The watcher is thread-safe: `Add`, `AddRecursive`, `Remove`, and
`Close` may be called concurrently. Event ordering is preserved as
far as the underlying OS allows.

## Add vs. AddRecursive

Recursive watching is exposed as a dedicated method, not as an option
or variadic argument on `Add`:

- `Add(path, op)` registers exactly one path.
- `AddRecursive(path, op)` registers `path` and every directory
  below it.

The split is intentional. Recursion is bug-prone (subdirectory
lifecycle, walk-vs-event races, fd budgets), so opting in is an
explicit choice the call site makes.

## Remove scope

`Remove` only succeeds on a path that was passed to `Add` or
`AddRecursive`. Sub-watches that `AddRecursive` adds on the user's
behalf cannot be removed independently — calling `Remove` on a
descendant returns `ErrNotAdded`. `Remove` on a recursive root tears
the entire subtree down at once.

## Recursive directory lifecycle

For `AddRecursive`, the watcher is responsible for tracking the
shape of the tree as it changes:

- New subdirectories created inside a recursive root are picked up
  automatically and watched. The walk also descends into the new
  directory in case it appeared with pre-existing children (e.g. a
  rename of an existing tree into the watched root).
- Removed subdirectories are dropped automatically — Linux and macOS
  rely on the kernel's deletion notification, Windows relies on
  `bWatchSubtree`.

## Path normalization

Every path passed to `Add`, `AddRecursive`, or `Remove` is run through
the same canonicalization pipeline so two spellings of the same path
dedupe and `Event.Name` is stable:

- `filepath.Abs` + `filepath.Clean` — relative paths and `.` / `..`
  components collapse.
- `filepath.EvalSymlinks` when the target exists — two paths that
  reach the same target through different symlinks dedupe.
- Windows: `GetLongPathName` to expand 8.3 short forms
  (`C:\PROGRA~1` → `C:\Program Files`), plus a lowercase fold for
  map keys so case-insensitive NTFS comparisons work.

## Testing

Integration tests use real file system events — no mocks. The CI
suite runs on Linux, macOS, Windows, and FreeBSD (under a VM) with
`-race` always enabled; if a backend gets flaky under `-race`, the
timeout grows rather than `-race` getting dropped.

## Platform support

| OS      | Backend                | Status    |
|---------|------------------------|-----------|
| Linux   | inotify                | Supported |
| Windows | ReadDirectoryChangesW  | Supported |
| macOS   | FSEvents               | Supported |
| FreeBSD | kqueue                 | Supported |
| other   | stub returning `ErrUnsupported` | — |
