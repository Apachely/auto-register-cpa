# ASCII only. Sweep every api-key with a tiny request + probe billing endpoints.
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
Write-Output ('base_url = ' + $baseUrl)
Write-Output ('root     = ' + $root)
Write-Output ('keys     = ' + $keys.Count)

$proxy = 'http://127.0.0.1:12450'
function Curl([string]$key, [string]$url, [string]$bodyFile) {
  $o = Join-Path $outDir ([IO.Path]::GetRandomFileName())
  $h = $o + '.h'
  $a = @('-sS','-o',$o,'-D',$h,'-w','%{http_code}','--proxy',$proxy,'--connect-timeout','8','--max-time','75',
         '-H',('Authorization: Bearer '+$key),'-H','Content-Type: application/json')
  if ($bodyFile) { $a += @('--data-binary',('@'+$bodyFile)) }
  $a += $url
  $code = & curl.exe @a 2>&1
  $txt = ''
  if (Test-Path $o) { $txt = [IO.File]::ReadAllText($o,[Text.Encoding]::UTF8); Remove-Item $o -Force }
  $hdrs = @()
  if (Test-Path $h) { $hdrs = @(Get-Content $h -Encoding UTF8); Remove-Item $h -Force }
  return @{ code = ($code -join ' '); body = $txt; hdrs = $hdrs }
}

# ---- 1. tiny chat sweep over all keys ----
$body = '{"model":"anthropic/claude-opus-5.5","messages":[{"role":"user","content":"Hi"}],"max_tokens":4}'
$bf = Join-Path $outDir 'tiny.json'
[IO.File]::WriteAllText($bf,$body,$enc)
Write-Output ''
Write-Output '=== TINY CHAT SWEEP (max_tokens=4) ==='
for ($i=0; $i -lt $keys.Count; $i++) {
  $k = $keys[$i]
  $mask = $k.Substring(0,7)+'...'+$k.Substring($k.Length-4)
  $r = Curl $k ($baseUrl.TrimEnd('/')+'/chat/completions') $bf
  $b = $r.body -replace '\s+',' '
  if ($b.Length -gt 220) { $b = $b.Substring(0,220)+'...' }
  Write-Output ('[' + ('{0,2}' -f $i) + '] ' + $mask + '  http=' + $r.code + '  ' + $b)
  Start-Sleep -Milliseconds 200
}

# ---- 2. billing / usage endpoint discovery with key[0] ----
Write-Output ''
Write-Output '=== ENDPOINT PROBE with key[0] ==='
$paths = @(
 '/v1/dashboard/billing/subscription',
 '/v1/dashboard/billing/usage',
 '/v1/billing/subscription',
 '/v1/billing/usage',
 '/v1/balance',
 '/v1/usage',
 '/v1/key',
 '/v1/keys',
 '/v1/me',
 '/v1/user',
 '/v1/models',
 '/dashboard/billing/subscription',
 '/api/billing/usage',
 '/api/usage',
 '/api/me'
)
foreach ($p in $paths) {
  $r = Curl $keys[0] ($root + $p) $null
  $b = $r.body -replace '\s+',' '
  if ($b.Length -gt 260) { $b = $b.Substring(0,260)+'...' }
  Write-Output ('{0,-42} http={1}  {2}' -f $p, $r.code, $b)
  Start-Sleep -Milliseconds 150
}
