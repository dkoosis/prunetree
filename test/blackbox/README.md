# Blackbox CLI tests

These tests exercise the public `snipe` CLI as an external binary. The binary
is built once in `TestMain`, then every test invokes it via `os/exec`.

## Running

```sh
go test ./tests/blackbox -run Test...
```

## Golden files

Some tests compare normalized JSON output to golden fixtures in
`tests/blackbox/testdata/golden`. Normalization removes unstable fields (timing,
index fingerprints, etc.) and replaces the repo root with a placeholder so
outputs are stable across machines.

To update goldens:

```sh
UPDATE_GOLDENS=1 go test ./tests/blackbox -run Test...
```

## Human output

`--human` output is expected to be non-JSON human-readable text. If it ever
switches to JSON in the implementation, tests will assert the output is
pretty-printed JSON instead.
