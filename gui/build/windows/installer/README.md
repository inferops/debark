# The Windows installer

**This project does not ship a Windows installer.** Windows builds and runs
and is covered by CI, but it is not a supported platform — `.github/workflows/
ci.yml` says so in its own header, and nothing in this repository or in the
release pipeline runs `wails build --nsis`. Linux is the shipping target.

This directory therefore holds exactly one file, `project.nsi`, and it is kept
for a reason that is worth reading before anyone deletes it.

## Why `wails_tools.nsh` is not committed here

It used to be, unchanged, from `wails init`. `docs/security-review.md` §6.6
read it — as far as that review could tell, the first time anyone had — and
found two defaults nobody chose:

```nsis
!define REQUEST_EXECUTION_LEVEL "admin"          # wails_tools.nsh:31
RequestExecutionLevel "${REQUEST_EXECUTION_LEVEL}"
…
ExecWait '"$pluginsdir\webview2bootstrapper\MicrosoftEdgeWebview2Setup.exe" /silent /install'
```

The committed copy was **byte-for-byte identical** to the upstream Wails
v2.12.0 template at
`pkg/buildassets/build/windows/installer/wails_tools.nsh`, verified by diff.

It has been removed, and a fix was **not** applied to it, because a fix there
cannot survive. `pkg/commands/build/nsis_installer.go` in Wails v2.12.0 does:

| file | Wails call | effect |
|---|---|---|
| `project.nsi` | `buildassets.ReadFile` | written only if **absent**; an existing file is left alone |
| `wails_tools.nsh` | `buildassets.ReadOriginalFileWithProjectDataAndSave` | **rewritten from the upstream template on every `--nsis` build** |

Wails' own `.gitignore` template lists
`build/windows/installer/wails_tools.nsh` for exactly this reason. Committing
it made an unreviewed upstream template look like a reviewed project artefact,
which is how it stayed unread.

**If you are adding it to `.gitignore`, that is correct.** It is generated
output.

## Why `project.nsi` is kept rather than deleted too

Because deleting it does not remove the problem. `wails build --nsis` on a
tree with no `project.nsi` writes the stock template back — administrator,
silent online WebView2 bootstrapper and all — and there would be nothing left
to say why that is wrong. `project.nsi` is the only file in the pair that can
hold a decision, so it holds three, each commented in place:

1. **`!define REQUEST_EXECUTION_LEVEL "user"`**, set *before* the
   `!include "wails_tools.nsh"` so that the template's `!ifndef` guard skips
   its `"admin"` default. This application needs no privilege at any point,
   which is the same principle `docs/security-review.md` §5 enforces for the
   `.deb`.
2. **The online WebView2 bootstrapper is not inserted.** `wails.webview2runtime`
   ships Microsoft's `MicrosoftEdgeWebview2Setup.exe` inside the installer and
   runs it `/silent /install`; that binary is the *online* installer, so it
   downloads at install time and says nothing about it. In its place,
   `debark.requireWebView2` reads the same two registry keys and, if the
   runtime is missing, **tells the operator where to get it and stops**. That
   is what the readiness screen does for every other missing prerequisite.
3. **The uninstaller is registered under `HKCU`, not `HKLM`.**
   `wails.writeUninstaller` writes to `HKLM` unconditionally. A non-elevated
   installer cannot, and NSIS does not fail the install for it — so a per-user
   install using the stock macro would silently produce no entry in Apps &
   features and no way to uninstall. The macro cannot be corrected, because it
   is regenerated; so it is not inserted and the registration is written here
   against the hive this installer can write to.

No file association and no custom protocol handler is registered. The
application opens a snapshot the operator picks in a file dialog.

## Proof, not intent

Both scripts were compiled with `makensis` 3.x on Ubuntu 24.04 against the
same regenerated `wails_tools.nsh`, and the produced installers' embedded
manifests read:

| script | `requestedExecutionLevel` |
|---|---|
| upstream Wails `project.nsi`, unchanged | `requireAdministrator` |
| this `project.nsi` | `asInvoker` |

and a `strings` scan of the installer built from this script finds no
`MicrosoftEdgeWebview2Setup` and no `webview2bootstrapper`. The commands are
in `docs/packaging.md`.

## Building one by hand

```
wails build --target windows/amd64 --nsis      # regenerates wails_tools.nsh
makensis -DARG_WAILS_AMD64_BINARY=..\..\bin\debark-gui.exe project.nsi
```

Pass `-DINFO_PRODUCTVERSION=x.y.z` to stamp a version; it defaults to `0.0.0`.
