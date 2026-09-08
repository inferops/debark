# E7 — Network-postinst heuristic false-positive rate

**Status: complete.** Result: **very low false-positive rate on a real
sample, but not zero** — 5 of 1,749 real packages matched (0.29%); of those
5, 2 are unambiguous genuine network fetches and 3 are one shared false-
positive pattern, all hand-verified against the actual postinst source.
Reported plainly, including the residual ambiguity the heuristic cannot
resolve from text alone.

## Question

`doctor`'s `network-postinst` check greps `preinst`/`postinst`/`prerm`/
`postrm` for signs of network activity (curl, wget, `apt-get update|
install`, `pip install`, `git clone`, `http(s)://` URLs, `nc`, `ssh`,
`add-apt-repository`). Exactly this measurement is what the
check's wording turns on: scan a real sample, and report how many packages
match and what fraction of the matches are genuine. This settles
whether the design's binding wording rule — doctor says **"no obvious
network action found"**, never **"proven safe"** — is doing real work, and
whether the check's false-positive suppressions (comments, echo/message
strings, debconf-style template text) are enough in practice.

## Method

Two live, current package archives, sampled independently in Docker so the
result is not an artefact of one distro's packaging conventions:

- **Debian 12 (bookworm)** — `debian:bookworm-slim`, `apt-cache pkgnames`
  shuffled, `apt-get download` one at a time (sequentially — this batch ran
  for a while in the background across other work in this session, which is
  why it ended up larger than originally requested: **850** packages
  downloaded successfully).
- **Ubuntu 24.04 (noble)** — `ubuntu:24.04`, with `universe` and
  `multiverse` explicitly enabled in `/etc/apt/sources.list.d/ubuntu.sources`
  (not on by default in the base image, and exactly the components with
  looser review than `main`/`restricted`), `apt-cache pkgnames` shuffled,
  `apt-get download` **16-way parallel** via `xargs -P 16`: **900/900**
  requested names downloaded successfully.

Combined sample: **1,750 real `.deb` files**, no synthetic or hand-picked
packages. Every file was scanned with the actual production code path —
`openDebFile` (`core/doctor/deb.go`) + `scanScriptForNetwork`
(`core/doctor/network.go`) — via a gated test,
[`core/doctor/e7_experiment_test.go`](../../core/doctor/e7_experiment_test.go)
(`TestE7NetworkPostinstSample`, skipped unless `DEBARK_E7_SAMPLE_DIR` is
set, so it never runs in ordinary CI):

```bash
DEBARK_E7_SAMPLE_DIR=<dir with debian/ and ubuntu/ subdirs of .deb files> \
  go test ./core/doctor/... -run TestE7NetworkPostinstSample -v
```

One of the 1,750 files (`piglit_0~git20231002-24207f5be-1_amd64.deb`, a
30 MB package) failed to open with `unexpected EOF` reading its
`control.tar` — a corrupted/truncated download from the parallel batch, not
a doctor parsing bug (confirmed: the ar member's declared size did not match
the bytes actually present). **1,749 files were successfully scanned.**

## Raw results

```text
1,750 .deb files found, 1,749 scanned, 0 unscannable, 5 matched (0.29% of scanned)
control.tar compression: gzip=6, none(uncompressed)=14, xz=917, zstd=812
signal counts: curl-or-wget=2, url=4
```

**Every one of the 5 matches, verbatim** (`<script>:<line>: <text>`):

```text
nova-api_2%3a26.2.2-1~deb12u4_all.deb        | postinst:688: pkgos_inifile set ${READ_ADMIN_CRED_CONF_FNAME} ${READ_ADMIN_CRED_SEARCH_SECTION} www_authenticate_uri http://${RET}:5000
octavia-api_11.0.0-2_all.deb                 | postinst:688: pkgos_inifile set ${READ_ADMIN_CRED_CONF_FNAME} ${READ_ADMIN_CRED_SEARCH_SECTION} www_authenticate_uri http://${RET}:5000
trove-common_1%3a18.0.0-2_all.deb            | postinst:688: pkgos_inifile set ${READ_ADMIN_CRED_CONF_FNAME} ${READ_ADMIN_CRED_SEARCH_SECTION} www_authenticate_uri http://${RET}:5000
astrometry-data-2mass-07_1.1build1_all.deb   | postinst:8:   BASE_URL=http://data.astrometry.net/${SERIES}00
astrometry-data-2mass-07_1.1build1_all.deb   | postinst:12:  curl --create-dirs -o "${TARGETDIR}/index-2mass-${STEP}-#1.fits" \
cpl-plugin-vimos-calib_4.1.7+dfsg-2build3_all.deb | postinst:24:  wget -O- ${URL} | tar xzC ${TARGETDIR} ${TAR}
```

