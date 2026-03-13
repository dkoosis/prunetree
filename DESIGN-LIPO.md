# lipo — liposuction for git repos

Remove fat from git repositories: identify detritus, compress binaries, purge history bloat.

## Problem

Repos accumulate binary blobs over time — rebuilt tools, database snapshots, vendored artifacts. The working tree might be 50MB but `.git/` is 744MB because golangci-lint was committed 8 times. Clone becomes the bottleneck (CI timeouts, sandbox failures, slow onboarding).

## Commands

```
lipo scan [path]       # analyze — report bloat, change nothing
lipo strip [path]      # compress binaries in working tree
lipo purge [path]      # rewrite history to remove identified bloat
lipo full [path]       # scan → strip → purge (interactive)
```

All commands default to current directory.

### `lipo scan`

Read-only analysis. Three reports:

**1. History blobs** — largest objects in git history, grouped by path:
```
.bin/linux-amd64/golangci-lint    6 versions   287MB total
.snipe/index.db                   4 versions    55MB total
.bin/linux-arm64/golangci-lint    4 versions   168MB total
```

**2. Working tree detritus** — files matching known-bad patterns:
```
DETRITUS  .bin/linux-amd64/golangci-lint   54MB  unstripped Go binary
DETRITUS  .snipe/index.db                  14MB  SQLite database (tracked)
OK        .codex/setup.sh                   4KB  shell script
```

**3. Summary**:
```
.git/         456MB
working tree   50MB (code: 2MB, binaries: 48MB)
clone size    ~744MB

Potential savings:
  strip binaries    -67MB (40% of binary weight)
  purge history    -370MB (6 duplicate blob families)
  total            -437MB → ~307MB clone
```

### `lipo strip`

Compress binaries in the working tree. No history changes.

1. Find ELF/Mach-O binaries
2. Detect Go binaries via `go version -m` — check if already stripped (`-s -w`)
3. If unstripped and source available: suggest rebuild with `-ldflags='-s -w'`
4. UPX compress all ELF binaries (skip if already packed)
5. Report before/after per file

Flags:
- `--upx` — UPX compress (default: true if upx available)
- `--dry-run` — show what would change
- `--skip GLOB` — exclude files from stripping

### `lipo purge`

Rewrite git history to remove bloat. Uses `git-filter-repo` underneath.

1. Run `scan` to identify targets
2. Present removal plan with projected savings
3. Confirm interactively (unless `--yes`)
4. Save remote URL, run filter-repo, restore remote
5. Report before/after `.git/` size

Flags:
- `--paths FILE` — explicit path list (one per line, supports globs)
- `--threshold SIZE` — auto-select blobs above this size (default: 1MB with >2 versions)
- `--keep-current` — purge from history but preserve current HEAD versions (default)
- `--yes` — skip confirmation
- `--push` — force push after purge

`--keep-current` is the key design choice: purge duplicates from history but re-add the current working tree copy in a clean commit. This is what you almost always want — the tool stays in the repo, just without 8 historical copies.

### `lipo full`

Interactive pipeline:

```
$ lipo full

scanning...
  .git/ is 456MB (code is 2MB — 99.6% is bloat)

found 14 blob families (>1MB, >1 version):
  .bin/linux-amd64/golangci-lint    6 versions   287MB
  .bin/linux-arm64/golangci-lint    4 versions   168MB
  ...

found 3 unstripped Go binaries in working tree:
  .bin/linux-amd64/golangci-lint   54MB → ~14MB (strip+upx)
  .bin/linux-amd64/snipe           19MB → ~6MB
  ...

plan:
  1. strip+upx 3 binaries        (save ~100MB working tree)
  2. purge 14 blob families       (save ~370MB history)
  estimated clone: 744MB → ~138MB

proceed? [y/n]
```

## Detritus patterns

Built-in heuristics for files that usually shouldn't be in git:

| Category | Patterns |
|----------|----------|
| Compiled | `*.exe` `*.dll` `*.so` `*.dylib` `*.o` `*.a` `*.class` `*.pyc` |
| Database | `*.db` `*.sqlite` `*.sqlite3` `*.db-wal` `*.db-shm` |
| Archive | `*.zip` `*.tar` `*.tar.gz` `*.tgz` `*.jar` `*.war` |
| Package | `node_modules/` `vendor/` (with binaries) `.venv/` |
| Build | `dist/` `build/` `__pycache__/` `*.wasm` |
| Media | `*.mp4` `*.mov` `*.avi` (>1MB) |
| IDE | `.idea/` (>100KB) |

These are suggestions, not rules. `scan` flags them; `purge` only acts on explicit confirmation or `--paths`.

Custom patterns via `.lipoignore` (gitignore syntax) — mark files as intentionally tracked:
```
# these are fine, don't flag them
!.bin/linux-amd64/*
!docs/diagrams/*.png
```

## Detection: unstripped Go binaries

```bash
go version -m $BINARY 2>/dev/null
```
If it returns build info → it's a Go binary. Check for:
- Missing `-s` (symbol table present → strippable)
- Missing `-w` (DWARF present → strippable)
- Not UPX-packed (`file $BINARY | grep -q "no section header"`)

For non-Go ELF binaries: `file $BINARY | grep "not stripped"`.

## "Blob family" grouping

Multiple versions of the same path across commits. Key metric:

```
blob_family_waste = (num_versions - 1) * avg_blob_size
```

A 50MB binary committed 6 times = 250MB of waste (keeping 1 copy is fine). This is the primary signal for what to purge.

Computed via:
```bash
git rev-list --objects --all \
  | git cat-file --batch-check='%(objecttype) %(objectsize) %(rest)' \
  | awk '/^blob/ {print $2, $3}'
```
Group by path, count distinct sizes, sum totals.

## Implementation

Bash script, like prunetree. Dependencies:

| Required | Optional |
|----------|----------|
| `git` | `upx` (for compression) |
| `git-filter-repo` (for purge) | `gum` (for UI) |
| `file` (for binary detection) | |

Falls back gracefully: no gum → plain output, no upx → skip compression with warning.

### Location

`~/.local/bin/lipo` alongside prunetree. Both are repo-level utilities, language-agnostic (though lipo has Go-specific smarts for strip detection).

## Non-goals

- Not a git LFS migration tool (different problem)
- Not a secrets scanner (use gitleaks/trufflehog)
- Doesn't rewrite code or configs — only binary/blob hygiene
- No daemon/watch mode — run it when you notice bloat

## Open questions

1. **Should `purge --keep-current` be the only mode?** Dropping current HEAD copies too is valid (e.g., you want to stop tracking `.snipe/index.db` entirely). Maybe `--keep-current` vs `--drop` with keep as default.

2. **Interactive blob selection for purge?** `gum choose` multi-select from the blob family list, vs. threshold-based auto-select, vs. explicit `--paths` file. All three? Start with threshold + interactive.

3. **Should strip suggest Makefile targets?** When it finds unstripped Go binaries, it could emit a Makefile snippet for rebuilding them stripped. Feels like scope creep — maybe a `--emit-makefile` flag for later.
