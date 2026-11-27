[CmdletBinding()]
param(
  [ValidateSet('Start', 'Stop', 'Restart')]
  [string]$Action = 'Restart',
  [switch]$OpenBrowser
)

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$backendSource = Join-Path $root 'cpa-dev\cpa-backend'
$panelRoot = Join-Path $root 'cpa-dev\cpa-panel'
$registerbotProfiles = Join-Path $root 'registerbot\profiles'
$registerbotBot = Join-Path $root 'registerbot-bot'
$runtimeDir = Join-Path $root '.cpa-runtime'
$logsDir = Join-Path $root 'logs'
$backendExe = Join-Path $runtimeDir 'cli-proxy-api.exe'
$configPath = Join-Path $root 'config.yaml'
$backendPidFile = Join-Path $root 'cpa.pid'
$frontendPidFile = Join-Path $root 'frontend.pid'
$backendURL = 'http://127.0.0.1:8317'
$frontendURL = 'http://127.0.0.1:5173/#/login'
$registerbotProxyPort = 11460
$registerbotControllerPort = 11461

function Write-Step([string]$Message) {
  Write-Host ('[CPA] ' + $Message)
}

function Test-TextContainsPath([string]$Value, [string]$Path) {
  if ([string]::IsNullOrWhiteSpace($Value) -or [string]::IsNullOrWhiteSpace($Path)) {
    return $false
  }
  return $Value.IndexOf($Path, [StringComparison]::OrdinalIgnoreCase) -ge 0
}

function Test-CpaProcess($Process) {
  if ($null -eq $Process) {
    return $false
  }
  $name = ([string]$Process.Name).ToLowerInvariant()
  $commandLine = [string]$Process.CommandLine
  $executablePath = [string]$Process.ExecutablePath

  if ($name -eq 'cli-proxy-api.exe') {
    return (Test-TextContainsPath $commandLine $root) -or
      (Test-TextContainsPath $executablePath $root)
  }
  if ($name -in @('node.exe', 'vite.exe', 'cmd.exe')) {
    return Test-TextContainsPath $commandLine $panelRoot
  }
  if ($name -in @('chrome.exe', 'msedge.exe')) {
    return Test-TextContainsPath $commandLine $registerbotProfiles
  }
  if ($name -in @('sing-box.exe', 'core.exe')) {
    return Test-TextContainsPath $commandLine $registerbotBot
  }
  return $false
}

function Get-CpaPidFiles {
  $files = New-Object 'System.Collections.Generic.List[string]'
  [void]$files.Add($backendPidFile)
  [void]$files.Add($frontendPidFile)
  [void]$files.Add((Join-Path $runtimeDir 'backend.pid'))
  [void]$files.Add((Join-Path $runtimeDir 'frontend.pid'))
  foreach ($directory in @((Join-Path $root 'registerbot'), $registerbotBot)) {
    if (Test-Path -LiteralPath $directory) {
      foreach ($file in @(Get-ChildItem -LiteralPath $directory -Filter '*.pid' -File -ErrorAction SilentlyContinue)) {
        [void]$files.Add($file.FullName)
      }
    }
  }
  return @($files)
}