## Hand-check: all 5 matches, not a sub-sample

The sample only produced 5 matches, so all 5 (100%) were hand-verified
against the full extracted `postinst` (`dpkg-deb -e`), not just the matched
line.

**2 genuine (true positives) — 2 of 3 distinct packages:**

- **`astrometry-data-2mass-07`** — the *entire* postinst is four lines:
  set `BASE_URL=http://data.astrometry.net/...`, then
  `curl --create-dirs -o ".../index-2mass-...fits" ${BASE_URL}/...`. This
  downloads astronomical index catalogue files from a specific external host
  at install time. Unambiguous: this package **will fail exactly as
  designed to be caught** on an air-gapped target.
- **`cpl-plugin-vimos-calib`** — full postinst: for each of several kit
  suffixes, `URL=ftp://ftp.eso.org/pub/dfs/pipelines/vimos/...`, then
  `wget -O- ${URL} | tar xzC ...`, with a SHA-1 check on the downloaded
  archive before use. This is ESO's VIMOS instrument-pipeline calibration
  data, fetched from ESO's FTP server. Also unambiguous — and note the
  `wget` word alone was the trigger; the URL itself is `ftp://`, which the
  `url` signal's `https?://` pattern does not even match, so this is
  entirely down to keeping "wget" as a bare-word signal per the design's
  list.

**3 false positives — one shared root cause, not three independent bugs:**

- **`nova-api`**, **`octavia-api`**, **`trove-common`** (three unrelated
  OpenStack service packages) all embed the same vendored
  `openstack-pkg-tools` `pkgos_func` shell helper library, verbatim. The
  matched line sits inside a **config-migration** block:

  ```sh
  pkgos_inifile get ${READ_ADMIN_CRED_CONF_FNAME} ${READ_ADMIN_CRED_SEARCH_SECTION} auth_host
  if [ "${RET}" != "NOT_FOUND" ] && [ -n "${RET}" ] ; then
      pkgos_inifile set ${READ_ADMIN_CRED_CONF_FNAME} ${READ_ADMIN_CRED_SEARCH_SECTION} www_authenticate_uri http://${RET}:5000
  fi
  ```

  `${RET}` here is the *return value of the line immediately above it* — a
  value **read out of the package's own already-existing local config
  file** (migrating an old `auth_host` key to a new `www_authenticate_uri`
  key on upgrade), not a network target. No bytes cross any wire; this is
  local text substitution. Confirmed false positive.

**Net read**: of the 5 matches, hand-checking is not probabilistic here (all
were checked) — **2/5 matches (40%) are genuine**, **3/5 (60%) are one
false-positive shape appearing in three copies of the same shared library**.
Looked at by *distinct root cause* rather than by file count, it's 2 real
patterns vs. 1 false one. Zero of the 1,744 non-matching packages needed
hand-checking — silence is silence.

## Why the false positive survives the existing suppressions

The task's named suppressions (comments, echo/printf message strings,
debconf-style heredoc template text) all fire on lines where the network-
looking text is *never executed as a value* — it's prose. The `pkgos_*`
case is different: the line **is** live code, and it **is** assigning what
is textually a URL — the ambiguity is entirely in whether `${RET}` holds a
local or a remote value, which is invisible to a line-of-text scanner and
only resolvable by reading the surrounding control flow (which this specific
case needed: the *previous* line). A mid-development refinement, made after
first seeing this exact match and confirmed against the E7 numbers above,
already narrows one closely related case: `network.go`'s `url` signal now
requires at least one **non-loopback** URL literal on the line —
`http://127.0.0.1:5000/...` (a literal loopback address, also observed in
this same sample, inside a parameter-expansion default) is suppressed, but
`http://${RET}:5000` is not, because `${RET}` is a variable, not a literal,
and nothing in the static text says what it resolves to. That residual gap
is now documented directly in `core/doctor/network.go` rather than chased
further: the two real shapes this experiment actually found
(loopback-literal vs. locally-sourced-variable) are different enough that
closing the second one *safely* — without also swallowing a genuine
`curl $REMOTE_HOST/...` — would need actual shell data-flow analysis, which
is a different (and much bigger) tool than a line scanner, and is not
something this project should build (it is on the do-not-build list: this
stays a heuristic, not a static analyser).

