# test/e2e — the integration matrix

This is the harness that proves debark's central claim: **a bundle that
resolves online installs offline, every time, on every supported release**
(the kill criterion). It drives
the real `debark` CLI, in real Linux containers, across real apt — never
core Go packages directly — because the whole point is to prove the product
the way an air-gapped operator experiences it.

Two ways to run it:

- `go test ./test/e2e/ -run TestFixtures -v` — local development, one process,
  runs every fixture once against its own default release/arch.
- `go run ./hack/matrix` — the full matrix: every fixture expanded across
  every supported release/arch it targets, bounded parallelism, a JSON result
  file. See `../../hack/matrix/README.md`.

Both need Docker and are guarded behind `DEBARK_E2E=1` for the `go test`
path (`hack/matrix` doesn't need the guard — running it at all is already an
explicit choice to start containers):

```bash
DEBARK_E2E=1 go test ./test/e2e/ -run TestFixtures -v
DEBARK_E2E=1 go test ./test/e2e/ -run 'TestFixtures/alt-deps' -v   # one fixture
go test ./test/e2e/...          # fixture files only: parses + validates every
                                 # fixture, no container, runs on any OS
```

## What a fixture is

A fixture is one JSON file under `fixtures/`: a target release and installed
state, a request, and an expected outcome. **Adding a row to the matrix means
adding a file here, not writing Go.** The Go types are in
`harness/types.go`; a fixture file is that shape, JSON-encoded, with `//` and
`/* */` comments (see "Why JSON, not YAML" below).

```jsonc
{
  "schema_version": "debark.e2e.fixture/v1",
  "name": "my-fixture",                 // unique; becomes the subtest/row name
  "protects": "...",       // required — see "Why every fixture..." below
  "tags": ["edge"],                     // free-form, informational
  "determinism": true,                  // optional: build twice, compare manifest+repo metadata

  "target": {
    "distro": "debian", "version": "12", "arch": "amd64",
    "matrix": false,                    // true = run across every supported release (see below)
    "matrix_arches": ["amd64", "arm64"],
    "foreign_archs": ["i386"],          // dpkg --add-architecture, before anything installs
    "installed": {
      "packages": ["jq"],               // real archive packages, installed for real
      "holds": ["jq"],                  // apt-mark hold, after installation
      "repos": [ /* SyntheticRepo — see below */ ],
      "from_repos": ["acme-tool=1.0"]   // installed after repos are added
    },
    "sources": [ {"filename": "x.sources", "content": "..."} ],      // /etc/apt/sources.list.d/
    "preferences": [ {"filename": "x.pref", "content": "..."} ],     // /etc/apt/preferences.d/
    "apt_conf": { "APT::Install-Recommends": "\"false\"" }           // /etc/apt/apt.conf.d/
  },

  "request": {
    "packages": ["jq", "tree"],
    "urls": ["https://..."],
    "vendor_debs": [ {"name": "mytool", "version": "1.0", "depends": "jq, tree", "bin": true} ],
    "build_flags": ["--upgrades"], "install_flags": [], "verify_flags": [],
    "sign": true                        // generates a key, builds --sign, verifies --key
  },

  "expect": {
    "build": { "exit_class": "success" },              // required
    "verify": { "exit_class": "success" },              // omit = success; "skip" = don't run this stage
    "install": { "exit_class": "success" },
    "packages_present": ["jq"], "packages_absent": ["tree"],
    "binaries": [ {"path": "/usr/bin/jq", "args": ["--version"], "exit_code": 0, "stdout_contains": "jq-"} ],
    "elf_checks": [ {"package": "zlib1g:i386", "class": "ELF32"} ],
    "unresolved_contains": ["mytool"],                  // for exit_class "incomplete"
    "doctor_contains": ["dkms"]                          // substring in `debark doctor --json`
  },

  "tamper": { "kind": "modified-deb" }   // corrupted-bundle fixtures only — see tamper-*.json
}
```

`exit_class` is one of `success`, `usage`, `environment`, `incomplete`,
`verification`, `resolution`, `policy`, `target-mismatch` (`core/dferr`'s
taxonomy, by name) or `skip` for verify/install.

### Synthetic packages and repositories, and why

