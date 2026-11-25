$ErrorActionPreference = 'Stop'
$artifact = $PSScriptRoot
$root = [IO.Path]::GetFullPath((Join-Path $artifact '..\..'))
$backendCommit = (git -C (Join-Path $root 'cpa-dev\cpa-backend') rev-parse HEAD).Trim()
$panelCommit = (git -C (Join-Path $root 'cpa-dev\cpa-panel') rev-parse HEAD).Trim()
Copy-Item -LiteralPath (Join-Path $root 'cpa-dev\cpa-backend\LICENSE') -Destination (Join-Path $artifact 'LICENSE-backend.txt') -Force
Copy-Item -LiteralPath (Join-Path $root 'cpa-dev\cpa-panel\LICENSE') -Destination (Join-Path $artifact 'LICENSE-panel.txt') -Force
$backendPatch = Join-Path $artifact 'backend.patch'
$panelPatch = Join-Path $artifact 'panel.patch'
git -C (Join-Path $root 'cpa-dev\cpa-backend') diff --binary HEAD^ HEAD "--output=$backendPatch"
if ($LASTEXITCODE -ne 0) { throw 'Backend patch export failed.' }
git -C (Join-Path $root 'cpa-dev\cpa-panel') diff --binary HEAD^ HEAD "--output=$panelPatch"
if ($LASTEXITCODE -ne 0) { throw 'Panel patch export failed.' }
$manifest = [ordered]@{
  version = '8.1.2-registerbot'
  date = '2026-11-25'
  backendCommit = $backendCommit
  panelCommit = $panelCommit
  backendSha256 = (Get-FileHash -LiteralPath (Join-Path $artifact 'cli-proxy-api.exe')).Hash
  panelSha256 = (Get-FileHash -LiteralPath (Join-Path $artifact 'management.html')).Hash
  runtimeStateIncluded = $false
  credentialsIncluded = $false
}
$manifest | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $artifact 'manifest.json') -Encoding utf8
$files = @(
  'cli-proxy-api.exe', 'management.html', 'backend.patch', 'panel.patch', 'manifest.json',
  'README_CN.md', 'LICENSE-backend.txt', 'LICENSE-panel.txt', 'deploy.ps1', 'start-live-run.ps1', 'verify-panel.cjs', 'verify-stream.cjs',
  'registerbot-desktop.jpg', 'registerbot-mobile.jpg', 'registerbot-success.jpg',
  'panel-verify-network-error.log',
  'verification\e2e-success.json', 'verification\live-run.json', 'verification\summary.json',
  'verification\panel-browser.json', 'verification\sse-live.json', 'verification\secret-scan.json',
  'verification\go-final-pass.log', 'verification\go-google-selector.log',
  'verification\go-baseline-failures.log', 'verification\go-baseline-tui.log',
  'verification\go-quota-baseline-repeated.log'
)
$zipPath = Join-Path $artifact 'CPA-registerbot-v8.1.2.zip'
$temporaryZip = $zipPath + '.tmp'
Add-Type -AssemblyName System.IO.Compression
$archive = [IO.Compression.ZipFile]::Open($temporaryZip, [IO.Compression.ZipArchiveMode]::Create)
try {
  foreach ($relative in $files) {
    $source = Join-Path $artifact $relative
    [IO.Compression.ZipFileExtensions]::CreateEntryFromFile($archive, $source, $relative.Replace('\', '/'), [IO.Compression.CompressionLevel]::Optimal) | Out-Null
  }
} finally { $archive.Dispose() }
Move-Item -LiteralPath $temporaryZip -Destination $zipPath -Force
$checksum = (Get-FileHash -LiteralPath $zipPath).Hash
($checksum + '  CPA-registerbot-v8.1.2.zip') | Set-Content -LiteralPath (Join-Path $artifact 'SHA256SUMS.txt') -Encoding ascii
$readback = [IO.Compression.ZipFile]::OpenRead($zipPath)
try {
  if ($readback.Entries.Count -ne $files.Count) { throw 'Package readback file count mismatch.' }
  if (@($readback.Entries | Where-Object { $_.FullName -match 'config\.yaml|admin-key|state\.key|queue\.enc|profiles/' }).Count) { throw 'Runtime material found in package.' }
  Write-Output ('PACKAGE_FILES=' + $readback.Entries.Count)
} finally { $readback.Dispose() }
Get-Item -LiteralPath $zipPath | Select-Object Name, Length
