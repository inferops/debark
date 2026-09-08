package harness

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// ApplyTargetState brings a freshly started container to the dpkg/apt state
// spec describes, using nothing but the container's own real apt — never a
// private root — because a real target's dpkg status and apt config are
// exactly what a real apt-get and real file writes leave behind, and that is
// what `debark snapshot create` will eventually read straight off disk
// (dpkg_status verbatim, sources.list.d/preferences.d/
// apt.conf.d verbatim). Order matters: apt.conf.d overrides land before
// anything is installed, so Install-Recommends and similar settings are in
// effect for the installed-state construction too, exactly as they would
// have been on a real machine all along.
//
// repoHostDir/repoBaseURL locate this run's RepoServer: any TargetSpec.
// Installed.Repos are built once on the host into repoHostDir and referenced
// from the container's sources.list.d via repoBaseURL (see httprepo.go and
// syntheticdeb.go's package doc for why — a container-local file:// URI
// would be invisible to the separately-started builder container that must
// see the very same repository once the snapshot is resolved).
func ApplyTargetState(ctx context.Context, c *Container, spec TargetSpec, repoHostDir, repoBaseURL string) error {
	if len(spec.AptConf) > 0 {
		if err := writeAptConf(ctx, c, spec.AptConf); err != nil {
			return err
		}
	}

	for _, arch := range spec.ForeignArchs {
		if _, err := c.MustSucceed(ctx, ExecOpts{}, "dpkg", "--add-architecture", arch); err != nil {
			return fmt.Errorf("add-architecture %s: %w", arch, err)
		}
	}

	for _, f := range spec.Sources {
		if err := c.WriteFile(ctx, "/etc/apt/sources.list.d/"+f.Filename, []byte(f.Content)); err != nil {
			return fmt.Errorf("write source %s: %w", f.Filename, err)
		}
	}
	for _, f := range spec.Preferences {
		if err := c.WriteFile(ctx, "/etc/apt/preferences.d/"+f.Filename, []byte(f.Content)); err != nil {
			return fmt.Errorf("write preference %s: %w", f.Filename, err)
		}
	}

	if len(spec.Installed.Repos) > 0 {
		for _, repo := range spec.Installed.Repos {
			stanza, err := WriteSyntheticRepo(repoHostDir, repoBaseURL, repo, spec.Arch)
			if err != nil {
				return fmt.Errorf("synthetic repo %s: %w", repo.Name, err)
			}
			if err := c.WriteFile(ctx, "/etc/apt/sources.list.d/dfe2e-"+sanitize(repo.Name)+".sources", []byte(stanza)); err != nil {
				return fmt.Errorf("synthetic repo %s: write sources file: %w", repo.Name, err)
			}
		}
	}

	if err := aptUpdate(ctx, c); err != nil {
		return err
	}

	if len(spec.Installed.Packages) > 0 {
		if err := aptInstall(ctx, c, spec.Installed.Packages); err != nil {
			return fmt.Errorf("install target packages %v: %w", spec.Installed.Packages, err)
		}
	}

	if len(spec.Installed.FromRepos) > 0 {
		if err := aptInstall(ctx, c, spec.Installed.FromRepos); err != nil {
			return fmt.Errorf("install from synthetic repos %v: %w", spec.Installed.FromRepos, err)
		}
	}

	if len(spec.Installed.Holds) > 0 {
		args := append([]string{"apt-mark", "hold"}, spec.Installed.Holds...)
		if _, err := c.MustSucceed(ctx, ExecOpts{}, args...); err != nil {
			return fmt.Errorf("apt-mark hold %v: %w", spec.Installed.Holds, err)
		}
	}

	return nil
}

func writeAptConf(ctx context.Context, c *Container, conf map[string]string) error {
	keys := make([]string, 0, len(conf))
	for k := range conf {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s %s;\n", k, conf[k])
	}
	return c.WriteFile(ctx, "/etc/apt/apt.conf.d/99-dfe2e-fixture", []byte(b.String()))
}

func aptUpdate(ctx context.Context, c *Container) error {
	_, err := c.MustSucceed(ctx, ExecOpts{Env: map[string]string{"DEBIAN_FRONTEND": "noninteractive"}},
		"apt-get", "update", "-qq")
	if err != nil {
		return fmt.Errorf("apt-get update: %w", err)
	}
	return nil
}

func aptInstall(ctx context.Context, c *Container, packages []string) error {
	args := append([]string{"apt-get", "install", "-y", "-qq"}, packages...)
	_, err := c.MustSucceed(ctx, ExecOpts{Env: map[string]string{"DEBIAN_FRONTEND": "noninteractive"}}, args...)
	return err
}
