[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$botDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$root = Split-Path -Parent $botDir
$singBox = Join-Path $root '_download\singbox\sing-box.exe'
$config = Join-Path $botDir 'config.json'
$runtimeConfig = Join-Path $botDir 'runtime-config.json'
$prepareScript = Join-Path $botDir 'prepare-runtime.cjs'
$healthScript = Join-Path $botDir 'health-check.cjs'
$healthyNodes = Join-Path $botDir 'healthy-nodes.json'
$pidFile = Join-Path $botDir 'bot-core.pid'
$logDir = Join-Path $botDir 'logs'
$mixedPort = 11460
$controllerPort = 11461

function Test-Listening([int]$Port) {
  return $null -ne (Get-NetTCPConnection -LocalAddress '127.0.0.1' -LocalPort $Port -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1)
}

function Test-ControllerReady {
  try {
    $response = Invoke-WebRequest -UseBasicParsing -Uri ('http://127.0.0.1:' + $controllerPort + '/version') -TimeoutSec 1
    return $response.StatusCode -eq 200
  } catch {
    return $false
  }
}

if (-not (Test-Path -LiteralPath $singBox)) {
  throw ('registerbot sing-box binary missing: ' + $singBox)
}
if (-not (Test-Path -LiteralPath $config)) {
  throw ('registerbot proxy config missing: ' + $config)
}
if (-not (Test-Path -LiteralPath $prepareScript)) {
  throw ('registerbot runtime config generator missing: ' + $prepareScript)
}
if (-not (Test-Path -LiteralPath $healthScript)) {
  throw ('registerbot node health checker missing: ' + $healthScript)
}

try {
  $configText = [IO.File]::ReadAllText($config, [Text.Encoding]::UTF8)
  $document = $configText | ConvertFrom-Json
} catch {
  throw 'registerbot proxy config is not valid UTF-8 JSON'
}
$configuredMixed = @($document.inbounds | Where-Object { $_.listen_port -eq $mixedPort }).Count -eq 1
$configuredController = [string]$document.experimental.clash_api.external_controller
if (-not $configuredMixed -or $configuredController -notin @('127.0.0.1:11461', 'http://127.0.0.1:11461')) {
  throw 'registerbot proxy config must own only mixed 11460 and controller 11461'
}

if (Test-Path -LiteralPath $pidFile) {
  $existingID = 0
  [int]::TryParse((Get-Content -LiteralPath $pidFile -Raw).Trim(), [ref]$existingID) | Out-Null
  if ($existingID -gt 0) {
    $existing = Get-CimInstance Win32_Process -Filter ('ProcessId=' + $existingID) -ErrorAction SilentlyContinue
    if ($null -ne $existing -and $existing.Name -eq 'sing-box.exe' -and $existing.CommandLine -like ('*' + $runtimeConfig + '*')) {
      if ((Test-Listening $mixedPort) -and (Test-ControllerReady)) {
        & node $healthScript
        if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $healthyNodes)) {
          throw 'registerbot node health check failed'
        }
        Write-Output ('ALREADY_RUNNING PID=' + $existingID)
        exit 0
      }
      Stop-Process -Id $existingID -Force -ErrorAction SilentlyContinue
    }
  }
  Remove-Item -LiteralPath $pidFile -Force -ErrorAction SilentlyContinue
}

foreach ($port in @($mixedPort, $controllerPort)) {
  $listener = Get-NetTCPConnection -LocalAddress '127.0.0.1' -LocalPort $port -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1
  if ($null -ne $listener) {
    $owner = Get-CimInstance Win32_Process -Filter ('ProcessId=' + $listener.OwningProcess) -ErrorAction SilentlyContinue
    throw ('registerbot port {0} is occupied by PID {1} ({2})' -f $port, $listener.OwningProcess, $owner.Name)
  }
}

$physicalRoute = Get-NetRoute -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue |
  Where-Object { $_.InterfaceAlias -notmatch '(?i)tun|tap|loopback|vethernet' -and $_.NextHop -ne '0.0.0.0' } |
  Sort-Object @{ Expression = {
    $interface = Get-NetIPInterface -InterfaceIndex $_.InterfaceIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue
    $_.RouteMetric + $interface.InterfaceMetric
  } } |
  Select-Object -First 1
if ($null -eq $physicalRoute) {
  throw 'registerbot physical network interface was not found'
}
$env:REGISTERBOT_INTERFACE = $physicalRoute.InterfaceAlias
$physicalAddress = Get-NetIPAddress -InterfaceIndex $physicalRoute.InterfaceIndex -AddressFamily IPv4 -AddressState Preferred -ErrorAction SilentlyContinue |
  Where-Object { $_.IPAddress -notlike '169.254.*' } |
  Select-Object -First 1
if ($null -eq $physicalAddress) {
  throw 'registerbot physical IPv4 address was not found'
}
$env:REGISTERBOT_LOCAL_ADDRESS = $physicalAddress.IPAddress
& node $prepareScript
if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $runtimeConfig)) {
  throw 'registerbot runtime config generation failed'
}

& $singBox check -c $runtimeConfig -D $botDir
if ($LASTEXITCODE -ne 0) {
  throw ('registerbot proxy config check failed with exit code ' + $LASTEXITCODE)
}

New-Item -ItemType Directory -Path $logDir -Force | Out-Null
$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$stdoutLog = Join-Path $logDir ('bot-core-stdout-' + $stamp + '.log')
$stderrLog = Join-Path $logDir ('bot-core-stderr-' + $stamp + '.log')
$process = Start-Process -FilePath $singBox `
  -ArgumentList @('run', '-c', ('"' + $runtimeConfig + '"'), '-D', ('"' + $botDir + '"')) `
  -WorkingDirectory $botDir `
  -RedirectStandardOutput $stdoutLog `
  -RedirectStandardError $stderrLog `
  -WindowStyle Hidden `
  -PassThru
Set-Content -LiteralPath $pidFile -Value ([string]$process.Id) -Encoding ASCII

$deadline = (Get-Date).AddSeconds(10)
while ((Get-Date) -lt $deadline) {
  $process.Refresh()
  if ($process.HasExited) {
    break
  }
  if ((Test-Listening $mixedPort) -and (Test-ControllerReady)) {
    & node $healthScript
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $healthyNodes)) {
      Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
      Remove-Item -LiteralPath $pidFile -Force -ErrorAction SilentlyContinue
      throw 'registerbot node health check failed'
    }
    Write-Output ('STARTED PID={0} MIXED=127.0.0.1:{1} CTRL=127.0.0.1:{2}' -f $process.Id, $mixedPort, $controllerPort)
    exit 0
  }
  Start-Sleep -Milliseconds 200
}

Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
Remove-Item -LiteralPath $pidFile -Force -ErrorAction SilentlyContinue
if (Test-Path -LiteralPath $stderrLog) {
  Get-Content -LiteralPath $stderrLog -Tail 20 -ErrorAction SilentlyContinue
}
throw 'registerbot dedicated proxy did not become ready within 10 seconds'
