// Frozen public API of the verify package.

package verify

// New returns the standard verifier.
func New() Verifier { return standardVerifier{} }
