param([int]$WaitSeconds = 20)
$ErrorActionPreference='Stop'
$root='E:\LY\Tool\CPA'
$artifactRoot=$PSScriptRoot
$runId=(Get-Content -LiteralPath "$artifactRoot\live-run.json" -Raw|ConvertFrom-Json).id
$headers=@{Authorization=('Bearer '+(Get-Content -LiteralPath "$root\admin-key.txt" -Raw).Trim())}
$deadline=(Get-Date).AddSeconds($WaitSeconds)
$last=''
do {
  $snapshot=Invoke-RestMethod -Uri 'http://127.0.0.1:8317/v8/management/registerbot' -Headers $headers
  $run=@($snapshot.runs|Where-Object {$_.id -eq $runId})[0]
  if(-not $run){throw 'Recorded live run is missing'}
  $json=$run|ConvertTo-Json -Depth 8
  if($json -ne $last){Write-Output $json;$last=$json}
  $json|Set-Content -LiteralPath "$artifactRoot\live-result.json" -Encoding utf8
  if($run.status -notin @('queued','running')){break}
  Start-Sleep -Seconds 2
} while((Get-Date) -lt $deadline)
$events=@(Get-Content -LiteralPath "$root\registerbot\events.jsonl"|ForEach-Object {try {$entry=$_|ConvertFrom-Json;if($entry.run -eq $runId){$entry}}catch{}})
$events|ConvertTo-Json -Depth 8|Set-Content -LiteralPath "$artifactRoot\live-events.json" -Encoding utf8
Write-Output ($events|Select-Object seq,stage,level,message|ConvertTo-Json -Depth 5)
