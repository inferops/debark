# testdata — real index excerpts, with provenance

Every file here is a **byte-exact slice of a real archive index**. Nothing was
hand-written, hand-edited, minimised, or synthesised. Stanzas and YAML
documents were selected by name from the parent file and copied verbatim; the
only transformation is selection.

That matters because these fixtures exist to prove parsers survive what the
archive actually ships — a 70,830-byte field value, a duplicate YAML key, a
package name that occurs twice. A tidied fixture proves nothing.

The measured characterisation of these formats is in
`../docs/dev/index-formats.md`. Re-fetch the parent corpus with:

```
go run ../hack/fetch-indexes.go -out <dir-outside-the-repo> -icons
```

**Corpus fetched: 2026-09-06.** Ubuntu noble and Debian bookworm are frozen
release pockets, so the parent files should still hash identically. If a
SHA-256 below no longer matches, the archive re-published the pocket — treat
that as a signal to re-cut the fixtures, not to edit them in place.

---

## Ubuntu 24.04 LTS (noble), amd64

Mirror: `http://archive.ubuntu.com/ubuntu/`

### `ubuntu-noble-universe-Packages.excerpt`

- **Cut from:** `dists/noble/universe/binary-amd64/Packages`
- **Source URL:** <http://archive.ubuntu.com/ubuntu/dists/noble/universe/binary-amd64/Packages.gz>
- **Parent SHA-256 (uncompressed):** `687ffc969e7700137677b9cfe2f614ee2d638052a91a7c9bf31f3f6a3040a99a`
- **Parent SHA-256 (`.gz` as fetched):** `ce57756edcce0a5f7993ceb89057e1510148e04e3b85a066d780e4bc00485c8a`
- **Parent size:** 73,379,142 bytes (19,315,644 compressed), 64,755 stanzas
- **Excerpt:** 52 stanzas, 165,693 bytes, 49 distinct field names

Chosen for the awkward cases:

| Package | Why it is here |
|---|---|
| `librust-winapi-dev` | `Provides:` is **70,830 bytes on one line** — a default `bufio.Scanner` fails with `bufio.ErrTooLong` on this stanza |
| `libmono-cil-dev` | longest `Depends:` in the corpus, 8,637 bytes |
| `android-platform-frameworks-native-headers` | **appears twice**, at `1:10.0.0+r36-1` and `1:34.0.4-1build3`, from two different sources |
| `btm`, `git-delta` | folded `X-Cargo-Built-Using:` — continuation lines |
| `adwaita-qt`, `agda-stdlib-doc`, `apertium-nno-nob` | non-ASCII in `Description:` |
| `liblucene++0t64`, `revu-tools`, `sbuild-launchpad-chroot` | non-ASCII in `Maintainer:` |
| `librust-dirs-next-dev` | longest `Description:` short line, 348 bytes |
| `aggregate` | `Tag:` (rare on Ubuntu, common on Debian) |
| `bamf-dbg` | `Build-Ids:` |
| `certspotter`, `hugo` | `Static-Built-Using:` |
| `gstreamer1.0-libav` | `Gstreamer-Elements/Decoders/Encoders/Version` |
| `rtl8812au-dkms` | `Modaliases:` |
| `lua-argparse`, `batalert` | `Lua-Versions:`, `Ruby-Versions:` |
| `libghc-prettyprinter-convert-ansi-wl-pprint-dev` | `Ghc-Package:` |
| `python3-biomaj3-process` | `Python-Egg-Name:` |
| `golang-github-svent-go-nbreader-dev` | `Go-Import-Path:` |
| `libcleri1-dbgsym` | `Auto-Built-Package:` |
| `node-buble` | `Javascript-Built-Using:` |
| `kawari8` | `Python-Version:` |
| `python3.12-nopie` | `Cnf-Visible-Pkgname:` |
| `0ad-data` … `a56` (24 leading stanzas) | ordinary rows; `Multi-Arch:` on 16, `Provides:` on 9, `universe/` section prefixes |

### `ubuntu-noble-main-Packages.excerpt`

- **Cut from:** `dists/noble/main/binary-amd64/Packages`
- **Source URL:** <http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages.gz>
- **Parent SHA-256 (uncompressed):** `8f6f71ae839c8cba390a7643fcbbdacddb0bc7d12c1583a2dd80a1f8443a30e5`
- **Parent SHA-256 (`.gz` as fetched):** `e0d7e4cbb09d2aa7f9e104a1488817417bc9d85f3e5d9a21156a52ec641ae531`
- **Parent size:** 7,165,069 bytes (1,808,488 compressed), 6,099 stanzas
- **Excerpt:** 31 stanzas, 54,787 bytes

