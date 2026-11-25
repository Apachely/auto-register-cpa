param([string]$Root = 'E:\LY\Tool\CPA', [string]$Channel = 'freeaiapikey')
$ErrorActionPreference = 'Stop'
if ([string]::IsNullOrWhiteSpace($env:CPA_REGISTERBOT_ACCOUNT)) { throw 'Set CPA_REGISTERBOT_ACCOUNT to account|password for this process.' }
$key = (Get-Content -LiteralPath (Join-Path $Root 'admin-key.txt') -Raw).Trim()
$headers = @{Authorization = "Bearer $key"}
$body = @{channel=$Channel; accounts=$env:CPA_REGISTERBOT_ACCOUNT} | ConvertTo-Json -Compress
$env:CPA_REGISTERBOT_ACCOUNT = $null
$response = Invoke-RestMethod -Uri 'http://127.0.0.1:8317/v8/management/registerbot/runs' -Method Post -Headers $headers -ContentType 'application/json' -Body $body -TimeoutSec 15
$body = $null
$output = Join-Path $Root 'cpa-dev\artifacts-registerbot\verification\live-run.json'
$response | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $output -Encoding utf8
Write-Output ('RUN_ID=' + $response.run.id)
Write-Output ('STATUS=' + $response.run.status)
