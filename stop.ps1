$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$pidFile = Join-Path $root 'cpa.pid'
$stopped = $false

if (Test-Path -LiteralPath $pidFile) {
  $existingId = 0
  [int]::TryParse((Get-Content -LiteralPath $pidFile -Raw).Trim(), [ref]$existingId) | Out-Null
  if ($existingId -gt 0) {
    $existing = Get-Process -Id $existingId -ErrorAction SilentlyContinue
    if ($existing) {
      Stop-Process -Id $existingId -Force
      Write-Output ("STOPPED PID=" + $existingId)
      $stopped = $true
    }
  }
  Remove-Item -LiteralPath $pidFile -Force -ErrorAction SilentlyContinue
}

$others = Get-CimInstance Win32_Process | Where-Object {
  $_.Name -eq 'cli-proxy-api.exe' -and $_.CommandLine -match [regex]::Escape($root)
}
foreach ($p in $others) {
  Stop-Process -Id $p.ProcessId -Force -ErrorAction SilentlyContinue
  Write-Output ("STOPPED PID=" + $p.ProcessId)
  $stopped = $true
}

if (-not $stopped) { Write-Output 'NOT_RUNNING' }