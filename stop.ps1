$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
& (Join-Path $root 'cpa-service.ps1') -Action Stop
exit $LASTEXITCODE