No attempt was made to special-case the `pkgos_*` function names themselves
— that would fix exactly one vendored library's naming convention and
nothing else, which is overfitting to this sample rather than a general
improvement.

## What this means for the design

- **The binding wording rule earns its keep.** A tool that said "verified
  safe" or "no network access" on `nova-api`/`octavia-api`/`trove-common`
  would be making a false claim about specific, real, currently-archived
  packages — not a hypothetical. "No obvious network action found" is
  exactly the right strength of claim, and this experiment is why: the
  check's true job is narrowing attention (2 packages out of 1,749
  genuinely deserved a look; the tool found both, at the cost of 3 extra
  lines an operator can dismiss in seconds by reading the evidence line
  debark already prints), not proving a negative.
- **Precision is good in practice, not just in the abstract.** 1,744/1,749
  (99.7%) of real, current Debian 12 + Ubuntu 24.04 archive packages —
  spanning `main`, `universe`, `restricted` and `multiverse` — correctly
  produce **zero** findings. "A warning on every package is a warning on
  none" (the task's own framing) is the failure mode this result rules out
  for the current implementation.
- **The compression finding mattered more than the false-positive rate.**
  Before xz/zstd decoding was added (`core/doctor/deb.go`), only 20/1,749
  (1.1%) of these real packages' `control.tar` members would have been
  readable at all (gzip=6, uncompressed=14) — the other 98.9% (xz=917,
  zstd=812) would have silently produced **no finding for the wrong
  reason**: not "scanned and clean" but "never actually looked", which is a
  much worse failure mode for a security-adjacent heuristic than a few
  false positives, because it fails silently. `debFile.Unscannable` exists
  specifically so those two situations are never conflated in the `Report`;
  after adding xz/zstd support, `Unscannable` was 0 across the entire
  sample.
- **`ftp://` is out of scope for the `url` signal**, and that's fine —
  `cpl-plugin-vimos-calib` was still caught, via the bare `wget` word. The
  design's signal list already covers this by including the tool names
  independently of the URL-scheme signal.
- No change to the check's binary behaviour (warn, never block) is
  suggested by this data — both false positives and the two true positives
  are `SeverityWarn`, matching the "warn, never refuse" redistribution
  posture extended to this check.

## Reproducing this

The sample itself (1,750 real `.deb` files, ~1.3 GB) is not checked in.
`core/doctor/testdata/fixture-real-xz.deb` is one small (948-byte) file kept
from the Debian half of this sample, as the unit-test fixture that proves xz
decoding works end to end (`TestParseDebReader_RealXZFixture`). zstd has its
own *synthetic* round-trip test instead (`TestDecompressMember_Zstd`):
`klauspost/compress/zstd` can both write and read, so a fixture can be built
on the fly; `github.com/xi2/xz` is decode-only, so xz needed a real sample
file rather than a hand-built one.

```bash
# Debian half
docker run --rm -v "$PWD/sample:/sample" debian:bookworm-slim bash -c '
  apt-get update -qq
  apt-cache pkgnames | sort -R > /tmp/names.txt
  cd /sample && xargs -P 16 -I{} apt-get download {} < /tmp/names.txt'

# Ubuntu half (universe+multiverse enabled first)
docker run --rm -v "$PWD/sample:/sample" ubuntu:24.04 bash -c '
  sed -i "s/^Components: main restricted$/Components: main restricted universe multiverse/" /etc/apt/sources.list.d/ubuntu.sources
  apt-get update -qq
  apt-cache pkgnames | sort -R > /tmp/names.txt
  cd /sample && xargs -P 16 -I{} apt-get download {} < /tmp/names.txt'

DEBARK_E7_SAMPLE_DIR=./sample go test ./core/doctor/... -run TestE7NetworkPostinstSample -v
```
