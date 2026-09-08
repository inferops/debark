package snapshot

import (
	"path"
	"strconv"
	"strings"
)

// apt.conf(5) parsing: a small recursive-descent reader for exactly the
// subset InstallRecommends needs -- scalar assignments, in both the dotted
// form ("APT::Install-Recommends "false";") and the nested-block form
// ("APT { Install-Recommends "0"; };"), which apt treats as identical. It is
// not a general apt.conf parser: list-valued keys ("DPkg::Options { "-a";
// "-b"; };"), #include and #clear are recognised just well enough not to
// corrupt the scalar keys we do care about; their content is otherwise
// discarded, which is safe because nothing here ever reads a list key.

// aptToken is one lexical token of a comment-stripped apt.conf file.
type aptToken struct {
	kind byte // 'w' word (bareword or de-escaped quoted string), '{', '}', ';'
	text string
}

// stripAPTComments removes "//" line comments and "/* */" block comments,
// respecting double-quoted strings (so a proxy URL or password containing
// "//" is never mistaken for a comment). Escaped characters inside a quoted
// string are copied verbatim; unescaping happens later, in the tokenizer.
func stripAPTComments(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inQuote := false
	n := len(data)
	for i := 0; i < n; {
		c := data[i]
		if inQuote {
			if c == '\\' && i+1 < n {
				out = append(out, c, data[i+1])
				i += 2
				continue
			}
			if c == '"' {
				inQuote = false
			}
			out = append(out, c)
			i++
			continue
		}
		switch {
		case c == '"':
			inQuote = true
			out = append(out, c)
			i++
		case c == '/' && i+1 < n && data[i+1] == '/':
			for i < n && data[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && data[i+1] == '*':
			i += 2
			for i+1 < n && !(data[i] == '*' && data[i+1] == '/') {
				i++
			}
			i = min(i+2, n)
		default:
			out = append(out, c)
			i++
		}
	}
	return out
}

func isAPTDelim(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', '{', '}', ';', '"':
		return true
	}
	return false
}

// tokenizeAPTConf lexes a comment-stripped apt.conf file into words and
// structural punctuation. A quoted string is one word token with escapes
// resolved; a bareword runs until the next delimiter.
func tokenizeAPTConf(data []byte) []aptToken {
	data = stripAPTComments(data)
	var toks []aptToken
	n := len(data)
	for i := 0; i < n; {
		c := data[i]
		switch c {
		case ' ', '\t', '\r', '\n':
			i++
		case '{', '}', ';':
			toks = append(toks, aptToken{c, string(c)})
			i++
		case '"':
			var sb strings.Builder
			j := i + 1
			for j < n && data[j] != '"' {
				if data[j] == '\\' && j+1 < n {
					sb.WriteByte(data[j+1])
					j += 2
					continue
				}
				sb.WriteByte(data[j])
				j++
			}
			toks = append(toks, aptToken{'w', sb.String()})
			i = j + 1
		default:
			j := i
			for j < n && !isAPTDelim(data[j]) {
				j++
			}
			toks = append(toks, aptToken{'w', string(data[i:j])})
			i = j
		}
	}
	return toks
}

// parseAPTConfInto tokenizes one apt.conf-syntax file and merges its scalar
// assignments into out (lower-cased "a::b::c" -> value), overwriting any
// existing entry -- the same last-one-wins rule apt itself applies when a
// later file (or a later line) reassigns a key.
func parseAPTConfInto(data []byte, out map[string]string) {
	toks := tokenizeAPTConf(data)
	var prefix []string
	i := 0
	for i < len(toks) {
		switch toks[i].kind {
		case '}':
			if len(prefix) > 0 {
				prefix = prefix[:len(prefix)-1]
			}
			i++
			i = skipToken(toks, i, ';')
		case 'w':
			word := toks[i].text
			i++
			switch strings.ToLower(word) {
			case "#clear":
				for i < len(toks) && toks[i].kind == 'w' {
					key := joinAPTKey(prefix, toks[i].text)
					delete(out, key)
					i++
				}
				i = skipToken(toks, i, ';')
				continue
			case "#include", "#include-dir":
				i = skipToken(toks, i, 'w')
				i = skipToken(toks, i, ';')
				continue
			}
			if i >= len(toks) {
				continue
			}
			switch toks[i].kind {
			case '{':
				prefix = append(prefix, word)
				i++
			case 'w':
				value := toks[i].text
				i++
				i = skipToken(toks, i, ';')
				out[joinAPTKey(prefix, word)] = value
			default:
				i = skipToken(toks, i, ';')
			}
		default:
			i++
		}
	}
}

