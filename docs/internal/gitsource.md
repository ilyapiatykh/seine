# Package `internal/gitsource`

Thin wrapper around [go-git](https://github.com/go-git/go-git) that
exposes a single operation: "fetch the bytes of a file at a tracked
branch's tip, plus the commit SHA". Both the management server and
agents use it through `specsource.Watcher`.

## Files

- `source.go` — `Source`, `Snapshot`, `Open`, `Pull`, `Close`.
- `auth.go` — `Auth` interface and three implementations.

## Why this wrapper exists

`go-git` is a full repository abstraction. The seine reconciliation
loops only need a tiny slice of it: clone once, fetch on each pull,
read the file at the resolved branch tip. Wrapping it keeps:

- the surface area testable (a Source backed by a `file://` URL is
  enough),
- the rest of the codebase free of `go-git` types,
- and the auth model unified across HTTPS and SSH.

## Public API

### `type Config`

```go
type Config struct {
    URL     string  // https://, ssh:// or file://; required
    Branch  string  // short ref name (e.g. "main"); required
    Path    string  // file path inside the working tree; required
    Workdir string  // local cache; if empty a tempdir is created
    Auth    Auth    // nil or NoAuth{} for public repos
}
```

### `type Source`

Constructed by `Open`. Wraps a working directory plus the configuration
needed to clone or fetch into it. Concurrency-unsafe — wrap with your
own mutex if you need it from multiple goroutines (`specsource.Watcher`
does this).

### `func Open(cfg Config) (*Source, error)`

Validates that `URL`, `Branch` and `Path` are set, prepares the working
directory (creating a tempdir if `Workdir == ""`), and returns the
`Source`. The remote is **not** contacted here; only `Pull` does I/O.
If `Open` allocated a tempdir it sets an internal flag so `Close`
removes it.

### `func (s *Source) Pull(ctx context.Context) (*Snapshot, error)`

Performs the clone-or-fetch dance:

1. If `<workdir>/.git` does not exist: shallow-clone (`Depth: 1`,
   `SingleBranch: true`) the configured branch.
2. Otherwise open the existing repo and run `FetchContext` with a
   refspec that maps `refs/heads/<branch>` to
   `refs/remotes/origin/<branch>` with `+` (force).
3. Resolve `refs/remotes/origin/<branch>` to a commit, walk to the
   tree, look up `cfg.Path`, and return its contents.

Returns `*Snapshot{CommitSHA, Branch, Path, Data}`. Errors include the
underlying `go-git` failure plus a label that identifies the step
(`clone`, `fetch`, `resolve`, `tree`, `file`, `read`).

`go-git`'s `NoErrAlreadyUpToDate` from `FetchContext` is intentionally
treated as success.

### `func (s *Source) Close() error`

Removes the tempdir if `Open` created one. Safe to call multiple times
and on a `nil` receiver.

### `func (s *Source) Workdir() string`

Returns the on-disk path. Useful for logs and tests.

### `type Snapshot`

```go
type Snapshot struct {
    CommitSHA string
    Branch    string
    Path      string
    Data      []byte
}
```

`Data` is the file content at `CommitSHA`; consumers parse it with the
appropriate decoder (the seine project uses `spec.Parse`).

## Authentication

The `Auth` interface has one method: `build() (transport.AuthMethod, error)`.
It is implemented by three concrete types.

### `NoAuth`

Explicit zero value. Equivalent to passing `nil` for `Config.Auth`.

### `TokenAuth`

```go
type TokenAuth struct {
    Username string // optional; defaults to "git"
    Token    string // required
}
```

HTTPS basic auth. Most providers (GitHub, GitLab) accept any non-empty
username for token auth, so the package defaults `Username` to `git`
to keep `host:port` URLs working without per-provider tweaks.

### `SSHKeyAuth`

```go
type SSHKeyAuth struct {
    User           string // typically "git"
    PrivateKeyPath string // required
    Passphrase     string // optional
}
```

OpenSSH private key file. The file's existence is checked at `build`
time (not at `Open` time) to keep failure mode close to the use site.

## Error contract

Callers are expected to treat all `Pull` errors as transient and retry.
`specsource.Watcher` does this with exponential backoff. There is no
sentinel error to distinguish "clone failed because the remote is
unreachable" from "the configured file path is missing"; the wrapping
error string makes this obvious in logs.

## Concurrency

`Source` is not safe for concurrent `Pull` calls — `go-git` shares
internal state through the working directory. The intended usage is a
single goroutine driving a single source, which matches how
`specsource.Watcher` is structured.

## Tests

Integration tests in `internal/gitsource/source_test.go` build a real
local repo with `go-git`, push commits to it, and verify that:

- The first `Pull` clones and reads the configured file.
- A subsequent `Pull` with no upstream change returns the same SHA.
- A new commit upstream is observed on the next `Pull`.
- Missing file or branch produces a clear error.

## Used by

- `specsource.Watcher` — the only direct consumer.
