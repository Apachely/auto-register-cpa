# ASCII only. Probes the freeai provider balance gate with the earliest api key.
$ErrorActionPreference = 'Stop'
$cfgPath = Join-Path ((Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path) 'config.yaml'
$outDir = Join-Path $PSScriptRoot 'balance-probe'
New-Item -ItemType Directory -Force -Path $outDir | Out-Null

$lines = Get-Content -Path $cfgPath -Encoding UTF8
$baseUrl = ''
$keys = @()
$proxies = @()
$inCompat = $false
foreach ($l in $lines) {
  if ($l -match '^\s*aiCompatibility\s*:' -or $l -match '^\s*ai-compatibility\s*:' -or $l -match '^\s*aiCompatibility') { $inCompat = $true }
  if ($l -match 'base-url\s*:\s*"?(https?://[^"\s]+)"?') { $baseUrl = $Matches[1] }
  if ($l -match 'key\s*:\s*"?(sk-[A-Za-z0-9_\-]+)"?') { $keys += $Matches[1] }
  if ($l -match 'proxy-url\s*:\s*"?(https?://[^"\s]+)"?') { $proxies += $Matches[1] }
}
Write-Output ("base_url = " + $baseUrl)
Write-Output ("keys_found = " + $keys.Count + "  proxies_found = " + $proxies.Count)
for ($i = 0; $i -lt $keys.Count; $i++) {
  $k = $keys[$i]
  $mask = $k.Substring(0, 7) + '...' + $k.Substring($k.Length - 4)
  $p = ''
  if ($i -lt $proxies.Count) { $p = $proxies[$i] }
  Write-Output ("[" + $i + "] " + $mask + "   proxy=" + $p)
}

$key = $keys[0]
$proxy = 'http://127.0.0.1:12450'
Write-Output ''
Write-Output ('USING key[0] proxy=' + $proxy)

function Invoke-Chat([string]$tag, [string]$userText, [string]$maxTokens) {
  $bodyObj = @{
    model    = 'anthropic/claude-opus-5.5'
    messages = @(@{ role = 'user'; content = $userText })
    max_tokens = [int]$maxTokens
  }
  $json = $bodyObj | ConvertTo-Json -Depth 6 -Compress
  $bodyFile = Join-Path $outDir ($tag + '.req.json')
  [IO.File]::WriteAllText($bodyFile, $json, (New-Object Text.UTF8Encoding($false)))
  $respFile = Join-Path $outDir ($tag + '.resp.txt')
  $headFile = Join-Path $outDir ($tag + '.head.txt')
  $url = $baseUrl.TrimEnd('/') + '/chat/completions'
  $code = & curl.exe -s -o $respFile -D $headFile -w '%{http_code}' `
    --proxy $proxy --connect-timeout 8 --max-time 60 `
    -H ('Authorization: Bearer ' + $key) -H 'Content-Type: application/json' `
    --data-binary ('@' + $bodyFile) $url
  $bytes = [IO.File]::ReadAllBytes($respFile)
  $text = [Text.Encoding]::UTF8.GetString($bytes)
  if ($text.Length -gt 900) { $text = $text.Substring(0, 900) + ' ...[truncated]' }
  Write-Output ''
  Write-Output ('--- ' + $tag + '  req_bytes=' + $json.Length + '  http=' + $code)
  Write-Output ('    resp: ' + $text)
  $heads = Get-Content $headFile -Encoding UTF8
  foreach ($h in $heads) {
    if ($h -match '^(x-ratelimit|x-request-id|retry-after|openai-|x-usage|x-balance|credit|remaining)') { Write-Output ('    hdr ' + $h.Trim()) }
  }
}

Invoke-Chat 't01_tiny'      'Hi' '8'
Invoke-Chat 't02_small'     ('Repeat the word apple. ' + ('pad word ' * 40))  '16'
Invoke-Chat 't03_medium'    (('Explain briefly. ' + ('filler token ' * 400))) '64'
Invoke-Chat 't04_large'     (('Explain briefly. ' + ('filler token ' * 4000))) '256'
