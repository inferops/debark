package harness

import "context"

// Debark invokes a debark binary already present inside a container.
// Every subcommand's exact flags live here and only here (
// docs/dev/resolve-contract.md), so a CLI contract change is a one-file fix
// instead of a hunt through twenty fixture-driving call sites.
type Debark struct {
	C       *Container
	BinPath string // path to the debark binary inside the container

	// Env is set on every subcommand this value invokes. It exists for
	// SOURCE_DATE_EPOCH (see scenario.go's builder construction): a
	// determinism claim is only testable against a fixed build clock, and
	// the clock has to be fixed for BOTH builds being compared, so it
	// belongs on the Debark value the row builds through rather than on
	// any single call's args.
	Env map[string]string
}

func (d Debark) run(ctx context.Context, workdir string, args ...string) (CmdResult, error) {
	return d.C.Exec(ctx, ExecOpts{WorkDir: workdir, Env: d.Env}, append([]string{d.BinPath}, args...)...)
}

// SnapshotCreateArgs configures `debark snapshot create`.
type SnapshotCreateArgs struct {
	Out    string
	Redact bool
	Labels map[string]string
}

func (d Debark) SnapshotCreate(ctx context.Context, a SnapshotCreateArgs) (CmdResult, error) {
	args := []string{"snapshot", "create", "--out", a.Out}
	if a.Redact {
		args = append(args, "--redact")
	}
	for k, v := range a.Labels {
		args = append(args, "--label", k+"="+v)
	}
	return d.run(ctx, "", args...)
}

// BuildArgs configures `debark build`.
type BuildArgs struct {
	Snapshot     string
	Lists        []string
	LocalDirs    []string
	Packages     []string // positional args: apt: / url: / file: / bare names
	Out          string   // directory output (this harness always uses --out, never --tar, so a bundle is inspectable/tamperable as a plain directory)
	Update       bool
	NoPrune      bool
	Upgrades     bool
	NoRecommends bool
	Backend      string // "local" inside a same-release container (see resolve-contract.md's own re-entry convention)
	Sign         string // KEYREF, e.g. a private key file path
	NoSign       bool
	ApprovedKeys string
	Policy       string
	AckRedist    bool
	ExtraFlags   []string
}

func (d Debark) Build(ctx context.Context, a BuildArgs) (CmdResult, error) {
	args := []string{"build", "--snapshot", a.Snapshot}
	for _, l := range a.Lists {
		args = append(args, "--list", l)
	}
	for _, ld := range a.LocalDirs {
		args = append(args, "--local-dir", ld)
	}
	if a.Out != "" {
		args = append(args, "--out", a.Out)
	}
	if a.Update {
		args = append(args, "--update")
	}
	if a.NoPrune {
		args = append(args, "--no-prune")
	}
	if a.Upgrades {
		args = append(args, "--upgrades")
	}
	if a.NoRecommends {
		args = append(args, "--no-recommends")
	}
	if a.Backend != "" {
		args = append(args, "--backend", a.Backend)
	}
	if a.Sign != "" {
		args = append(args, "--sign", a.Sign)
	}
	if a.NoSign {
		args = append(args, "--no-sign")
	}
	if a.ApprovedKeys != "" {
		args = append(args, "--approved-keys", a.ApprovedKeys)
	}
	if a.Policy != "" {
		args = append(args, "--policy", a.Policy)
	}
	if a.AckRedist {
		args = append(args, "--acknowledge-redistribution")
	}
	args = append(args, a.ExtraFlags...)
	args = append(args, a.Packages...)
	return d.run(ctx, "", args...)
}

// VerifyArgs configures `debark verify`.
type VerifyArgs struct {
	Bundle        string
	Keys          []string
	Keyrings      []string
	AllowUnsigned bool
	JSON          bool
	// ExtraFlags are appended verbatim, for a fixture's request.verify_flags.
	ExtraFlags []string
}

func (d Debark) Verify(ctx context.Context, a VerifyArgs) (CmdResult, error) {
	args := []string{"verify", a.Bundle}
	for _, k := range a.Keys {
		args = append(args, "--key", k)
	}
	for _, k := range a.Keyrings {
		args = append(args, "--keyring", k)
	}
	if a.AllowUnsigned {
		args = append(args, "--allow-unsigned")
	}
	if a.JSON {
		args = append(args, "--json")
	}
	args = append(args, a.ExtraFlags...)
	return d.run(ctx, "", args...)
}

// InstallArgs configures `debark install`.
type InstallArgs struct {
	Bundle        string
	Status        bool
	DryRun        bool
	Upgrade       bool
	All           bool
	KeepSource    bool
	Fast          bool
	Dpkg          bool
	Yes           bool
	Keys          []string
	Keyrings      []string
	AllowUnsigned bool
	JSON          bool
	// ExtraFlags are appended verbatim, for a fixture's request.install_flags.
	ExtraFlags []string
}

func (d Debark) Install(ctx context.Context, a InstallArgs) (CmdResult, error) {
	args := []string{"install", a.Bundle}
	if a.Status {
		args = append(args, "--status")
	}
	if a.DryRun {
		args = append(args, "--dry-run")
	}
	if a.Upgrade {
		args = append(args, "--upgrade")
	}
	if a.All {
		args = append(args, "--all")
	}
	if a.KeepSource {
		args = append(args, "--keep-source")
	}
	if a.Fast {
		args = append(args, "--fast")
	}
	if a.Dpkg {
		args = append(args, "--dpkg")
	}
	if a.Yes {
		args = append(args, "--yes")
	}
	for _, k := range a.Keys {
		args = append(args, "--key", k)
	}
	for _, k := range a.Keyrings {
		args = append(args, "--keyring", k)
	}
	if a.AllowUnsigned {
		args = append(args, "--allow-unsigned")
	}
	if a.JSON {
		args = append(args, "--json")
	}
	args = append(args, a.ExtraFlags...)
	return d.run(ctx, "", args...)
}

// Keygen runs `debark keygen --out KEYFILE`, generating an unencrypted
// ed25519 private key at out and a public key alongside it at
// strings.TrimSuffix(out, ".key") + ".pub" (sign.PrivateKeyFileSuffix /
// PublicKeyFileSuffix, mirrored in cmd_keygen.go).
func (d Debark) Keygen(ctx context.Context, out, comment string) (CmdResult, error) {
	args := []string{"keygen", "--out", out}
	if comment != "" {
		args = append(args, "--comment", comment)
	}
	return d.run(ctx, "", args...)
}

// DoctorArgs configures `debark doctor`.
type DoctorArgs struct {
	Bundle   string // exactly one of Bundle or Snapshot
	Snapshot string
	JSON     bool
}

func (d Debark) Doctor(ctx context.Context, a DoctorArgs) (CmdResult, error) {
	var args []string
	if a.Snapshot != "" {
		args = []string{"doctor", "--snapshot", a.Snapshot}
	} else {
		args = []string{"doctor", a.Bundle}
	}
	if a.JSON {
		args = append(args, "--json")
	}
	return d.run(ctx, "", args...)
}
