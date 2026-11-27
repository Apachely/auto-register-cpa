# ASCII only. NOTE: never name a function "Curl" in PowerShell - the built-in curl alias wins.
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
$proxy = 'http://127.0.0.1:12450'
Write-Output ('base_url = ' + $baseUrl)
Write-Output ('keys     = ' + $keys.Count)

function Invoke-Probe([string]$key, [string]$url, [string]$bodyFile) {
  $tag = [IO.Path]::GetRandomFileName()
  $o = Join-Path $outDir ($tag + '.b')
  $h = Join-Path $outDir ($tag + '.h')
  $argList = New-Object System.Collections.ArrayList
  [void]$argList.Add('-sS'); [void]$argList.Add('-o'); [void]$argList.Add($o)
  [void]$argList.Add('-D'); [void]$argList.Add($h)
  [void]$argList.Add('-w'); [void]$argList.Add('%{http_code}')
  [void]$argList.Add('--proxy'); [void]$argList.Add($proxy)
  [void]$argList.Add('--connect-timeout'); [void]$argList.Add('8')
  [void]$argList.Add('--max-time'); [void]$argList.Add('75')
  [void]$argList.Add('-H'); [void]$argList.Add('Authorization: Bearer ' + $key)
  [void]$argList.Add('-H'); [void]$argList.Add('Content-Type: application/json')
  if ($bodyFile) { [void]$argList.Add('--data-binary'); [void]$argList.Add('@' + $bodyFile) }
  [void]$argList.Add($url)
  $code = (& curl.exe @argList 2>&1) -join ' '
  $txt = ''
  if (Test-Path $o) { $txt = [IO.File]::ReadAllText($o,[Text.Encoding]::UTF8); Remove-Item $o -Force }
  $hdrs = @()
  if (Test-Path $h) { $hdrs = @(Get-Content $h -Encoding UTF8); Remove-Item $h -Force }
  return @{ code = $code; body = $txt; hdrs = $hdrs }
}

function New-BodyFile([string]$tag, [string]$json) {
  $p = Join-Path $outDir ($tag + '.json')
  [IO.File]::WriteAllText($p, $json, $enc)
  return $p
}

# ---------- 1. tiny chat sweep over all 25 keys ----------
$chatUrl = $baseUrl.TrimEnd('/') + '/chat/completions'
$tiny = New-BodyFile 'tiny' '{"model":"anthropic/claude-opus-5.5","messages":[{"role":"user","content":"Hi"}],"max_tokens":4}'
Write-Output ''
Write-Output '=== 1) TINY CHAT SWEEP  max_tokens=4 ==='
for ($i=0; $i -lt $keys.Count; $i++) {
  $k = $keys[$i]
  $mask = $k.Substring(0,7) + '...' + $k.Substring($k.Length-4)
  $r = Invoke-Probe $k $chatUrl $tiny
  $b = ($r.body -replace '\s+',' ')
  if ($b.Length -gt 200) { $b = $b.Substring(0,200) + '...' }
  Write-Output ('[' + ('{0,2}' -f $i) + '] ' + $mask + '  http=' + $r.code + '  ' + $b)
  Start-Sleep -Milliseconds 150
}

# ---------- 2. endpoint discovery with key[0] ----------
Write-Output ''
Write-Output '=== 2) ENDPOINT PROBE (key[0]) ==='
$paths = @('/v1/dashboard/billing/subscription','/v1/dashboard/billing/usage','/v1/billing/subscription',
           '/v1/billing/usage','/v1/balance','/v1/usage','/v1/key','/v1/me','/v1/models',
           '/dashboard/billing/subscription','/api/billing/usage','/api/usage','/api/me','/pricing','/docs')
foreach ($p in $paths) {
  $r = Invoke-Probe $keys[0] ($root + $p) $null
  $b = ($r.body -replace '\s+',' ')
  if ($b.Length -gt 200) { $b = $b.Substring(0,200) + '...' }
  Write-Output ('{0,-40} http={1}  {2}' -f $p, $r.code, $b)
  Start-Sleep -Milliseconds 120
}

# ---------- 3. does the required amount scale with request size? ----------
Write-Output ''
Write-Output '=== 3) SIZE SCALING on key[0] (does "required" move?) ==='
$cases = @(
  @{ n='a_mt8_pad0';     mt=8;    pad=0 },
  @{ n='b_mt64_pad0';    mt=64;   pad=0 },
  @{ n='c_mt2048_pad0';  mt=2048; pad=0 },
  @{ n='d_mt8192_pad0';  mt=8192; pad=0 },
  @{ n='e_mt64000_pad0'; mt=64000; pad=0 },
  @{ n='f_mt64_pad2000'; mt=64;   pad=2000 },
  @{ n='g_mt64_pad20000';mt=64;   pad=20000 }
)
foreach ($c in $cases) {
  $pad = ''
  if ($c.pad -gt 0) { $pad = ' ' + (('pad word ' * $c.pad)) }
  $json = '{"model":"anthropic/claude-opus-5.5","messages":[{"role":"user","content":"say ok' + $pad + '"}],"max_tokens":' + $c.mt + '}'
  $bf = New-BodyFile $c.n $json
  $r = Invoke-Probe $keys[0] $chatUrl $bf
  $b = ($r.body -replace '\s+',' ')
  if ($b.Length -gt 300) { $b = $b.Substring(0,300) + '...' }
  Write-Output ('{0,-16} req_bytes={1,-7} http={2}  {3}' -f $c.n, $json.Length, $r.code, $b)
  foreach ($h in $r.hdrs) { if ($h -match '^(retry-after|x-ratelimit)') { Write-Output ('    HDR ' + $h.Trim()) } }
  Start-Sleep -Milliseconds 200
}
