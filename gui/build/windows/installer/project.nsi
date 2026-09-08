Unicode true

####
## Debark — Windows installer script.
##
## THIS FILE IS HAND-MAINTAINED. Read build/windows/installer/README.md before
## changing it, and read it before assuming that a fix belongs in
## wails_tools.nsh instead. It does not, and the reason is mechanical:
##
##   pkg/commands/build/nsis_installer.go (Wails v2.12.0) calls
##   ReadOriginalFileWithProjectDataAndSave() on wails_tools.nsh, which
##   rewrites that file from the upstream template on EVERY `wails build
##   --nsis`. It calls ReadFile() on this file, which only writes the template
##   when the file is absent. Wails' own .gitignore template lists
##   build/windows/installer/wails_tools.nsh for exactly this reason.
##
## So wails_tools.nsh cannot hold a decision and is not committed here.
## project.nsi can, and holds these three.
##
## They exist because docs/security-review.md §6.6 read the stock template and
## found that it elevates to administrator and silently runs Microsoft's
## ONLINE WebView2 bootstrapper — neither of which anyone chose; both arrived
## with `wails init`. §5 of that same review exists because a Linux package
## that acquires root is indefensible for this audience. The Windows side
## should not quietly be the exception.
##
## Windows is built and tested, not supported (.github/workflows/ci.yml says
## so in its header). No release pipeline builds this installer today. It is
## kept, corrected, because deleting it does not remove the problem: the next
## person to run `wails build --nsis` on a tree without a project.nsi gets the
## stock template back, administrator and online bootstrapper included, with
## nothing to tell them why that is wrong.
##
## To build it by hand:
##   wails build --target windows/amd64 --nsis     (regenerates wails_tools.nsh)
##   makensis -DARG_WAILS_AMD64_BINARY=..\..\bin\debark-gui.exe project.nsi
####

####
## Decision 1 — no administrator, ever.
##
## The stock wails_tools.nsh does `!ifndef REQUEST_EXECUTION_LEVEL / !define
## REQUEST_EXECUTION_LEVEL "admin"`, then RequestExecutionLevel on it. The
## !ifndef is what lets this line win, and it must come before the !include.
##
## "user" also changes wails.setShellContext to SetShellVarContext current, so
## the Start Menu entry is written to this user's profile rather than to All
## Users. That is the correct scope for an application that installs nothing
## system-wide.
####
!define REQUEST_EXECUTION_LEVEL "user"

!define INFO_PROJECTNAME    "debark-gui"
!define INFO_COMPANYNAME    "The debark Authors"
!define INFO_PRODUCTNAME    "Debark"
!ifndef INFO_PRODUCTVERSION
    !define INFO_PRODUCTVERSION "0.0.0"
!endif
!define INFO_COPYRIGHT      "Copyright the debark Authors. Apache-2.0."
!define UNINST_KEY_NAME     "DebarkAuthorsDebark"

!include "wails_tools.nsh"

# The version information must consist of 4 parts.
VIProductVersion "${INFO_PRODUCTVERSION}.0"
VIFileVersion    "${INFO_PRODUCTVERSION}.0"

VIAddVersionKey "CompanyName"     "${INFO_COMPANYNAME}"
VIAddVersionKey "FileDescription" "${INFO_PRODUCTNAME} Installer"
VIAddVersionKey "ProductVersion"  "${INFO_PRODUCTVERSION}"
VIAddVersionKey "FileVersion"     "${INFO_PRODUCTVERSION}"
VIAddVersionKey "LegalCopyright"  "${INFO_COPYRIGHT}"
VIAddVersionKey "ProductName"     "${INFO_PRODUCTNAME}"

ManifestDPIAware true

!include "MUI.nsh"
!include "LogicLib.nsh"

!define MUI_ICON "..\icon.ico"
!define MUI_UNICON "..\icon.ico"
!define MUI_FINISHPAGE_NOAUTOCLOSE
!define MUI_ABORTWARNING

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_INSTFILES

!insertmacro MUI_LANGUAGE "English"

Name "${INFO_PRODUCTNAME}"
OutFile "..\..\bin\${INFO_PROJECTNAME}-${ARCH}-installer.exe"

####
## A per-user install directory, because a per-user installer cannot write to
## Program Files. $LOCALAPPDATA\Programs is where Windows itself puts
## per-user applications.
####
InstallDir "$LOCALAPPDATA\Programs\${INFO_PRODUCTNAME}"
ShowInstDetails show