func skipToken(toks []aptToken, i int, kind byte) int {
	if i < len(toks) && toks[i].kind == kind {
		return i + 1
	}
	return i
}

func joinAPTKey(prefix []string, name string) string {
	parts := append(append([]string{}, prefix...), name)
	return strings.ToLower(strings.Join(parts, "::"))
}

// parseAPTBool accepts apt's own boolean spellings (Configuration::FindB):
// true/yes/on and false/no/off, plus the bare "1"/"0" the task's contract
// calls out explicitly. Anything else reports ok=false so a malformed value
// falls back to the caller's default instead of being guessed at.
func parseAPTBool(s string) (value bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "on", "1":
		return true, true
	case "false", "no", "off", "0":
		return false, true
	default:
		return false, false
	}
}

// InstallRecommends reports the effective APT::Install-Recommends value from
// the captured apt.conf.d, and whether the target set it explicitly. Ubuntu
// and Debian default to true.
//
// s.APT.Conf is walked in its stored order, which is sorted by Path (ADR-005)
// and therefore has "/etc/apt/apt.conf" -- a strict prefix of, and so always
// sorting before, "/etc/apt/apt.conf.d/..." -- ahead of the apt.conf.d
// fragments, which themselves sort in filename order. That is the same order
// apt's own Configuration::Load applies them in, so the last assignment seen
// here is the same one that would win on the real target.
//
// An apt.conf.d entry whose filename apt's own GetListOfFilesInDir would
// skip (isPlainRunPartsName) -- an editor backup, a dpkg ".dpkg-new" or
// ".orig" sibling -- is captured (for completeness) but not consulted here:
// apt itself never reads it, so treating its content as live configuration
// would make this function *less* correct than the value it is meant to
// reproduce.
func doInstallRecommends(s *Snapshot, fs *FileSet) (value bool, explicit bool) {
	value = true
	if s == nil || fs == nil {
		return value, false
	}
	kv := map[string]string{}
	for _, f := range s.APT.Conf {
		if strings.Contains(f.Path, "/apt.conf.d/") && !isPlainRunPartsName(path.Base(f.Path)) {
			continue
		}
		data, ok := fs.Bytes[f.ArchivePath]
		if !ok {
			continue
		}
		parseAPTConfInto(data, kv)
	}
	raw, ok := kv["apt::install-recommends"]
	if !ok {
		return value, false
	}
	if b, ok := parseAPTBool(raw); ok {
		return b, true
	}
	return value, false
}

// --- effective solver (experiment E2) ---------------
//
// solverInternal and solver3 are apt's own effective-solver values: the
// classic pre-3.x resolver ("internal" -- confirmed by E2 to work
// identically to no APT::Solver override at all, on every apt release
// tested) and the SAT-based resolver apt's 3.x line ships
// (docs/experiments/E2-solver-divergence.md). Unexported: core/apt has its
// own copy (core/apt/solver.go) for the same reason solverMajorVersion below
// does -- core/apt already imports core/snapshot, so the reverse import
// needed to share a single definition is not available.
const (
	solverInternal = "internal"
	solver3        = "3.0"
)