function Stop-CpaRuntime {
  $processes = @(Get-CimInstance Win32_Process -ErrorAction SilentlyContinue)
  $byID = @{}
  foreach ($process in $processes) {
    $byID[[int]$process.ProcessId] = $process
  }

  $targets = New-Object 'System.Collections.Generic.HashSet[int]'
  foreach ($process in $processes) {
    if (Test-CpaProcess $process) {
      [void]$targets.Add([int]$process.ProcessId)
    }
  }

  foreach ($pidFile in @(Get-CpaPidFiles)) {
    if (-not (Test-Path -LiteralPath $pidFile)) {
      continue
    }
    $processID = 0
    if ([int]::TryParse((Get-Content -LiteralPath $pidFile -Raw).Trim(), [ref]$processID) -and
      $byID.ContainsKey($processID) -and (Test-CpaProcess $byID[$processID])) {
      [void]$targets.Add($processID)
    }
  }

  $changed = $true
  while ($changed) {
    $changed = $false
    foreach ($process in $processes) {
      $processID = [int]$process.ProcessId
      $parentID = [int]$process.ParentProcessId
      if ($targets.Contains($parentID) -and -not $targets.Contains($processID)) {
        [void]$targets.Add($processID)
        $changed = $true
      }
    }
    foreach ($processID in @($targets)) {
      if (-not $byID.ContainsKey($processID)) {
        continue
      }
      $parentID = [int]$byID[$processID].ParentProcessId
      if (-not $byID.ContainsKey($parentID) -or $targets.Contains($parentID)) {
        continue
      }
      $parent = $byID[$parentID]
      $parentName = ([string]$parent.Name).ToLowerInvariant()
      $parentCommand = [string]$parent.CommandLine
      if ($parentName -in @('node.exe', 'vite.exe', 'cmd.exe') -and
        $parentCommand -match '(?i)(vite|npm(?:-cli)?\.js|npm\.cmd.+run dev)') {
        [void]$targets.Add($parentID)
        $changed = $true
      }
    }
  }

  $ordered = @($targets | Where-Object { $byID.ContainsKey($_) } | Sort-Object -Descending)
  foreach ($processID in $ordered) {
    $process = $byID[$processID]
    Stop-Process -Id $processID -Force -ErrorAction SilentlyContinue
    Write-Step ('Stopped PID {0} ({1})' -f $processID, $process.Name)
  }

  foreach ($pidFile in @(Get-CpaPidFiles)) {
    if (Test-Path -LiteralPath $pidFile) {
      Remove-Item -LiteralPath $pidFile -Force -ErrorAction SilentlyContinue
    }
  }

  if ($ordered.Count -eq 0) {
    Write-Step 'No CPA runtime process was running.'
  }
}

function Test-PortListening([int]$Port) {
  return $null -ne (Get-NetTCPConnection -LocalAddress '127.0.0.1' -LocalPort $Port -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1)
}

function Assert-PortFree([int]$Port) {
  $listener = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1
  if ($null -eq $listener) {
    return
  }
  $owner = Get-CimInstance Win32_Process -Filter ('ProcessId=' + $listener.OwningProcess) -ErrorAction SilentlyContinue
  $ownerName = if ($null -ne $owner) { $owner.Name } else { 'unknown' }
  throw ('Port {0} is still occupied by PID {1} ({2}).' -f $Port, $listener.OwningProcess, $ownerName)
}

function Get-LatestBackendSourceTime {
  $latest = [DateTime]::MinValue
  $paths = @(
    (Join-Path $backendSource 'cmd'),
    (Join-Path $backendSource 'internal'),
    (Join-Path $backendSource 'sdk')
  )
  foreach ($sourcePath in $paths) {
    foreach ($file in @(Get-ChildItem -LiteralPath $sourcePath -Recurse -File -ErrorAction Stop)) {
      if ($file.LastWriteTimeUtc -gt $latest) {
        $latest = $file.LastWriteTimeUtc
      }
    }
  }
  foreach ($name in @('go.mod', 'go.sum')) {
    $file = Get-Item -LiteralPath (Join-Path $backendSource $name) -ErrorAction Stop
    if ($file.LastWriteTimeUtc -gt $latest) {
      $latest = $file.LastWriteTimeUtc
    }
  }
  return $latest
}