Covers the Ubuntu-specific and rarest fields: `Task:` (`accountsservice`),
`Essential:` (`base-files`, `bash`, `coreutils`, `dpkg`), `Protected:`
(`boot-managed-by-snapd`), `Important:` (`e2fsprogs`), `Efi-Vendor:`
(`grub-efi-amd64-bin`), `Gstreamer-*` (`gstreamer1.0-alsa`), `Build-Ids:`
(`libc6-dbg`, 11,192 bytes), `Postgresql-Catversion:` (`postgresql-16`),
`Ubuntu-Oem-Kernel-Flavour:` and `Modaliases:` (`oem-qemu-meta`),
`Cnf-Extra-Commands:` (`default-jdk`), `Cnf-Ignore-Commands:`
(`python-dev-is-python3`), `X-Cargo-Built-Using:` (`mdevctl`),
`Original-Vcs-Git/Browser:` (`glibc-doc`), `Enhances:` (`qemu-block-extra`),
`Ruby-Versions:` (`rake`), plus non-ASCII `Description:` (`gnome-themes-extra`,
`language-pack-gnome-nb`) and bare (unprefixed) `Section:` values.

### `ubuntu-noble-main-Translation-en.excerpt`

- **Cut from:** `dists/noble/main/i18n/Translation-en`
- **Source URL:** <http://archive.ubuntu.com/ubuntu/dists/noble/main/i18n/Translation-en.gz>
- **Parent SHA-256 (uncompressed):** `198c6729ca7c0a287ba391f310d6f8c323e6945b7364e7f69182f88069947e12`
- **Parent SHA-256 (`.gz` as fetched):** `6b4c6b6175b81aa3f4b3f301bfcbd50a86b9271a95628e8e61f51f32eebd38b9`
- **Parent size:** 3,095,169 bytes (721,109 compressed), 6,370 stanzas
- **Excerpt:** 25 stanzas, 13,584 bytes

**This is where folded `Description` parsing is actually tested.** `Packages`
never folds `Description:` (measured: 0 of 70,854 stanzas), so a parser
exercised only against `Packages` never runs its continuation-line path for
descriptions. All 25 stanzas here contain the ` .` paragraph-break marker.

Three fields only: `Package`, `Description-md5`, `Description-en`. The
`Description-md5` values join to the same field in the `Packages` excerpts —
`accountsservice`, `acct`, `acl`, `apt`, `base-files`, `bash`, `coreutils`,
`dpkg`, `libc6`, `perl`, `python3-heat`, `casper`, `postgresql-16`,
`grub-efi-amd64-bin`, `gstreamer1.0-alsa`, `qemu-block-extra`, `glibc-doc`,
`rake`, `mdevctl`, `util-linux`, `e2fsprogs` and others appear in both, so the
join can be unit-tested end to end.

### `ubuntu-noble-main-Components-amd64.yml.excerpt`

- **Cut from:** `dists/noble/main/dep11/Components-amd64.yml`
- **Source URL:** <http://archive.ubuntu.com/ubuntu/dists/noble/main/dep11/Components-amd64.yml.gz>
- **Parent SHA-256 (uncompressed):** `ca96739892d2e5861f01f4d397639eb01e59ceb1a3ba872bf3ce749903c6f661`
- **Parent SHA-256 (`.gz` as fetched):** `2b0bae7a028eae95be48da94c11bae3c77faf062b82a2aedaaab64d76a1bc50b`
- **Parent size:** 1,923,546 bytes (664,565 compressed), 1 header + 94 components
- **Excerpt:** 1 header document + 32 component documents, 107,851 bytes

The header document is preserved verbatim and first (`File: DEP-11`,
`Version: '0.14'`, `Origin: ubuntu-noble-main`, `MediaBaseUrl`, `Time`).

Coverage: 7 of the 9 `Type` values — `desktop-application` (9), `font` (11),
`inputmethod` (4), `generic` (3), `addon` (2), `codec` (2),
`console-application` (1). Specifically:

- **Several `Categories`:** `libreoffice-draw.desktop` has 5; `info.desktop`
  and `htop.desktop` have 3.