// solverMajorVersion extracts the leading major version number from an apt
// version string such as "2.8.3" or "3.1.6ubuntu2". A narrow, local
// equivalent of core/apt's aptMajorMinor (core/apt/dpkgversion.go); only the
// major component is needed here, and duplicating the tiny parse is simpler
// than threading a shared helper across the import-direction boundary.
func solverMajorVersion(aptVersion string) (major int, ok bool) {
	v := strings.TrimSpace(aptVersion)
	i := 0
	for i < len(v) && v[i] >= '0' && v[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(v[:i])
	if err != nil {
		return 0, false
	}
	return n, true
}

// defaultSolverForAPTVersion returns the solver a given apt version resolves
// with when nothing in its apt.conf.d overrides it -- that version's own
// shipped default (E2: "solver3 is compiled into libapt-pkg itself", i.e. a
// stock install carries no config file entry for it at all; the default is a
// property of the binary, not of any file this package captures).
//
// Evidence status, exactly as measured by
// hack/experiments/e2-solver-divergence.sh (raw probe output:
// hack/experiments/out/e2/e2-run.log; narrative:
// docs/experiments/E2-solver-divergence.md) on 2026-09-03:
//   - MEASURED directly (`apt-config dump | grep -i solv`, stock untouched
//     image): Ubuntu 24.04/noble, apt 2.8.3 -> internal (no APT::Solver key
//     of any kind present in the dump). Ubuntu 25.10/questing, apt
//     3.1.6ubuntu2 -> 3.0 (via the scoped `binary::apt-get::APT::Solver
//     "3.0"`, alongside a blank top-level `APT::Solver ""`). Ubuntu
//     26.04/resolute, apt 3.2.0 -> 3.0 (via a bare top-level `APT::Solver
//     "3.0"`, with no scoped key present at all -- the two solver3 releases
//     surface the same effective default through two different dump
//     shapes, which is exactly why a caller must check both, see
//     core/apt/solver.go's parser).
//   - INFERRED for every other apt major version: E2's own "what this means
//     for the design" section treats "apt 3.x" as the general boundary
//     ("apt 3.x's solver3 in Ubuntu 25.10+ is the concrete case"), so this
//     generalises the three measured points to "major < 3 -> internal,
//     major >= 3 -> 3.0" for any apt version this can parse a major number
//     from. This is a best-effort fallback only, always beaten by an
//     explicit APT::Solver actually captured in apt.conf.d (see
//     doEffectiveSolver above it in this file); it is NOT independently
//     verified for an apt version E2 did not test -- e.g. a hypothetical
//     later 3.x point release that reverts the default, or a Debian build
//     whose compile-time default differs from Ubuntu's.
func defaultSolverForAPTVersion(aptVersion string) (solver string, ok bool) {
	major, ok := solverMajorVersion(aptVersion)
	if !ok {
		return "", false
	}
	if major >= 3 {
		return solver3, true
	}
	return solverInternal, true
}

// EffectiveSolver is api.go's EffectiveSolver body.
//
// It reads the target's effective APT::Solver the same way doInstallRecommends
// reads APT::Install-Recommends -- identical file walk, identical
// parseAPTConfInto/kv-map mechanics, so see that function's doc for why the
// stored order already matches apt's own Configuration::Load precedence --
// and reports whether it found an explicit override.
//
// Absent an explicit override, the effective solver is that apt version's
// own shipped default (defaultSolverForAPTVersion above), resolved from
// s.Target.APTVersion -- never a single fixed constant the way
// InstallRecommends' bare "true" is, because unlike Install-Recommends the
// correct default genuinely depends on which apt version the target runs.
// value is "" only when neither an explicit override nor a recognisable apt
// version is available, which a caller must treat as "unknown", not as a
// third solver.
func doEffectiveSolver(s *Snapshot, fs *FileSet) (value string, explicit bool) {
	if s == nil || fs == nil {
		return "", false
	}
	kv := map[string]string{}
	for _, f := range s.APT.Conf {
		if strings.Contains(f.Path, "/apt.conf.d/") && !isPlainRunPartsName(path.Base(f.Path)) {
			continue
		}
		data, ok := fs.Bytes[f.ArchivePath]
		if !ok {
			continue
		}
		parseAPTConfInto(data, kv)
	}
	if raw, ok := kv["apt::solver"]; ok {
		if trimmed := strings.TrimSpace(raw); trimmed != "" {
			return trimmed, true
		}
	}
	if d, ok := defaultSolverForAPTVersion(s.Target.APTVersion); ok {
		return d, false
	}
	return "", false
}