function Ensure-BackendBinary {
  if (-not (Test-Path -LiteralPath (Join-Path $backendSource 'go.mod'))) {
    throw ('Backend source not found: ' + $backendSource)
  }
  New-Item -ItemType Directory -Path $runtimeDir -Force | Out-Null
  $needsBuild = -not (Test-Path -LiteralPath $backendExe)
  if (-not $needsBuild) {
    $needsBuild = (Get-LatestBackendSourceTime) -gt (Get-Item -LiteralPath $backendExe).LastWriteTimeUtc
  }
  if (-not $needsBuild) {
    Write-Step 'Backend binary is current.'
    return
  }

  $goCommand = Get-Command go.exe -ErrorAction SilentlyContinue
  if ($null -eq $goCommand) {
    $goCommand = Get-Command go -ErrorAction Stop
  }
  $temporaryExe = Join-Path $runtimeDir 'cli-proxy-api.build.exe'
  if (Test-Path -LiteralPath $temporaryExe) {
    Remove-Item -LiteralPath $temporaryExe -Force
  }
  Write-Step 'Building backend from current source...'
  Push-Location -LiteralPath $backendSource
  try {
    & $goCommand.Source build -o $temporaryExe ./cmd/server
    if ($LASTEXITCODE -ne 0) {
      throw ('Backend build failed with exit code ' + $LASTEXITCODE)
    }
  } finally {
    Pop-Location
  }
  Move-Item -LiteralPath $temporaryExe -Destination $backendExe -Force
  Write-Step 'Backend build completed.'
}

function Wait-LocalPort([int]$Port, [int]$TimeoutSeconds, [System.Diagnostics.Process]$Process) {
  $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
  while ((Get-Date) -lt $deadline) {
    if ($Process.HasExited) {
      return $false
    }
    if (Test-PortListening $Port) {
      return $true
    }
    Start-Sleep -Milliseconds 200
    $Process.Refresh()
  }
  return $false
}

function Show-LogTail([string]$Path) {
  if (Test-Path -LiteralPath $Path) {
    Write-Host ('--- ' + $Path + ' ---')
    Get-Content -LiteralPath $Path -Tail 30 -ErrorAction SilentlyContinue
  }
}

function Start-RegisterbotProxy {
  $startScript = Join-Path $registerbotBot 'start-bot-core.ps1'
  if (-not (Test-Path -LiteralPath $startScript)) {
    throw ('Registerbot proxy start script not found: ' + $startScript)
  }
  $internetSettings = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Internet Settings'
  $before = Get-ItemProperty -LiteralPath $internetSettings -ErrorAction SilentlyContinue
  $beforeFingerprint = '{0}|{1}|{2}' -f $before.ProxyEnable, $before.ProxyServer, $before.AutoConfigURL
  Write-Step ('Starting isolated registerbot proxy on 127.0.0.1:' + $registerbotProxyPort + ' ...')
  & powershell.exe -NoProfile -ExecutionPolicy Bypass -File $startScript
  if ($LASTEXITCODE -ne 0) {
    throw ('Registerbot proxy startup failed with exit code ' + $LASTEXITCODE)
  }
  if (-not (Test-PortListening $registerbotProxyPort) -or -not (Test-PortListening $registerbotControllerPort)) {
    throw ('Registerbot proxy ports {0}/{1} are not ready.' -f $registerbotProxyPort, $registerbotControllerPort)
  }
  $after = Get-ItemProperty -LiteralPath $internetSettings -ErrorAction SilentlyContinue
  $afterFingerprint = '{0}|{1}|{2}' -f $after.ProxyEnable, $after.ProxyServer, $after.AutoConfigURL
  if ($afterFingerprint -ne $beforeFingerprint) {
    throw 'Registerbot proxy startup changed the Windows system proxy.'
  }
  Write-Step 'Isolated registerbot proxy ready; Windows system proxy is unchanged.'
}

