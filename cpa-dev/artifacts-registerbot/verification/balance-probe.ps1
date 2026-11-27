# ASCII only. Probe the freeai balance gate with the earliest api-key from config.yaml.
$ErrorActionPreference = 'Stop'
$cfgPath = Join-Path ((Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path) 'config.yaml'
$outDir = Join-Path $PSScriptRoot 'balance-probe'
New-Item -ItemType Directory -Force -Path $outDir | Out-Null

$lines = Get-Content -Path $cfgPath -Encoding UTF8
$baseUrl = ''
$keys = @()
foreach ($l in $lines) {
  if ($l -match 'base-url:\s*"(https?://[^"]+)"') { $baseUrl = $Matches[1] }
  if ($l -match 'api-key:\s*"(sk-[^"]+)"') { $keys += $Matches[1] }
}
Write-Output ('base_url  = ' + $baseUrl)
Write-Output ('keys      = ' + $keys.Count)
$k = $keys[0]
Write-Output ('using key[0] = ' + $k.Substring(0,7) + '...' + $k.Substring($k.Length-4))

$proxy = 'http://127.0.0.1:12450'
$url = $baseUrl.TrimEnd('/') + '/chat/completions'

function Invoke-Chat([string]$tag, [string]$userText, [int]$maxTokens) {
  $body = '{"model":"anthropic/claude-opus-5.5","messages":[{"role":"user","content":"' + $userText + '"}],"max_tokens":' + $maxTokens + '}'
  $reqFile = Join-Path $outDir ($tag + '.req.json')
  $respFile = Join-Path $outDir ($tag + '.resp.txt')
  $headFile = Join-Path $outDir ($tag + '.head.txt')
  [IO.File]::WriteAllText($reqFile, $body, (New-Object Text.UTF8Encoding($false)))
  $code = & curl.exe -s -o $respFile -D $headFile -w '%{http_code}' --proxy $proxy `
        --connect-timeout 8 --max-time 90 `
        -H ('Authorization: Bearer ' + $script:k) -H 'Content-Type: application/json' `
        --data-binary ('@' + $reqFile) $url
  $text = [IO.File]::ReadAllText($respFile, [Text.Encoding]::UTF8)
  if ($text.Length -gt 700) { $text = $text.Substring(0,700) + ' ...[TRUNC]' }
  Write-Output ''
  Write-Output ('--- ' + $tag + '  req_bytes=' + $body.Length + '  http=' + $code)
  Write-Output ('    ' + $text)
  foreach ($h in (Get-Content $headFile -Encoding UTF8)) {
    if ($h -match '^(x-ratelimit|retry-after|openai-|x-usage|x-balance|x-request-id|credit|remaining)') {
      Write-Output ('    HDR ' + $h.Trim())
    }
  }
}

Invoke-Chat 't1_tiny'   'Hi' 8
Invoke-Chat 't2_small'  (('say ok. ' + ('pad word ' * 60))) 16
Invoke-Chat 't3_medium' (('say ok. ' + ('pad word ' * 900))) 64
Invoke-Chat 't4_large'  (('say ok. ' + ('pad word ' * 6000))) 4096
