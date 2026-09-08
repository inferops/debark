package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"

	"github.com/inferops/debark/core/dferr"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"DEBARK_CONFIG", "DEBARK_PROFILE", "DEBARK_BACKEND", "DEBARK_IMAGE",
		"DEBARK_CONTAINER_RUNTIME", "DEBARK_SIGN", "DEBARK_POLICY",
		"DEBARK_APPROVED_KEYS", "DEBARK_KEYS", "DEBARK_KEYRING_DIRS",
		"DEBARK_SELF_BINARY",
		"XDG_CONFIG_HOME", "APPDATA",
	} {
		t.Setenv(k, "")
	}
}

// flagsFor builds a *pflag.FlagSet carrying --config and --profile (as the
// real CLI's persistent flags do) plus any extra flags a test wants to
// exercise flag > env > file precedence with (e.g. "backend", "image").
func flagsFor(t *testing.T, configPath, profile string, extra ...string) *pflag.FlagSet {
	t.Helper()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.String("config", "", "")
	fs.String("profile", "", "")
	for _, name := range extra {
		fs.String(name, "", "")
	}
	var args []string
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	if profile != "" {
		args = append(args, "--profile", profile)
	}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	return fs
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.yaml")

	cfg, err := Load(flagsFor(t, path, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Loaded {
		t.Error("Loaded = true for a missing file")
	}
	if cfg.Backend != "auto" {
		t.Errorf("Backend = %q, want default %q", cfg.Backend, "auto")
	}
	if cfg.Path != path {
		t.Errorf("Path = %q, want %q", cfg.Path, path)
	}
}

func TestLoadNilFlagSetUsesDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("DEBARK_CONFIG", filepath.Join(t.TempDir(), "nope.yaml"))
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load(nil): %v", err)
	}
	if cfg.Backend != "auto" {
		t.Errorf("Backend = %q", cfg.Backend)
	}
}

func TestLoadMalformedFileIsUsageError(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("not: valid: yaml: [x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(flagsFor(t, path, ""))
	if err == nil {
		t.Fatal("Load: want error for malformed YAML")
	}
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("class = %v, want Usage", dferr.ClassOf(err))
	}
}

