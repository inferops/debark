package engine

import (
	"os"
	"time"

	"github.com/inferops/debark/core/apt"
	"github.com/inferops/debark/core/bundle"
	"github.com/inferops/debark/core/doctor"
	"github.com/inferops/debark/core/fetch"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
	"github.com/inferops/debark/core/policy"
	"github.com/inferops/debark/core/repository"
	"github.com/inferops/debark/core/sign"
	"github.com/inferops/debark/core/snapshot"
	"github.com/inferops/debark/core/store"
)

// Injection pattern: every real collaborator that Deps does not already give
// us an injection point for is called through a package-level function
// variable here, defaulted to the real implementation. Tests reassign the var
// (saving and restoring the original, typically via t.Cleanup) to substitute
// a fake, without needing apt, a network, a disk-backed store or a
// subprocess. This keeps every non-Deps dependency test-controllable the same
// way the fields of Deps already are, per the contract brief guidance for
// packages whose collaborators are still stub bodies. Pure, side-effect-free
// helpers that already have real (non-stub) bodies — canonical.*,
// resolve.SortSelections, repository.PoolPath, manifest.NewBundleID and
// similar — are called directly and are not listed here: there is nothing to
// fake, they cannot fail and they touch nothing external.
var (
	openSnapshot              = snapshot.Open
	snapshotInstallRecommends = snapshot.InstallRecommends

	openStore = store.Open

	newRepositoryWriter = repository.NewWriter

	loadPolicy       = policy.Load
	loadApprovedKeys = policy.LoadApprovedKeys

	signerFor = sign.SignerFor

	selectBackendFn = apt.SelectBackend

	parseListFile = fetch.ParseListFile
	scanDir       = fetch.ScanDir
	fromLocalFile = fetch.FromLocalFile
	newFetcher    = fetch.New

	doctorRun      = doctor.Run
	doctorFlagsFor = doctor.FlagsFor

	saveLock = lock.Save

	assembleBundle = bundle.Assemble
	exportTar      = bundle.ExportTar
	readmeText     = bundle.ReadmeText

	buildManifest         = manifest.Build
	saveManifest          = manifest.Save
	canonicalManifest     = manifest.Canonical
	saveManifestSignature = manifest.SaveSignature

	mkdirTemp = os.MkdirTemp
	// timeNow is the single clock every artefact timestamp is derived from
	// (via build.createdAt, computed once in engineImpl.Build). Tests fix it
	// to get byte-identical, reproducible output across runs (principle 2).
	timeNow = time.Now

	// debPackageName reads the Package: control field out of a staged .deb
	// file. It parses the ar container and control tar in pure Go via
	// core/fetch.ReadControlInfo, so a Windows or macOS builder handling
	// vendor .deb inputs needs no dpkg-deb on the host even when it uses the
	// container backend for resolution.
	debPackageName = dpkgDebPackageName
)
