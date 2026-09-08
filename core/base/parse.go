package base

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/snapshot"
)

// maxDefinitionSize bounds an operator-supplied definition file. A base
// definition is a few dozen lines; anything at this size is a mistake or an
// attempt to make the parser the interesting part of the program, and
// refusing is cheaper than finding out which.
const maxDefinitionSize = 1 << 20 // 1 MiB

// Resolved is a definition plus where it came from, which is exactly what a
// synthesized snapshot's origin block records.
type Resolved struct {
	Definition Definition
	// Builtin says which branch of Resolve produced this: a compiled-in
	// definition, or a file on disk.
	//
	// It is a bool rather than something a caller re-derives from Source,
	// because Source is a *string* and "builtin" is a legal file name. A
	// caller comparing Source against OriginSourceBuiltin would decide that
	// ./builtin was compiled into the binary — and the container path, which
	// uses exactly that decision to choose whether to mount the operator's
	// file, would then hand the inner binary a host path as though it were an
	// id. Resolve is the one place that knows the answer; it records it
	// rather than making every caller guess.
	Builtin bool
	// Source is snapshot.OriginSourceBuiltin, or the BASE NAME of the file
	// the definition was read from.
	//
	// Never the full path. The snapshot travels to a builder and then into a
	// bundle that crosses an air gap; the directory layout of the machine
	// that happened to run from-base is not the target's business, and is
	// exactly the kind of incidental host detail the bundle format keeps
	// out of artefacts.
	Source string
	// Digest is Digest(Definition).
	Digest string
}

// Resolve turns the operator's `--base` argument into a definition.
//
// The argument is either a builtin id or a path to a definition file, and the
// order below is the whole rule:
//
//  1. a builtin id wins outright, so `ubuntu:26.04/desktop` always means the
//     same thing on every machine no matter what files are lying about;
//  2. otherwise, an existing file is read as a definition;
//  3. otherwise it is a usage error naming the closed set of builtin ids.
//
// Step 1 comes first deliberately. The reverse order would let a file in the
// working directory silently redefine a documented base — the same class of
// surprise as a `./git` on PATH — and the ids are shaped (`distro:version/variant`)
// so that no plausible filename collides with one.
func Resolve(idOrPath, arch string) (Resolved, error) {
	if IsBuiltinID(idOrPath) {
		d, err := Lookup(idOrPath, arch)
		if err != nil {
			return Resolved{}, err
		}
		dg, err := Digest(d)
		if err != nil {
			return Resolved{}, err
		}
		return Resolved{Definition: d, Builtin: true, Source: snapshot.OriginSourceBuiltin, Digest: dg}, nil
	}

	if _, statErr := os.Stat(idOrPath); statErr != nil {
		// One message however the stat failed, because from the operator's
		// point of view there is one mistake: this string names neither a
		// base debark ships nor a file on disk.
		//
		// It is deliberately NOT gated on fs.ErrNotExist. A mistyped base id
		// is the single most likely thing to arrive here, and on Windows
		// os.Stat("ubuntu:99.04/desktop") fails with ERROR_INVALID_NAME
		// rather than ENOENT — the colon makes it a syntactically illegal
		// filename — so gating on not-exist handed a Windows operator
		// "CreateFile ...: The filename, directory name, or volume label
		// syntax is incorrect" in place of the closed set of ids this
		// diagnostic exists to print. The underlying error is still shown
		// when it says something a reader could act on (a permission
		// problem on a file that really is there), just never instead of
		// the list.
		err := dferr.New(dferr.Usage,
			"base: %q is neither a builtin base id nor a file that exists", idOrPath)
		hint := "builtin bases: " + strings.Join(BuiltinIDs(), ", ") +
			"\nor pass a path to a base definition file (see docs/formats.md)"
		if !errors.Is(statErr, fs.ErrNotExist) {
			hint = "reading it as a file also failed: " + statErr.Error() + "\n" + hint
		}
		return Resolved{}, err.WithHint("%s", hint)
	}

	// A file whose base name is exactly the builtin sentinel cannot be
	// recorded honestly: origin.source would read "builtin" and claim the
	// definition was compiled into this binary when it was read off the
	// operator's disk. That is a false claim in the one block of the format
	// whose entire job is provenance, so it is refused rather than papered
	// over. Renaming the file costs nothing.
	if filepath.Base(idOrPath) == snapshot.OriginSourceBuiltin {
		return Resolved{}, dferr.New(dferr.Usage,
			"base: a definition file may not be named %q; a snapshot records that name to mean \"compiled into the debark binary\"",
			snapshot.OriginSourceBuiltin).
			WithHint("rename it, e.g. to builtin.yaml")
	}

	d, err := LoadFile(idOrPath, arch)
	if err != nil {
		return Resolved{}, err
	}
	dg, err := Digest(d)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{Definition: d, Builtin: false, Source: filepath.Base(idOrPath), Digest: dg}, nil
}

