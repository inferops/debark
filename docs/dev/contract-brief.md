# Contract brief — read this before writing any code

The rules below are the engineering contract for this repository. The schemas,
the interfaces and the public API of every package are frozen and compile; the
command bodies behind them are being filled in.

## The one-paragraph product

`debark` moves Debian/Ubuntu software across an air gap. A snapshot is taken
on the offline target; an online builder resolves the exact missing closure
with the **target release's own apt** running in a private apt root; the output
is a bundle containing a real flat apt repository, a lock plan and a signed
manifest; the target verifies the manifest **before apt ever sees the
repository** and then installs exact versions from the lock.

## Non-negotiable rules

1. **apt is the oracle.** Never write a dependency solver, never re-derive what
   apt decided, never "fix up" apt's choice. Go orchestrates, fetches, indexes,
   signs, verifies and reports.
2. **Determinism.** Two builds of the same request must produce byte-identical
   manifests and repository metadata. Sort everything. Never write a host path,
   a wall-clock timestamp or a map iteration order into an artefact.
3. **No telemetry, no phone-home, no update check, ever.** Not even behind a
   flag. No analytics dependency may appear in the tree.
4. **No new dependencies.** Everything needed is already in `go.mod`. If you
   genuinely need something else, stop and report it instead of running
   `go get`.
5. **Errors carry a class.** Return `dferr` errors so the CLI can map them to
   the exit-code table (`core/dferr`). Exit codes 0–7 are frozen.
6. **Canonical JSON for anything hashed or signed.** `core/canonical`. Never
   hash `json.MarshalIndent` output.
7. **Nothing in `core/` may import `internal/cli`.** Nothing in `core/` may
   print to stdout/stderr or read a terminal. Progress and human output are the
   CLI's job; core packages emit `evidence` events instead.

## Frozen files — do not edit

These were written first and everything else compiles against them. If one is
wrong, **report it, do not change it**:

```
core/canonical/  core/digest/  core/dferr/  core/version/  core/distro/
core/snapshot/types.go        core/lock/types.go       core/manifest/types.go
core/evidence/types.go        core/resolve/types.go
core/apt/iface.go             core/store/iface.go      core/repository/iface.go
core/sign/iface.go            core/verify/iface.go     core/policy/iface.go
core/install/iface.go         core/engine/iface.go
api/buildjob/v1/types.go      api/plugin/v1/types.go
go.mod  go.sum
```

The `api.go` file in each package holds that package's **frozen public API**:
the signatures other packages compile against. You own the file — replace the
stub bodies with real implementations — but **do not change an existing
signature**, and do not remove a declaration. Adding new exported functions is
fine.

## Package ownership

| Area | Directories (writes here only) |
|---|---|
| Snapshot capture | `core/snapshot/` |
| Resolution — local apt backend | `core/apt/` (local backend), non-`types.go` files in `core/resolve/` |
| Bundle storage and repository | `core/store/`, `core/repository/`, `core/bundle/` |
| Signing and verification | `core/sign/`, `core/verify/`, `core/manifest/`, `core/lock/`, `examples/` |
| Install | `core/install/` |
| CLI | `internal/cli/`, `cmd/debark/`, sink implementations in `core/evidence/` |
| Build engine | `core/engine/` |
| Fetching | `core/fetch/` |
| Resolution — container backend | container backend files in `core/apt/` |
| Doctor and policy | `core/doctor/`, `core/policy/` |
| Repository foundations | root files, `.github/`, `Makefile`, `.golangci.yml`, `.goreleaser.yaml` |
| Schemas and formats | `api/schema/`, `docs/formats.md`, `docs/adr/` |
| Experiments | `docs/experiments/`, `hack/experiments/` |

Other packages are running **concurrently in this same working tree**. Writing
outside your directories will collide with someone else's work.

- Put test fixtures in your own package's `testdata/` directory.
- Do **not** run `git` commands. Do **not** run `go mod tidy` or `go get`.
- Do **not** run `gofmt -w` on the whole tree; format your own files only.
- `go build ./...` may fail because of another package's in-progress code. Build
  and test **your own packages**: `go build ./core/apt/... && go test ./core/apt/...`.

## Testing

- Unit tests must pass with no external services. CI proves that on
  **Windows and Linux** — the whole of the matrix in
  `.github/workflows/ci.yml`; macOS is not tested there and no macOS binary
  is released, so portability to it remains the goal but is unenforced.
  Anything requiring apt, dpkg, gpg or a container goes behind a guard and
  skips cleanly elsewhere:

  ```go
  func requireLinuxAPT(t *testing.T) {
      t.Helper()
      if runtime.GOOS != "linux" || os.Getenv("DEBARK_E2E") == "" {
          t.Skip("needs Linux with apt; set DEBARK_E2E=1")
      }
  }
  ```

- To run tests on Linux with a real apt, from the repo root:

  ```bash
  bash hack/linux-test.sh ./core/apt/...              # unit tests on Linux
  DEBARK_E2E=1 bash hack/linux-test.sh ./core/apt/...   # with apt-backed tests
  ```

  The image is Debian 12 with Go and apt. `DEBARK_IMAGE=golang:1.26-trixie`
  switches releases. Docker is available and working on this machine.

- Golden files: write them under `testdata/`, regenerate with a `-update` flag
  on the test, and check them in.

## The data flow, in one picture

```
snapshot.Open ──► apt.SelectBackend ──► Backend.Resolve ──► resolve.Plan
                                                               │
   fetch (vendor URLs) ──► store.Store ◄── apt's archives dir ──┤
                                                               ▼
                     bundle.Assemble ──► repository.Writer ──► lock.Save
                                                               ▼
                     manifest.Build ──► sign.Signer ──► manifest.SaveSignature
                                                               ▼
                                             media ──► verify.Verify ──► install.Apply
```

## Reference material in this checkout

- [`prototype-baseline.md`](prototype-baseline.md) — a recorded run of the
  validated Bash prototype this implementation is measured against. Its apt
  invocations, private-root layout and install-side option set are proven;
  read it before designing your own.
- `pault.ag/go/debian` (in the module cache) — `deb.LoadFile` for `.deb`
  control data, `version.Compare` for dpkg version ordering, `control.Unmarshal`
  for deb822 parsing, `dependency.Parse` for relationships.

## Definition of done

- Every stub in your `api.go` implemented, or explicitly reported as deferred.
- Unit tests for parsers, pure functions and error paths; table-driven where
  the input space is wide.
- Errors classified with `dferr`.
- Deterministic output where the design demands it, with a test that proves it.
- Package doc comment explaining what the package is for.
- No `TODO` left without a sentence saying who resolves it and when.
