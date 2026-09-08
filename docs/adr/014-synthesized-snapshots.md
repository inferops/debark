# ADR-014: a base definition produces a synthesized snapshot, not a second build path

**Status:** Accepted — 2026-09-06
**Design reference:** the do-not-build list and the supported-releases commitment (deliberate scope addition, 2026-09-06); relies on ADR-001, ADR-004, ADR-005, ADR-008

## Context

`build --snapshot` was a required flag, so debark could not serve a target
that does not exist yet — fifty machines arriving at a site with no network,
none of them installed — and could not be evaluated at all without physical
access to the air-gapped machine *and* an unreleased binary on it, which in
the regulated environments this tool targets is a procurement event rather
than a command (the buyer-cannot-install problem). It was also a
regression from the Bash prototype, whose `download-packages.sh` assumed an
empty system when given no snapshot and produced a bundle that was correct
because it was a superset, merely larger than it needed to be.

The obvious shape — make `--snapshot` optional and resolve differently when
it is absent — is the one thing that must not be built. A snapshot exists in
every build whether the operator ever sees one or not: `snapshot_digest` is
a required field in both `debark.lock/v1` and `debark.manifest/v1`, and
`snapshot.json` is a mandatory bundle member (`docs/formats.md` §2). It is
part of what the signature covers, it is what `verify` checks the target
against, and it is what makes a divergence report on the target possible at
all. The artefact is not optional. Only the *command* that produces one is.

## Decision

A base definition (`debark.base/v1`, `docs/formats.md` §3.13) is a
**producer of snapshots**, not a build mode. `snapshot from-base` writes one;
`build --base` synthesizes one and then enters the existing pipeline
unchanged. There is exactly one resolution path and exactly one input
artefact, and `build` never branches on where its snapshot came from. What a
stock install contains is decided by the release's own apt resolving the
definition's seed packages in a private root where nothing is installed —
still apt as the oracle, still ADR-001, with no hand-curated package list
anywhere in the feature.

Seven parts, each load-bearing:

1. **`origin` is required in the snapshot schema, not optional.**
   Absent-means-captured would make the dangerous case the silent default: a
   synthesized snapshot that lost the field would read as a measurement of a
   real machine — the strongest claim the format can make, asserted by the
   document that says least. `snapshot create` writes `{"kind":"captured"}`
   explicitly, and `core/snapshot`'s validator refuses a snapshot with no
   `origin.kind` rather than guessing.
2. **`origin.assumed_installed` duplicates the synthesized dpkg status on
   purpose.** A bundle carries `snapshot.json` but not the snapshot's files,
   so on the target — the one place the assumption can finally be compared
   against a real dpkg status — this list is the only copy there is. It is
   safe *only* because the snapshot is synthesized: it is a claim about a
   public stock release. A real machine's installed-package list is the
   sensitive inventory D8 keeps out of artefacts, which is why the field must
   be empty for a captured snapshot and is validated as such.
3. **Err toward "not installed", every time.** Assuming a package is present
   when it is not produces a bundle that is short and fails at the far side
   of the gap — the exact defect the product exists to prevent. Assuming it
   is absent when it is present produces a bundle that is merely larger. The
   two errors are not symmetric, so neither are the choices: every variant
   seeds the smaller of two plausible metapackages
   (`ubuntu-desktop-minimal`, not `ubuntu-desktop`; `ubuntu-server-minimal`,
   not `ubuntu-server`), `Recommends` defaults to false, and a bare
   `distro:version` id means the `minimal` variant.
4. **No hosted catalogue, ever.** Everything a base needs is either compiled
   into the binary or on infrastructure the project does not run: the seeds
   resolve against the distribution's own archive, the sources are generated,
   and the keyrings are read from the machine doing the resolving. A
   catalogue would make every `from-base` an outbound call revealing which
   release a site is provisioning — the same sensitive inventory D8 exists to
   protect. `from-base` needs exactly the network access `build` already
   needs, and introduces no new trust boundary.
5. **Never the default.** Snapshot if you can; base if you cannot. Every
   document leads with `snapshot create` and presents `from-base` as the
   fallback for when no target exists yet. The moment `from-base` is the
   happy path, debark is a nicer `apt-get --download-only` and the
   differentiation against `apt-offline` is gone.
