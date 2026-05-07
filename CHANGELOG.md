# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- FreeBSD support based on the darwin kqueue backend (#5)
- FreeBSD CI job via `vmactions/freebsd-vm`
- Dependabot configuration for GitHub Actions updates (#4)
- Test coverage for paths containing spaces

### Changed
- Replace the cgo FSEvents backend on macOS with a purego implementation (#9)
- Document FreeBSD support in README and DESIGN
- Update macOS backend docs to reflect the purego implementation (#14)
- Rename `CLAUDE.md` to `AGENTS.md`
- Harden GitHub Actions workflows: set `persist-credentials: false` and `timeout-minutes` (#6)

### Fixed
- Do not resolve symbolic links for paths that do not exist (#7)
- macOS: notify `Rename`-only registrations on `RootChanged` fallback so renames of the watched root are not lost (#11)
- macOS: deliver events for `Add` on individual files; root-path suppression now only applies to directories (#12)
- `TestCanonicalize` on systems where the working directory traverses a symlink

## [0.0.3] - 2026-05-06

### Added
- Benchmarks for `Add`/`Remove`, event throughput, and `AddRecursive`
- README mention of the `All` `Op` constant

### Changed
- Refresh `DESIGN.md` with the decisions made during the v0.0.x cycle

### Fixed
- Make Windows `Close` wait for the IOCP read loop to exit
- Make macOS `Close` wait for the kqueue read loop to exit
- Recursively close the kqueue subtree when a watched directory disappears

## [0.0.2] - 2026-05-06

### Added
- `AddRecursive` for watching whole directory subtrees
- MIT `LICENSE`
- pkg.go.dev badge in the README
- README documentation for `AddRecursive` and path canonicalization

### Changed
- Pin GitHub Actions versions and bump the module version (#3)
- Wrap the inotify fd in `*os.File` and wait for the goroutine on `Close`
- Keep the Windows read buffer `DWORD`-aligned in `winWatch`
- Loosen `TestAddRecursiveExistingTree` event matching
- Bump the per-event test timeout to 10s

## [0.0.1] - 2026-05-06

### Added
- Initial design docs and README
- `Op`, `Event` types and the Linux inotify backend
- Windows backend and a stub for other platforms
- macOS `Watcher` backed by kqueue
- Cross-platform tests including `Rename`, `Chmod`, and multi-watcher cases
- Regression tests for concurrent `Add` and `Close`
- CI workflow for Linux, macOS, and Windows
- Auto-create a GitHub release on version tag push

### Changed
- Rename the module path to `github.com/gofsnotify/fsnotify`
- Normalize paths in `Add`/`Remove` and fold case on Windows
- Resolve symlinks in canonicalize so aliases dedupe
- Wrap `Add`/`Remove` errors with path context
- Expand Windows 8.3 short paths via `GetLongPathName`
- Drop watches when the target goes away
- Tighten the `Chmod` test for Windows
- Canonicalize temp directories in tests for cross-platform stability

[Unreleased]: https://github.com/gofsnotify/fsnotify/compare/v0.0.3...HEAD
[0.0.3]: https://github.com/gofsnotify/fsnotify/compare/v0.0.2...v0.0.3
[0.0.2]: https://github.com/gofsnotify/fsnotify/compare/v0.0.1...v0.0.2
[0.0.1]: https://github.com/gofsnotify/fsnotify/releases/tag/v0.0.1
