package doctor

import (
	"fmt"
	"regexp"
	"strings"
)

// NoNetworkActionFound is the exact sentence the design mandates for a clean
// network-postinst result. doctor never claims a script is safe — only that
// nothing obvious was seen — and any caller rendering a clean result (the CLI,
// a report summary) should use this string verbatim rather than inventing a
// stronger claim.
const NoNetworkActionFound = "no obvious network action found"

// networkMatch is one surviving signal inside a maintainer script.
type networkMatch struct {
	Script string // preinst, postinst, prerm or postrm
	Line   int    // 1-based
	Text   string // the matched line, trimmed, comment stripped
	Signal string // which pattern fired: curl-or-wget, apt-get, add-apt-repository, pip-install, git-clone, url, nc, ssh
}

func (m networkMatch) String() string {
	return fmt.Sprintf("%s:%d: %s", m.Script, m.Line, m.Text)
}

// Regexes for the signals the design names: curl, wget, apt-get update|
// install, pip install, git clone, http(s):// URLs, nc, ssh,
// add-apt-repository. All are word-boundary matches over a line that has
// already had its comment and heredoc/message-string context removed by
// scanScriptForNetwork — see there for why precision is prioritised over
// recall.
var (
	reCurlWget   = regexp.MustCompile(`\b(curl|wget)\b`)
	reAptGetNet  = regexp.MustCompile(`\bapt-get\s+(update|install)\b`)
	reAddAptRepo = regexp.MustCompile(`\badd-apt-repository\b`)
	rePipInstall = regexp.MustCompile(`\bpip[23]?\s+install\b`)
	reGitClone   = regexp.MustCompile(`\bgit\s+clone\b`)
	reURL        = regexp.MustCompile(`\bhttps?://[^\s"'` + "`" + `<>)]+`)
	reNCWord     = regexp.MustCompile(`\bnc\b`)
	reSSHWord    = regexp.MustCompile(`\bssh\b`)
	reHasDigit   = regexp.MustCompile(`\d`)

	// reEchoLike matches statements that only print text: echo/printf/logger
	// and the debconf helper messages. A curl or URL mentioned only inside
	// one of these is documentation, not a fetch (task requirement: "URLs in
	// echo/message strings" must not fire).
	reEchoLike = regexp.MustCompile(`^(echo|printf|logger)\b`)

	// reHeredocStart finds a shell heredoc introducer: `<<EOF`, `<<-EOF`,
	// `<<'EOF'`, `<<"EOF"`. Its body is documentation or debconf template
	// text, not executed code, so it is skipped until the matching terminator
	// line (task requirement: suppress "debconf template text").
	reHeredocStart = regexp.MustCompile(`<<-?\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)
)

// maxScriptMatches bounds how many signals one maintainer script may
// produce, and scanScriptForNetwork stops scanning the moment it is reached.
//
// This is the bound that keeps a hostile .deb from turning doctor into a
// memory bomb, and the numbers are the reason it is set where it is. A
// maintainer script is read through scanByteLimit, so 8 MiB of "curl
// http://a.example/x" lines produced 699,048 matches (349,354 findings after
// checkNetworkPostinst dedupes by line) in 714ms — out of a 20,754-byte .deb.
// Because Run accumulates findings across every package in the lock and the
// lock is attacker-controlled too, that multiplies: a 20-package lock naming
// that same 20 KB file yielded 6,987,080 findings and 1,742 MiB of live heap
// in 39s, and at 200 packages the process died on `fatal error: runtime:
// cannot allocate memory` — an unrecoverable Go runtime failure, from about
// 25 KB of attacker input, in the one command an operator runs FIRST on
// media that just arrived.
//
// 256 is far above anything real. Experiment E7
// (docs/experiments/E7-network-postinst.md) scanned 1,749 current Debian 12
// and Ubuntu 24.04 packages: 5 matched at all, the most any single package
// produced was 2 matched lines, and the whole corpus totalled 6. So this cap
// is ~128x the busiest real package and ~42x the entire measured corpus, and
// no real .deb can reach it.
//
// Truncating here cannot manufacture a false clean result — the one outcome
// this package must never produce. A script that hits the cap has already
// produced 256 findings; it is reported as maximally suspicious either way,
// and the cap only drops repetitions of a warning the operator has already
// been given. That asymmetry is why this is a safe place to bound and the
// control member (ar.go) is not.
const maxScriptMatches = 256

// scanScriptForNetwork scans one maintainer script's source and returns every
// network-activity signal that survives the false-positive suppressions the
// design calls for: comments, echo/printf message strings, and heredoc bodies
// (documentation references and debconf template text) never fire.
//
// This is a heuristic, not a parser: it does not understand shell syntax. It
// is deliberately tuned toward precision over recall (a warning on every
// package is a warning on none) and was measured against a real-world sample;
// see docs/experiments/E7-network-postinst.md.
//
// At most maxScriptMatches signals are returned; see there for why that bound
// exists and why truncating it is safe.
func scanScriptForNetwork(script, content string) []networkMatch {
	var out []networkMatch
	var heredocEnd string
	i := -1
	// SplitSeq, not Split: Split materialises the whole line slice before the
	// loop runs even one iteration, so a script decompressing to scanByteLimit
	// bytes of short lines costs ~350,000 string headers up front — allocated
	// even when the cap below stops the scan after 256 of them.
	for line := range strings.SplitSeq(content, "\n") {
		i++
		if len(out) >= maxScriptMatches {
			// Stop scanning, not just stop appending: the scan itself is the
			// other half of the cost this bounds (714ms per 8 MiB script,
			// once per package in the lock).
			break
		}
		if heredocEnd != "" {
			if strings.TrimRight(line, " \t\r") == heredocEnd {
				heredocEnd = ""
			}
			continue
		}
		if m := reHeredocStart.FindStringSubmatch(line); m != nil {
			heredocEnd = m[1]
			// The line that opens the heredoc can still itself run a real
			// command (e.g. `curl -sSL ... <<'EOF' | tee ...`), so it is
			// still scanned below; only the body starting next line is
			// skipped.
		}

		code := stripShellComment(line)
		trimmed := strings.TrimSpace(code)
		if trimmed == "" {
			continue
		}
		if reEchoLike.MatchString(trimmed) {
			continue
		}

		for _, sig := range findSignals(code) {
			out = append(out, networkMatch{Script: script, Line: i + 1, Text: trimmed, Signal: sig})
		}
	}
	return out
}

// stripShellComment removes a trailing unquoted '#' comment. It is a
// heuristic, not a shell lexer: it tracks single/double quote state only, so
// it can be fooled by an escaped quote inside a double-quoted string. That
// only ever makes debark strip more than a real shell would, which removes
// potential signal text rather than inventing a false one — the direction
// this project's precision-over-recall rule prefers.
func stripShellComment(line string) string {
	var inSingle, inDouble bool
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '#' && !inSingle && !inDouble:
			return line[:i]
		}
	}
	return line
}

