package apt

import (
	"fmt"
	"strings"
)

// DroppedConfEntry records one apt.conf.d setting that was removed while
// materialising a private root, and why, so the lock can report exactly what
// was withheld from the target's captured configuration.
type DroppedConfEntry struct {
	// File is the snapshot ArchivePath of the apt.conf.d (or apt.conf) file
	// the entry came from.
	File string
	// Key is the entry's full dotted path, e.g. "Dir::Cache::archives" or
	// "Acquire::http::Proxy".
	Key string
	// Reason is a one-line human explanation.
	Reason string
}

// filterAptConf parses one captured apt.conf / apt.conf.d file and returns
// the part of it a private root is allowed to carry, plus a record of
// everything withheld.
//
// This is an ALLOW-LIST: a setting survives only if it is at or under an
// aptConfAllowList entry (or is the operator-opted-in proxy family), and
// everything else is dropped and recorded. It replaces a deny-list of the
// families known to be dangerous, which fails open by construction — the
// next apt release that adds an executable or verification-weakening key
// reopens the hole with no code change and no signal. That is precisely how
// the Pre-Invoke family got through: the deny-list had already anticipated
// the obvious "swap the verifier binary" attack (Dir::Bin::gpgv) and had
// simply never heard of the hooks. An allow-list cannot fail that way,
// because a key nobody has thought about is a key nobody has allowed.
//
// The input this runs on is untrusted by construction: a snapshot is produced
// on another machine and carried across the air gap on removable media, its
// apt.conf.d fragments are copied into the private root, and
// writeAptConfigLoader points APT_CONFIG at that directory precisely so apt
// really reads them (E4 established that is the only mechanism that works —
// -o Dir::Etc::parts= is silently a no-op). Every resolve then runs apt-get
// update against it. Confirmed against apt 2.6.1 with a fragment of exactly
// the shape the old filter preserved: the hook ran as root and apt-get update
// still exited 0, so nothing downstream noticed. "Unknown key" therefore has
// to mean "not carried", not "carried and hope".
//
// What this trades away is fidelity, which is the product's core promise: a
// captured setting that genuinely changes what apt selects, and that nobody
// has put on the allow-list yet, will not reach the resolving apt. The
// mitigation — the entire mitigation — is that the loss is REPORTED. Every
// drop goes through DroppedConfEntry, which root.go turns into a
// private-root.apt-conf-dropped warning naming the key, the file it came from
// and why it went. A silent drop here would be a regression, not a fix; an
// operator reading the lock can see exactly which of the target's settings
// did not make it, and say whether it mattered.
//
// There is deliberately NO passthrough escape hatch, and none should be
// added. A --apt-conf-passthrough flag would be set by whoever wanted the
// dropped-key warning to stop, which hands back the exact vulnerability this
// closes, at the moment someone has decided not to think about it. The remedy
// for a real fidelity loss is to add that one key to aptConfAllowList: a
// small, reviewable change that arrives with its reasoning attached.
//
// Directives (#include, #clear) are dropped and recorded for the same reason:
// #include names a path that exists on the target, not on the builder, and
// #clear removes configuration rather than adding it, so the only thing it
// could ever clear is something the allow-list already decided to carry. A
// "#" line that is neither is a comment, and is discarded without a warning —
// see classifyDirective for why reporting those actively damaged the report.
//
// What survives is preserved semantically and re-serialised in a consistent
// style; original comments and exact formatting are not preserved, but no
// surviving value or nesting is changed.
func filterAptConf(file string, data []byte, allowProxy bool) ([]byte, []DroppedConfEntry) {
	toks := tokenizeAptConf(data)
	pos := 0
	items := parseAptConfItems(toks, &pos)
	var dropped []DroppedConfEntry
	items = filterAptConfItems(items, nil, allowProxy, file, &dropped)
	var b strings.Builder
	serializeAptConfItems(&b, items, 0)
	return []byte(b.String()), dropped
}

// --- tokenizer -------------------------------------------------------------

type confTokKind byte

const (
	tokWord confTokKind = iota
	tokString
	tokOpen
	tokClose
	tokSemi
	tokDirective // a "#..." line, kept as raw text for pass-through
)

type confTok struct {
	kind confTokKind
	text string
}

