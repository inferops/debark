// Package config is debark's config loader: precedence flag > env
// (DEBARK_*) > config file > defaults, XDG paths, and profiles for
// repeated targets — built on knadh/koanf/v2, as specified
// .
//
// The four layers are koanf Load/Merge calls, in precedence order low to
// high: defaults are pre-set on the target struct (koanf's mapstructure
// decode only touches keys a source actually provides, so an absent key
// never clobbers a default); the YAML file, if one exists, via
// providers/file + parsers/yaml; a named profile's own sub-tree
// ("profiles.<name>.*"), koanf-Cut and Merged on top of the file's
// top-level settings; DEBARK_* environment variables via providers/env/v2
// with an explicit key transform (env var names and Settings' field names
// are not always the same word, e.g. DEBARK_SIGN -> sign_key); and
// finally, when the caller passes the running command's *pflag.FlagSet,
// providers/posflag — which itself already implements exactly "an
// explicitly-set flag always wins; an unset flag's default never
// overwrites a value config already has" (see posflag.Read), so no flag
// precedence logic needs to be reimplemented here. posflag only overlays
// flags whose name matches a Settings key verbatim (backend, image); --key
// /--keyring and similar remain each command's own explicit
// flag-then-config fallback (cmd_verify.go, cmd_install.go) because their
// flag names (key, keyring) intentionally differ from the config field
// names (verify_keys, verify_keyring_dirs) they feed.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	kyaml "github.com/knadh/koanf/parsers/yaml"
	kenv "github.com/knadh/koanf/providers/env/v2"
	kfile "github.com/knadh/koanf/providers/file"
	kposflag "github.com/knadh/koanf/providers/posflag"
	"github.com/knadh/koanf/v2"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"

	"github.com/inferops/debark/core/dferr"
)

// SchemaVersion identifies the config file format. It is internal to the
// CLI — it never crosses the gap — but is versioned so a future migration
// has somewhere to hook in.
const SchemaVersion = "debark.config/v1"

// Settings is the part of a Config that can also appear inside a named
// profile. A zero value for any field means "unset", so a profile can
// override a subset of the base settings without clobbering the rest, and
// koanf's decode leaves an unset field exactly as the caller initialised it
// (see the package doc).
type Settings struct {
	// StoreDir overrides the content-addressed store location. Empty defers
	// to store.DefaultRoot(), which already honours DEBARK_STORE.
	StoreDir string `koanf:"store_dir" yaml:"store_dir,omitempty" json:"store_dir,omitempty"`

	// Backend is auto, local or container.
	Backend string `koanf:"backend" yaml:"backend,omitempty" json:"backend,omitempty"`
	// Image overrides the container image the distro table would pick.
	Image string `koanf:"image" yaml:"image,omitempty" json:"image,omitempty"`
	// ContainerRuntime is docker or podman; empty autodetects.
	ContainerRuntime string `koanf:"container_runtime" yaml:"container_runtime,omitempty" json:"container_runtime,omitempty"`
	// SelfBinary is a static linux/<arch> build of debark for the
	// container backend to mount and re-enter. It has to be settable
	// somewhere other than the command line because on Windows and macOS
	// the running executable is never a Linux ELF binary, so the container
	// backend cannot work at all without one, and typing the path on every
	// build is not a workflow. Empty looks beside the running executable
	// for bin/debark-linux-<arch> and then falls back to the running
	// executable itself (core/apt's containerSelfPath).
	SelfBinary string `koanf:"self_binary" yaml:"self_binary,omitempty" json:"self_binary,omitempty"`

	// SignKey is the default --sign key reference for `build`.
	SignKey string `koanf:"sign_key" yaml:"sign_key,omitempty" json:"sign_key,omitempty"`
	// PolicyFile is the default --policy file for `build`.
	PolicyFile string `koanf:"policy_file" yaml:"policy_file,omitempty" json:"policy_file,omitempty"`
	// ApprovedKeysFile is the default --approved-keys file for `build`.
	ApprovedKeysFile string `koanf:"approved_keys_file" yaml:"approved_keys_file,omitempty" json:"approved_keys_file,omitempty"`

	// VerifyKeys and VerifyKeyringDirs are the default --key/--keyring
	// sources for `verify` and `install`.
	VerifyKeys        []string `koanf:"verify_keys" yaml:"verify_keys,omitempty" json:"verify_keys,omitempty"`
	VerifyKeyringDirs []string `koanf:"verify_keyring_dirs" yaml:"verify_keyring_dirs,omitempty" json:"verify_keyring_dirs,omitempty"`
}

