// Package engine is the library the CLI calls and a scheduler could call
// tomorrow: BuildRequest in, BuildResult out, no terminal, no global config,
// no assumption about who is calling (principle 3, ADR-003).
package engine

import (
	"context"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/apt"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/policy"
	"github.com/inferops/debark/core/repository"
	"github.com/inferops/debark/core/sign"
	"github.com/inferops/debark/core/store"
)

// Engine builds bundles.
type Engine interface {
	// Build runs the whole pipeline: load the snapshot, select a backend,
	// resolve, fetch, ingest, index, lock, closed-world check, manifest, sign,
	// assemble, prune, export.
	//
	// It returns a BuildResult even for the incomplete case (exit 3), because
	// an incomplete bundle still exists and the operator needs to know exactly
	// what is missing from it. It returns an error with no result only when no
	// bundle was produced at all.
	Build(ctx context.Context, req buildjob.BuildRequest) (*buildjob.BuildResult, error)
}

// Deps are the engine's collaborators. Every one of them is an interface, so
// the pipeline is testable without apt, without a network and without a store
// on disk.
type Deps struct {
	// Backend resolves. When nil, the engine calls apt.SelectBackend using
	// the request's backend option.
	Backend apt.Backend
	// SelectBackend overrides backend selection entirely, for tests.
	SelectBackend func(ctx context.Context, sel apt.Selection) (apt.Backend, apt.Capabilities, error)
	// Store holds fetched objects. When nil, the engine opens the store at
	// the request's StoreDir or store.DefaultRoot().
	Store store.Store
	// Repo writes the repository indices. When nil, repository.NewWriter().
	Repo repository.Writer
	// Signer signs the manifest. When nil, the engine builds one from the
	// request's SignerRef; a request with no signer and Required false
	// produces an unsigned bundle.
	Signer sign.Signer
	// Policy evaluates local policy. When nil, loaded from the request.
	Policy policy.Evaluator
	// Events receives the evidence stream. When nil, evidence.Discard{}.
	Events evidence.Sink
	// SelfPath is the debark binary the container backend mounts and
	// re-enters: a static linux/<arch> build. When empty, core/apt looks for
	// bin/debark-linux-<arch> beside the running executable and then falls
	// back to the running executable itself, which is the only fallback that
	// can work on a host whose own executable is not a Linux ELF binary.
	SelfPath string
}

// New builds an engine from its dependencies.
//
// This file is otherwise frozen (it defines the Engine/Deps contract other packages compile against), but New's body is core/engine's one deliverable
// from that contract, so — following the same rule api.go files in every
// other package follow ("you own the file — replace the stub bodies with
// real implementations — but do not change an existing signature, and do not
// remove a declaration") — only this function body is implemented here. The
// substantive pipeline lives in the rest of this package, not in this file.
func New(deps Deps) (Engine, error) {
	return &engineImpl{deps: deps}, nil
}
