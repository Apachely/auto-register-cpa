[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$botDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$config = Join-Path $botDir 'runtime-config.json'
$pidFile = Join-Path $botDir 'bot-core.pid'

if (-not (Test-Path -LiteralPath $pidFile)) {
  Write-Output 'NOT_RUNNING'
  exit 0
}

$processID = 0
[int]::TryParse((Get-Content -LiteralPath $pidFile -Raw).Trim(), [ref]$processID) | Out-Null
if ($processID -gt 0) {
  $process = Get-CimInstance Win32_Process -Filter ('ProcessId=' + $processID) -ErrorAction SilentlyContinue
  if ($null -ne $process -and $process.Name -eq 'sing-box.exe' -and $process.CommandLine -like ('*' + $config + '*')) {
    Stop-Process -Id $processID -Force
    Write-Output ('STOPPED PID=' + $processID)
  }
}
Remove-Item -LiteralPath $pidFile -Force -ErrorAction SilentlyContinue
