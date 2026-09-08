param([Parameter(Mandatory=$true)][string]$Destination)
$ErrorActionPreference = 'Stop'
$guiRoot = (Resolve-Path (Join-Path $PSScriptRoot '../..')).Path
$workspaceRoot = (Resolve-Path (Join-Path $guiRoot '..')).Path
$snapshotPath = [IO.Path]::GetFullPath($Destination)
if (-not $snapshotPath.StartsWith($workspaceRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) { throw 'Snapshot must be inside the workspace.' }
if (Test-Path -LiteralPath $snapshotPath) { throw 'Use a new snapshot directory to preserve capture provenance.' }
New-Item -ItemType Directory -Path (Join-Path $snapshotPath 'frontend') | Out-Null
foreach ($part in @('src','wailsjs','index.html')) {
  Copy-Item -LiteralPath (Join-Path $guiRoot "frontend/$part") -Destination (Join-Path $snapshotPath 'frontend') -Recurse
}
New-Item -ItemType Directory -Path (Join-Path $snapshotPath 'hack/ui-review') | Out-Null
Get-ChildItem -LiteralPath $PSScriptRoot -File | ForEach-Object {
  Copy-Item -LiteralPath $_.FullName -Destination (Join-Path $snapshotPath 'hack/ui-review')
}
$entries = Get-ChildItem -LiteralPath $snapshotPath -Recurse -File | Sort-Object FullName | ForEach-Object {
  [ordered]@{path=$_.FullName.Substring($snapshotPath.Length + 1).Replace('\','/'); sha256=(Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()}
}
$manifestPath = Join-Path $snapshotPath 'content-manifest.json'
[IO.File]::WriteAllText($manifestPath, ($entries | ConvertTo-Json), [Text.UTF8Encoding]::new($false))
(Get-FileHash -LiteralPath $manifestPath -Algorithm SHA256).Hash.ToLowerInvariant()
