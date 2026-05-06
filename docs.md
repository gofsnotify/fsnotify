What to build next

# Create fsnotify on GitHub

This is a separate project from fsnotify/fsnotify; its source must not be consulted.

# Requirements

This is a Watcher. Create an instance with NewWatcher. Register a directory together with the events to observe. Ideally, detect duplicate paths and return an error.
File system change notifications are delivered through a channel. Preserve event ordering as much as possible. Thread-safe.
Supported OSes are currently Windows and Linux. Plan ahead for macOS so behavior stays consistent across platforms.