Real packages are used wherever the fixture's question is genuinely about a
specific real closure (`basic-install-matrix.json`'s jq/tree, matching
`docs/dev/prototype-baseline.md`'s own recorded run). Everywhere else — an
alternative dependency, a virtual package, two versions of "the same"
package, a third-party repository, a phased update — the fixture builds a
tiny synthetic `.deb` on the fly rather than depending on a real vendor
download or a real archive's current, moving-target state (task constraint:
"simulation over real downloads wherever a fixture's question does not
require the bytes"). This also keeps every row's network use to the base
image plus a handful of small, well-known packages.

```jsonc
{ "name": "acme-tool", "version": "1.0",
  "depends": "jq, tree", "provides": "acme-virtual-service", "recommends": "tree",
  "bin": true,       // installs /usr/bin/acme-tool, prints "acme-tool 1.0", exits 0
  "postinst": "..."  // raw postinst body — for doctor's snap-shim/DKMS heuristics
}
```

`bin: true` is what makes `expect.binaries` assertions meaningful: the
installed script prints its own name and version, so an assertion can prove
*which* version actually ended up on the target, not merely that some
version of the name is present.

A `repos` entry (`SyntheticRepo` in `harness/types.go`) builds a real flat
apt repository — `Packages`, and a `Release` when the fixture needs one for
origin/release pinning (`origin-release-pin.json`) — and serves it over HTTP
from the host for the whole fixture run (`harness/httprepo.go`). **Not a
`file://` source**: the "state" container that builds a fixture's installed
set and the separately-started "builder" container that later resolves the
snapshot's recorded sources do not share a filesystem, so a repository has
to be reachable from more than one container, which only HTTP (via
`host.docker.internal`) achieves. See `harness/syntheticdeb.go`'s package
doc for the full reasoning — this was a real bug caught and fixed by the integration matrix
development, not the original design.

### The `matrix` field: which fixtures run everywhere

Most fixtures test one specific mechanism (a pin, a hold, an alternative
dependency) and are pinned to one representative release — running them five
times would mostly re-prove the same thing at five times the container cost
(task: "each row is minutes of container time"). A fixture with
`target.matrix: true` is different: it's the correctness backbone itself,
and `hack/matrix` expands it across every `core/distro.Supported()` release
crossed with `target.matrix_arches` (or just `target.arch` if that list is
empty). Only `basic-install-matrix.json` is marked this way today; mark a
new one the same way if its claim is genuinely "this must hold everywhere,"
not "this must hold at all."

`go test`'s `TestFixtures` never expands a `matrix: true` fixture — it always
runs a fixture exactly once, against its own stated default release/arch,
because it exists for fast local debugging, not for the full sweep.

### Why every fixture states what it protects

Every fixture's `protects` field, plus a `/* ... */` comment at the top of
the file, names the design section or failure mode it exists to catch, and
what would break if the behaviour regressed. This is not decoration — a
fixture nobody understands gets deleted by the next maintainer (the task's
own words), and six months from now "why does this fixture exist" needs an
answer that doesn't require reconstructing the reasoning from scratch. When
you add a row, write this first: if you can't say what regression it would
catch, it's not ready to add.

### Why JSON, not YAML

`go.mod` is frozen (`docs/dev/contract-brief.md` rule 4: "No new
dependencies... report it, do not change it") and carries no YAML library.
Fixture files are JSON with `//` and `/* */` comments, stripped by
`fixture.go`'s `stripJSONComments` before parsing (a small, tested,
string-literal-aware lexer — not a new dependency, ~60 lines). This keeps the
literal human explanation the task asks for ("a comment explaining...") as
an actual comment, not a squeezed-in string field.

## How the scenario runs (`harness/scenario.go`)

For each row: build a static linux debark binary for the target arch
(cached once per architecture per run) → start a **state** container, bring
it to the fixture's installed state with its own real apt, run
`debark snapshot create` → start a **builder** container of the same
release, copy the snapshot in, run `debark build` (`--backend local`,
mirroring `docs/dev/resolve-contract.md`'s own container re-entry
convention — the harness is already inside the correct release's image, so
there is no need for debark's own container-backend auto-selection to run
a second, nested container) → copy the bundle out to the host → apply a
tamper mutation if the fixture has one (pure host-side byte edits, no
container needed) → start a **fresh** container **with `--network none`** →
copy the bundle and binary in → `debark verify`, then `debark install`
→ assert the requested packages are (or are not) present, and — the
assertion that actually matters, per the task — **run the requested
binaries** and check their exit code and output, since a package can install
cleanly and still be unusable if a dependency was missed. Every container is
torn down in `cleanup()`, which always runs (a `defer`-equivalent reached
through a top-level `recover()`), even on a panic or a timeout.

Every stage's expected `dferr` exit class is compared against what actually
happened; a mismatch is classified `blocked` when the output contains the
literal string `"not implemented"` (the exact pattern unimplemented stub bodies use
throughout this tree — e.g. `core/engine/iface.go`'s `New`) and `fail`
otherwise — the difference between "this doesn't exist yet" and "this exists
and is wrong." See `harness/result.go`'s doc comment for the full rule.

## How to add a row

1. Pick the smallest real or synthetic package that exercises the mechanism.
2. Write the fixture file: name, `protects` (see above), target, request,
   expected outcome.
3. `go test ./test/e2e/ -run TestRealFixturesLoad -v` — validates the file
   parses and passes schema checks, no container, seconds.
4. `DEBARK_E2E=1 go test ./test/e2e/ -run 'TestFixtures/<name>' -v` — runs
   it for real.
5. `gofmt` doesn't apply to JSON; just keep the existing files' style
   (2-space indent, comment block at the top).

## How to debug a failing one

- The test log prints `status=... stage=... blocker=...` and a `transcript:`
  path — that file has every stage's full command line and combined
  stdout/stderr, in order, plus every cleanup action.
- `stage` says exactly how far the row got: `build-binary`, `target-setup`,
  `snapshot-create`, `target-commit`, `build`, `bundle-transfer`,
  `determinism`, `tamper`,
  `verify`, `install`, `assert`, `done`.
- A row's scratch directory (`DEBARK_E2E_WORKDIR`, default
  `<tmp>/debark-e2e/<run>/<fixture>-<distro>-<version>-<arch>/`) is kept on
  anything other than a full pass — it has the built binary, the snapshot
  archive, the bundle directory (post-tamper, if any), and the transcript.
  It is **not** inside the repository; nothing under `test/e2e/` or
  `hack/matrix/` is ever a run artefact.
- If nothing ran at all: check `docker ps -a --filter label=debark.e2e=1`
  — every container this harness creates carries that label plus
  `debark.e2e.run=<run-id>`, `debark.e2e.fixture=<name>` and
  `debark.e2e.role=state|builder|freshtarget`, so a stray one is easy to
  find and remove by hand if a process was killed hard enough to skip its
  own cleanup (`harness.Sweep` is the automatic backstop for that; it only
  ever touches containers carrying the current run's own label).
- A `blocked` status with the literal text `not implemented` in the blocker
  means the product doesn't do this yet, not that the fixture is wrong.

## Files

```
test/e2e/
  fixture.go            fixture envelope, JSONC loader, validation
  fixture_test.go        unit tests (no Docker)
  e2e_test.go             `go test` driver: TestMain (run-scoped sweep), TestFixtures
  fixtures/*.json          the rows — one file each
  harness/
    types.go                the fixture/scenario data model
    scenario.go               RunFixture: the whole pipeline
    docker.go                  docker CLI wrapper (exec, cp, containers, Sweep)
    binary.go                   cross-compile+cache cmd/debark for linux/<arch>
    cli.go                       debark subcommand flags, in one place
    syntheticdeb.go               pure-Go .deb/Packages/Release builder
    httprepo.go                    serves synthetic repos to every container in a run
    target.go                       applies a fixture's installed state to a container
    tamper.go                       the six corrupted-bundle mutations
    assert.go                       packages/binaries/ELF/doctor assertions
    result.go                       RowResult / MatrixResult (shared with hack/matrix)
    workdir.go                       host scratch-directory root
    guard.go                        RequireE2E (DEBARK_E2E=1 gate)
    *_test.go                        harness-level tests, some real-Docker-backed
```

## What this does *not* do

It does not call any `core/*` Go package directly (only `core/dferr` and
`core/distro`, both single-file and frozen — see `harness/tamper.go`'s doc
comment for why even those are treated carefully). Everything else happens
by invoking a real `debark` binary the way an operator would. That is
deliberate: the point of an integration matrix is to prove the product from
the outside, not to re-test a package's internals a unit test already
covers.