// tokenizeAptConf lexes apt.conf syntax: '//' line comments, '{' '}' ';',
// double-quoted strings with backslash escapes, bare words, and '#...' lines
// (apt's #include/#clear directives — distinct from '//' comments).
func tokenizeAptConf(data []byte) []confTok {
	var toks []confTok
	i, n := 0, len(data)
	for i < n {
		c := data[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case c == '/' && i+1 < n && data[i+1] == '/':
			for i < n && data[i] != '\n' {
				i++
			}
		case c == '{':
			toks = append(toks, confTok{kind: tokOpen})
			i++
		case c == '}':
			toks = append(toks, confTok{kind: tokClose})
			i++
		case c == ';':
			toks = append(toks, confTok{kind: tokSemi})
			i++
		case c == '"':
			j := i + 1
			var sb strings.Builder
			for j < n && data[j] != '"' {
				if data[j] == '\\' && j+1 < n {
					sb.WriteByte(data[j+1])
					j += 2
					continue
				}
				sb.WriteByte(data[j])
				j++
			}
			toks = append(toks, confTok{kind: tokString, text: sb.String()})
			if j < n {
				j++ // skip closing quote
			}
			i = j
		case c == '#':
			j := i
			for j < n && data[j] != '\n' {
				j++
			}
			toks = append(toks, confTok{kind: tokDirective, text: strings.TrimRight(string(data[i:j]), "\r")})
			i = j
		default:
			j := i
			for j < n {
				ch := data[j]
				if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' || ch == '{' || ch == '}' || ch == ';' || ch == '"' {
					break
				}
				if ch == '/' && j+1 < n && data[j+1] == '/' {
					break
				}
				j++
			}
			if j == i {
				i++ // stray byte (e.g. a lone '/'); skip it rather than loop forever
				continue
			}
			toks = append(toks, confTok{kind: tokWord, text: string(data[i:j])})
			i = j
		}
	}
	return toks
}

// --- parser ------------------------------------------------------------

// confItem is one statement in an apt.conf block: either a directive
// (Directive != ""), a bare value inside a value-list block (Key == "",
// HasValue == true), or a "Key value;" / "Key { ... };" entry.
type confItem struct {
	Directive string
	Key       string
	Value     string
	HasValue  bool
	Block     []confItem
	IsBlock   bool
}

func parseAptConfItems(toks []confTok, pos *int) []confItem {
	var items []confItem
	for *pos < len(toks) {
		t := toks[*pos]
		switch t.kind {
		case tokClose:
			return items
		case tokDirective:
			*pos++
			items = append(items, confItem{Directive: t.text})
		case tokString:
			*pos++
			skipTok(toks, pos, tokSemi)
			items = append(items, confItem{Value: t.text, HasValue: true})
		case tokWord:
			key := t.text
			*pos++
			switch {
			case *pos < len(toks) && toks[*pos].kind == tokOpen:
				*pos++
				block := parseAptConfItems(toks, pos)
				skipTok(toks, pos, tokClose)
				skipTok(toks, pos, tokSemi)
				items = append(items, confItem{Key: key, Block: block, IsBlock: true})
			case *pos < len(toks) && (toks[*pos].kind == tokWord || toks[*pos].kind == tokString):
				val := toks[*pos].text
				*pos++
				skipTok(toks, pos, tokSemi)
				items = append(items, confItem{Key: key, Value: val, HasValue: true})
			default:
				skipTok(toks, pos, tokSemi)
				items = append(items, confItem{Key: key})
			}
		default:
			// Stray ';' or unmatched '{' at this level: skip defensively so a
			// slightly malformed captured file never aborts the whole parse.
			*pos++
		}
	}
	return items
}

func skipTok(toks []confTok, pos *int, kind confTokKind) {
	if *pos < len(toks) && toks[*pos].kind == kind {
		*pos++
	}
}

// --- filter --------------------------------------------------------------

