# ASCII only. Direct egress. Measures the upstream balance pre-check ("required") as a function of
# prompt size and max_tokens, using the earliest api-key from config.yaml.
$ErrorActionPreference = 'Continue'
$cfgPath = Join-Path ((Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path) 'config.yaml'
$outDir = Join-Path $PSScriptRoot 'balance-probe'
New-Item -ItemType Directory -Force -Path $outDir | Out-Null
$enc = New-Object Text.UTF8Encoding($false)

$lines = Get-Content -Path $cfgPath -Encoding UTF8
$baseUrl = ''
$keys = @()
foreach ($l in $lines) {
  if ($l -match 'base-url:\s*"(https?://[^"]+)"') { $baseUrl = $Matches[1] }
  if ($l -match 'api-key:\s*"(sk-[^"]+)"') { $keys += $Matches[1] }
}
$root = $baseUrl -replace '/v1/?$',''
$chatUrl = $baseUrl.TrimEnd('/') + '/chat/completions'

function Invoke-Probe([string]$key, [string]$url, [string]$bodyJson, [string]$method) {
  $tag = [IO.Path]::GetRandomFileName()
  $o = Join-Path $outDir ($tag + '.b')
  $h = Join-Path $outDir ($tag + '.h')
  $f = $null
  $a = @('-sS','-o',$o,'-D',$h,'-w','%{http_code} %{time_total}','--connect-timeout','6','--max-time','75',
         '-H',('Authorization: Bearer '+$key),'-H','Content-Type: application/json')
  if ($method) { $a = @('-X',$method) + $a }
  if ($bodyJson) {
    $f = Join-Path $outDir ($tag + '.json')
    [IO.File]::WriteAllText($f,$bodyJson,$enc)
    $a = $a + @('--data-binary',('@' + $f))
  }
  $a = $a + @($url)
  $meta = (& curl.exe @a 2>&1) -join ' '
  $txt = ''
  if (Test-Path $o) { $txt = [IO.File]::ReadAllText($o,[Text.Encoding]::UTF8); Remove-Item $o -Force }
  $hdrs = @()
  if (Test-Path $h) { $hdrs = @(Get-Content $h -Encoding UTF8); Remove-Item $h -Force }
  if ($f) { Remove-Item $f -Force }
  return @{ meta = $meta; body = $txt; hdrs = $hdrs }
}

function Get-Required([string]$body) {
  if ($body -match 'available,\s*\$([0-9.]+)\s+required') { return [double]$Matches[1] }
  return $null
}
function Get-Available([string]$body) {
  if ($body -match '\$([0-9.]+)\s+available') { return [double]$Matches[1] }
  return $null
}

Write-Output ('keys = ' + $keys.Count + '   base = ' + $baseUrl)

# ================= A. tiny sweep over all keys =================
Write-Output ''
Write-Output '=== A) TINY SWEEP (max_tokens=4, prompt "Hi") ==='
$tinyJson = '{"model":"anthropic/claude-sonnet-5","messages":[{"role":"user","content":"Hi"}],"max_tokens":4}'
for ($i=0; $i -lt $keys.Count; $i++) {
  $k = $keys[$i]
  $mask = $k.Substring(0,7) + '...' + $k.Substring($k.Length-4)
  $r = Invoke-Probe $k $chatUrl $tinyJson $null
  $b = ($r.body -replace '\s+',' ')
  $note = ''
  if ($b -match '"total_tokens":(\d+)') { $note = 'tokens=' + $Matches[1] }
  if ($b -match '"message":"([^"]{0,120})') { $note = 'ERR=' + $Matches[1] }
  if ($note -eq '') { $note = $b.Substring(0,[Math]::Min(60,$b.Length)) }
  Write-Output ('[' + ('{0,2}' -f $i) + '] ' + $mask + '  ' + $r.meta + '  ' + $note)
}

# ================= B. billing endpoint discovery =================
Write-Output ''
Write-Output '=== B) ENDPOINT PROBE (key[0]) ==='
$paths = @('/v1/dashboard/billing/subscription','/v1/dashboard/billing/usage','/v1/billing/subscription',
           '/v1/billing/usage','/v1/balance','/v1/usage','/v1/key','/v1/me','/v1/subscriptions',
           '/dashboard/billing/subscription','/api/billing/usage','/api/usage','/api/me','/api/dashboard/stats')
foreach ($p in $paths) {
  $r = Invoke-Probe $keys[0] ($root + $p) $null 'GET'
  $b = ($r.body -replace '\s+',' ')
  if ($b.Length -gt 170) { $b = $b.Substring(0,170) + '...' }
  Write-Output ('{0,-38} {1}  {2}' -f $p, $r.meta, $b)
}

# ================= C. required-amount vs max_tokens =================
Write-Output ''
Write-Output '=== C) required vs max_tokens (tiny prompt, model claude-opus-5.5) ==='
foreach ($mt in @(4,1000,32000,100000,200000,400000,800000)) {
  $j = '{"model":"anthropic/claude-opus-5.5","messages":[{"role":"user","content":"say ok"}],"max_tokens":' + $mt + '}'
  $r = Invoke-Probe $keys[0] $chatUrl $j $null
  $req = Get-Required $r.body
  $avl = Get-Available $r.body
  $b = ($r.body -replace '\s+',' ')
  if ($b.Length -gt 200) { $b = $b.Substring(0,200) + '...' }
  Write-Output ('max_tokens=' + ('{0,7}' -f $mt) + '  ' + $r.meta + '  required=' + $req + '  available=' + $avl)
  if ($req -eq $null) { Write-Output ('    body: ' + $b) }
}

# ================= D. required vs prompt size =================
Write-Output ''
Write-Output '=== D) required vs prompt size (max_tokens fixed at 400000) ==='
foreach ($pad in @(0,2000,8000,20000)) {
  $words = ''
  if ($pad -gt 0) { $words = ' ' + (('pad word ' * $pad)) }
  $j = '{"model":"anthropic/claude-opus-5.5","messages":[{"role":"user","content":"say ok' + $words + '"}],"max_tokens":400000}'
  $r = Invoke-Probe $keys[0] $chatUrl $j $null
  $req = Get-Required $r.body
  Write-Output ('pad_words=' + ('{0,6}' -f $pad) + '  req_bytes=' + ('{0,8}' -f $j.Length) + '  ' + $r.meta + '  required=' + $req)
  if ($req -eq $null) { $b = ($r.body -replace '\s+',' '); if ($b.Length -gt 200) { $b = $b.Substring(0,200)+'...' }; Write-Output ('    body: ' + $b) }
}
