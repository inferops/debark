package engine

import (
	"context"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/store"
	"github.com/inferops/debark/core/version"
)

// resolveDeps fills every nil dependency and validates the handful of things
// that must be true before any work starts. It runs before anything is
// written to disk (other than the store, which is content-addressed and
// shared across builds — writing into it is never "producing a bundle"), so
// every error path here satisfies Build's "no bundle was produced at all"
// contract trivially.
func (b *build) resolveDeps(ctx context.Context) error {
	if b.req.Output.Path == "" {
		return dferr.New(dferr.Usage, "engine: output.path is required")
	}
	if b.req.SnapshotRef == "" {
		return dferr.New(dferr.Usage, "engine: snapshot_ref is required")
	}

	b.st = b.eng.deps.Store
	if b.st == nil {
		dir := b.req.Options.StoreDir
		if dir == "" {
			dir = store.DefaultRoot()
		}
		st, err := openStore(dir)
		if err != nil {
			return classify(err, dferr.Environment, "engine: open store")
		}
		b.st = st
	}

	b.repoWr = b.eng.deps.Repo
	if b.repoWr == nil {
		b.repoWr = newRepositoryWriter()
	}

	b.pol = b.eng.deps.Policy
	if b.pol == nil {
		pol, err := loadPolicy(b.req.Options.PolicyRef)
		if err != nil {
			return classify(err, dferr.Usage, "engine: load policy")
		}
		b.pol = pol
	}

	// The caller's sink (or Discard{}) is fanned out together with an
	// internal collector this build keeps for itself, so evidence.json can be
	// written from it later without asking the caller for its history back.
	// evidenceCtx is kept alongside collector, not just handed to
	// NewCollector and forgotten, so finalizeBundle can build evidence.json's
	// Document itself later (see the field's doc comment on build).
	b.evidenceCtx = evidenceContext()
	b.collector = evidence.NewCollector(b.evidenceCtx)
	callerSink := b.eng.deps.Events
	if callerSink == nil {
		callerSink = evidence.Discard{}
	}
	// Only the COLLECTOR is clock-pinned, not the fan-out.
	//
	// b.collector is the copy finalizeBundle writes into evidence.json, which
	// the manifest hashes and the signature covers, so that is the one that
	// must carry the build clock (see buildClockSink in evidence.go).
	// callerSink is Deps.Events — the operator's --json-events NDJSON stream
	// and the progress renderer. Wrapping the whole MultiSink pinned those
	// too, so under SOURCE_DATE_EPOCH a live stream reported every line at
	// the same instant, and in the project's own reproducible-build path
	// (hack/reproducible-check.sh exports `git log -1 --format=%ct`) that
	// instant is the last commit's date. Nothing downstream reads Event.TS
	// for ordering or display, so nothing broke — but log correlation on an
	// NDJSON consumer did, and a fix for artefact reproducibility has no
	// business rewriting what an operator watches happen in real time.
	//
	// Wrapping the collector alone satisfies buildClockSink's own
	// justification exactly ("an event cannot reach this build's collector
	// without passing through it") without touching the caller.
	b.sink = evidence.MultiSink{callerSink, buildClockSink{b: b, next: b.collector}}

	// Empty is passed through, deliberately. This used to fall back to
	// os.Executable() here, which looked harmless and was not: SelfPath's
	// only destination is ContainerOptions.SelfPath, and core/apt's
	// containerSelfPath already resolves an unset value -- but it resolves
	// it BETTER, because only it knows which architecture the container will
	// run and can therefore look for bin/debark-linux-<arch> beside the
	// running executable first. Defaulting here filled the field in before
	// that search could ever happen, so on Windows and macOS every build
	// reached the backend holding a path to a PE or Mach-O binary and was
	// refused with "does not look like a Linux ELF binary" even when the
	// correct Linux build was sitting right beside it.
	b.selfPath = b.eng.deps.SelfPath

	if b.req.Options.ApprovedKeysRef != "" {
		keys, err := loadApprovedKeys(b.req.Options.ApprovedKeysRef)
		if err != nil {
			return classify(err, dferr.Usage, "engine: load approved keys")
		}
		b.approvedKeys = keys
	}
	// A policy file's inline approved_keys is documented as "equivalent to
	// --approved-keys" (policy.Policy.ApprovedKeys, and examples/policy.yaml
	// says the same). It was not equivalent. b.approvedKeys reaches TWO
	// places — apt's real, keyring-derived per-source check
	// (resolveWithBackend threads it into apt.ResolveInput; core/apt/root.go's
	// checkApprovedKeys) and the policy evaluator's much weaker check of what
	// the plan CLAIMS (policy.go) — while an inline list reached only the
	// second. An operator who configured keys in their policy file got half
	// the control they were promised, and nothing said so.
	//
	// Folding it in here, before resolveWithBackend runs, is the whole fix:
	// one list, both consumers.
	b.approvedKeys = unionApprovedKeys(b.approvedKeys, inlineApprovedKeys(b.pol))

	signer := b.eng.deps.Signer
	if signer == nil && b.req.Output.Sign.SignerRef != "" {
		s, err := signerFor(ctx, b.req.Output.Sign.SignerRef)
		if err != nil {
			return classify(err, dferr.Usage, "engine: build signer")
		}
		signer = s
		b.signerOwned = true
	}
	if signer == nil && b.req.Output.Sign.Required {
		return dferr.New(dferr.Usage, "engine: output.sign.required is true but no signer is configured (no Deps.Signer and no signer_ref)")
	}
	b.signer = signer

	return nil
}

// evidenceContext is the run identity written into evidence.json's header.
// Only build/tool identity, never a machine fingerprint: the same rule the
// snapshot's own Labels field follows.
func evidenceContext() map[string]any {
	v := version.Get()
	return map[string]any{
		"tool_name":    v.Name,
		"tool_version": v.Version,
		"tool_edition": v.Edition,
		"go_version":   v.GoVersion,
	}
}