function Start-CpaRuntime {
  if (-not (Test-Path -LiteralPath $configPath)) {
    throw ('CPA config not found: ' + $configPath)
  }
  if (-not (Test-Path -LiteralPath (Join-Path $panelRoot 'node_modules\vite\bin\vite.js'))) {
    throw ('Frontend dependencies are missing. Run npm install in ' + $panelRoot)
  }

  Assert-PortFree 8317
  Assert-PortFree 5173
  Start-RegisterbotProxy
  Ensure-BackendBinary
  New-Item -ItemType Directory -Path $logsDir -Force | Out-Null

  $stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
  $backendOut = Join-Path $logsDir ('backend-stdout-' + $stamp + '.log')
  $backendErr = Join-Path $logsDir ('backend-stderr-' + $stamp + '.log')
  $frontendOut = Join-Path $logsDir ('frontend-stdout-' + $stamp + '.log')
  $frontendErr = Join-Path $logsDir ('frontend-stderr-' + $stamp + '.log')

  Write-Step ('Starting backend on ' + $backendURL + ' ...')
  $backend = Start-Process -FilePath $backendExe `
    -ArgumentList @('--config', ('"' + $configPath + '"')) `
    -WorkingDirectory $root `
    -RedirectStandardOutput $backendOut `
    -RedirectStandardError $backendErr `
    -WindowStyle Hidden `
    -PassThru
  Set-Content -LiteralPath $backendPidFile -Value ([string]$backend.Id) -Encoding ASCII
  Set-Content -LiteralPath (Join-Path $runtimeDir 'backend.pid') -Value ([string]$backend.Id) -Encoding ASCII

  $nodeCommand = Get-Command node.exe -ErrorAction SilentlyContinue
  if ($null -eq $nodeCommand) {
    $nodeCommand = Get-Command node -ErrorAction Stop
  }
  $viteScript = Join-Path $panelRoot 'node_modules\vite\bin\vite.js'
  Write-Step 'Starting frontend on http://127.0.0.1:5173 ...'
  $hadBackendURL = Test-Path Env:CPA_BACKEND_URL
  $previousBackendURL = $env:CPA_BACKEND_URL
  try {
    $env:CPA_BACKEND_URL = $backendURL
    $frontend = Start-Process -FilePath $nodeCommand.Source `
      -ArgumentList @(('"' + $viteScript + '"'), '--host', '127.0.0.1', '--port', '5173') `
      -WorkingDirectory $panelRoot `
      -RedirectStandardOutput $frontendOut `
      -RedirectStandardError $frontendErr `
      -WindowStyle Hidden `
      -PassThru
  } finally {
    if ($hadBackendURL) {
      $env:CPA_BACKEND_URL = $previousBackendURL
    } else {
      Remove-Item Env:CPA_BACKEND_URL -ErrorAction SilentlyContinue
    }
  }
  Set-Content -LiteralPath $frontendPidFile -Value ([string]$frontend.Id) -Encoding ASCII
  Set-Content -LiteralPath (Join-Path $runtimeDir 'frontend.pid') -Value ([string]$frontend.Id) -Encoding ASCII

  $backendReady = Wait-LocalPort 8317 30 $backend
  $frontendReady = Wait-LocalPort 5173 30 $frontend
  if (-not $backendReady -or -not $frontendReady) {
    Show-LogTail $backendErr
    Show-LogTail $frontendErr
    Stop-CpaRuntime
    throw ('Startup failed. Backend ready={0}; frontend ready={1}' -f $backendReady, $frontendReady)
  }

  Write-Step ('Backend ready. PID={0}; log={1}' -f $backend.Id, $backendErr)
  Write-Step ('Frontend ready. PID={0}; log={1}' -f $frontend.Id, $frontendErr)
  Write-Step ('Registration uses isolated proxy/controller {0}/{1}; system proxy 12450 was left unchanged.' -f $registerbotProxyPort, $registerbotControllerPort)

  if ($OpenBrowser) {
    Start-Process $frontendURL | Out-Null
  }
}

try {
  Set-Location -LiteralPath $root
  switch ($Action) {
    'Stop' {
      Stop-CpaRuntime
    }
    'Start' {
      if ((Test-PortListening 8317) -and (Test-PortListening 5173) -and (Test-PortListening $registerbotProxyPort) -and (Test-PortListening $registerbotControllerPort)) {
        Write-Step 'Backend, frontend, and isolated registerbot proxy are already running.'
        if ($OpenBrowser) {
          Start-Process $frontendURL | Out-Null
        }
      } else {
        Stop-CpaRuntime
        Start-CpaRuntime
      }
    }
    'Restart' {
      Stop-CpaRuntime
      Start-CpaRuntime
    }
  }
  exit 0
} catch {
  Write-Host ('[CPA] ERROR: ' + $_.Exception.Message) -ForegroundColor Red
  exit 1
}
