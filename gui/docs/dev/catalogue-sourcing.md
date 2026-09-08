# Where the catalogue's package list comes from

**Decision, taken at the outset. Revised once — read the revision note at the
bottom, it matters.** Binding on all four catalogue packages: `packages.go`,
`dep11.go`, `cache.go` and `search.go` / `catalog.go`.

## The question

The picker shows ~70,000 rows drawn from *the target's own* apt indexes — not
from whatever the builder machine happens to have configured. Two ways:

- **A. Fetch the indexes directly over HTTPS.** Work out the archive URIs,
  suites and components from the target's sources, and download
  `dists/<suite>/<component>/binary-<arch>/Packages.gz` and the matching DEP-11
  ourselves.
- **B. Reuse debark's apt machinery.** Materialise the private apt root
  debark knows how to build, let `apt-get update` fetch the indexes, and read
  them back with the exported `apt.ReadPackagesIndex`.

## The decision: A

### What decided it

Both options need to parse apt sources. That is the fact the first version of
this document got wrong.

`base.Definition.Sources` is a **`string`** — a raw deb822 document
(`Types:`/`URIs:`/`Suites:`/`Components:`/`Signed-By:`), not structured data
(`core/base/base.go:101`). A snapshot's sources are likewise raw captured
*files* (`snapshot.APT.Sources[i].ArchivePath` plus a digest), never parsed
URIs. And `core/apt`'s own sources parser is unexported, and even internally
extracts only URIs and `trusted=` — never suites or components.

So there is no route, under either option, that gets suites and components
without parsing deb822. Option B avoids the parse only for the *reading* step
(`ReadPackagesIndex` recovers suite and component from index filenames) — but
something still has to know which archives to configure before apt can fetch
anything.

With the parser required either way, the remaining differences all favour A:

- **B cannot work on Windows, and is heavy on Linux.** It needs a working
  `apt-get`, natively or in a container. Making "browse a list of packages"
  depend on materialising an apt root and running `apt-get update` in a
  container is a large cost for a browsing index — and it puts a container
  between the operator and the first useful screen in the app.
- **A gives honest, granular progress.** Fetching a known set of index files
  yields real per-file byte counts and a cancellable job with phases. B gives
  one opaque `apt-get update` subprocess.
- **A avoids two traps B walks into**: the lists directory is not exported
  (four call sites in `core/apt` hardcode
  `filepath.Join(root.Dir, "var", "lib", "apt", "lists")`), and
  `apt.NewRunner` cannot carry `APT_CONFIG` — it actively strips it — so
  honouring a snapshot's captured `apt.conf.d` would require implementing our
  own `apt.Runner` anyway.

### Why this is not the forbidden second implementation

Contract-brief rule 2 draws the line: *"Parsing a `Packages` index to show the
operator a list is catalogue work and is fine. Parsing it to decide what gets
installed is engine work and is forbidden."*

The catalogue **lists**. `debark build` **decides**. Nothing in
`internal/catalog` may compare two version strings or reason about a
dependency, and `Entry.Version` is display-only.

The blast radius is what makes this safe. If the catalogue's view of a
target's sources is imperfect — it misses a third-party flat repo, say — the
consequence is a **missing row in a picker**. The operator can still add that
`.deb` by URL, and `debark build` still performs the real resolution against
the real apt with the real sources. A catalogue bug cannot produce a wrong
bundle. That asymmetry is why a modest amount of parsing is acceptable here and
would not be acceptable anywhere near the build path.

### Scope limits, taken deliberately

- **The catalogue is not a trust boundary.** It does not verify archive
  signatures and ships no keyrings; `Source` carries no `Signed-By`, because
  recording one would imply otherwise. debark verifies everything that is
  actually installed.
- `InRelease` is not fetched — the detached `Release` is smaller and this
  package verifies nothing.
- `deb-src` stanzas and flat repositories (a suite ending in `/`) are skipped:
  a flat vendor repo has no components and no DEP-11, and the operator reaches
  those `.debs` by URL, on a different screen.

### The one thing to be careful about

The deb822 parser is the only duplicated logic in this repository. Confine it
to one file, test it hard against **real** captured sources — both the deb822
`.sources` form and the older one-line `sources.list` form with its
`[arch=… signed-by=…]` options — and when a source line cannot be parsed
confidently, **surface it** rather than silently dropping it. A silently
ignored source is a silently incomplete catalogue, which is the one failure
mode here that an operator cannot see.

## Core-repo gaps this uncovered

Recorded because the definition of done permits core changes only where the CLI
genuinely lacked something, each as its own commit with tests. **None of these
are needed under option A** — they are noted so a future reader knows they were
found and why they were not acted on:

1. `(*PrivateRoot).ListsDir()` — an accessor for a path four call sites already
   hardcode.
2. An exported way to run apt with `APT_CONFIG` set (`withAptConfig` is
   unexported and `execRunner.Run` strips the variable).
3. Exported source parsing that yields URI, suite and component. This is the
   one that would genuinely help this repository — it would delete our only
   duplicated logic. Worth proposing upstream once the parser here has proved
   itself against real-world sources.

## Revision note

The first version of this document chose **B**, on the reasoning that A would
mean re-implementing debark's sources parsing. That reasoning was wrong in
its central fact: it assumed B avoided the parse entirely, and it had not
checked that `base.Definition.Sources` is a raw string. Once both options are
seen to need the same parser, B's remaining cost — an apt root and a container
between the operator and the first screen, and no Windows support at all — is
not worth paying for a browsing index whose worst failure is a missing row.

W0-C had already designed the frozen `Target`/`Source`/`IndexRef` types around
A, independently, before this was resolved. That design stands.

**Deviation from the brief, stated plainly:** the implementation plan says the
catalogue packages should "fetch and parse `Packages` indexes from the
target's private apt root". This document departs from that wording
deliberately, for the reasons above. If the private-apt-root route is wanted
regardless — for a correctness property
this analysis has missed — that is a reasonable call to make, and the frozen
`IndexRef` type is the only thing that would need to change.
