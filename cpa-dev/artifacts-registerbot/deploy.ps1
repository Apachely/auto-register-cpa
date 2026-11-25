param([string]$Root = 'E:\LY\Tool\CPA')
$ErrorActionPreference = 'Stop'
$artifact = Join-Path $Root 'cpa-dev\artifacts-registerbot'
$backend = Join-Path $artifact 'cli-proxy-api.exe'
$panel = Join-Path $Root 'cpa-dev\cpa-panel\dist\index.html'
if (!(Test-Path -LiteralPath $panel)) { $panel = Join-Path $artifact 'management.html' }
if (!(Test-Path -LiteralPath $backend) -or !(Test-Path -LiteralPath $panel)) {
  throw 'Build the backend and panel before deployment.'
}
$backup = Join-Path $Root ('cpa-dev\deployment-backups\registerbot-' + (Get-Date -Format 'yyyyMMdd-HHmmss'))
New-Item -ItemType Directory -Path $backup -Force | Out-Null
Copy-Item -LiteralPath (Join-Path $Root 'cli-proxy-api.exe') -Destination (Join-Path $backup 'cli-proxy-api.exe')
Copy-Item -LiteralPath (Join-Path $Root 'static\management.html') -Destination (Join-Path $backup 'management.html')
Copy-Item -LiteralPath (Join-Path $Root 'config.yaml') -Destination (Join-Path $backup 'config.yaml')
$pidFile = Join-Path $Root 'cpa.pid'
if (Test-Path -LiteralPath $pidFile) {
  $serverId = [int](Get-Content -LiteralPath $pidFile -Raw).Trim()
  $process = Get-Process -Id $serverId -ErrorAction SilentlyContinue
  if ($process) {
    if ($process.Path -ne (Join-Path $Root 'cli-proxy-api.exe')) { throw 'PID does not own the expected CPA executable.' }
    Stop-Process -Id $serverId -Force
    $process.WaitForExit()
  }
  Remove-Item -LiteralPath $pidFile
}
try {
  Copy-Item -LiteralPath $backend -Destination (Join-Path $Root 'cli-proxy-api.exe') -Force
  Copy-Item -LiteralPath $panel -Destination (Join-Path $Root 'static\management.html') -Force
  $packagedPanel = Join-Path $artifact 'management.html'
  if ([IO.Path]::GetFullPath($panel) -ne [IO.Path]::GetFullPath($packagedPanel)) {
    Copy-Item -LiteralPath $panel -Destination $packagedPanel -Force
  }
  $global:LASTEXITCODE = 0
  & (Join-Path $Root 'start.ps1')
  if ($LASTEXITCODE -ne 0) { throw 'CPA start script failed.' }
} catch {
  if (Test-Path -LiteralPath $pidFile) {
    $failedStartId = [int](Get-Content -LiteralPath $pidFile -Raw).Trim()
    $failedStart = Get-Process -Id $failedStartId -ErrorAction SilentlyContinue
    if ($failedStart -and $failedStart.Path -eq (Join-Path $Root 'cli-proxy-api.exe')) {
      Stop-Process -Id $failedStartId -Force
      $failedStart.WaitForExit()
      Remove-Item -LiteralPath $pidFile
    }
  }
  Copy-Item -LiteralPath (Join-Path $backup 'cli-proxy-api.exe') -Destination (Join-Path $Root 'cli-proxy-api.exe') -Force
  Copy-Item -LiteralPath (Join-Path $backup 'management.html') -Destination (Join-Path $Root 'static\management.html') -Force
  & (Join-Path $Root 'start.ps1')
  throw
}
Write-Output ('DEPLOYED backup=' + $backup)
