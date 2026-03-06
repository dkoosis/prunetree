# ADR-001: SQLite Driver Selection

## Status
Accepted (existing implementation)

## Context
Snipe needs an embedded SQLite database for storing the code index. Two main options:

| Driver | Pros | Cons |
|--------|------|------|
| `go-sqlite3` (mattn) | Faster, battle-tested | CGO required, cross-compile pain |
| `modernc.org/sqlite` | Pure Go, single binary, easy cross-compile | ~10-20% slower |

## Decision
Use **modernc.org/sqlite**.

## Rationale
1. **Single binary goal** — SPEC requires "Zero external runtime dependencies (single binary)"
2. **Cross-compilation** — Pure Go enables `GOOS=linux GOARCH=amd64 go build` without C toolchain
3. **Performance acceptable** — Index builds and queries are I/O bound; SQLite driver overhead is negligible
4. **Already implemented** — Codebase uses `modernc.org/sqlite v1.44.0`

## Consequences
- No CGO in build process
- Cross-compilation works out of the box
- Slightly slower than CGO alternative (acceptable tradeoff)
- Must use `PRAGMA busy_timeout` for concurrency (see snipe-sqlite-locks task)

## References
- SPEC.md: "Zero external runtime dependencies"
- go.mod: `modernc.org/sqlite v1.44.0`
