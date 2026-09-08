# ADR-012: the 0–7 exit-code scheme and versioned `--json` object contracts

**Status:** Accepted — 2026-09-03
**Design reference:** principle 7, the "Exit codes" section, decision D12

## Context

debark must be scriptable — CI pipelines, air-gapped change-management
tooling, and human operators over SSH all need to branch on outcome without
parsing prose, and principle 7 requires the same tool to "degrade
gracefully" from rich TTY output down to a machine-readable contract. An
error taxonomy that is just "zero or nonzero" cannot distinguish "fix your
command line" from "the archive rejected the request" from "the media was
tampered with" — three situations calling for entirely different operator
or script responses.

## Decision

Exactly eight exit codes, frozen: `0` success, `1`
usage/configuration error, `2` environment (no apt, no container runtime, no
disk, no permission), `3` incomplete (a URL failed or an external `.deb` had
unsatisfiable dependencies — the bundle still exists), `4` verification
failed, `5` resolution failed (apt could not satisfy the request), `6`
policy violation, `7` target mismatch on install. Every core-package error
is returned as a `dferr.Error` carrying one of these eight classes; only the
CLI (never a `core/` package) turns a class into a process exit status.
Every `--json` output is one of the versioned, schema-published object
shapes in `api/schema/` — never ad hoc JSON-formatted log lines — and
`BuildResult.exit_class` mirrors the same eight-value enum as a string, so a
caller that only ever sees a returned value (never a process exit code —
e.g. the engine called as a library) still learns the outcome.

## Consequences

- **Accepted happily.** A script can distinguish "nothing to do, my command
  was wrong" (1) from "come back when there's a container runtime" (2) from
  "the media is compromised, escalate" (4) from a single integer, with no
  output parsing required; `--json` consumers get the identical distinction
  as a stable string (`exit_class`) even when they never see the process
  exit status.
- **Accepted unhappily.** Eight classes is a real constraint on every error
  site in the codebase — every returned error must be classified into
  exactly one of them, which occasionally forces a judgement call between
  two plausible classes that a less disciplined "just return an error"
  approach would not force.
- The scheme is frozen (`core/dferr` is on the frozen-file list) precisely
  so no future feature can quietly repurpose an exit code's meaning out
  from under existing scripts and CI pipelines built against it — new
  capabilities must fit inside the existing eight, not add a ninth casually.

## Alternatives considered

- **The prototype's own three-value scheme (0/1/3).** Rejected as too
  coarse for a tool whose failure modes now include cryptographic
  verification, policy evaluation and target-architecture mismatches, none
  of which existed in the Bash prototype and none of which should share an
  exit code with a plain usage error.
- **Free-form/unversioned JSON output** (whatever `json.Marshal` of an
  internal struct happens to produce). Rejected as inconsistent with
  the schema-freeze requirement, and because an internal struct's shape
  is not a contract — a refactor could silently change a script-facing
  field name with no version bump to signal it, exactly the failure this
  project's schema discipline exists to prevent everywhere else.