6. **Base closure is not a `Backend` method, and the container runs the whole
   command.** A Windows or macOS host — precisely the host that has no
   backend but the container — holds no copy of the target distribution's
   archive keyring, so it can neither build the bootstrap root the seeds
   resolve in nor supply the key material the finished snapshot must carry.
   There is therefore nothing for a container implementation of "resolve this
   closure" to receive. What crosses the boundary is the whole operation: the
   container runs `snapshot from-base`, one snapshot archive comes back out,
   and the host validates it with `snapshot.Open` — the same full reader
   `build` uses.
7. **User-supplied base definition files on day one.** A base id may be a
   builtin identifier or a path to a definition file, and both entry points
   accept either. This stops "which bases do you support?" becoming unbounded
   maintenance across Mint, Pop!_OS and Raspberry Pi OS, and it matches
   reality: an organisation's real baseline is their own golden image, not
   stock Ubuntu. It costs a parser rather than a catalogue.

## Consequences

- **Accepted happily.** debark can now be evaluated, and can provision
  machines that do not exist yet, without anyone touching the air-gapped
  side first. Because synthesis is a producer, everything downstream —
  `build`, `verify`, `install`, `snapshot inspect`, diffing, committing a
  baseline to git — works on a synthesized snapshot with no code that knows
  it is one, and a fleet can provably build against a single assumed
  baseline by committing one archive.
- **Accepted unhappily.** debark now ships a claim it has not measured.
  A captured snapshot proves the bundle is complete for that machine; a
  synthesized one proves it only if the machine really is a stock install of
  that release, and the gap between a seed closure and a real ISO install is
  a genuine fidelity question rather than a formality. It is measured in
  `docs/experiments/E8-base-fidelity.md`, and the builtin table must shrink
  until the assumed-but-absent direction is empty for every supported
  release. Choice 3 exists because that measurement can come out wrong.
- The builtin table is a maintenance surface that tracks release codenames,
  archive layout and seed metapackage names. It is bounded deliberately to
  the releases the design already commits to CI-testing, and it already
  budgets a maintenance release per new distribution release.
- `origin` becoming required is a change to a schema that D12 freezes before
  the first alpha. It was taken now, while no release is tagged, precisely
  because after 1.0 the same field would cost a `debark.snapshot/v2` and a
  documented migration.

## Alternatives considered

- **Make `--snapshot` optional and have `build` resolve against an empty
  system when it is absent**, as the prototype did. Rejected: the bundle
  format requires a snapshot regardless, so this does not remove the
  artefact — it removes the operator's ability to see, inspect, diff or
  commit it, and it puts a second resolution path inside `build`, which is
  the duplication principle 2 exists to prevent. The prototype could afford
  it because it had no lock, no manifest and no signature binding the
  snapshot to the bundle.
- **Leave `origin` optional, with absent meaning captured.** Rejected: it is
  the tempting shortcut and it inverts the safe default. A stripped,
  re-serialised or hand-edited document would silently acquire the stronger
  of the two claims, and every consumer would then have to decide for itself
  whether to trust an absence.
- **Publish a hosted base catalogue** so new releases and derivatives could
  be added without a debark release. Rejected: it makes every `from-base`
  an outbound call that reveals which release a site is provisioning, adds a
  trust boundary debark would have to secure and operate, and contradicts
  D8 outright. Operator-supplied definition files serve the same need with
  no server.
- **Add a third `Backend` method for base closure**, alongside `Resolve` and
  `ClosedWorld`. Rejected: `Backend` exists so one question has one answer
  whichever machine apt runs on, and this question cannot be split that way.
  A container implementation would have to be handed a bootstrap root the
  host cannot build, because the host has no archive keyring for the target
  distribution.
- **Seed the full `ubuntu-desktop` / `ubuntu-server` metapackages**, which
  describe what most machines of that variant actually have. Rejected: a
  machine installed with the installer's "Minimal installation" option is a
  legitimate member of the same variant, and claiming the larger set against
  it produces a short bundle. The reverse error costs disk space on the
  media, which is a cost rather than a defect.