func filterAptConfItems(items []confItem, path []string, allowProxy bool, file string, dropped *[]DroppedConfEntry) []confItem {
	out := make([]confItem, 0, len(items))
	for _, it := range items {
		if it.Directive != "" {
			// Dropped either way; only a real directive is worth a warning.
			if key, reason, report := classifyDirective(it.Directive); report {
				*dropped = append(*dropped, DroppedConfEntry{File: file, Key: key, Reason: reason})
			}
			continue
		}
		if it.Key == "" {
			out = append(out, it) // bare value item; filtering happens at its parent key
			continue
		}
		// it.Key is the raw token as written, which for the common flat
		// single-line style is itself a whole dotted path ("Dir::Cache::
		// pkgcache", not three nested block levels) — it must be split
		// before joining the path, or a flat "Dir::..." key never matches
		// path[0]=="Dir" below.
		newPath := make([]string, 0, len(path)+2)
		newPath = append(newPath, path...)
		newPath = append(newPath, splitConfKeyPath(it.Key)...)
		if reason, drop := dropConfReason(newPath, it, allowProxy); drop {
			*dropped = append(*dropped, DroppedConfEntry{File: file, Key: strings.Join(newPath, "::"), Reason: reason})
			continue
		}
		if it.IsBlock {
			had := len(it.Block)
			it.Block = filterAptConfItems(it.Block, newPath, allowProxy, file, dropped)
			// A block kept only because an allowed key could live somewhere
			// underneath it (classifyConfKey's confKeyContainer) can end up
			// with every child dropped. Emitting the husk — "APT {};" — would
			// write a key nothing carried into the file apt actually reads.
			// Each dropped child is already in *dropped, so pruning the husk
			// loses no report.
			if had > 0 && len(it.Block) == 0 {
				continue
			}
		}
		out = append(out, it)
	}
	return out
}

// splitConfKeyPath splits one confItem key into its dotted-colon path
// segments, e.g. "Dir::Cache::pkgcache" -> ["Dir","Cache","pkgcache"], and
// "APT::Architectures::" (the list-append marker) -> ["APT","Architectures"].
func splitConfKeyPath(key string) []string {
	trimmed := strings.TrimSuffix(key, "::")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "::")
}

// classifyDirective classifies one "#..." line. Every one of them is dropped;
// what differs is whether the drop is worth reporting.
//
// apt defines exactly two directives, and both change configuration, so both
// are recorded: #include names a path that exists on the target and not on
// the builder, and #clear removes configuration rather than adding it — under
// an allow-list the only thing it could clear is something the allow-list had
// already decided to carry, so honouring it can only ever subtract fidelity,
// never add any.
//
// Every OTHER "#" line is a comment, and is discarded as quietly as a "//"
// one. That apt accepts such lines is not an assumption: a real lock from a
// stock Docker target recorded 12 of them in the captured
// etc/apt/apt.conf.d/docker-clean alone — keyed "#", so the first field was
// "#" and not "#include" — and that file ships on every Debian and Ubuntu
// image, where apt-get update reads it without complaint.
//
// Reporting comments as withheld settings is what that same lock shows the
// cost of: 34 of its ~48 apt-conf-dropped warnings were comment lines, all
// keyed "#", burying the five that actually mattered (two Dir overrides and
// two command hooks, including Docker's own DPkg::Post-Invoke). Since the
// report IS the mitigation for everything this filter withholds, drowning it
// is a defect in the mitigation and not a cosmetic one — and a comment
// carries no setting, so staying quiet about it withholds nothing an operator
// could have acted on.
//
// If apt ever defines a third directive it must be added here, or its drop
// goes unreported. That is the same maintenance contract as aptConfAllowList,
// with the same remedy: one small, reviewable entry. Note the asymmetry that
// makes it a safe one — an unrecognised "#" line is still DROPPED, so this
// judgement can cost a warning, never the security property.
func classifyDirective(directive string) (key, reason string, report bool) {
	fields := strings.Fields(directive)
	if len(fields) == 0 {
		return "", "", false
	}
	switch {
	case strings.EqualFold(fields[0], "#include"):
		return "#include", "references a path on the target that would not exist on the builder", true
	case strings.EqualFold(fields[0], "#clear"):
		return "#clear", "apt.conf directive: a private root carries settings on the resolution allow-list, never directives", true
	}
	return "", "", false
}