####
## Decision 2 — detect WebView2; never install it.
##
## The stock template inserts wails.webview2runtime, which ships Microsoft's
## MicrosoftEdgeWebview2Setup.exe inside the installer and runs it
## `/silent /install`. That bootstrapper is the ONLINE installer: it downloads
## the runtime from Microsoft at install time, and /silent means the operator
## is never told that installing this application caused a download.
##
## This is not a rule-3 violation — microsoft.com is not a debark-operated
## host — but it is the same shape rule 3 exists to refuse, and on the
## locked-down machines this tool is for it is the kind of surprise that gets
## a tool banned. So the check stays and the install does not: the operator is
## told what is missing and where to get it, which is exactly what the
## readiness screen does for every other missing prerequisite.
##
## The registry keys are the ones Microsoft documents for detecting the
## Evergreen runtime, and they are the same two the stock macro reads.
####
!macro debark.requireWebView2
    SetRegView 64
    ReadRegStr $0 HKLM "SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
    ${If} $0 != ""
        Goto webview2_ok
    ${EndIf}
    ReadRegStr $0 HKCU "Software\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
    ${If} $0 != ""
        Goto webview2_ok
    ${EndIf}

    IfSilent silentNoWebView notSilentNoWebView
    silentNoWebView:
        SetErrorLevel 66
        Abort
    notSilentNoWebView:
        MessageBox MB_OK|MB_ICONEXCLAMATION \
            "${INFO_PRODUCTNAME} needs the Microsoft Edge WebView2 Runtime, which is not installed.$\r$\n$\r$\n\
             This installer will not download or install it for you. Install the WebView2 Evergreen Runtime from Microsoft, then run this installer again.$\r$\n$\r$\n\
             https://developer.microsoft.com/microsoft-edge/webview2/"
        Abort
    webview2_ok:
!macroend

Function .onInit
    !insertmacro wails.checkArchitecture
FunctionEnd

Section
    !insertmacro wails.setShellContext

    !insertmacro debark.requireWebView2

    SetOutPath $INSTDIR

    !insertmacro wails.files

    CreateShortcut "$SMPROGRAMS\${INFO_PRODUCTNAME}.lnk" "$INSTDIR\${PRODUCT_EXECUTABLE}"

    ####
    ## Decision 3 — register the uninstaller under HKCU, not HKLM.
    ##
    ## wails.writeUninstaller writes unconditionally to HKLM, which a
    ## non-elevated process cannot do. NSIS does not fail the install for
    ## that: it logs an error and carries on, so a per-user install that
    ## used the stock macro would silently leave no entry in Apps &
    ## features and no way to uninstall from the UI. Since wails_tools.nsh
    ## is regenerated on every build, the macro cannot be corrected — so it
    ## is not inserted, and the same registration is written here against
    ## the hive this installer can actually write to.
    ##
    ## No file association and no custom protocol handler is registered.
    ## The application opens a snapshot the operator picks in a file dialog;
    ## it does not want to be the system handler for anything.
    ####
    WriteUninstaller "$INSTDIR\uninstall.exe"
    SetRegView 64
    WriteRegStr HKCU "${UNINST_KEY}" "Publisher" "${INFO_COMPANYNAME}"
    WriteRegStr HKCU "${UNINST_KEY}" "DisplayName" "${INFO_PRODUCTNAME}"
    WriteRegStr HKCU "${UNINST_KEY}" "DisplayVersion" "${INFO_PRODUCTVERSION}"
    WriteRegStr HKCU "${UNINST_KEY}" "DisplayIcon" "$INSTDIR\${PRODUCT_EXECUTABLE}"
    WriteRegStr HKCU "${UNINST_KEY}" "InstallLocation" "$INSTDIR"
    WriteRegStr HKCU "${UNINST_KEY}" "UninstallString" "$\"$INSTDIR\uninstall.exe$\""
    WriteRegStr HKCU "${UNINST_KEY}" "QuietUninstallString" "$\"$INSTDIR\uninstall.exe$\" /S"
    WriteRegDWORD HKCU "${UNINST_KEY}" "NoModify" 1
    WriteRegDWORD HKCU "${UNINST_KEY}" "NoRepair" 1
    ${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
    IntFmt $0 "0x%08X" $0
    WriteRegDWORD HKCU "${UNINST_KEY}" "EstimatedSize" "$0"
SectionEnd

Section "uninstall"
    !insertmacro wails.setShellContext

    # The WebView2 user-data directory the application creates at run time.
    RMDir /r "$AppData\${PRODUCT_EXECUTABLE}"

    RMDir /r $INSTDIR

    Delete "$SMPROGRAMS\${INFO_PRODUCTNAME}.lnk"

    SetRegView 64
    DeleteRegKey HKCU "${UNINST_KEY}"
SectionEnd
