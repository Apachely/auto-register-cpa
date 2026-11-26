$ErrorActionPreference = 'Stop'
$botDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$pidFile = Join-Path $botDir 'bot-core.pid'
if (Test-Path -LiteralPath $pidFile) {
  $id = 0
  [int]::TryParse((Get-Content -LiteralPath $pidFile -Raw).Trim(), [ref]$id) | Out-Null
  if ($id -gt 0) {
    $p = Get-Process -Id $id -ErrorAction SilentlyContinue
    if ($p) { Stop-Process -Id $id -Force; Write-Output ("STOPPED PID=" + $id) }
  }
  Remove-Item -LiteralPath $pidFile -Force
} else {
  Write-Output 'NO_PID_FILE'
}
