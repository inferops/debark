# Contributing to debark

Contributions to code, documentation, tests, accessibility, and packaging are
welcome. You do not need to start with a code change: a clear bug report, a
tested target fixture, or a corrected setup guide also helps.

Participation follows the [Code of Conduct](CODE_OF_CONDUCT.md).
[GOVERNANCE.md](GOVERNANCE.md) describes how maintainers review changes and make
project decisions.

## Choose a contribution

Search [existing issues](https://github.com/inferops/debark/issues) and pull
requests before starting. For a substantial feature, new dependency, or public
contract change, discuss the approach in an issue first. Small fixes and
documentation improvements can go straight to a pull request.

Useful starting points include:

- Improving a confusing command example or error message.
- Adding a minimal regression fixture for a reported bug.
- Testing a target or container combination listed in [known limitations](docs/status.md).
- Checking desktop keyboard navigation and accessibility.
- Reproducing an issue and documenting the environment and result.

Use [SUPPORT.md](SUPPORT.md) for questions and bug reports. Report security
vulnerabilities privately through [SECURITY.md](SECURITY.md).

## Set up a checkout

Fork the repository on GitHub, clone your fork, and create a branch. The
commands below use a POSIX shell and run from the repository root.

Requirements:

- **Go 1.26.8+** for both modules (includes required security fixes).
- Git.
- GNU Make and Bash for convenience targets, or run the underlying Go
  commands directly.
- Docker for the Linux test helper; native apt tests need the appropriate
  Debian/Ubuntu environment.

The repository contains two Go modules:

| Path | Purpose |
| --- | --- |
| `.` | CLI and engine; pure Go, no desktop libraries |
| `gui/` | Desktop app; Wails and native webview dependencies |

The GUI resolves the engine through
`replace github.com/inferops/debark => ../`. **One checkout is sufficient.**
Go commands at the repository root do not test the nested GUI module.

## Build and check the CLI

```sh
go build ./...
go vet ./...
go test ./...
gofmt -l .
```

`gofmt -l .` should produce no output. Build a runnable CLI with
`go build -o debark ./cmd/debark` (`-o debark.exe` on Windows), or use
`make build`.

Run the configured linters with `make lint`. The pinned version lives in
[Makefile](Makefile); with Make and Bash you can install that exact version:

```sh
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(make -s print-golangci-lint-version)
make lint
```

Make does not install tools automatically. Windows contributors can run direct
Go commands in PowerShell, and use Git Bash, MSYS2, or WSL for Bash/Make helpers.

Run `make vulncheck` for the CLI and engine. The pinned `govulncheck` version
is in the root Makefile. Run the same tool from `gui/` with
`-tags desktop,production,webkit2_41 ./...` to cover the desktop application.
These checks query the public Go vulnerability database; CI runs them for both
modules on Linux and Windows, including a weekly scan for newly reported issues.

## Tests that need apt or containers

Ordinary unit tests must pass without `DEBARK_E2E` set. Tests requiring a
real apt/dpkg/GPG environment or containers use platform and environment guards.

The Docker helper runs tests in a Linux environment:

```sh
bash hack/linux-test.sh
bash hack/linux-test.sh ./core/apt/...
DEBARK_E2E=1 bash hack/linux-test.sh ./core/apt/...
```

`make test-linux` wraps the same script. Module and build caches persist in
named Docker volumes. See [hack/linux-test.sh](hack/linux-test.sh) for image
selection and environment requirements.

For a complete offline workflow, see [hack/demo-airgap.sh](hack/demo-airgap.sh).
For release/architecture coverage, use the [integration matrix guide](hack/matrix/README.md)
and [end-to-end fixture guide](test/e2e/README.md).

Report the environment and any skips with your results. A portable unit run
does not validate apt-backed behavior. CI checks Linux and Windows, with
dedicated Linux integration jobs; macOS is not in the CI matrix.

## Build and check the desktop app

Follow [gui/README.md](gui/README.md#building-from-source) for native libraries
and the pinned Wails CLI. From `gui/`:

```sh
make frontend
make check
make build
```

`make frontend` copies the vanilla JavaScript assets into the embedded
frontend directory. There are no npm dependencies or bundler. The Go checks use
the `webkit2_41` build tag on Linux through the GUI Makefile.

For visible UI changes, describe the behavior you exercised and include
screenshots when helpful. Check keyboard navigation and consult the
[UI review guide](gui/docs/dev/ui-review.md) and
[accessibility record](gui/docs/accessibility.md).

## Code map

These areas describe responsibilities, not restrictions on who may contribute.

| Area | Directories |
| --- | --- |
| Target capture and baselines | `core/snapshot/`, `core/base/` |
| apt resolution and containers | `core/apt/`, `core/resolve/` |
| Fetching, storage, and repository assembly | `core/fetch/`, `core/store/`, `core/repository/`, `core/bundle/` |
| Signing and verification | `core/sign/`, `core/verify/`, `core/manifest/`, `core/lock/` |
| Installation | `core/install/` |
| Build orchestration | `core/engine/` |
| Diagnostics and policy | `core/doctor/`, `core/policy/` |
| CLI and rendering | `cmd/debark/`, `internal/cli/`, `core/evidence/` |
| Public formats and protocols | `api/`, `docs/formats.md`, `examples/` |
| Desktop app | `gui/` |
| CI, tooling, and integration fixtures | `.github/`, `hack/`, `test/` |

## Engineering principles

The [engineering contract](docs/dev/contract-brief.md) and
[architecture decisions](docs/adr/README.md) provide the detailed rationale.

1. **apt decides dependencies.** Go orchestrates resolution; it does not
   implement a second dependency solver or reinterpret apt's decisions.
2. **Deterministic artifacts.** Keep hashed and signed output independent of
   map iteration, incidental host paths, and uncontrolled timestamps. Use
   fixed inputs and clocks in determinism tests.
3. **No telemetry or automatic update checks.** Do not add analytics,
   crash-reporting uploads, or entitlement checks.
4. **Discuss dependency changes.** Keep them explicit and review their impact;
   avoid incidental `go get` or `go mod tidy` changes in unrelated work.
5. **Classify command failures.** Use `core/dferr` so the CLI preserves its
   [exit-code contract](docs/cli.md#exit-codes).
6. **Canonicalize hashed or signed JSON.** Use `core/canonical`, rather than
   hashing raw `json.Marshal` or `json.MarshalIndent` output.
7. **Keep the engine independent of the CLI.** `core/` must not import
   `internal/cli`, print to a terminal, or read terminal input. Use evidence
   events for progress.
8. **Keep security capabilities in the community project.** Follow the
   [free/paid policy](docs/free-paid-policy.md).

Use `gofmt`, package documentation, and focused tests for behavior changes.
Table-driven tests help with parser and error-classification cases. Keep fixtures
in the relevant package's `testdata/` and review regenerated golden files as
carefully as code.

## Public contracts and schemas

The published schemas under `api/schema/`, mirrored Go types, and shared
interfaces are compatibility boundaries. The engineering contract lists the
frozen files, including shared primitives, `types.go`, `iface.go`, and public
`api.go` declarations.

Discuss changes to those contracts before implementation. A change that affects
their meaning or an existing exported signature needs an
[ADR](docs/adr/README.md) and maintainer agreement under
[GOVERNANCE.md](GOVERNANCE.md). Do not silently change a v1 format.

For a schema change:

- Explain compatibility and migration in the ADR.
- Use a new schema version for a breaking change.
- For an additive, backward-compatible change, explain why existing readers
  remain compatible.
- Update `docs/formats.md`, mirrored types, and valid/invalid fixtures together.

## Sign off every commit

Every commit must include a `Signed-off-by` trailer certifying the
[Developer Certificate of Origin 1.1](DCO). There is **no CLA**.

```sh
git commit -s -m "docs: clarify container builder setup"
```

Git uses the name and email in your configuration. A sign-off certifies your
right to contribute under the project's license; it is separate from a
cryptographic commit signature.

To add a missing sign-off to your latest unpublished commit:

```sh
git commit --amend --no-edit -s
```

For several commits, follow the DCO check's instructions and rewrite only your
own contribution branch. Do not sign off work you cannot certify.

## Open a pull request

Describe the problem and resulting behavior, link an issue if there is one,
and explain how you validated the change. Keep unrelated fixes in separate
pull requests.

For code changes, run the build, vet, tests, formatting, and lint checks for
each affected module. Run the relevant apt/container tests when those paths
change. For documentation-only changes, check links, examples, and consistency;
mark code-only checks as not applicable.

Review the diff before submitting. Do not include private keys, real machine
inventories, generated binaries, or unrelated dependency changes. The
[pull request template](.github/PULL_REQUEST_TEMPLATE.md) records the checks
and compatibility considerations reviewers need.
