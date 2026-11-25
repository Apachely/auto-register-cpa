$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location -LiteralPath $root
$exe = Join-Path $root 'cli-proxy-api.exe'
$config = Join-Path $root 'config.yaml'
$pidFile = Join-Path $root 'cpa.pid'
$logsDir = Join-Path $root 'logs'
New-Item -ItemType Directory -Path $logsDir -Force | Out-Null

if (Test-Path -LiteralPath $pidFile) {
  $existingId = 0
  [int]::TryParse((Get-Content -LiteralPath $pidFile -Raw).Trim(), [ref]$existingId) | Out-Null
  if ($existingId -gt 0) {
    $existing = Get-Process -Id $existingId -ErrorAction SilentlyContinue
    if ($existing) {
      Write-Output ("ALREADY_RUNNING PID=" + $existing.Id)
      exit 0
    }
  }
}

$listening = Get-NetTCPConnection -LocalPort 8317 -State Listen -ErrorAction SilentlyContinue
if ($listening) {
  Write-Output 'Port 8317 is already in use; not starting a second instance.'
  exit 1
}

$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$outLog = Join-Path $logsDir ("cpa-stdout-$stamp.log")
$errLog = Join-Path $logsDir ("cpa-stderr-$stamp.log")
$proc = Start-Process -FilePath $exe -ArgumentList @('--config', $config) -WorkingDirectory $root -RedirectStandardOutput $outLog -RedirectStandardError $errLog -WindowStyle Hidden -PassThru
Set-Content -LiteralPath $pidFile -Value ([string]$proc.Id) -Encoding ASCII
Write-Output ("STARTED PID=" + $proc.Id)
Write-Output ("STDOUT_LOG=" + $outLog)
Write-Output ("STDERR_LOG=" + $errLog)