// file is the on-disk shape written by Init and read by Load.
type file struct {
	SchemaVersion string `yaml:"schema_version"`
	Settings      `yaml:",inline"`
	Profiles      map[string]Settings `yaml:"profiles,omitempty"`
}

// Config is the fully resolved configuration for one run: defaults, the
// config file, any selected profile, the DEBARK_* environment and
// (when Load was given one) the running command's flags, merged in that
// order.
type Config struct {
	Settings
	// Path is the file that was read, or would be written by `config init`;
	// it is set even when the file does not exist.
	Path string
	// PathExplicit is true when Path came from --config or DEBARK_CONFIG
	// rather than XDG discovery.
	PathExplicit bool
	// Loaded is true when a file actually existed at Path.
	Loaded bool
	// Profile is the profile name that was applied, if any.
	Profile string
	// ProfileNames lists every profile defined in the file, sorted, for
	// `config show` and error messages.
	ProfileNames []string
}

func defaults() Settings {
	return Settings{Backend: "auto"}
}

// DefaultPath returns the XDG-correct config file location:
// $DEBARK_CONFIG, else $XDG_CONFIG_HOME/debark/config.yaml, else
// %APPDATA%\debark\config.yaml on Windows, else ~/.config/debark/config.yaml.
func DefaultPath() string {
	if v := os.Getenv("DEBARK_CONFIG"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "debark", "config.yaml")
	}
	if runtime.GOOS == "windows" {
		if v := os.Getenv("APPDATA"); v != "" {
			return filepath.Join(v, "debark", "config.yaml")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "debark", "config.yaml")
	}
	return filepath.Join(home, ".config", "debark", "config.yaml")
}

// envTransform maps a DEBARK_* environment variable to the koanf key it
// overlays. Names differ from Settings' own field names in a few places
// (DEBARK_SIGN -> sign_key, not sign), which is exactly what koanf's
// generic prefix-strip transform cannot express and an explicit table can.
// DEBARK_CONFIG, DEBARK_PROFILE and DEBARK_STORE are deliberately
// absent: the first two are resolved before this overlay ever runs (see
// Load), and the third is left to store.DefaultRoot(), which already reads
// it — re-reading it here would race a second copy of the same rule.
var envKeyMap = map[string]string{
	"DEBARK_BACKEND":           "backend",
	"DEBARK_IMAGE":             "image",
	"DEBARK_CONTAINER_RUNTIME": "container_runtime",
	"DEBARK_SIGN":              "sign_key",
	"DEBARK_POLICY":            "policy_file",
	"DEBARK_APPROVED_KEYS":     "approved_keys_file",
	"DEBARK_SELF_BINARY":       "self_binary",
}

var envListKeyMap = map[string]string{
	"DEBARK_KEYS":         "verify_keys",
	"DEBARK_KEYRING_DIRS": "verify_keyring_dirs",
}

func envTransform(k, v string) (string, any) {
	// An unset env var never reaches this function at all (env.Provider
	// only calls it for variables that exist in os.Environ()); an
	// explicitly-empty one — DEBARK_BACKEND="" — is still treated as
	// "says nothing" for every key, scalar or list, the same way an
	// unpassed flag or an absent file key does, rather than overwriting a
	// file- or profile-configured value with the empty string.
	if v == "" {
		return "", nil
	}
	if key, ok := envKeyMap[k]; ok {
		return key, v
	}
	if key, ok := envListKeyMap[k]; ok {
		return key, filepath.SplitList(v)
	}
	return "", nil
}

