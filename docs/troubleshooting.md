# Troubleshooting

[Documentation index](README.md) · [CLI guide](cli.md) · [Get help](../SUPPORT.md)

Start with the error message and exit code. Record `debark version --json`,
the builder OS, target release/architecture, and backend when reporting a problem.
Review logs and snapshots for sensitive configuration before sharing them.

## apt cannot resolve a request (exit 5)

Check that the package name and pinned version exist in the target's configured
repositories. A package available on the builder may be absent from the target
release. Vendor packages may also require dependencies those sources do not carry.

Inspect the snapshot and retry with a smaller request. Correct the target's
sources or request, capture a fresh snapshot when appropriate, and rebuild.
See [package inputs](cli.md#package-inputs).

## The builder cannot run apt or a container (exit 2)

`--backend auto` needs compatible local apt or a usable container runtime.
For Windows and cross-release builds, start Docker/Podman in Linux-container
mode and provide a static Linux debark helper using `--self-binary`.

Check the architecture and the helper path. Automatic discovery looks beside
the running executable, not your working directory. A Windows executable cannot
run inside a Linux container. See [container builders](platforms.md#container-builders).

## Verification fails (exit 4)

Check that you supplied the intended public key or keyring and that it was
authenticated independently. Then check that the bundle was copied completely
and that no files were changed after signing.

If the trusted source copy verifies but the transferred copy does not, copy it
again and investigate the media. If the source fails too, rebuild and sign
again after identifying the cause. Do not use `--allow-unsigned` to bypass an
unexpected failure on a signed bundle.

## The target does not match (exit 7 or a warning)

An incompatible architecture is a hard failure. A release difference can be
a warning, so review the complete status report:

```sh
debark install ./bundle --key operator.pub --status
```

Build for the actual target release and architecture. If a foreign architecture
is required, configure it on the target through your normal administration
process, then capture a fresh snapshot and rebuild. An offline target cannot
repair its package indexes by contacting an online archive.

Missing packages assumed by a baseline are also warnings. Use a captured
snapshot when the chosen baseline does not fit.

## The bundle is larger than expected

Check recommendations with `debark inspect ./bundle`. Captured snapshots
preserve the target's recommendation setting; `--no-recommends` excludes
optional recommended packages.

Also check `last-run-unreferenced.txt` for retained pool files after updates.
A new output directory avoids those leftovers. See [updates and storage](cli.md#updates-and-storage).

## A package still tries to use the network during installation

A `.deb` can contain maintainer scripts that download additional data or
contact services. Supplying the package itself does not make those scripts
work offline.

Run `debark doctor ./bundle`, review its findings, and test the package set
with networking disabled. The checks are heuristic. A signed bundle does not
certify offline behavior.

## Policy or redistribution blocks progress

Exit 6 indicates an enforced policy or approved-key violation. Review the
policy and archive fingerprints against the sources you intended to trust.

Redistribution notices are findings, and an interactive build may ask you to
acknowledge them. `--acknowledge-redistribution` suppresses that prompt while
keeping the warnings. It does not override policy failures or grant permission
to redistribute a package.

## Interactive mode will not start

`--interactive` requires a terminal on stdin and cannot be combined with
`--json` or `--json-events`. For CI or redirected input, supply explicit
`--snapshot` or `--base`, package inputs, output, and signing options.

## The desktop app cannot find debark

Install the CLI separately and put it on the desktop session's `PATH`.
Open **System check** and use **Recheck** after fixing the path or prerequisites.
See the [desktop user guide](../gui/docs/user-guide.md#system-check).

For other issues, open a [support request](../SUPPORT.md) with a minimal
reproduction and the relevant output.