// hookKeys name configuration values apt EXECUTES rather than reads. The
// Pre-Invoke/Post-Invoke families are run through /bin/sh by apt's
// RunScripts(); Pre-Install-Pkgs feeds a program on a pipe;
// Proxy-Auto-Detect names a binary apt forks per URL.
//
// A snapshot is untrusted input - a file produced on another machine and
// carried across the gap on removable media - and its apt.conf.d fragments
// are copied into the private root, which apt then reads via APT_CONFIG
// (the only mechanism that works; see writeAptConfigLoader). So a captured
// fragment naming any of these keys is arbitrary code execution as the
// build user, on the one host in the pipeline that holds a signing key,
// during the apt-get update that every resolve runs. Confirmed against
// apt 2.6.1: the hook runs as root and apt-get update still exits 0, so
// nothing downstream would notice.
//
// Matched on the FINAL path segment, so that a scoped re-entry such as
// Binary::apt-get::DPkg::Pre-Invoke is caught by the same rule.
//
// The allow-list below would drop every one of these anyway — that is the
// point of an allow-list. They are still named, for two reasons. First, a
// reason string: an operator reading the lock should be told a hook was
// removed from their snapshot, not merely that some key was unrecognised.
// Second, defence in depth: an allow-list entry may admit a whole subtree
// (APT::Solver does, and a later one may need to), and these rules are
// checked BEFORE the allow-list, so nobody can re-admit a hook underneath a
// subtree entry without first deleting a rule that says, in words, why it is
// there.
var hookKeys = []string{
	"Pre-Invoke",
	"Post-Invoke",
	"Post-Invoke-Success",
	"Post-Invoke-Stats",
	"Pre-Install-Pkgs",
	"Proxy-Auto-Detect",
	"ProxyAutoDetect",
}

// verificationKeys switch off apt's own signature or freshness checking.
// The target may legitimately run with these set, but honouring them here
// would let a snapshot disable apt-secure on the BUILDER, which is where
// packages are actually fetched over the network. Resolution fidelity is
// not worth the builder's trust anchor, so they are dropped and recorded
// rather than carried; the DroppedConfEntry list makes that visible.
var verificationKeys = []string{
	"AllowUnauthenticated",
	"AllowInsecureRepositories",
	"AllowDowngradeToInsecureRepositories",
	"AllowWeakRepositories",
	"Check-Valid-Until",
	"Check-Date",
	"Verify-Peer",
	"Verify-Host",
}

// aptConfEntry is one allow-list entry: a key path, compared segment by
// segment and case-insensitively (apt's own configuration keys are
// case-insensitive), and whether it admits only that exact key or the whole
// subtree beneath it.
type aptConfEntry struct {
	Path []string
	// Subtree admits everything under Path as well as Path itself, and is set
	// on exactly one entry. The default is deliberately the strict one. Nine
	// of the ten keys below are scalars or value lists with nothing legal
	// underneath them, so a captured path that runs DEEPER than the entry is
	// a key nobody on this list anticipated — and an allow-list exists
	// precisely so that such a key is dropped rather than carried. Matching
	// every entry as a bare prefix, which is what this filter did when the
	// allow-list first landed, quietly made all ten subtree roots and so
	// re-opened that gap under nine keys at once: the same fail-open shape,
	// one level down, that let Pre-Invoke through in the first place.
	//
	// A value list is unaffected by the strictness either way: apt writes one
	// as bare values inside a block ("APT::Architectures { "amd64"; };"), and
	// a bare value has no key path of its own to compare — it is filtered at
	// its parent. The list-append spelling collapses too, since
	// splitConfKeyPath trims the trailing "::" from "APT::Architectures::".
	Subtree bool
}