func TestLoadFileOverridesDefaults(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write(t, path, file{SchemaVersion: SchemaVersion, Settings: Settings{Backend: "local", Image: "myimage"}})

	cfg, err := Load(flagsFor(t, path, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Backend != "local" {
		t.Errorf("Backend = %q, want %q", cfg.Backend, "local")
	}
	if cfg.Image != "myimage" {
		t.Errorf("Image = %q, want %q", cfg.Image, "myimage")
	}
	if !cfg.Loaded {
		t.Error("Loaded = false, want true")
	}
}

func TestLoadProfileOverlaysBase(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write(t, path, file{
		SchemaVersion: SchemaVersion,
		Settings:      Settings{Backend: "local", SignKey: "base.key"},
		Profiles: map[string]Settings{
			"prod": {Backend: "container", Image: "prod-image"},
		},
	})

	cfg, err := Load(flagsFor(t, path, "prod"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Backend != "container" {
		t.Errorf("Backend = %q, want profile value %q", cfg.Backend, "container")
	}
	if cfg.Image != "prod-image" {
		t.Errorf("Image = %q, want %q", cfg.Image, "prod-image")
	}
	// Base-only field must survive the overlay.
	if cfg.SignKey != "base.key" {
		t.Errorf("SignKey = %q, want base value %q (profile should overlay, not replace)", cfg.SignKey, "base.key")
	}
	if cfg.Profile != "prod" {
		t.Errorf("Profile = %q, want %q", cfg.Profile, "prod")
	}
}

func TestLoadUnknownProfileIsUsageError(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write(t, path, file{SchemaVersion: SchemaVersion, Profiles: map[string]Settings{"staging": {}}})

	_, err := Load(flagsFor(t, path, "bogus"))
	if err == nil {
		t.Fatal("Load: want error for unknown profile")
	}
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("class = %v, want Usage", dferr.ClassOf(err))
	}
}

func TestLoadProfileWithNoFileIsUsageError(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	_, err := Load(flagsFor(t, path, "prod"))
	if err == nil {
		t.Fatal("Load: want error when a profile is requested but no file exists")
	}
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("class = %v, want Usage", dferr.ClassOf(err))
	}
}

func TestEnvOverridesFile(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write(t, path, file{SchemaVersion: SchemaVersion, Settings: Settings{Backend: "local"}})

	t.Setenv("DEBARK_BACKEND", "container")
	cfg, err := Load(flagsFor(t, path, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Backend != "container" {
		t.Errorf("Backend = %q, want env override %q", cfg.Backend, "container")
	}
}

func TestEnvKeyNameDiffersFromFieldName(t *testing.T) {
	// DEBARK_SIGN -> sign_key is the case the package doc calls out
	// explicitly: koanf's generic prefix-strip cannot express this, only
	// the explicit envKeyMap can.
	clearEnv(t)
	t.Setenv("DEBARK_SIGN", "gpg:ABCD1234")
	cfg, err := Load(flagsFor(t, filepath.Join(t.TempDir(), "none.yaml"), ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SignKey != "gpg:ABCD1234" {
		t.Errorf("SignKey = %q, want %q", cfg.SignKey, "gpg:ABCD1234")
	}
}

func TestEnvEmptyListDoesNotClearFileValue(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write(t, path, file{SchemaVersion: SchemaVersion, Settings: Settings{VerifyKeys: []string{"/etc/debark/release.pub"}}})

	t.Setenv("DEBARK_KEYS", "")
	cfg, err := Load(flagsFor(t, path, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.VerifyKeys) != 1 || cfg.VerifyKeys[0] != "/etc/debark/release.pub" {
		t.Errorf("VerifyKeys = %v, want the file's value preserved", cfg.VerifyKeys)
	}
}

func TestEnvProfileAppliesWhenFlagEmpty(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write(t, path, file{SchemaVersion: SchemaVersion, Profiles: map[string]Settings{"ci": {Backend: "container"}}})

	t.Setenv("DEBARK_PROFILE", "ci")
	cfg, err := Load(flagsFor(t, path, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Profile != "ci" || cfg.Backend != "container" {
		t.Errorf("cfg = %+v, want profile ci applied", cfg.Settings)
	}
}

func TestExplicitProfileFlagWinsOverEnv(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write(t, path, file{SchemaVersion: SchemaVersion, Profiles: map[string]Settings{
		"ci":   {Backend: "container"},
		"prod": {Backend: "local"},
	}})
	t.Setenv("DEBARK_PROFILE", "ci")

	cfg, err := Load(flagsFor(t, path, "prod"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Profile != "prod" || cfg.Backend != "local" {
		t.Errorf("explicit profile should win over env: cfg = %+v", cfg.Settings)
	}
}

func TestFlagOverridesEverything(t *testing.T) {
	// The full four-level chain: file says "local", env says "container",
	// the command's own --backend flag says "auto" and must win — this is
	// posflag.Provider's own "an explicitly Changed flag always overrides"
	// rule (see the package doc), not logic this package reimplements.
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write(t, path, file{SchemaVersion: SchemaVersion, Settings: Settings{Backend: "local"}})
	t.Setenv("DEBARK_BACKEND", "container")

	fs := flagsFor(t, path, "", "backend")
	if err := fs.Set("backend", "auto"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(fs)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Backend != "auto" {
		t.Errorf("Backend = %q, want the explicitly-set flag value %q", cfg.Backend, "auto")
	}
}

func TestUnchangedFlagDoesNotOverrideConfig(t *testing.T) {
	// A flag that was registered with a default but never actually passed
	// on the command line (f.Changed == false) must not clobber a
	// file-configured value with its zero-value default.
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write(t, path, file{SchemaVersion: SchemaVersion, Settings: Settings{Image: "from-file"}})

	fs := flagsFor(t, path, "", "image") // registered, never Set
	cfg, err := Load(fs)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Image != "from-file" {
		t.Errorf("Image = %q, want the file's value %q preserved", cfg.Image, "from-file")
	}
}

func TestInitRefusesToOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if _, err := Init(path, false); err != nil {
		t.Fatalf("first Init: %v", err)
	}
	_, err := Init(path, false)
	if err == nil {
		t.Fatal("second Init without --force: want error")
	}
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("class = %v, want Usage", dferr.ClassOf(err))
	}
	if _, err := Init(path, true); err != nil {
		t.Fatalf("Init with force=true: %v", err)
	}
}

func TestInitProducesLoadableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if _, err := Init(path, false); err != nil {
		t.Fatalf("Init: %v", err)
	}
	clearEnv(t)
	cfg, err := Load(flagsFor(t, path, ""))
	if err != nil {
		t.Fatalf("Load(Init'd file): %v", err)
	}
	if !cfg.Loaded {
		t.Error("Loaded = false after Init")
	}
	if len(cfg.ProfileNames) != 1 || cfg.ProfileNames[0] != "example" {
		t.Errorf("ProfileNames = %v, want [example] (Init's own sample profile)", cfg.ProfileNames)
	}
}

func TestInitWritesYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if _, err := Init(path, false); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatalf("Init did not write valid YAML: %v\n%s", err, b)
	}
	if m["schema_version"] != SchemaVersion {
		t.Errorf("schema_version = %v", m["schema_version"])
	}
}

func TestDefaultPathHonoursXDGConfigHome(t *testing.T) {
	clearEnv(t)
	t.Setenv("XDG_CONFIG_HOME", filepath.FromSlash("/xdgcfg"))
	got := DefaultPath()
	want := filepath.Join(filepath.FromSlash("/xdgcfg"), "debark", "config.yaml")
	if got != want {
		t.Errorf("DefaultPath() = %q, want %q", got, want)
	}
}

func TestDefaultPathHonoursExplicitEnvOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("DEBARK_CONFIG", filepath.FromSlash("/explicit/config.yaml"))
	if got := DefaultPath(); got != filepath.FromSlash("/explicit/config.yaml") {
		t.Errorf("DefaultPath() = %q", got)
	}
}

func TestConfigJSONRoundTrips(t *testing.T) {
	clearEnv(t)
	cfg := &Config{Settings: Settings{Backend: "local"}, Path: "/x/config.yaml", Loaded: true}
	b, err := cfg.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if m["schema_version"] != SchemaVersion {
		t.Errorf("schema_version = %v", m["schema_version"])
	}
	if m["backend"] != "local" {
		t.Errorf("backend = %v", m["backend"])
	}
}

func write(t *testing.T, path string, f file) {
	t.Helper()
	b, err := yaml.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// self_binary exists so the container backend is usable off Linux at all:
// on Windows and macOS the running executable is never a Linux ELF binary,
// so a path to one has to come from somewhere, and typing it on every
// build is not a workflow.
func TestSelfBinaryFromFileAndEnv(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write(t, path, file{SchemaVersion: SchemaVersion, Settings: Settings{SelfBinary: "/from/file"}})

	cfg, err := Load(flagsFor(t, path, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SelfBinary != "/from/file" {
		t.Errorf("SelfBinary = %q, want the file's value", cfg.SelfBinary)
	}

	t.Setenv("DEBARK_SELF_BINARY", "/from/env")
	cfg, err = Load(flagsFor(t, path, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SelfBinary != "/from/env" {
		t.Errorf("SelfBinary = %q, want the env override", cfg.SelfBinary)
	}
}

func TestSelfBinaryUnsetByDefault(t *testing.T) {
	clearEnv(t)
	cfg, err := Load(flagsFor(t, filepath.Join(t.TempDir(), "none.yaml"), ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SelfBinary != "" {
		t.Errorf("SelfBinary = %q, want empty: the default is core/apt's own per-architecture search", cfg.SelfBinary)
	}
}
