# Prototype baseline

The Bash prototype (`download-packages.sh`) that debark replaces is the
behavioural reference for the Go implementation. **It is not in this
repository** — it predates it and was never published — so what is preserved
here is a recorded run of it, letting a package compare against what actually
worked rather than against a description of it. The captured output is
checked in, at `testdata/baseline/bundle-metadata/`; the numbers below come
from it and stand on their own without the prototype in hand.

If you do have a copy of the prototype, `testdata/baseline/run-prototype.sh`
is the script that produced this run. It expects a workspace directory
holding both `download-packages.sh` and this repository, mounted at `/work`:

```bash
MSYS_NO_PATHCONV=1 docker run --rm -v /path/to/workspace:/work debian:bookworm-slim \
    bash /work/debark/testdata/baseline/run-prototype.sh
```

## The run

Debian 12 target snapshot (`testdata/real-targets/debian-12-state.tar.gz`),
request `jq` and `tree`, resolved in a `debian:bookworm-slim` container with
apt 2.6.1 on 2026-09-03. Result: **4 packages** — `jq`, `libjq1`, `libonig5`,
`tree` — which is the correct closure for that target's installed set, and the
exact behaviour the product exists to deliver.

## Two findings the Go implementation must act on

**1. The prototype produces no `Release` file in a slim container.** It calls
`apt-ftparchive`, which is not installed in `debian:bookworm-slim` (it lives in
`apt-utils`), and silently falls back to generating `Packages` only. The bundle
therefore ships an unsigned, `Release`-less flat repository that only works
because the installer adds the source with `[trusted=yes]`.

This is precisely the case for the pure-Go writer (ADR-011): debark generates
`Packages`, `Packages.gz` **and** `Release` itself, with no dependency on
`apt-utils` being present on the builder, and the `Release` digests are covered
by the signed manifest. A container-based builder is the normal case, so a
generator that only works when `apt-utils` happens to be installed is not
acceptable.

**2. The prototype's pool is flat.** `Filename: ./jq_1.6-2.1+deb12u2_amd64.deb`
with every `.deb` in one `debs/` directory. debark writes
`repo/pool/<prefix>/<pkg>/<file>` and a `Filename:` relative to the repository
root. Both work with apt; the pool layout is chosen so a bundle with
thousands of packages stays navigable and so the layout matches what every
Debian mirror looks like. Migration note for prototype users: the flat `debs/`
directory is readable by the new tool as an input, but new bundles use the pool.

## What stays the same

- The private-root resolution idiom, option for option.
- The list-file syntax, so an existing `packages.txt` keeps working.
- The installer's private apt view, which measured ~6.5 s for 74 packages /
  40 MB and does not need optimising.