// aptConfAllowList is the whole of what a captured apt.conf may contribute to
// a private root: the keys that change what apt SELECTS.
//
// Every entry has to earn its place by changing resolution, because every
// entry is also a key that a file from another machine gets to set on the
// builder. The evidence for each is this repo's own:
//
//   - APT::Install-Recommends, APT::Install-Suggests — apt's "is this
//     dependency important" test, the single biggest lever on the size of a
//     resolved set. E4 measured the cost of losing just the first: a plain
//     `git` install resolved to 110 files / 37.4 MB instead of the target's
//     real 50 files / 26.8 MB, i.e. 60 extra packages and 10.6 MB, from one
//     missing line. Install-Suggests is the same apt code path, one
//     dependency type over.
//   - APT::Default-Release — selects by suite, not by version number. E4
//     part 2 watched it pick 0.9-high over the numerically higher 1.0-low.
//     Nothing in the resolved set records why a version won, so losing this
//     key is a silently wrong answer rather than a visibly missing one.
//   - APT::Architecture, APT::Architectures — which architectures have
//     candidates at all. buildOptions also sets both from the snapshot's own
//     Target fields, and -o is applied last and wins, so this entry is about
//     the captured file remaining self-consistent, not about winning a
//     conflict.
//   - APT::Solver (subtree) — E2 found that resolution divergence tracks the
//     effective solver and nothing else: the same apt binary under two
//     solvers returned a one-package answer and a six-package answer for the
//     same real mail-transport-agent request, while two different apt
//     versions running the same solver never diverged at all. It is also the
//     one key this package reads back OUT of a private root:
//     probeSolverForRoot asks apt-config what the root's APT_CONFIG makes
//     effective and prefers exactly binary::apt-get::APT::Solver, which is
//     the scoped form E2 recorded on a stock questing image. Dropping it
//     would both stop the target's solver reaching the resolving apt and
//     blind the divergence report that exists to notice. It is also the only
//     Subtree entry, and the only one that has earned it: solver3's own
//     tuning keys really do live underneath it (APT::Solver::RemoveManual,
//     in E2's own dump of a stock questing image), whereas every other entry
//     here is a scalar or a value list with nothing legal below it.
//   - APT::Machine-ID, APT::Get::Never-Include-Phased-Updates,
//     APT::Get::Always-Include-Phased-Updates — phasing. E1 flipped one real
//     package between "selected 2.91" and "kept back 2.90" on nothing but the
//     machine-id, and confirmed all three names exist in libapt-pkg 2.8.3.
//     (E1 also narrows the claim, and the narrowing is worth keeping in mind
//     when reading a lock: these govern apt's automatic upgrade pass only —
//     an explicitly named install bypasses phasing entirely.) The file-based
//     alternative E1 also tested, Dir::Etc::machine-id, is deliberately NOT
//     here: it is a Dir path on the target, and the Dir rule outranks
//     fidelity for every Dir key.
//   - Acquire::Languages — decides which index files apt fetches at all;
//     buildOptions sets it too, for the same reason as the architecture keys.
//
// Deliberately absent, though real targets do set them: autoremove
// bookkeeping (APT::NeverAutoRemove, APT::Never-MarkAuto-Sections — debark
// never runs autoremove, so they cannot move a result); DPkg::Options and the
// rest of the dpkg front-end settings (they steer an install, not a
// selection, and core/install sets its own); Acquire retry/timeout/
// compression tuning (the builder's network is not the target's, and
// buildOptions sets Retries and GzipIndexes itself, the latter because this
// package's parsers cannot decompress lz4 indexes); and APT::Periodic /
// unattended-upgrade state (nothing to do with resolving a request). Each of
// those now produces a warning naming the key that went, which is the
// intended, visible price of the rule — and the list to consult first when
// an operator asks why a build's output moved.
var aptConfAllowList = []aptConfEntry{
	{Path: []string{"APT", "Install-Recommends"}},
	{Path: []string{"APT", "Install-Suggests"}},
	{Path: []string{"APT", "Default-Release"}},
	{Path: []string{"APT", "Architecture"}},
	{Path: []string{"APT", "Architectures"}},
	{Path: []string{"APT", "Solver"}, Subtree: true},
	{Path: []string{"APT", "Machine-ID"}},
	{Path: []string{"APT", "Get", "Never-Include-Phased-Updates"}},
	{Path: []string{"APT", "Get", "Always-Include-Phased-Updates"}},
	{Path: []string{"Acquire", "Languages"}},
}

// notOnAllowListReason is the reason recorded for everything the allow-list
// does not name. It is deliberately the most common message this filter
// emits: an operator who sees it has lost a captured setting, and the fix —
// if the setting really does change what apt selects — is to add that key to
// aptConfAllowList, not to switch the filter off.
const notOnAllowListReason = "not on the resolution allow-list: a private root carries only the captured settings that change what apt selects"

// confKeyClass is how one key path stands relative to the allow-list.
type confKeyClass int

const (
	// confKeyUnknown: nothing on the allow-list is at, or under, this path.
	confKeyUnknown confKeyClass = iota
	// confKeyAllowed: this path is an allow-list entry, or lives under one.
	confKeyAllowed
	// confKeyContainer: an allow-list entry lives strictly UNDER this path,
	// so a block here must be descended into to reach it — "APT { Install-
	// Recommends "false"; };" is the same setting as the dotted form and has
	// to survive in either shape. A non-block item at a container path (a
	// bare `APT "x";`) sets a value apt never reads and is dropped like any
	// other unrecognised key.
	confKeyContainer
)

