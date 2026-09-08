# Correctness experiments (E1–E8)

Experiments settling questions the design cannot answer from first
principles — each backed by a command actually run against a real apt/dpkg
stack, not a guess. E1–E6 settle the open questions the design left
for measurement. `E7` (network postinst heuristic) is not indexed here. E8
settles the question ADR-014 turns on, and postdates that original list.

Every experiment has a rerunnable script in `../../hack/experiments/` and a
findings document here. Run any script again to check whether apt/archive
behaviour has drifted since these were recorded (E1–E6: 2026-09-03; E8:
2026-09-06).

| # | Experiment | Finding | Doc | Script |
|---|---|---|---|---|
| E1 | Phased updates | `APT::Machine-ID` works, but only for the automatic `upgrade`/`full-upgrade` pass — an explicitly-named `install <pkg>` bypasses phasing entirely regardless of machine-id or `Never-Include-Phased-Updates` | [E1-phased-updates.md](E1-phased-updates.md) | [e1-phased-updates.sh](../../hack/experiments/e1-phased-updates.sh) |
| E2 | Solver divergence | Divergence is real (real `mail-transport-agent`: `postfix` vs. a 6-package `courier-mta` closure) but tracks the *solver algorithm* (`APT::Solver`), not apt major.minor — the same apt binary diverges from itself under `-o APT::Solver=internal`, while two different apt versions running the same solver never diverged | [E2-solver-divergence.md](E2-solver-divergence.md) | [e2-solver-divergence.sh](../../hack/experiments/e2-solver-divergence.sh) |
| E3 | Pin fidelity | Exact-version installs always match the lock, but the `--upgrade` (`full-upgrade`) path can silently re-select a version the target's own pin forbade, with zero warning — and the common bare `Pin: origin <name>` keyword keys on hostname, not the Release `Origin:` field, so no Release rewrite can ever restore it | [E3-pin-fidelity.md](E3-pin-fidelity.md) | [e3-pin-fidelity.sh](../../hack/experiments/e3-pin-fidelity.sh) |
| E4 | `apt.conf.d` leakage | Not carrying `apt.conf.d` cost 60 extra packages / 10.6 MB on one `git` install; worse, the design's own `-o Dir::Etc::parts=` idiom silently cannot carry it even if captured — only the `APT_CONFIG` env var mechanism works | [E4-aptconfd-leakage.md](E4-aptconfd-leakage.md) | [e4-aptconfd-leakage.sh](../../hack/experiments/e4-aptconfd-leakage.sh) |
| E5 | Container backend I/O | On Docker Desktop for Windows, a named volume is ~61× faster to write and ~8× faster to read than a bind mount to a native Windows path, for a 342 MB store | [E5-container-io.md](E5-container-io.md) | [e5-container-io.sh](../../hack/experiments/e5-container-io.sh) |
| E6 | Keyring portability | No rejection: Ubuntu 22.04 (jammy, the named target) and a bonus 18.04 (bionic) both resolve cleanly from 24.04/26.04 hosts using only the target's own captured keyring — including a key whose 2012 self-signature uses SHA-1, because what matters is the digest used on *today's* live signature (SHA-512), not the identity-binding signature's age | [E6-keyring-portability.md](E6-keyring-portability.md) | [e6-keyring-portability.sh](../../hack/experiments/e6-keyring-portability.sh) |
| E8 | Base fidelity | A base definition's seed closure is a strict subset of a real install of that release — but only after a fix: `ubuntu-desktop-minimal` resolved from an empty root pulls in `lsb-base`, a transitional package the published desktop image does not install, which would have made every desktop bundle short by one dependency. Zero assumed-but-absent across all three Ubuntu 24.04 variants once it is excluded; 441/431/1007 packages in the harmless direction | [E8-base-fidelity.md](E8-base-fidelity.md) | [e8-base-fidelity.sh](../../hack/experiments/e8-base-fidelity.sh) |

## What this changes in the design

_See each document's "What this means for the design" section for the full
reasoning; this list is only the index._

- **ADR-014 (base fidelity)** — the err-toward-not-installed rule is now
  measured rather than reasoned, and it needed enforcing rather than just
  intending: every seed in the builtin table was already the smaller of two
  plausible metapackages and `Recommends` was already off, and the desktop
  base was still wrong by one package. `Definition.Excludes` exists because
  of this run. A base is also a much smaller claim than an image (147 of 588
  packages for minimal, 802 of 1809 for desktop), which is what "an
  assumption, not a measurement" means in numbers. (E8)
