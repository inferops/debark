//go:build tools

// This file exists so that `go mod tidy` cannot prune dependencies the design
// has chosen but that no package imports yet.
//
// It is not a stylistic preference. During the initial parallel build, a stray
// `go mod tidy` removed every pre-fetched dependency nothing had imported at
// that moment. Two separate work then found the libraries genuinely absent
// and — correctly, per the project's "no new dependencies without review" rule
// — hand-rolled replacements: a flag parser and config loader in place of
// cobra and koanf, and a JSON Schema validator in place of santhosh-tekuri.
// That cost real work and would have cost more: cobra is load-bearing for
// shell completions and man-page generation, which Debian packaging needs
// (ADR-011).
//
// A blank import under a build tag that no build ever sets keeps each module
// in go.mod's require list while adding nothing to any binary. Delete an entry
// here only when the design decision behind it is reversed in an ADR — not
// merely because nothing imports it today.
package tools

import (
	// CLI, config, output — ADR-011.
	_ "github.com/charmbracelet/lipgloss"
	_ "github.com/knadh/koanf/parsers/yaml"
	_ "github.com/knadh/koanf/providers/env/v2"
	_ "github.com/knadh/koanf/providers/file"
	_ "github.com/knadh/koanf/providers/posflag"
	_ "github.com/knadh/koanf/v2"
	_ "github.com/spf13/cobra"
	_ "github.com/spf13/pflag"
	_ "github.com/vbauerster/mpb/v8"
	_ "golang.org/x/term"

	// Schema validation for the published api/schema documents.
	_ "github.com/santhosh-tekuri/jsonschema/v6"

	// Debian format parsing, compression, canonical JSON, OpenPGP — the
	// formats debark is made of.
	_ "github.com/ProtonMail/go-crypto/openpgp"
	_ "github.com/gowebpki/jcs"
	_ "github.com/klauspost/compress/zstd"
	_ "pault.ag/go/debian/deb"

	// Test infrastructure: testscript drives the CLI's user-facing behaviour.
	_ "github.com/rogpeppe/go-internal/testscript"

	_ "golang.org/x/sync/errgroup"
	_ "gopkg.in/yaml.v3"
)