// stripBinaryScope unwraps apt's per-front-end scoping, Binary::<program>::,
// so a scoped entry is judged as the key it actually sets: Binary::apt-get::
// APT::Solver is APT::Solver, applied when the program is apt-get. This is
// not a hypothetical shape — E2 recorded a stock Ubuntu questing carrying its
// solver default in exactly that form, and effectiveSolverFromDump reads
// binary::apt-get::apt::solver back out by preference. Returns the remaining
// path and whether a scope was stripped; "Binary" and "Binary::apt-get" on
// their own strip to nothing, which classifyConfKey reads as "a wrapper
// block: look inside".
//
// Only Binary:: is unwrapped. apt's dumps also show Version::<x.y>:: scoping,
// but that is apt's own compatibility defaulting rather than anything a
// captured file has been seen to carry, so a Version::-scoped entry is
// dropped and REPORTED. Guessing at a scope this filter has no evidence for
// is exactly the failing this change exists to end.
func stripBinaryScope(path []string) (rest []string, scoped bool) {
	if len(path) > 0 && strings.EqualFold(path[0], "Binary") {
		if len(path) >= 2 {
			return path[2:], true
		}
		return nil, true
	}
	return path, false
}

// classifyConfKey places one key path against the allow-list.
func classifyConfKey(path []string, allowProxy bool) confKeyClass {
	p, scoped := stripBinaryScope(path)
	if len(p) == 0 {
		if scoped {
			return confKeyContainer // the Binary:: / Binary::<program>:: wrapper itself
		}
		return confKeyUnknown
	}
	for _, entry := range aptConfAllowList {
		// Allowed: the exact key, or — for the one Subtree entry — anything
		// under it. A path that runs deeper than a non-Subtree entry falls
		// through to confKeyUnknown and is dropped, which is the point:
		// APT::Install-Recommends is a boolean, so APT::Install-Recommends::
		// <anything> is a key this list never anticipated.
		if hasConfPrefix(p, entry.Path) && (entry.Subtree || len(p) == len(entry.Path)) {
			return confKeyAllowed
		}
		// Container: an allow-list entry lives under this path, so a block
		// here has to be descended into to reach it. No length guard is
		// needed to make that "strictly under" — an exact match has already
		// returned from the allowed branch above.
		if hasConfPrefix(entry.Path, p) {
			return confKeyContainer
		}
	}
	// The operator-opted-in proxy family is the one allow-list entry that is
	// positional rather than a fixed prefix: apt spells a proxy
	// Acquire::Proxy, Acquire::http::Proxy and Acquire::http::Proxy::<host>,
	// so the rule is "an Acquire path with a Proxy segment in it" and any
	// Acquire block may contain one deeper down.
	if allowProxy && strings.EqualFold(p[0], "Acquire") {
		if containsConfSegment(p[1:], "Proxy") {
			return confKeyAllowed
		}
		return confKeyContainer
	}
	return confKeyUnknown
}

// hasConfPrefix reports whether path begins with every segment of prefix,
// compared case-insensitively.
func hasConfPrefix(path, prefix []string) bool {
	if len(path) < len(prefix) {
		return false
	}
	for i, seg := range prefix {
		if !strings.EqualFold(path[i], seg) {
			return false
		}
	}
	return true
}

func containsConfSegment(path []string, segment string) bool {
	for _, seg := range path {
		if strings.EqualFold(seg, segment) {
			return true
		}
	}
	return false
}