// Load resolves the effective configuration. fs, when non-nil, is the
// running command's flag set (cobra's cmd.Flags(), which already merges in
// the inherited persistent --config/--profile/etc.) — pass nil only from a
// context with no command flags at all (there is no such caller today; it
// exists so this package is independently testable). --config and
// --profile are read from fs when present; a missing config file is not an
// error — defaults (overlaid by env and flags) apply and Loaded is false.
// A present-but-malformed file is an error.
func Load(fs *pflag.FlagSet) (*Config, error) {
	explicitPath, _ := getStringFlag(fs, "config")
	explicitProfile, _ := getStringFlag(fs, "profile")

	path := explicitPath
	pathExplicit := explicitPath != ""
	if path == "" {
		path = DefaultPath()
	}

	profile := explicitProfile
	if profile == "" {
		profile = os.Getenv("DEBARK_PROFILE")
	}

	settings := defaults()
	k := koanf.New(".")
	loaded := false
	var profileNames []string

	if raw, err := os.Stat(path); err == nil && !raw.IsDir() {
		loaded = true
		full := koanf.New(".")
		if err := full.Load(kfile.Provider(path), kyaml.Parser()); err != nil {
			return nil, dferr.Wrap(dferr.Usage, err, "config: parse %s", path)
		}
		if err := k.Merge(full); err != nil {
			return nil, dferr.Wrap(dferr.Usage, err, "config: load %s", path)
		}
		profileNames = full.MapKeys("profiles")

		if profile != "" {
			if !full.Exists("profiles." + profile) {
				return nil, dferr.Usagef("config: profile %q not found in %s%s", profile, path, availableProfilesHint(profileNames))
			}
			if err := k.Merge(full.Cut("profiles." + profile)); err != nil {
				return nil, dferr.Wrap(dferr.Usage, err, "config: apply profile %q", profile)
			}
		}
	} else if err != nil && !os.IsNotExist(err) {
		return nil, dferr.Wrap(dferr.Environment, err, "config: stat %s", path)
	} else if profile != "" {
		return nil, dferr.Usagef("config: profile %q requested but %s does not exist", profile, path)
	}

	if err := k.Load(kenv.Provider(".", kenv.Opt{TransformFunc: envTransform}), nil); err != nil {
		return nil, dferr.Wrap(dferr.Environment, err, "config: read environment")
	}

	if fs != nil {
		if err := k.Load(kposflag.Provider(fs, ".", k), nil); err != nil {
			return nil, dferr.Wrap(dferr.Usage, err, "config: read flags")
		}
	}

	if err := k.Unmarshal("", &settings); err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "config: decode")
	}

	return &Config{
		Settings:     settings,
		Path:         path,
		PathExplicit: pathExplicit,
		Loaded:       loaded,
		Profile:      profile,
		ProfileNames: sortedCopy(profileNames),
	}, nil
}

func getStringFlag(fs *pflag.FlagSet, name string) (string, bool) {
	if fs == nil {
		return "", false
	}
	f := fs.Lookup(name)
	if f == nil {
		return "", false
	}
	return f.Value.String(), f.Changed
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func availableProfilesHint(names []string) string {
	if len(names) == 0 {
		return " (no profiles are defined)"
	}
	return fmt.Sprintf(" (available: %v)", sortedCopy(names))
}

// Init writes a default config file at path (DefaultPath() if empty). It
// refuses to overwrite an existing file unless force is set.
func Init(path string, force bool) (string, error) {
	if path == "" {
		path = DefaultPath()
	}
	if !force {
		if _, err := os.Stat(path); err == nil {
			return path, dferr.Usagef("config: %s already exists (use --force to overwrite)", path)
		} else if !os.IsNotExist(err) {
			return path, dferr.Wrap(dferr.Environment, err, "config: stat %s", path)
		}
	}
	f := file{SchemaVersion: SchemaVersion, Settings: defaults(), Profiles: map[string]Settings{
		"example": {Backend: "container", Image: ""},
	}}
	b, err := yaml.Marshal(f)
	if err != nil {
		return path, dferr.Wrap(dferr.Environment, err, "config: encode default")
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return path, dferr.Wrap(dferr.Environment, err, "config: create %s", dir)
		}
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return path, dferr.Wrap(dferr.Environment, err, "config: write %s", path)
	}
	return path, nil
}

// JSON renders cfg as the documented `config show --json` object: the
// effective settings plus where they came from.
func (c *Config) JSON() ([]byte, error) {
	type shown struct {
		SchemaVersion string   `json:"schema_version"`
		Path          string   `json:"path"`
		Loaded        bool     `json:"loaded"`
		Profile       string   `json:"profile,omitempty"`
		ProfileNames  []string `json:"profile_names,omitempty"`
		Settings
	}
	out := shown{
		SchemaVersion: SchemaVersion,
		Path:          c.Path,
		Loaded:        c.Loaded,
		Profile:       c.Profile,
		ProfileNames:  c.ProfileNames,
		Settings:      c.Settings,
	}
	return json.MarshalIndent(out, "", "  ")
}
