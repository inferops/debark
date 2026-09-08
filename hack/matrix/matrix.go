package main

import (
	"sort"
	"strings"

	"github.com/inferops/debark/core/distro"
	"github.com/inferops/debark/test/e2e"
)

// Job is one fixture x release x arch row this run will attempt (or, when
// Skip is already set by planJobs, will report as skipped without ever
// starting a container).
type Job struct {
	Fixture e2e.Fixture
	Release distro.Release
	Arch    string
	// SkipReason is set when planJobs already knows this row cannot run
	// (an unrecognised --release/--fixture filter matched nothing, or the
	// combination is nonsensical); left empty otherwise; image/emulator
	// availability is still checked per-job at run time (main.go), because
	// that requires a live probe planJobs deliberately does not perform
	// (task: "skip cleanly and say why", not "guess cleanly and say why").
	SkipReason string
}

func (j Job) rowLabel() string {
	return j.Fixture.Name + " / " + j.Release.DistroID + " " + j.Release.VersionID + " / " + j.Arch
}

// planJobs expands the loaded fixtures into concrete rows: a fixture whose
// target.matrix is true runs once per (release x arch) in the support
// matrix (core/distro.Supported(), crossed with target.matrix_arches or
// just its own target.arch when that list is empty); every other fixture
// runs exactly once, against its own stated target release/arch — most edge
// fixtures test one specific mechanism, not "does every release have apt",
// so multiplying them across the whole matrix would mostly spend container
// time re-proving the same thing (task: "each row is minutes of container
// time").
//
// fixtureFilter/releaseFilter (from --fixture/--release, comma-separated,
// case-insensitive substring match) narrow the result; an empty filter
// matches everything. releaseFilter matches against "<distro> <version>",
// "<distro>-<version>" and the release codename.
func planJobs(fixtures []e2e.Fixture, fixtureFilter, releaseFilter []string) []Job {
	var jobs []Job
	for _, f := range fixtures {
		if !matchesAny(f.Name, fixtureFilter) {
			continue
		}
		if f.Target.Matrix {
			arches := f.Target.MatrixArches
			if len(arches) == 0 {
				arches = []string{f.Target.Arch}
			}
			for _, rel := range distro.Supported() {
				for _, arch := range arches {
					if !releaseMatchesAny(rel, releaseFilter) {
						continue
					}
					jobs = append(jobs, Job{Fixture: f, Release: rel, Arch: arch})
				}
			}
			continue
		}
		rel, err := distro.Resolve(f.Target.Distro, f.Target.Version, "")
		if err != nil {
			jobs = append(jobs, Job{Fixture: f, Arch: f.Target.Arch, SkipReason: "fixture target: " + err.Error()})
			continue
		}
		if !releaseMatchesAny(rel, releaseFilter) {
			continue
		}
		jobs = append(jobs, Job{Fixture: f, Release: rel, Arch: f.Target.Arch})
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].Fixture.Name != jobs[j].Fixture.Name {
			return jobs[i].Fixture.Name < jobs[j].Fixture.Name
		}
		if jobs[i].Release.DistroID != jobs[j].Release.DistroID {
			return jobs[i].Release.DistroID < jobs[j].Release.DistroID
		}
		if jobs[i].Release.VersionID != jobs[j].Release.VersionID {
			return jobs[i].Release.VersionID < jobs[j].Release.VersionID
		}
		return jobs[i].Arch < jobs[j].Arch
	})
	return jobs
}

func matchesAny(name string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	name = strings.ToLower(name)
	for _, f := range filters {
		if strings.Contains(name, strings.ToLower(strings.TrimSpace(f))) {
			return true
		}
	}
	return false
}

func releaseMatchesAny(rel distro.Release, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	candidates := []string{
		strings.ToLower(rel.DistroID + " " + rel.VersionID),
		strings.ToLower(rel.DistroID + "-" + rel.VersionID),
		strings.ToLower(rel.Codename),
	}
	for _, f := range filters {
		f = strings.ToLower(strings.TrimSpace(f))
		for _, c := range candidates {
			if strings.Contains(c, f) {
				return true
			}
		}
	}
	return false
}

// splitList parses a comma-separated --fixture/--release flag value (each
// flag is also repeatable; runMain merges every occurrence) into trimmed,
// non-empty entries.
func splitList(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// nativePlatform is this host's own container platform — arm64 rows only
// need an emulation probe when they are not already it (main.go calls
// harness.PlatformSupported only in that case).
func nativePlatform(hostGOARCH string) string {
	p, _ := distro.Platform(hostGOARCH)
	return p
}
