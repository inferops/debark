# Contributing to debark

Thank you for considering a contribution. This document covers sign-off,
running the tests, how the codebase is currently organized, coding
conventions, and the rule about frozen schemas. See also
[GOVERNANCE.md](GOVERNANCE.md) for how decisions get made and
[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) for the standard we hold ourselves
to.

## Sign off your commits (DCO) — there is no CLA

Every commit must include a `Signed-off-by` trailer certifying the
[Developer Certificate of Origin](DCO) (the verbatim DCO 1.1 text is in that
file). Use `-s`:

```sh
git commit -s -m "core/snapshot: handle empty status file"
```

That appends a trailer like:

```
Signed-off-by: Jane Doe <jane@example.com>
```

using the name and email from your `git config`. A bot checks every pull
request for this trailer (`.github/workflows/dco.yml`) and will tell you
exactly which commits are missing it and how to fix them (`git commit
--amend -s`, or `git rebase --exec 'git commit --amend --no-edit -s'` for a
whole branch).

**We deliberately do not use a Contributor License Agreement.** debark's
business model does not depend on being able to relicense the community
code (see `docs/free-paid-policy.md`) — the code stays
Apache-2.0, permanently, and a commercial edition is a separate codebase
consuming the same public interfaces, not a relicensed fork of this one.
Since there is nothing a CLA would enable that the project intends to do, we
do not ask contributors to sign one. A CLA measurably suppresses
contributions without a corresponding benefit here, and this project needs
contributors more than it needs relicensing optionality. The DCO gives
everyone — the project and downstream users — a clear, low-friction
provenance record instead.

## Before you write code: the rules that do not bend

These come from [`docs/dev/contract-brief.md`](docs/dev/contract-brief.md), the
rules of engagement for this codebase. They apply to every
contribution, not just the initial build-out:

1. **apt is the oracle.** Never write a dependency solver, never re-derive
   what apt decided, never "fix up" apt's choice. Go orchestrates, fetches,
   indexes, signs, verifies and reports. This is ADR-001 and it is the
   single most important rule in the codebase — see the permanent
   do-not-build list in [GOVERNANCE.md](GOVERNANCE.md).
2. **Determinism.** Two builds of the same request must produce
   byte-identical manifests and repository metadata. Sort everything. Never
   write a host path, a wall-clock timestamp, or map iteration order into an
   artefact.
3. **No telemetry, no phone-home, no update check, ever.** Not even behind a
   flag. No analytics dependency may appear in the tree, in any form, in any
   edition.
4. **No new dependencies without discussion.** If the module needs something
   not already in `go.mod`, open an issue or discuss in the PR before adding
   it; do not add a dependency as a drive-by in an unrelated change.
5. **Errors carry a class.** Return `dferr` errors (`core/dferr`) so the CLI
   can map them to the exit-code table (0-7, frozen — see below). Do not
   return a bare `errors.New` from a code path a command can fail on.
6. **Canonical JSON for anything hashed or signed.** Use `core/canonical`.
   Never hash the output of `json.MarshalIndent` or `json.Marshal` directly.
7. **`core/` never imports `internal/cli`, and never prints.** Nothing under
   `core/` may write to stdout/stderr or read a terminal. Progress and human
   output belong to the CLI; core packages emit `evidence` events instead.
8. **Nothing security-relevant is ever paywalled**, and no capability is
   ever gated on a licence check compiled into this binary — see
   `docs/free-paid-policy.md` and [GOVERNANCE.md](GOVERNANCE.md).

## Building and running the tests

Standard Go workflow for anything that does not need a real `apt`:

```sh
go build ./...
go vet ./...
go test ./...
gofmt -l .        # should print nothing; see fmt/lint below
```

### Tests that need Linux and a real apt

Anything that shells out to `apt`, `dpkg`, or `gpg`, or needs a container,
is guarded so it skips cleanly on platforms that cannot run it:

```go
func requireLinuxAPT(t *testing.T) {
    t.Helper()
    if runtime.GOOS != "linux" || os.Getenv("DEBARK_E2E") == "" {
        t.Skip("needs Linux with apt; set DEBARK_E2E=1")
    }
}
```

Unit tests must pass without `DEBARK_E2E` set. CI proves that on **Windows
and Linux**, which is the whole of the matrix in
`.github/workflows/ci.yml`; macOS is not tested there and no macOS binary is
released, so keeping the code portable to it is still the goal but is
unenforced — if you develop on a Mac, you are the only check that it holds.
To actually exercise the apt-backed paths, run inside the project's Linux
test container (Debian 12 with Go and apt) via `hack/linux-test.sh`, which
this repository already has and which this document does not change:

```sh
bash hack/linux-test.sh                                # every package, unit tests only
bash hack/linux-test.sh ./core/apt/...                 # one package
DEBARK_E2E=1 bash hack/linux-test.sh ./core/apt/...  # include apt-backed tests
DEBARK_IMAGE=golang:1.26-trixie bash hack/linux-test.sh ./core/apt/...  # different release
```

