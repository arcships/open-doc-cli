# Testing Guide

> For contributors: how to run tests, how they're organized, how to write new tests, and the fixture red lines.

## How to Run

```bash
go test ./...          # the full suite, ~20 seconds, no network or credentials needed
go test ./internal/engine -run TestSync -v    # a single test in a single package
```

All tests are **hermetic**: no real API calls, no reading `~/.opendoc`, no dependency on tokens from the environment. Any test that requires network access or real credentials to pass will not be merged — this is a hard constraint, guaranteeing that CI and a new contributor's first `go test ./...` are always green.

## How Tests Are Organized

Tests live in the same directory and package as the code under test (standard Go practice) — there's no separate `/test` directory. The two levels are distinguished by file naming:

| Naming | Level | Example |
|---|---|---|
| `<unit>_test.go` | Unit test: whitebox, calls unexported functions directly | [ids_test.go](../../internal/notion/ids_test.go) tests `canonicalID` |
| `integration_test.go` / `*_integration_test.go` | Integration test: wires up the real engine + a mock adapter to run a full `Sync`, asserts on-disk results | [integration_test.go](../../internal/notion/integration_test.go) |

Even though "integration" is in the name, these tests are just as hermetic and just as fast (sub-second), so they aren't isolated behind a build tag — they run together with `go test ./...`.

## Two Mock Patterns (copy these for new tests)

**Notion: mock `http.RoundTripper`.** The Notion client accepts a custom `http.Client`; tests inject an in-memory `RoundTripper` that dispatches canned JSON by URL path (search pagination, the markdown endpoint, data_source query, S3 asset downloads). See `mockNotion` in [integration_test.go](../../internal/notion/integration_test.go).

**Feishu: mock the `Runner` interface.** All engine calls funnel through the `Runner` interface in [larkcli.go](../../internal/feishu/larkcli.go); the production implementation self-execs the embedded lark engine (`opendoc lark-engine …`), while the test implementation `fakeRunner` matches on argument substrings and returns canned output. See [enumerate_test.go](../../internal/feishu/enumerate_test.go).

Engine-layer tests go one step further: they implement an in-memory `adapter.Adapter` directly, touching no platform code at all.

## Fixture Red Line: No Real Data

**No real API response may ever be pasted verbatim into a test.** Workspace/page/database IDs, document titles, and page bodies in real responses are all personal data; this repository has previously done a full pass of scrubbing for exactly this reason. Rules for writing fixtures:

1. **IDs use obviously-fake synthetic values.** The convention in existing tests is UUIDs starting with a hex word (`facefeed-0000-4000-8000-...`, `c0ffee00-...`, `dbdbdbdb-...`) or all-repeating segments (`11111111-1111-...`). When testing case/hyphen-stripping normalization, make sure the fake ID contains a–f letters (an all-numeric UUID makes case assertions meaningless).
2. **Titles and paths use generic words.** (`School`, `Term 1`, `欢迎`, `手册`) — no real school, company, project, or course names.
3. **Signatures and tokens use obvious placeholders.** (`X-Amz-Signature=deadbeef`, `ntn_from_file`).
4. **Structure must be real, content must be fake.** The JSON shape of a fixture must be faithful to the real endpoint (field names, nesting, pagination cursor semantics); when a new shape is needed, check the platform's official API docs (or capture a real response and scrub it per the rules above) — don't invent fields from memory.

Self-check command (run before submitting, to confirm no real IDs snuck in):

```bash
grep -rnoE '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' --include='*_test.go' .
```

## Conventions for Writing Tests

- **Assert on-disk artifacts, not intermediate state.** The acceptance target for integration tests is the directory tree, frontmatter, and manifest rows on disk — this is consistent with opendoc's deliverable (a tree).
- **Use `t.TempDir()` for temp directories** — don't hardcode paths, don't touch `~/.opendoc`.
- **Hardcode timestamps** (e.g. `2026-07-14T09:00:00Z`), don't use `time.Now()`, to guarantee cross-machine reproducibility.
- **The degradation contract must be tested on both sides**: test both the placeholder output when degradation occurs and that the `Degradation` count is correctly reported — "lossy changes must leave a trace" is a red line (see the degradation contract in [architecture.md](architecture.md)), and an implementation change that silently drops content should be caught by tests.