// unsafeSolverValue guards the one allow-list entry whose VALUE apt can turn
// into an exec. APT::Solver names a helper apt looks up under
// Dir::Bin::solvers and runs — E2 saw apt 2.8.3 refuse the value "3.0" with
// "Can't call external solver '3.0' as it is not in a configured directory!",
// which is that lookup failing — and the name is joined to the solver
// directory rather than validated, so a value carrying a path separator names
// a program somewhere else. A real solver name never does ("internal", "3.0",
// "dump"), so requiring a plain scalar costs no fidelity and keeps the
// allow-list from becoming a way to choose which program apt runs. This is a
// precaution rather than a measured exploit; it is priced at one comparison,
// and the drop is recorded like any other.
func unsafeSolverValue(p []string, it confItem) (reason string, drop bool) {
	if len(p) != 2 || !strings.EqualFold(p[0], "APT") || !strings.EqualFold(p[1], "Solver") {
		return "", false
	}
	if it.IsBlock || !it.HasValue {
		return "solver override: a solver is named by one plain value, never a list or an empty key", true
	}
	if strings.ContainsAny(it.Value, `/\`) {
		return "solver override: the value names a path, and apt runs the solver it names from Dir::Bin::solvers", true
	}
	return "", false
}

// dropConfReason decides whether one parsed entry may enter the private root,
// and says why not when it may not.
//
// Order matters, and only for the sake of the reason string: the named
// dangerous families are checked first so that a hook is reported as a hook
// rather than as an unrecognised key, then the allow-list decides everything
// else. Nothing reaches the allow-list check that the deny rules would have
// caught, and nothing the deny rules catch would have been allowed — they
// overlap on purpose (see hookKeys).
func dropConfReason(path []string, it confItem, allowProxy bool) (reason string, drop bool) {
	if len(path) == 0 {
		return "", false
	}
	// The deny rules judge the scope-stripped path, so Binary::apt-get::
	// Dir::Cache::archives is reported as the Dir override it is. The
	// final-segment rules are unaffected by stripping either way.
	p, _ := stripBinaryScope(path)
	if len(p) > 0 {
		if strings.EqualFold(p[0], "Dir") {
			return "Dir override: a private apt root sets every Dir::* path explicitly and never trusts a captured one", true
		}
		last := p[len(p)-1]
		for _, k := range hookKeys {
			if strings.EqualFold(last, k) {
				return "command hook: apt executes this value as a program, and a captured apt.conf is untrusted input", true
			}
		}
		for _, k := range verificationKeys {
			if strings.EqualFold(last, k) {
				return "verification override: a captured apt.conf never switches off signature or freshness checking on the builder", true
			}
		}
		// DPkg::Tools::* names external programs apt invokes around dpkg, so
		// the whole subtree goes, not just leaves that happen to be listed
		// above.
		if len(p) >= 2 && strings.EqualFold(p[0], "DPkg") && strings.EqualFold(p[1], "Tools") {
			return "DPkg::Tools subtree: names external programs apt runs around dpkg", true
		}
		// Acquire::gpgv::Options passes arbitrary argv to the signature
		// verifier.
		if len(p) >= 3 && strings.EqualFold(p[0], "Acquire") && strings.EqualFold(p[1], "gpgv") && strings.EqualFold(last, "Options") {
			return "gpgv options: arbitrary arguments to the signature verifier", true
		}
		// Proxies are the builder's network identity, never the target's
		// concern, unless the operator opted in. Matched on any Proxy segment
		// rather than the final one, which the old rule used: apt also spells
		// a per-host proxy Acquire::http::Proxy::<host>, whose final segment
		// is a hostname, and a final-segment rule let that form straight
		// through.
		if !allowProxy && strings.EqualFold(p[0], "Acquire") && containsConfSegment(p[1:], "Proxy") {
			return "proxy setting: the builder's network identity, never carried into a private root unless opted in", true
		}
	}
	switch classifyConfKey(path, allowProxy) {
	case confKeyAllowed:
		return unsafeSolverValue(p, it)
	case confKeyContainer:
		if it.IsBlock {
			return "", false
		}
	}
	return notOnAllowListReason, true
}

// --- serializer ------------------------------------------------------------

func serializeAptConfItems(b *strings.Builder, items []confItem, depth int) {
	indent := strings.Repeat("    ", depth)
	for _, it := range items {
		switch {
		case it.Directive != "":
			b.WriteString(it.Directive)
			b.WriteByte('\n')
		case it.Key == "":
			fmt.Fprintf(b, "%s%s;\n", indent, quoteConfValue(it.Value))
		case it.IsBlock:
			fmt.Fprintf(b, "%s%s\n%s{\n", indent, it.Key, indent)
			serializeAptConfItems(b, it.Block, depth+1)
			fmt.Fprintf(b, "%s};\n", indent)
		case it.HasValue:
			fmt.Fprintf(b, "%s%s %s;\n", indent, it.Key, quoteConfValue(it.Value))
		default:
			fmt.Fprintf(b, "%s%s {};\n", indent, it.Key)
		}
	}
}

func quoteConfValue(v string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c == '"' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	b.WriteByte('"')
	return b.String()
}