- **Localised names:** every component has locale-keyed `Name`/`Summary`.
  `htop.desktop` has 29 `Summary` locales including `sr@latin`,
  `sr@ijekavianlatin`, `en_GB`, `pt_BR`, `zh_TW` — and its `C` key is 19th,
  not first.
- **No `Icon`:** 12 components, including `gstreamer1.0-gtk3`,
  `org.freedesktop.fwupd`, `org.a11y.brltty`,
  `io.github.feralinteractive.gamemode` and the `evince-*` addons.
- **All three `Icon` kinds:** `cached` (20), `remote` (19), `stock` (9).
  `cached`/`remote` are lists of maps; `stock` is a plain string.
- **`Extends`:** 2 addon components (`evince-xpsdocument`,
  `evince-tiffdocument`) pointing at another component's `ID`.
- **Smallest real component:** `gstreamer1.0-gtk3` at 173 bytes — `Type`,
  `ID`, `Package`, `Name`, `Summary` and nothing else.

### `ubuntu-noble-universe-Components-amd64.yml.excerpt`

- **Cut from:** `dists/noble/universe/dep11/Components-amd64.yml`
- **Source URL:** <http://archive.ubuntu.com/ubuntu/dists/noble/universe/dep11/Components-amd64.yml.gz>
- **Parent SHA-256 (uncompressed):** `bfea6e19b6ca87e10ebbc0b0a8b03a7219f37e38437ef7dd21406c40c70c9f96`
- **Parent SHA-256 (`.gz` as fetched):** `e49d4232fe79502caab4e9cfb7990c8a48a7b6f76ad1a2370be9e90068527a1c`
- **Parent size:** 18,564,799 bytes (5,942,598 compressed), 1 header + 2,493 components
- **Excerpt:** 1 header document + 1 component document, 44,166 bytes

**One component, on purpose.** `org.gnome.Klotski` (package `gnome-klotski`)
declares the locale key `ca_ES` **twice** in its `Keywords` map — at lines
59,572 and 59,702 of the parent file, lines 595 and 725 of this excerpt.
`gopkg.in/yaml.v3` reports this as `*yaml.TypeError`:

```
line 725: mapping key "ca_ES" already defined at line 595
```

That error is **soft** — the decoder has populated what it could and the stream
is still correctly positioned — but a parser that treats any non-nil error as
fatal stops here. In the real `universe` file that means stopping at component
449 of 2,587 and silently losing 83% of the applications. A test that decodes
this fixture and asserts both documents are read is the regression guard for
that bug. Note `Name["C"]` survives the error; `Keywords` comes back empty.

The component is 44 KB because of the locale explosion. That is large for a
single component — the median `universe` component is 3,438 bytes and the 75th
percentile 7,756 — but it is not exotic: 83% of DEP-11's bytes are non-English
locale strings, so a well-translated GNOME application lands here. It is the
only component in this excerpt precisely because one 44 KB document is the
whole point of the file.

### `ubuntu-noble-Release.excerpt`

- **Cut from:** `dists/noble/Release`
- **Source URL:** <http://archive.ubuntu.com/ubuntu/dists/noble/Release>
- **Parent SHA-256:** `26758a13cecfaff9ff274d31ea9a4633674a999bd130c09f6ecd66a4b8071184`
- **Parent size:** 254,968 bytes
- **Excerpt:** 2,327 bytes

The header fields verbatim (`Origin`, `Label`, `Suite`, `Version`, `Codename`,
`Date`, `Architectures`, `Components`, `Description`), then a `SHA256:` section
reduced to the 18 entries this application actually reads — `Packages`,
`Components-amd64.yml` and `Translation-en` for `main` and `universe`, in
plain, `.gz` and `.xz` form. The `MD5Sum:` and `SHA1:` sections were dropped
whole; the remaining lines are unmodified.

Those digests and sizes are the cache key material, and they cross-check the
corpus: the listed size for `main/binary-amd64/Packages` (7,165,069) and its
SHA-256 match the file `fetch-indexes.go` downloaded exactly.

The signed variant, `dists/noble/InRelease` (255,850 bytes, SHA-256
`cdb2f31d809f589719a53c6ad15f255b27569c4059542ada282aaa21b8e164b0`), is the
same content wrapped in an inline PGP signature. It is fetched into the corpus
but not committed here: signature verification is `debark`'s job, not this
repository's.

---

## Debian 12 (bookworm), amd64

