# ASCII only. Measure the INPUT rate of each configured model with two 402 probes (costs nothing):
# required(pad=20000) - required(pad=0) over the same max_tokens gives the prompt-token delta.
$ErrorActionPreference = 'Continue'
$cfgPath = Join-Path ((Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path) 'config.yaml'
$outDir = Join-Path $PSScriptRoot 'balance-probe'
$enc = New-Object Text.UTF8Encoding($false)
$lines = Get-Content -Path $cfgPath -Encoding UTF8
$baseUrl = ''; $keys = @()
foreach ($l in $lines) {
  if ($l -match 'base-url:\s*"(https?://[^"]+)"') { $baseUrl = $Matches[1] }
  if ($l -match 'api-key:\s*"(sk-[^"]+)"') { $keys += $Matches[1] }
}
$k = $keys[0]
$chatUrl = $baseUrl.TrimEnd('/') + '/chat/completions'

function Read-Req([string]$body) {
  if ($body -match 'available,\s*\$([0-9.]+)\s+required') { return [double]$Matches[1] }
  return $null
}
function Probe([string]$json) {
  $f = Join-Path $outDir 'rt.json'; $o = Join-Path $outDir 'rt.b'
  [IO.File]::WriteAllText($f,$json,$enc)
  & curl.exe -sS -o $o -w '%{http_code}' --connect-timeout 6 --max-time 45 `
      -H ('Authorization: Bearer ' + $k) -H 'Content-Type: application/json' `
      --data-binary ('@' + $f) $chatUrl | Out-Null
  $t = ''
  if (Test-Path $o) { $t = [IO.File]::ReadAllText($o,[Text.Encoding]::UTF8); Remove-Item $o -Force }
  Remove-Item $f -Force
  return $t
}

$models = @('anthropic/claude-opus-5.5','anthropic/claude-sonnet-5.5','anthropic/claude-sonnet-5',
            'anthropic/claude-opus-5','anthropic/claude-opus-4.8','anthropic/claude-opus-4.7',
            'anthropic/claude-fable-5.1','openai/gpt-6.2-sol','openai/gpt-6.1-sol','openai/gpt-6-sol',
            'openai/gpt-5.6-sol','openai/gpt-6-Astra','MoonshotAI/kimi-k3','MoonshotAI/kimi-k3.1-pro')
$pad = ('pad word ' * 20000)
Write-Output ('model                              out$/1M   in$/1M    floor128k$  tok_per_req_bytes')
foreach ($m in $models) {
  $j0 = '{"model":"' + $m + '","messages":[{"role":"user","content":"x"}],"max_tokens":800000}'
  $j1 = '{"model":"' + $m + '","messages":[{"role":"user","content":"x ' + $pad + '"}],"max_tokens":800000}'
  $r0 = Read-Req (Probe $j0)
  $r1 = Read-Req (Probe $j1)
  if ($null -eq $r0 -or $null -eq $r1) { Write-Output ('{0,-34} probe failed ({1} / {2})' -f $m,$r0,$r1); continue }
  $outRate = (($r0 - 0.000003) / 800000) * 1e6
  $dTokBytes = $j1.Length - $j0.Length
  # tokens delta inferred at the upstream's own ~4 bytes/token; solve input rate from the money delta
  $inRate = (($r1 - $r0) / ($dTokBytes / 4.0)) * 1e6
  $floor = $outRate * 128000 / 1e6
  Write-Output ('{0,-34} {1,7:N2}  {2,7:N2}  {3,9:N3}  {4,5} -> {5:N0} tok' -f $m,$outRate,$inRate,$floor,$dTokBytes,($dTokBytes/4))
}
