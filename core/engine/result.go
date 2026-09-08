package engine

import (
	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
)

// buildResult assembles the final BuildResult. It runs only after a bundle
// genuinely exists on disk (every earlier failure path returns before this),
// so this always returns a non-nil result with a nil error, per Build's own
// contract: "returns an error with no result only when no bundle was
// produced at all".
func (b *build) buildResult() *buildjob.BuildResult {
	warnings := dedupeStrings(warningMessages(b.lockDoc.Warnings))

	unresolved := make([]string, 0, len(b.lockDoc.Unresolved))
	for _, u := range b.lockDoc.Unresolved {
		unresolved = append(unresolved, u.Input)
	}
	unresolved = dedupeStrings(unresolved)

	// One dedupe, two projections: fetch_failed[] is exactly the Input of
	// each fetch_failures[] entry, in the same order, because it is built
	// from it.
	fetchFailures := dedupeFetchFailures(b.fetchFailures)
	var fetchFailed []string
	if len(fetchFailures) > 0 {
		fetchFailed = make([]string, 0, len(fetchFailures))
		for _, f := range fetchFailures {
			fetchFailed = append(fetchFailed, f.Input)
		}
	}

	// Priority between the two non-success classes that can co-occur with a
	// real bundle: a closed-world failure means the bundle apt just built may
	// not actually install offline, which matters more than "one input was
	// missing", so it wins. See the comment on runClosedWorld for why it is
	// Resolution rather than Incomplete in the first place.
	exitClass := buildjob.ExitSuccess
	switch {
	case b.closedWorldFailed:
		exitClass = buildjob.ExitResolution
	case len(unresolved) > 0 || len(fetchFailed) > 0:
		exitClass = buildjob.ExitIncomplete
	}

	var added, removed, unchanged int
	if b.assembled != nil {
		added, removed, unchanged = len(b.assembled.Added), len(b.assembled.Removed), b.assembled.Stats.Unchanged
	}

	bundleID := ""
	if b.manifestDoc != nil {
		bundleID = b.manifestDoc.BundleID
	}

	return &buildjob.BuildResult{
		SchemaVersion: buildjob.SchemaVersion,
		LockRef:       lock.FileName,
		ManifestRef:   manifest.FileName,
		BundlePath:    b.resultBundlePath,
		BundleID:      bundleID,
		Signed:        b.signed,
		Stats: buildjob.Stats{
			Added:           added,
			Removed:         removed,
			Unchanged:       unchanged,
			Bytes:           b.totalBytes,
			DownloadedBytes: b.downloadedBytes,
			PackageCount:    len(b.lockDoc.Packages),
			DurationSeconds: int(timeNow().Sub(b.startedAt).Seconds()),
		},
		Warnings:      warnings,
		Unresolved:    unresolved,
		FetchFailed:   fetchFailed,
		FetchFailures: fetchFailures,
		ExitClass:     exitClass,
	}
}

func warningMessages(ws []lock.Warning) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Message)
	}
	return out
}