Mirror: `http://deb.debian.org/debian/`

Present so the parsers are not accidentally Ubuntu-shaped. Debian differs in
field order, ships `Tag:` on 47.77% of stanzas (Ubuntu: 0.01%), almost never
ships `Original-Maintainer:`, uses bare `Section:` names throughout, and its
DEP-11 declares a different format `Version`.

### `debian-bookworm-main-Packages.excerpt`

- **Cut from:** `dists/bookworm/main/binary-amd64/Packages`
- **Source URL:** <http://deb.debian.org/debian/dists/bookworm/main/binary-amd64/Packages.gz>
- **Parent SHA-256 (uncompressed):** `515e692f2c4121c6fcec444ef100cc18f79a991910615f3a88c8b7becfc94d2f`
- **Parent SHA-256 (`.gz` as fetched):** `6777ea16514725f9427f48c240d92e0a6b8496138e37a1491d8597e66774e309`
- **Parent size:** 50,060,337 bytes (12,084,798 compressed), 63,440 stanzas
- **Excerpt:** 33 stanzas, 35,509 bytes

| Package | Why it is here |
|---|---|
| `0ad`, `0xffff`, `2048-qt`, `2ping`, `3dchess`, `3depict` | **folded `Tag:`** — the multi-line continuation case Ubuntu's index does not exercise |
| `linux-doc`, `linux-source` | each **appears twice**, at `6.1.170-3` and `6.1.176-1` — the duplicate-name case, in its Debian form |
| `acme`, `acpi-call-dkms`, `advancecomp`, `aegisub`, `aephea`, `ahcpd` | non-ASCII `Maintainer:` |
| `adwaita-qt`, `libadwaitaqt1` | non-ASCII `Description:` |
| `libmono-cil-dev` | longest `Depends:`, 8,795 bytes (Debian's copy) |
| `0ad-data` | `Size: 1377557908` — a `.deb` over 1 GB, and `Installed-Size: 3218736` |
| `base-files`, `bash`, `coreutils`, `dpkg`, `libc6` | `Essential:`, `Multi-Arch:`, ordinary rows |

`librust-winapi-dev` is deliberately **not** here despite having Debian's
longest field (75,639 bytes) — the Ubuntu excerpt already carries that case,
and duplicating it would add 75 KB to the tree for no extra coverage.

### `debian-bookworm-main-Components-amd64.yml.excerpt`

- **Cut from:** `dists/bookworm/main/dep11/Components-amd64.yml`
- **Source URL:** <http://deb.debian.org/debian/dists/bookworm/main/dep11/Components-amd64.yml.gz>
- **Parent SHA-256 (uncompressed):** `90ae5d7226b1b80d83ee47edcf09b28eecd11249d6dd1eaf32d65da509f981e6`
- **Parent SHA-256 (`.gz` as fetched):** `dfaa966b65461cd7f5af43c182fd32ed58deb07cfcdb3f5f23f41a962b2a8c79`
- **Parent size:** 20,255,642 bytes (6,949,579 compressed), 1 header + 2,381 components
- **Excerpt:** 1 header document + 12 component documents, 10,759 bytes

The header declares `Version: '0.16'` and
`MediaBaseUrl: https://appstream.debian.org/media/bookworm` — **different from
Ubuntu's `'0.14'` and `appstream.ubuntu.com`**. A parser that hard-codes either
value, or decodes `Version` as a number rather than the quoted string it is,
fails on one distribution or the other.

Includes `eog-exif-display` (an `addon` with `Extends`), several small
`desktop-application` entries with 3–4 `Categories`
(`io.github.sanger_pathogens.artemis.bamview`, `vdr-wlfe.desktop`,
`bcnc.desktop`, `com.kroah.usbview`, `org.mrpt.rawlogviewer.desktop`), a
`codec` (`gstreamer1.0-gl`), and the smallest components in the file
(`org.pwmt.zathura-cb` at 224 bytes).

---

## How these were cut

Selection was by `Package:` name (control files) or `ID:` (YAML), copying whole
stanzas and whole documents. No field was reordered, truncated, reflowed or
re-encoded; line endings are the archive's own LF. The YAML excerpts keep the
parent's header document as document 0 so they remain valid DEP-11 streams.

Total directory size: 434,676 bytes. Keep it under 1 MB — if a new fixture
would push past that, cut a smaller slice rather than compressing, since a
compressed fixture cannot be read in a diff.