- **Pinning vs. the flat repository** — the three listed defences
  are sufficient for the default exact-version install path (confirmed
  directly: `apt-get install pkg=ver` always matches the lock, on both a
  single-version and a multi-version bundle), but **not** for the
  `--upgrade` flag, which itself specifies running a
  free `full-upgrade` against the bundle. A pin that genuinely changed the
  online resolution can be silently reversed there, reported as ordinary
  success, because defence 2's "succeeds with zero network fetches" check
  does not verify the plan matches the lock. Recommended fix (cheaper than
  partitioning): either plan `--upgrade` from the lock too, or strengthen
  the closed-world check to diff `full-upgrade`'s `Inst` lines against the
  lock. Separately, the common bare `Pin: origin <name>` preferences keyword
  keys on the archive's download-site **hostname**, not the Release
  `Origin:` field — no Release rewrite, partitioning included, can ever
  restore that pin style for a `file://` bundle. The open-questions table's
  "v1.x, only if a real pin case demands it" framing for partitioning should
  not stay parked — this experiment is that case — though the `--upgrade`
  gap itself is the more urgent, pre-GA fix. (E3)
- **ADR-006 (phased updates)** — narrow the claim: the machine-id /
  never-include policy only governs the automatic `full-upgrade`
  (`--upgrades`) pass. It has no effect on, and provides no protection for,
  explicitly-requested packages — apt bypasses phasing unconditionally for
  those regardless of policy. If shipping only fully-phased versions of
  *named* packages under a redacted machine-id is an actual requirement, the
  design needs an added post-resolution filter in Go; as written, ADR-006's
  fallback does not deliver that guarantee for this path. (E1)
- **ADR-005 (private root construction, `apt.conf.d` capture)** — add
  an explicit, separate mechanism for applying captured `apt.conf.d`: the
  `-o Dir::Etc::parts=<dir>` idiom used for every other private-root path
  (sources, preferences, trusted keys, state, cache) is a silent no-op for
  `apt.conf`/`apt.conf.d` specifically, because apt reads those before
  command-line `-o`/`-c` processing happens. The only mechanism that works is
  the `APT_CONFIG` environment variable pointed at a small generated loader
  file. This is a concrete implementation requirement for `core/apt`, not
  just a snapshot-schema question. (E4)
- **Store placement** — change from conditional ("named volume if a
  bind mount proves slow") to an unconditional default on Windows: bind
  mounts to a native Windows path measured ~61× slower to write than a named
  volume for a few hundred MB. macOS direction likely the same but not
  measured here — needs its own run before the design states a macOS number.
  (E5)
- **Keyring portability** — no design change
  needed. Ubuntu 22.04 (jammy, the release named in the brief) and a bonus
  18.04 (bionic, EOL-adjacent) both resolve cleanly — real `apt-get update`,
  zero cryptographic warnings, cross-checked independently with `gpgv` — from
  much newer hosts (apt 3.2.0/2.8.3) using only the target's own captured
  keyring, even though the keyring contains genuinely old material (two
  1024-bit DSA keys from 2004, and RSA-4096 keys whose own 2007-2012
  self-signatures use SHA-1). The reason nothing gets rejected: a key's
  self-signature age is independent of the digest algorithm used on a *live*
  document signature made today (bionic's `InRelease` is signed today, by
  the SHA-1-self-signed 2012 key, using SHA-512) — and it's the live
  signature that verification actually checks. Container-by-default is not
  required on keyring-age grounds for any currently-supported release.
  Caveat: this doesn't cover old, unmaintained *third-party* keyrings, or a
  target whose *live* signing key (not just its self-signature) is itself
  small/weak — neither was tested. (E6)
- **Backend auto-selection threshold** — "host apt
  major.minor == target apt major.minor" is **too loose** as the sole
  local-vs-container test: it happens to give the right answer for stock
  24.04/25.10/26.04 today only because major.minor currently predicts each
  release's default solver, but the thing that actually gates divergence is
  the *solver algorithm* (`APT::Solver`), which is an ordinary
  `apt.conf.d`-scoped setting, not tied to the apt version itself. Confirmed
  directly and repeatably: the identical apt 3.2.0 (and, independently, apt
  3.1.6ubuntu2) binary chooses a completely different, 6-package-heavier
  closure for a real virtual package (`mail-transport-agent`: `postfix`
  alone vs. `courier-mta` + 5 auxiliary packages) purely from `-o
  APT::Solver=internal`, while two different apt versions (25.10, 26.04)
  running the *same* solver never diverged on any of 7 fixtures tested
  (alternatives, real and synthetic virtual packages, multi-version
  requests). Recommended fix: gate on effective `APT::Solver` (`apt-config
  dump`, already `apt.conf.d`-adjacent) in addition to or instead
  of major.minor — or better, a direct capability probe (`-o
  APT::Solver=<target's value>`; an unsupported solver hard-errors rather
  than silently ignoring the flag, so the probe is unambiguous). Separately:
  neither solver generation automatically falls back to a lower, still-
  satisfying version of a dependency when the highest candidate has an
  unrelated broken sub-dependency — both fail the whole request outright, so
  a builder cannot rely on either solver to "route around" a bad candidate
  version on its own. (E2)