// reLoopbackURL matches an http(s) URL whose host is unambiguously loopback:
// 127.0.0.0/8 or localhost. Experiment E7 found this to be a real,
// non-negligible false-positive source for the bare "url" signal: OpenStack
// packages' postinst scripts write a default local API endpoint
// (http://127.0.0.1:5000/...) into a config file — text that matches
// "http(s):// URL" exactly as the design's signal list asks for, but is not
// network activity the target machine needs the internet for (see
// docs/experiments/E7-network-postinst.md for the exact packages and lines).
// A URL whose host is a shell variable (http://${HOST}:5000) is NOT caught by
// this — the literal text gives no way to know whether that variable resolves
// to a loopback address or a real remote host, and E7 found real examples of
// both shapes; that residual ambiguity is a documented limit of a
// line-of-text heuristic, not something this project tries to resolve by
// guessing.
var reLoopbackURL = regexp.MustCompile(`(?i)^https?://(127(?:\.\d{1,3}){3}|localhost|\[?::1\]?)(:\d+)?([/?#]|$)`)

// findSignals returns every signal name that fires on one (comment-stripped,
// non-message) line of shell.
func findSignals(code string) []string {
	urls := reURL.FindAllString(code, -1)
	hasURL, hasNonLoopbackURL := false, false
	for _, u := range urls {
		hasURL = true
		if !reLoopbackURL.MatchString(u) {
			hasNonLoopbackURL = true
		}
	}
	// allLoopback is true only when every URL literal on the line resolved to
	// loopback: a line with no URL literal at all (e.g. `curl -s $ENDPOINT`)
	// gives no evidence either way, so curl/wget still fires for it — only a
	// line whose only visible target is loopback is suppressed.
	allLoopback := hasURL && !hasNonLoopbackURL

	var sig []string
	if reCurlWget.MatchString(code) && !allLoopback {
		sig = append(sig, "curl-or-wget")
	}
	if reAptGetNet.MatchString(code) {
		sig = append(sig, "apt-get")
	}
	if reAddAptRepo.MatchString(code) {
		sig = append(sig, "add-apt-repository")
	}
	if rePipInstall.MatchString(code) {
		sig = append(sig, "pip-install")
	}
	if reGitClone.MatchString(code) {
		sig = append(sig, "git-clone")
	}
	if hasNonLoopbackURL {
		sig = append(sig, "url")
	}
	if idx := reNCWord.FindStringIndex(code); idx != nil && looksLikeCommandWord(code, idx) &&
		reHasDigit.MatchString(code[idx[1]:]) {
		sig = append(sig, "nc")
	}
	if idx := reSSHWord.FindStringIndex(code); idx != nil && looksLikeCommandWord(code, idx) &&
		strings.Contains(code[idx[1]:], "@") {
		sig = append(sig, "ssh")
	}
	return sig
}

// looksLikeCommandWord rejects matches that are really part of a longer token
// glued to the match by a character regexp word-boundaries don't stop at:
// path separators and dots (/etc/ssh/sshd_config, ~/.ssh/authorized_keys) and
// hyphens (ssh-keygen, ssh-copy-id, ssh-agent, nc-... tools). "ssh" and "nc"
// are, by far, the two signals in the design's list most prone to firing on
// ordinary file paths and unrelated tool names rather than an actual network
// invocation, so they get this extra, code-side filter instead of a purely
// regex-based one (Go's RE2 engine has no look-around to express it inline).
func looksLikeCommandWord(code string, idx []int) bool {
	start, end := idx[0], idx[1]
	if start > 0 {
		switch code[start-1] {
		case '/', '.', '-', '_':
			return false
		}
	}
	if end < len(code) {
		switch code[end] {
		case '-', '/', '.', '_':
			return false
		}
	}
	return true
}
