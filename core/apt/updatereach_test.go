package apt

import (
	"strings"
	"testing"
)

// noNetworkUpdateOutput is a verbatim capture of
//
//	docker run --rm --network none ubuntu:24.04 apt-get update
//
// taken on 2026-09-06. It is the exact shape docs/dev/error-catalogue.md 3.1
// reports: apt exits 0, prints no "E:" line, and the run therefore parses as
// UpdateResult{Failed: false}. Four sources, none of them reachable, sixteen
// acquire lines -- twelve "Ign:" from Acquire::Retries and four "Err:".
//
// Kept verbatim rather than reduced, because every previous attempt to
// summarise apt's output in this package has lost the detail that mattered.
const noNetworkUpdateOutput = `Ign:1 http://archive.ubuntu.com/ubuntu noble InRelease
Ign:2 http://security.ubuntu.com/ubuntu noble-security InRelease
Ign:3 http://archive.ubuntu.com/ubuntu noble-updates InRelease
Ign:4 http://archive.ubuntu.com/ubuntu noble-backports InRelease
Ign:1 http://archive.ubuntu.com/ubuntu noble InRelease
Ign:2 http://security.ubuntu.com/ubuntu noble-security InRelease
Ign:3 http://archive.ubuntu.com/ubuntu noble-updates InRelease
Ign:4 http://archive.ubuntu.com/ubuntu noble-backports InRelease
Ign:1 http://archive.ubuntu.com/ubuntu noble InRelease
Ign:3 http://archive.ubuntu.com/ubuntu noble-updates InRelease
Ign:2 http://security.ubuntu.com/ubuntu noble-security InRelease
Ign:4 http://archive.ubuntu.com/ubuntu noble-backports InRelease
Err:1 http://archive.ubuntu.com/ubuntu noble InRelease
  Temporary failure resolving 'archive.ubuntu.com'
Err:2 http://security.ubuntu.com/ubuntu noble-security InRelease
  Temporary failure resolving 'security.ubuntu.com'
Err:3 http://archive.ubuntu.com/ubuntu noble-updates InRelease
  Temporary failure resolving 'archive.ubuntu.com'
Err:4 http://archive.ubuntu.com/ubuntu noble-backports InRelease
  Temporary failure resolving 'archive.ubuntu.com'
Reading package lists...
W: Failed to fetch http://archive.ubuntu.com/ubuntu/dists/noble/InRelease  Temporary failure resolving 'archive.ubuntu.com'
W: Failed to fetch http://archive.ubuntu.com/ubuntu/dists/noble-updates/InRelease  Temporary failure resolving 'archive.ubuntu.com'
W: Failed to fetch http://archive.ubuntu.com/ubuntu/dists/noble-backports/InRelease  Temporary failure resolving 'archive.ubuntu.com'
W: Failed to fetch http://security.ubuntu.com/ubuntu/dists/noble-security/InRelease  Temporary failure resolving 'security.ubuntu.com'
W: Some index files failed to download. They have been ignored, or old ones used instead.
`

// healthyUpdateOutput is the same command with a network: three sources hit,
// one downloaded, plus the Translation index apt skips by design.
const healthyUpdateOutput = `Hit:1 http://archive.ubuntu.com/ubuntu noble InRelease
Get:2 http://security.ubuntu.com/ubuntu noble-security InRelease [126 kB]
Hit:3 http://archive.ubuntu.com/ubuntu noble-updates InRelease
Hit:4 http://archive.ubuntu.com/ubuntu noble-backports InRelease
Ign:5 http://archive.ubuntu.com/ubuntu noble/main Translation-en_GB
Fetched 126 kB in 1s (126 kB/s)
Reading package lists...
`