// LoadFile reads an operator-supplied definition, materialises it for arch and
// validates it.
//
// Unknown fields are refused rather than ignored (yaml.Decoder.KnownFields),
// matching every published debark schema's additionalProperties:false. A
// typo in a field name must not silently mean "the default": a misspelled
// `recommend: true` that quietly resolved with recommends off would produce a
// base whose claim differs from what the file says, and the operator would
// have no way to see it.
func LoadFile(path, arch string) (Definition, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Definition{}, dferr.Wrap(dferr.Usage, err, "base: reading %s", path)
	}
	if info.IsDir() {
		return Definition{}, dferr.New(dferr.Usage, "base: %s is a directory, not a base definition file", path)
	}
	if info.Size() > maxDefinitionSize {
		return Definition{}, dferr.New(dferr.Usage,
			"base: %s is %d bytes; a base definition may not exceed %d", path, info.Size(), maxDefinitionSize)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Definition{}, dferr.Wrap(dferr.Usage, err, "base: reading %s", path)
	}

	var d Definition
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		return Definition{}, dferr.Wrap(dferr.Usage, err, "base: parsing %s", path)
	}
	// A second document in the same file would be silently ignored by the
	// single Decode above, and "the part of my file after the --- did
	// nothing" is a bad way to find that out.
	//
	// The test is "did the stream end", not "did a second Definition parse".
	// Discarding the error instead — the obvious spelling — would only catch
	// a second document that happens to be a well-formed Definition, and the
	// second documents an operator writes by mistake are precisely the ones
	// that are not: a stray sequence, a bare scalar, a copy with a typo'd
	// field that KnownFields would reject.
	var extra Definition
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Definition{}, dferr.New(dferr.Usage,
			"base: %s contains more than one YAML document; a definition file describes exactly one base", path)
	}

	if d.Arch != "" && arch != "" && d.Arch != arch {
		// A file that pins an architecture is making a claim ("this is our
		// arm64 image"), and quietly overriding it with the requested arch
		// would produce a base that is neither what the file says nor what
		// the operator asked for.
		return Definition{}, dferr.New(dferr.Usage,
			"base: %s pins arch %q but %q was requested", path, d.Arch, arch)
	}
	d = d.Materialise(arch)
	if err := Validate(d); err != nil {
		return Definition{}, err
	}
	if err := checkArch(d.Arch); err != nil {
		return Definition{}, err
	}
	return d, nil
}

// Marshal renders a definition as the YAML an operator would commit, so
// `snapshot list-bases --yaml`-style output and the docs' examples come from
// the same code as the parser reads.
func Marshal(d Definition) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(d); err != nil {
		return nil, dferr.Wrap(dferr.Environment, err, "base: rendering definition")
	}
	if err := enc.Close(); err != nil {
		return nil, dferr.Wrap(dferr.Environment, err, "base: rendering definition")
	}
	return buf.Bytes(), nil
}
