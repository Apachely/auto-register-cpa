# ASCII only. Proves (1) the 402 gate leaves no hold on the balance and (2) the 402 text is a free
# exact-balance oracle, and measures the per-model reservation floor at max_tokens=128000.
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
$k = $keys[0]

function Read-Bal([string]$body) {
  if ($body -match '\$([0-9.]+)\s+available') { return [double]$Matches[1] }
  return $null
}
function Read-Req([string]$body) {
  if ($body -match 'available,\s*\$([0-9.]+)\s+required') { return [double]$Matches[1] }
  return $null
}
function Probe([string]$json) {
  $tag = [IO.Path]::GetRandomFileName()
  $o = Join-Path $outDir ($tag + '.b'); $f = Join-Path $outDir ($tag + '.json')
  [IO.File]::WriteAllText($f,$json,$enc)
  $meta = (& curl.exe -sS -o $o -w '%{http_code}' --connect-timeout 6 --max-time 90 `
        -H ('Authorization: Bearer ' + $k) -H 'Content-Type: application/json' `
        --data-binary ('@' + $f) $chatUrl 2>&1) -join ' '
  $txt = ''
  if (Test-Path $o) { $txt = [IO.File]::ReadAllText($o,[Text.Encoding]::UTF8); Remove-Item $o -Force }
  Remove-Item $f -Force
  return @{ code = $meta; body = $txt }
}

Write-Output '=== E) no-hold proof + balance oracle (key[0], claude-opus-5.5) ==='
$b1 = Probe ('{"model":"anthropic/claude-opus-5.5","messages":[{"role":"user","content":"say ok"}],"max_tokens":800000}')
$av1 = Read-Bal $b1.body
Write-Output ('step1 probe   http=' + $b1.code + '  available=' + $av1)

$mid = ' ' + (('pad word ' * 3000))
$b2 = Probe ('{"model":"anthropic/claude-opus-5.5","messages":[{"role":"user","content":"count to 3 slowly' + $mid + '"}],"max_tokens":200}')
$u = ''
if ($b2.body -match '"usage":\{([^}]*)\}') { $u = $Matches[1] }
Write-Output ('step2 real call http=' + $b2.code + '  usage=' + $u)

$b3 = Probe ('{"model":"anthropic/claude-opus-5.5","messages":[{"role":"user","content":"say ok"}],"max_tokens":800000}')
$av3 = Read-Bal $b3.body
Write-Output ('step3 probe   http=' + $b3.code + '  available=' + $av3)
Write-Output ('delta_available = ' + ($av1 - $av3))
Write-Output ('required_at_800k = ' + (Read-Req $b3.body))

Write-Output ''
Write-Output '=== F) reservation floor at max_tokens=128000, per configured model ==='
foreach ($m in @('anthropic/claude-opus-5.5','anthropic/claude-sonnet-5','anthropic/claude-sonnet-5.5',
                 'openai/gpt-6.2-sol','openai/gpt-5.6-sol','MoonshotAI/kimi-k3','anthropic/claude-fable-5.1')) {
  $r = Probe ('{"model":"' + $m + '","messages":[{"role":"user","content":"say ok"}],"max_tokens":128000}')
  $req = Read-Req $r.body
  $avl = Read-Bal $r.body
  $line = ('{0,-32} http={1}  required={2}  available={3}' -f $m, $r.code, $req, $avl)
  if ($null -eq $req) { $s = ($r.body -replace '\s+',' '); if ($s.Length -gt 120) { $s = $s.Substring(0,120)+'...' }; $line += '  body=' + $s }
  Write-Output $line
}
