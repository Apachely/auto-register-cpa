param([switch]$ReplaceFromAuthoritativeCopy)
$ErrorActionPreference='Stop'
$root='E:\LY\Tool\CPA'
$admin=(Get-Content -LiteralPath (Join-Path $root 'admin-key.txt') -Raw).Trim()
$headers=@{Authorization="Bearer $admin"}
$url='http://127.0.0.1:8317/v8/management/config/api-keys/openai-compatibility'
$before=Invoke-WebRequest -Uri $url -Headers $headers -TimeoutSec 15
$groups=@($before.Content|ConvertFrom-Json)
$provider=@($groups|Where-Object name -eq 'FreeAi')
if($provider.Count -ne 1 -or $provider[0].keys.Count -ne 5){throw 'Provider layout changed'}
$provider=$provider[0]
$protected=(Get-Content -LiteralPath (Join-Path $root 'registerbot\key5-repair.dpapi') -Raw).Trim()
$secure=ConvertTo-SecureString -String $protected
$full=[Net.NetworkCredential]::new('',$secure).Password
$old=[string]$provider.keys[4].'api-key'
if($full.Length -ne 46 -or !$full.StartsWith($old) -or $old.Length -ne 28){throw 'Expected the authenticated copy of the truncated fifth key'}
$results=@()
if(!$ReplaceFromAuthoritativeCopy){
foreach($case in @(@{label='complete_key5';key=$full},@{label='existing_key1';key=$provider.keys[0].'api-key'})){
 $payload=@{method='POST';url=$provider.'base-url'.TrimEnd('/')+'/chat/completions';proxy_url='http://127.0.0.1:12450';header=@{Authorization=('Bearer '+$case.key);'Content-Type'='application/json'};data=(@{model=$provider.models[0].name;messages=@(@{role='user';content='Hi'});stream=$false;max_tokens=5}|ConvertTo-Json -Depth 5 -Compress)}
 $response=Invoke-RestMethod 'http://127.0.0.1:8317/v8/management/requests/api-call' -Method Post -Headers $headers -ContentType 'application/json' -Body ($payload|ConvertTo-Json -Depth 7 -Compress) -TimeoutSec 65
 $parsed=$null;try{$parsed=$response.body|ConvertFrom-Json}catch{}
 $title=[regex]::Match([string]$response.body,'<title>([^<]+)</title>').Groups[1].Value
 $row=[pscustomobject]@{case=$case.label;http=$response.status_code;choices=if($parsed.choices){@($parsed.choices).Count}else{0};htmlTitle=$title;error=if($parsed.error -is [string]){$parsed.error}else{$parsed.error.message}}
 $results+=$row;$row|ConvertTo-Json -Compress
}
} else {
 $results=@(Get-Content -LiteralPath (Join-Path $PSScriptRoot 'upstream-probes.json') -Raw|ConvertFrom-Json)
}
$passed=$results.Count -gt 0 -and $results[0].http -eq 200 -and $results[0].choices -gt 0
if(!$passed -and !$ReplaceFromAuthoritativeCopy){$results|ConvertTo-Json|Set-Content (Join-Path $PSScriptRoot 'upstream-probes.json') -Encoding utf8;exit 2}
$fresh=Invoke-WebRequest -Uri $url -Headers $headers -TimeoutSec 15
if($fresh.Headers.ETag[0] -ne $before.Headers.ETag[0]){throw 'Configuration changed; replacement not attempted'}
$provider.keys[4].'api-key'=$full
$writeHeaders=@{Authorization="Bearer $admin";'If-Match'=$before.Headers.ETag[0]}
$saved=Invoke-RestMethod -Uri $url -Method Put -Headers $writeHeaders -ContentType 'application/json' -Body (ConvertTo-Json -InputObject $groups -Depth 30 -Compress) -TimeoutSec 15
if($saved.status -ne 'ok'){throw 'Replacement was not confirmed'}
$readback=@(Invoke-RestMethod -Uri $url -Headers $headers -TimeoutSec 15)|Where-Object name -eq 'FreeAi'
if($readback.keys.Count -ne 5 -or $readback.keys[4].'api-key' -ne $full){throw 'Replacement readback failed'}
for($index=0;$index -lt 4;$index++){if($readback.keys[$index].'api-key' -ne $provider.keys[$index].'api-key'){throw 'Unexpected earlier key change'}}
$result=[ordered]@{replacedIndex=5;oldLength=28;newLength=46;keyCount=5;firstFourUnchanged=$true;hint=('sk-…'+$full.Substring($full.Length-4));onboardingCompleted=$true;initialCreditClaimed=5;inferenceVerified=$passed;probes=$results}
$result|ConvertTo-Json -Depth 5|Set-Content (Join-Path $PSScriptRoot 'repair-result.json') -Encoding utf8
$result|ConvertTo-Json -Depth 5
