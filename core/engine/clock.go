package engine

import (
	"github.com/inferops/debark/core/canonical"
)

// effectiveCreatedAt is the ONLY function in this package that decides what
// "now" means for a build's artefacts. It is called exactly once, from
// engineImpl.Build, and its result becomes build.createdAt — the single
// timestamp every artefact in the bundle threads through from then on (see
// the doc comment on that field, and build.emit/build.warn in evidence.go for
// how evidence.json's own timestamps are kept in the same single-source
// chain). That is what makes the "one value flows to everything" rule
// structural rather than a convention someone could violate one file at a
// time: exactly one place in this package reads the clock, and everything
// downstream only ever reads the string it already computed.
//
// The rule it applies — SOURCE_DATE_EPOCH when set, the real clock when not,
// and a dferr.Usage refusal rather than a silent fallback when the variable
// is set to something malformed — lives in canonical.EffectiveTime. It moved
// there when core/base became a second producer of hashed artefacts (a
// synthesized snapshot, whose digest a bundle then records): two copies of
// the rule would be two chances for one of them to quietly stop honouring the
// variable, and a reproducible build that quietly is not reproducible is the
// exact failure it exists to prevent.
//
// timeNow (vars.go) stays here as this package's own test seam.
func effectiveCreatedAt() (string, error) {
	return canonical.EffectiveTime(timeNow)
}