`make test-linux` runs the same script with no arguments; see the
[Makefile](Makefile). Module and build caches persist in named Docker
volumes, so only the first run pays for downloads.

**`DEBARK_E2E=1`** is the guard for anything that needs a real apt/dpkg/gpg
or a container: set it only when you have Docker (or the target
environment) available, and expect those tests to be skipped — not
failed — everywhere else, including in most of CI's own unit-test jobs. The
CI workflow (`.github/workflows/ci.yml`) runs a dedicated job with
`DEBARK_E2E=1` inside Debian 12 and Ubuntu 24.04 containers so this path is
still covered on every pull request, even though your local `go test ./...`
on Windows or macOS will not touch it.

### Golden files

Golden fixtures live under each package's own `testdata/`. Regenerate them
with the test's `-update` flag where one exists, review the diff like code,
and check the result in.

## How the codebase is currently organized

The codebase is split into areas with exclusive write ownership of a set of
directories, to let people work concurrently without colliding. This table
(from `docs/dev/contract-brief.md`) reflects the initial split; as areas
mature, ownership naturally becomes "whoever maintains this package now" —
check recent history and open PRs if this table looks stale, and feel free to
open an issue asking who owns an area if it is not obvious.

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
| Repository foundations | root files, `.github/`, `Makefile`, `.golangci.yml`, `.goreleaser.yaml`, `hack/` (excluding `hack/experiments/` and `hack/linux-test.sh`) |
| Schemas and formats | `api/schema/`, `docs/formats.md`, `docs/adr/` |
| Experiments | `docs/experiments/`, `hack/experiments/` |
| Desktop app | `gui/` (its own Go module; see `gui/docs/dev/contract-brief.md`) |

If you are picking up a `good first issue`, it most likely lives in a
package that has already landed its first implementation — check the issue
for which directory it touches.

## Frozen files and the schema-freeze rule

A short list of files was written first and everything else compiles
against them: `core/canonical/`, `core/digest/`, `core/dferr/`,
`core/version/`, `core/distro/`, the `types.go` files in `core/snapshot/`,
`core/lock/`, `core/manifest/`, `core/evidence/`, `core/resolve/`, every
`iface.go` under `core/`, `api/buildjob/v1/types.go`,
`api/plugin/v1/types.go`, and `go.mod`/`go.sum`. **If one of these looks
wrong, open an issue or start a discussion — do not just change it.**

More generally: **the JSON Schemas published under `api/schema/`, and the Go
types that mirror them, are a frozen public contract.** Bundles, manifests
and locks produced by one version of debark must remain readable
according to the schema version they declare. A pull request that changes a
published schema's meaning — not just its Go representation — needs:

1. An ADR under `docs/adr/` explaining what is changing and why the freeze
   is being broken (see [`docs/adr/README.md`](docs/adr/README.md) for the
   numbering and style already in use).
2. A new schema version (`v2`, and so on) rather than a silent change to an
   existing one, unless the change is additive and backward-compatible
   (a new optional field, for instance) — in which case say so explicitly in
   the PR description and update the format documentation
   (`docs/formats.md`) alongside the schema.
3. Updated golden fixtures (valid and invalid) under the schema's own
   `testdata/`.

Each `api.go` file in a `core/` package holds that package's frozen **public
API** — the signatures other packages and packages compile against. You own
implementing the file (replacing stub bodies with real logic) but must not
change an existing exported signature or remove a declaration; adding new
exported functions is fine and encouraged where it helps.

## Coding conventions

- Run `gofmt` (or `make fmt`) before committing; CI fails on any unformatted
  file (`gofmt -l` must print nothing).
- `go vet ./...` and the configured linters (`.golangci.yml`, run via
  `golangci-lint run` or `make lint`) must be clean for the packages you
  touched. See that file for the exact linter set and what is relaxed for
  `_test.go` files.
- Every package should have a package doc comment explaining what it is for
  — see `core/version/version.go` or `core/dferr/dferr.go` for the tone.
- Table-driven tests where the input space is wide (parsers, version
  comparisons, error classification).
- Do not leave a `TODO` without a sentence saying who resolves it and when.
- Put test fixtures in your own package's `testdata/` directory, not a
  shared location.
- Do not run `go mod tidy` or `go get` as a side effect of an unrelated
  change; dependency changes are their own, discussed, PR.

## Opening a pull request

- Small, focused PRs review faster than large ones, especially while
  multiple areas are landing concurrently.
- Describe what you tested and how (including whether you ran the
  `DEBARK_E2E=1` path, and on what).
- Link the issue you are addressing, if any.
- Expect CI (`.github/workflows/ci.yml`) to run lint, build, vet, the
  cross-platform unit test matrix, the container-based `DEBARK_E2E` job,
  a determinism check (build twice, compare), and a `gofmt -l` check.

## Reporting a security issue

Do not open a public issue for a vulnerability — see
[SECURITY.md](SECURITY.md) for the private disclosure process.
