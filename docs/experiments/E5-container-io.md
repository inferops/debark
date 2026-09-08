# E5 — Container backend I/O: bind mount vs named volume

**Status: complete for Windows.** macOS **not measured** (no macOS hardware
available in this environment) — see "What this means for the design" for
what can and cannot be concluded for macOS from this run.

## Question

On Docker Desktop for Windows, is a bind-mounted store directory fast enough,
or does the store need to live in a named volume? Settles the open question of whether the store has to live in a named
volume if a bind mount proves slow.

## Method

Script: [`hack/experiments/e5-container-io.sh`](../../hack/experiments/e5-container-io.sh).
Run 2026-09-03, Docker Desktop **29.6.2**, server `linux/amd64` (confirms the
WSL2 backend — `wsl -l -v` shows the `docker-desktop` distro `Running`),
image `ubuntu:24.04` (digest
`sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517`),
host repo checked out on a native Windows drive (written `<repo>` below; a
real path such as `D:\dev\debark`, and specifically **not** inside a WSL
distro's own filesystem).

Wrote, then read back, **300 files sized 200 KB–2.2 MB (342 MB total)** —
representative of a store/pool of `.deb` files — inside a container, 3
repeats each, for two mount strategies:

- **bind mount**: `-v <repo>\hack\experiments\out\e5\bind-store:/store`
  (a native Windows path, which is what a store directory under the repo
  checkout, or any user-chosen Windows path, would be).
- **named volume**: `docker volume create` + `-v debark-e5-store:/store`.

Same deterministic file-size manifest both times. Write phase: `dd
if=/dev/zero of=<file> bs=1024 count=<size>` per file (avoids `/dev/urandom`
CSPRNG throughput becoming the bottleneck instead of the filesystem), then
`sync`. Read phase: `cat` every file to `/dev/null`. Timed with
`date +%s%N` inside the container (nanosecond precision, nothing host-side
to distort it).

**Caveat**: `sync` inside the container guarantees the guest-side flush; it
cannot force a flush of any Windows-host-side disk cache behind a bind mount,
so the true steady-state numbers for a much larger store than fits in cache
could differ from a single 342 MB run. Repeated 3× per case specifically to
see whether caching was making later runs faster — it was not (see below) —
which suggests these numbers already reflect the actual mount-layer
throughput, not a cold/warm cache artifact.

## Raw evidence

Full transcript: `hack/experiments/out/e5-run.log`. All 3 repeats, then the
mean:

| mount | write (3 runs) | write throughput | read (3 runs) | read throughput |
|---|---|---|---|---|
| bind mount (`D:\...`) | 49.24s, 44.51s, 57.95s | mean **6.9 MB/s** | 2.88s, 2.57s, 3.29s | mean **119 MB/s** |
| named volume | 0.72s, 0.88s, 0.87s | mean **419 MB/s** | 0.27s, 0.46s, 0.42s | mean **939 MB/s** |

```text
bind mount   run 1: write 49.24s (7.0 MB/s)   read 2.88s (118.9 MB/s)
bind mount   run 2: write 44.51s (7.7 MB/s)   read 2.57s (133.2 MB/s)
bind mount   run 3: write 57.95s (5.9 MB/s)   read 3.29s (104.0 MB/s)
named volume run 1: write 0.72s (472.5 MB/s)  read 0.27s (1258.5 MB/s)
named volume run 2: write 0.88s (390.3 MB/s)  read 0.46s (745.5 MB/s)
named volume run 3: write 0.87s (393.5 MB/s)  read 0.42s (814.2 MB/s)
```

342 MB across 300 files. No warm-up trend within either group (bind mount's
3 runs are 49.2/44.5/58.0s — noisy but not monotonically improving; named
volume's are 0.72/0.88/0.87s, also flat) — the gap is the mount mechanism,
not a cache artifact of run order.

## Finding

**On Docker Desktop for Windows with a bind mount rooted on a native Windows
drive, a named volume is roughly 61× faster to write to and 8× faster to
read from than a bind mount, for a few hundred megabytes of small-to-medium
files.** Writing 342 MB of package-sized files took 45-58 seconds through the
bind mount versus well under one second through the named volume; reading it
back took 2.6-3.3 seconds through the bind mount versus well under half a
second through the named volume. This is a large enough gap to be
user-visible on every build (multi-minute waits vs sub-second), not a
rounding error — it matches the well-documented general behaviour of Docker
Desktop's WSL2 backend, where a bind mount from a Windows-native path crosses
a host↔VM filesystem-sharing boundary for every file operation, while a named
volume is backed by a virtual disk that lives natively inside the same Linux
VM the container runs in.

## What this means for the design

- The store-placement rule should be updated from "the store lives in a named
  volume if a bind mount proves slow" (conditional, pending this experiment) to an
  **unconditional default: on Windows, the store MUST default to a Docker
  named volume, not a bind mount**, when using the container backend. A bind
  mount to a native Windows path is roughly two orders of magnitude slower
  for writes.
- This creates a secondary design question the current text doesn't address:
  a named volume is invisible to Windows Explorer / the host filesystem,
  which cuts against any workflow that expects the store to be a plain
  directory a user can browse, back up, or point other tools at. If that
  matters, the design should say so explicitly and offer a documented escape
  hatch (`--store-dir` forcing a bind mount, with a printed warning about the
  measured cost) rather than silently picking one.
- **A store rooted inside the WSL2 VM's own filesystem** (e.g. a path under
  the user's Linux home directory in the `Ubuntu` WSL distro, as opposed to
  a Windows drive letter) was not tested here but is architecturally exactly
  the case a named volume already covers — both live inside the same VM
  boundary — so it is very unlikely to change this recommendation; it was
  out of scope to test separately given the explicit ask was bind-mount vs
  named-volume on a Windows-native path.
- **macOS is NOT measured** — no macOS hardware was available in this
  environment. Docker Desktop for macOS uses a comparable (but not
  identical) architecture: containers run inside a lightweight VM
  (virtualization.framework, with virtiofs for file sharing on current
  versions) and a bind mount similarly crosses a host↔VM boundary that a
  named volume avoids. The *direction* of the recommendation (prefer a named
  volume, or at minimum benchmark before defaulting to a bind mount) likely
  carries over, but virtiofs on recent Docker Desktop for Mac is reported
  (elsewhere, not measured here) to be substantially faster than WSL2's
  bind-mount path for Windows — so the **magnitude** found here should not
  be assumed for macOS. This needs its own run of
  `hack/experiments/e5-container-io.sh` on macOS hardware before the design
  doc states a number for it; until then, state the Windows number
  (measured) and mark macOS "presumed same direction, not yet measured."
- Recommended default store location to write into the design: **named
  Docker volume** for the container backend on Windows; Linux/macOS local
  backend (no container) is unaffected since there's no host↔VM boundary at
  all.