func TestSourceOutcomes(t *testing.T) {
	cases := []struct {
		name        string
		out         Output
		wantFetched int
		wantFailed  int
	}{
		{
			name:        "no network at all",
			out:         Output{Stdout: []byte(noNetworkUpdateOutput), ExitCode: 0},
			wantFetched: 0,
			// Four sources, not sixteen lines: the twelve Ign: lines are
			// Acquire::Retries printing the same four sources three times.
			wantFailed: 4,
		},
		{
			name:        "everything worked",
			out:         Output{Stdout: []byte(healthyUpdateOutput), ExitCode: 0},
			wantFetched: 4,
			// The Ign:5 Translation index is absent by design, not a
			// failure. Counting it would report a healthy machine as broken.
			wantFailed: 0,
		},
		{
			name: "one mirror down out of three",
			out: Output{Stdout: []byte(`Hit:1 http://archive.ubuntu.com/ubuntu noble InRelease
Err:2 http://broken.example/ubuntu noble InRelease
  Could not connect
Hit:3 http://archive.ubuntu.com/ubuntu noble-updates InRelease
`), ExitCode: 0},
			wantFetched: 2,
			wantFailed:  1,
		},
		{
			name: "a source that failed on the first attempt and succeeded on the retry",
			out: Output{Stdout: []byte(`Err:1 http://archive.ubuntu.com/ubuntu noble InRelease
  Method gave a blank filename
Get:1 http://archive.ubuntu.com/ubuntu noble InRelease [256 kB]
`), ExitCode: 0},
			// Not a failed source. ParseUpdate's doc records this exact
			// case (a bare Packages file over file:// logging Err: for a few
			// attempts before succeeding), and a per-source verdict has to
			// agree with it or the two disagree about the same run.
			wantFetched: 1,
			wantFailed:  0,
		},
		{
			name:        "nothing at all",
			out:         Output{Stdout: []byte("Reading package lists...\n"), ExitCode: 0},
			wantFetched: 0,
			wantFailed:  0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upd := ParseUpdate(tc.out)
			fetched, failed := upd.SourceOutcomes()
			if fetched != tc.wantFetched || failed != tc.wantFailed {
				t.Errorf("SourceOutcomes() = (%d fetched, %d failed), want (%d, %d); Entries=%d",
					fetched, failed, tc.wantFetched, tc.wantFailed, len(upd.Entries))
			}
		})
	}
}

// TestNoNetworkIsRecorded is the defect itself: with --network none the
// apt.update event carried {"entries": 12, "failed": false} and nothing on
// the wire said the network was down. The only signal reaching the operator
// was a resolution failure naming packages they had never chosen -- the base
// definition's own seeds.
func TestNoNetworkIsRecorded(t *testing.T) {
	upd := ParseUpdate(Output{Stdout: []byte(noNetworkUpdateOutput), ExitCode: 0})

	// apt's own verdict is unchanged, and must stay unchanged: apt is the
	// oracle for whether an update failed, and this change reports what apt
	// said rather than overruling it.
	if upd.Failed {
		t.Fatalf("ParseUpdate reported Failed=true; apt exited 0 and printed no E: line, so this test no longer describes the case it was written for")
	}

	warnings := updateDiagnosticWarnings(upd)
	var found *string
	for i := range warnings {
		if warnings[i].Code == "apt-update.no-index-fetched" {
			found = &warnings[i].Message
			break
		}
	}
	if found == nil {
		var codes []string
		for _, w := range warnings {
			codes = append(codes, w.Code)
		}
		t.Fatalf("no apt-update.no-index-fetched warning; got codes %v.\nWithout it the operator is told that \"apt\" and \"init\" cannot be located, when the archive was simply unreachable.", codes)
	}
	// The sentence has to say the thing, not merely exist.
	for _, want := range []string{"could not fetch an index from any", "exited 0", "unreachable"} {
		if !strings.Contains(*found, want) {
			t.Errorf("the warning does not say %q:\n%s", want, *found)
		}
	}

	// It must be first: the four per-source warnings read as four separate
	// hiccups when they are one cause.
	if warnings[0].Code != "apt-update.no-index-fetched" {
		t.Errorf("warnings[0].Code = %q, want the whole-machine cause first", warnings[0].Code)
	}
}

// TestPartialUpdateIsNotReportedAsNoNetwork is the half that must not move.
// One unreachable mirror alongside two that worked is an ordinary partial
// failure, already covered by the per-source apt-update.fetch-error warning,
// and must not be escalated into "the network is down".
func TestPartialUpdateIsNotReportedAsNoNetwork(t *testing.T) {
	upd := ParseUpdate(Output{Stdout: []byte(`Hit:1 http://archive.ubuntu.com/ubuntu noble InRelease
Err:2 http://broken.example/ubuntu noble InRelease
  Could not connect
Hit:3 http://archive.ubuntu.com/ubuntu noble-updates InRelease
`), ExitCode: 0})

	var sawNoIndex, sawFetchError bool
	for _, w := range updateDiagnosticWarnings(upd) {
		switch w.Code {
		case "apt-update.no-index-fetched":
			sawNoIndex = true
		case "apt-update.fetch-error":
			sawFetchError = true
		}
	}
	if sawNoIndex {
		t.Error("a partial failure was reported as no-index-fetched; two of three sources were fetched")
	}
	if !sawFetchError {
		t.Error("the per-source fetch-error warning went missing; the partial case still has to be recorded")
	}

	// And the healthy case says nothing at all.
	if got := updateDiagnosticWarnings(ParseUpdate(Output{Stdout: []byte(healthyUpdateOutput), ExitCode: 0})); len(got) != 0 {
		t.Errorf("a healthy update produced warnings: %+v", got)
	}
}
