# snipe

[![Release](https://img.shields.io/github/v/release/dkoosis/snipe)](https://github.com/dkoosis/snipe/releases)

Static code navigation for Go, built for LLM tool use. Indexes once, queries in under 50ms, returns structured JSON.

## Install

```bash
# From source
go install github.com/dkoosis/snipe@latest

# Or download a binary from GitHub Releases
# https://github.com/dkoosis/snipe/releases
```

Requires Go 1.24+ and [ripgrep](https://github.com/BurntSushi/ripgrep). Run `snipe doctor` to verify.

## Quick start

```bash
cd your-go-project
snipe index                     # Build index (~5s for most projects)
snipe def ProcessOrder          # Definition by name
snipe refs ProcessOrder         # All references
snipe callers ProcessOrder      # Call graph: who calls this?
snipe callees ProcessOrder      # Call graph: what does this call?
snipe pack ProcessOrder         # All of the above in one query
```

The index is stored in `.snipe/` — add it to your `.gitignore`.

## How it works

snipe uses `go/packages` for static analysis (symbols, references, call graph) and stores everything in SQLite. Queries resolve against the index. Text search falls through to ripgrep.

```
                        snipe CLI
                           |
          +----------------+----------------+
          |                |                |
     go/packages       ripgrep          SQLite
   (static analysis)  (text search)   (.snipe/index.db)
          |                                 |
          v                                 v
  symbols, refs, call graph         indexed queries (<50ms)
          |
          +--- optional -----------------------+
          |                                    |
     Voyage AI                          Anthropic API
   (voyage-code-3)                    (claude-haiku)
          |                                    |
   semantic embeddings               symbol purposes
          |                                    |
          v                                    v
       sim search                     explain, context
```

The optional enrichment layer adds semantic embeddings (for similarity search) and LLM-generated symbol purposes (for explain and context commands). These require API keys and are off by default.

## Commands

### Navigation

| Command | Description |
|---------|-------------|
| `def [symbol]` | Symbol definition |
| `refs [symbol]` | All references to a symbol |
| `callers [symbol]` | Functions that call a symbol |
| `callees [symbol]` | Functions called by a symbol |
| `show <id>` | Expand a result by its hex ID |
| `search <pattern>` | Text search via ripgrep (no index needed) |

### Composite

| Command | Description |
|---------|-------------|
| `pack [symbol]` | Definition + refs + callers + callees + role + purpose |
| `sym [symbol]` | Definition + refs + callers + callees |
| `explain [symbol]` | Structured function explanation with mechanism steps |
| `context [path]` | LLM-optimized project architecture summary |

### Type system

| Command | Description |
|---------|-------------|
| `types [type]` | Type relationships: methods, fields, embeds |
| `impl [interface]` | Types implementing an interface |
| `imports <file>` | Packages imported by a file |
| `importers <pkg>` | Files that import a package |
| `pkg <name>` | Package overview with exported symbols |

### Index management

| Command | Description |
|---------|-------------|
| `index [path]` | Build or update the index |
| `status` | Index statistics |
| `doctor` | Verify installation and dependencies |
| `edit [symbol]` | AST-aware code editing |

## Query patterns

**By name, position, or ID** — all navigation commands accept any of these:

```bash
snipe def ProcessOrder              # By name
snipe def --at main.go:42:12       # By position (maps to compiler output)
snipe def a3f2c1de89ab0123         # By 16-char hex ID (auto-detected)
```

**Scoped queries** — filter by file or package:

```bash
snipe def --file store.go          # Symbols in a file
snipe def --pkg query              # Exported symbols in a package
snipe refs Open --file store.go    # References filtered to a file
```

**ID chaining** — pipe results across commands:

```bash
snipe def --at handler.go:142:15   # Returns id: "a3f2c1de89ab0123"
snipe callers a3f2c1de89ab0123     # Who calls it?
snipe show b4e3d2c1a0f98765 --with-body  # Full source of a caller
```

**Indexing modes:**

```bash
snipe index                                    # Full (with embeddings + enrichment if keys set)
snipe index --embed-mode=off --enrich=false    # Minimal (symbols only, no API calls)
snipe index --force                            # Force full re-index
```

## Flags

| Flag | Effect |
|------|--------|
| `--at file:line:col` | Query by source position |
| `--with-body` | Include full function/method body |
| `--limit N` | Cap results (default: 50) |
| `--offset N` | Skip first N results |
| `--context N` | Surrounding lines per match (default: 3) |
| `--format` | `concise` (default), `detailed`, or `summary` |
| `--select` | `all` (default), `best`, `top3`, `top5` |
| `--max-tokens N` | Truncate output to fit a token budget |
| `--signature-only` | Signature only, no body or context |
| `--human` | Pretty-print for terminal use |

## Output format

Every command returns JSON with a stable envelope:

```json
{
  "protocol": 1,
  "ok": true,
  "results": [
    {
      "id": "427f6c9bb244e3a7",
      "file": "internal/store/write.go",
      "range": {
        "start": {"line": 33, "col": 1},
        "end": {"line": 98, "col": 2}
      },
      "kind": "method",
      "name": "WriteIndex",
      "receiver": "(*Store)",
      "package": "github.com/dkoosis/snipe/internal/store",
      "match": "func (s *Store) WriteIndex(...) error",
      "ref_count": 2,
      "edit_target": "internal/store/write.go:33:1-98:2#427f6c9b"
    }
  ],
  "suggestions": [
    {
      "command": "snipe refs WriteIndex",
      "description": "Find all usages of this symbol",
      "priority": 1
    }
  ],
  "meta": {
    "command": "def",
    "ms": 12,
    "total": 1,
    "index_state": "fresh",
    "token_estimate": 450,
    "stale_files": []
  }
}
```

**Envelope contract:**
- `protocol` — wire format version (currently 1)
- `ok` — `true` on success, `false` with `error` object on failure
- `results` — array of result objects, never null
- `meta.index_state` — `fresh`, `stale`, or `missing`
- `meta.stale_files` — files modified since the last index run
- `suggestions` — structured next-step commands for LLM consumers
- `id` — 16-char hex, stable across queries, usable as input to other commands

## Configuration

### Environment variables

| Variable | Purpose | Required |
|----------|---------|----------|
| `VOYAGE_API_KEY` | Semantic embeddings (Voyage AI) | No |
| `VOYAGE_MODEL` | Override embedding model (default: `voyage-code-3`) | No |
| `VOYAGE_API_URL` | Override Voyage API endpoint | No |
| `ANTHROPIC_API_KEY` | LLM-generated symbol purposes | No |

None of these are required. Without them, snipe runs on static analysis only.

### Project config

Optional. Create `.snipe.json` in the project root:

```json
{
  "limit": 100,
  "context_lines": 5
}
```

Global config at `~/.config/snipe/config.json` is also supported. Project config takes precedence.

## Requirements

- **Go 1.24+** (for `go/packages` static analysis)
- **[ripgrep](https://github.com/BurntSushi/ripgrep)** (`rg`) for text search
- SQLite is bundled (via `modernc.org/sqlite`, no CGO)
