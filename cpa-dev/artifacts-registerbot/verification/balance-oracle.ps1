# ASCII only. Free balance oracle: a deliberately unaffordable request makes the upstream 402 body
# print the exact remaining balance of each api-key. Costs nothing (rejected before generation).
$ErrorActionPreference = 'Continue'
$cfgPath = Join-Path ((Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path) 'config.yaml'
$outDir = Join-Path $PSScriptRoot 'balance-probe'
New-Item -ItemType Directory -Force -Path $outDir | Out-Null
$enc = New-Object Text.UTF8Encoding($false)
$lines = Get-Content -Path $cfgPath -Encoding UTF8
$baseUrl = ''; $keys = @()
foreach ($l in $lines) {
  if ($l -match 'base-url:\s*"(https?://[^"]+)"') { $baseUrl = $Matches[1] }
  if ($l -match 'api-key:\s*"(sk-[^"]+)"') { $keys += $Matches[1] }
}
$chatUrl = $baseUrl.TrimEnd('/') + '/chat/completions'
$json = '{"model":"anthropic/claude-opus-5.5","messages":[{"role":"user","content":"x"}],"max_tokens":800000}'
$bf = Join-Path $outDir 'oracle.json'
[IO.File]::WriteAllText($bf,$json,$enc)

$rows = @()
for ($i=0; $i -lt $keys.Count; $i++) {
  $k = $keys[$i]
  $o = Join-Path $outDir ('k' + $i + '.b')
  $code = (& curl.exe -sS -o $o -w '%{http_code}' --connect-timeout 6 --max-time 40 `
        -H ('Authorization: Bearer ' + $k) -H 'Content-Type: application/json' `
        --data-binary ('@' + $bf) $chatUrl 2>&1) -join ''
  $body = ''
  if (Test-Path $o) { $body = [IO.File]::ReadAllText($o,[Text.Encoding]::UTF8); Remove-Item $o -Force }
  $avail = $null; $req = $null
  if ($body -match '\$([0-9.]+)\s+available') { $avail = [double]$Matches[1] }
  if ($body -match 'available,\s*\$([0-9.]+)\s+required') { $req = [double]$Matches[1] }
  $mask = $k.Substring(0,7) + '...' + $k.Substring($k.Length-4)
  $opusOK = 'no'
  if ($null -ne $avail) { if ($avail -ge 2.16) { $opusOK = 'YES' } }
  if ($null -eq $avail) { $opusOK = '?' + $code }
  $rows += ,@($i, $mask, $avail, $opusOK)
  Write-Output ('[' + ('{0,2}' -f $i) + '] ' + $mask + '  http=' + $code + '  balance=' + $avail + '  opus5.5_usable=' + $opusOK)
}
Write-Output ''
$sum = 0.0; $known = 0
foreach ($r in $rows) { if ($null -ne $r[2]) { $sum += $r[2]; $known++ } }
Write-Output ('keys_with_balance = ' + $known + '/' + $rows.Count)
Write-Output ('TOTAL_REMAINING     = $' + [Math]::Round($sum,6))
Write-Output ('keys locked below the $2.16 opus-5.5 floor = ' + (@($rows | Where-Object { $_[3] -eq 'no' }).Count))
Write-Output ('keys still usable on opus-5.5             = ' + (@($rows | Where-Object { $_[3] -eq 'YES' }).Count))
