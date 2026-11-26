$ErrorActionPreference = 'Stop'
$botDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$root   = Split-Path -Parent $botDir
$singBox = Join-Path $root '_download\singbox\sing-box.exe'
$cfg     = Join-Path $botDir 'config.json'
$pidFile = Join-Path $botDir 'bot-core.pid'
$logDir  = Join-Path $botDir 'logs'
New-Item -ItemType Directory -Path $logDir -Force | Out-Null

if (-not (Test-Path -LiteralPath $singBox)) { throw "sing-box binary missing: $singBox" }
if (-not (Test-Path -LiteralPath $cfg)) {
  node (Join-Path $botDir 'bootstrap.cjs') --check-only
}

if (Test-Path -LiteralPath $pidFile) {
  $oldId = 0
  [int]::TryParse((Get-Content -LiteralPath $pidFile -Raw).Trim(), [ref]$oldId) | Out-Null
  if ($oldId -gt 0 -and (Get-Process -Id $oldId -ErrorAction SilentlyContinue)) {
    Write-Output ("ALREADY_RUNNING PID=" + $oldId)
    exit 0
  }
}

$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$outLog = Join-Path $logDir ("bot-core-stdout-$stamp.log")
$errLog = Join-Path $logDir ("bot-core-stderr-$stamp.log")
$proc = Start-Process -FilePath $singBox -ArgumentList @('-c', $cfg, '-D', $botDir) -WorkingDirectory $botDir -RedirectStandardOutput $outLog -RedirectStandardError $errLog -WindowStyle Hidden -PassThru
Set-Content -LiteralPath $pidFile -Value ([string]$proc.Id) -Encoding ASCII
Start-Sleep -Milliseconds 800

# Wait for the controller to answer /version.
$ready = $false
for ($i = 0; $i -lt 25; $i++) {
  try {
    $r = Invoke-WebRequest -Uri 'http://127.0.0.1:12461/version' -TimeoutSec 2 -UseBasicParsing
    if ($r.StatusCode -eq 200) { $ready = $true; break }
  } catch {}
  Start-Sleep -Milliseconds 400
}
if ($ready) {
  Write-Output ("STARTED PID=" + $proc.Id + " MIXED=127.0.0.1:12460 CTRL=127.0.0.1:12461")
} else {
  Write-Output ("STARTED_WITH_WARNING PID=" + $proc.Id + " controller not ready yet; see $errLog")
}
Write-Output ("STDOUT_LOG=" + $outLog)
Write-Output ("STDERR_LOG=" + $errLog)
