[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$')]
    [string]$Version,
    [string]$Output = 'release',
    [string]$Source = 'local-release',
    [string]$WebView2Bootstrapper,
    [switch]$AllowDirty,
    [switch]$VerifyOnly
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location -LiteralPath $repoRoot
& where.exe go
if ($LASTEXITCODE -ne 0) { throw 'Go toolchain is unavailable in this session.' }

$webView2LockPath = [IO.Path]::Combine($repoRoot, 'build', 'webview2-bootstrapper-windows.lock.json')
$webView2Lock = Get-Content -LiteralPath $webView2LockPath -Raw | ConvertFrom-Json
if (-not $VerifyOnly) {
    if ([string]::IsNullOrWhiteSpace($WebView2Bootstrapper)) {
        $cacheRoot = [IO.Path]::Combine([IO.Path]::GetTempPath(), 'DouyinLiveBuildDependencies', 'WebView2')
        [IO.Directory]::CreateDirectory($cacheRoot) | Out-Null
        $WebView2Bootstrapper = [IO.Path]::Combine($cacheRoot, "MicrosoftEdgeWebview2Setup-$($webView2Lock.sha256).exe")
        $needsDownload = -not [IO.File]::Exists($WebView2Bootstrapper)
        if (-not $needsDownload) {
            $cachedHash = (Get-FileHash -LiteralPath $WebView2Bootstrapper -Algorithm SHA256).Hash.ToLowerInvariant()
            $cachedSize = (Get-Item -LiteralPath $WebView2Bootstrapper).Length
            $needsDownload = $cachedHash -ne $webView2Lock.sha256 -or $cachedSize -ne [int64]$webView2Lock.size
        }
        if ($needsDownload) {
            Invoke-WebRequest -UseBasicParsing -Uri $webView2Lock.url -OutFile $WebView2Bootstrapper
        }
    }
    $WebView2Bootstrapper = [IO.Path]::GetFullPath($WebView2Bootstrapper)
    if (-not [IO.File]::Exists($WebView2Bootstrapper)) {
        throw 'WebView2 Evergreen Bootstrapper is unavailable.'
    }
    $webView2Hash = (Get-FileHash -LiteralPath $WebView2Bootstrapper -Algorithm SHA256).Hash.ToLowerInvariant()
    $webView2Size = (Get-Item -LiteralPath $WebView2Bootstrapper).Length
    if ($webView2Hash -ne $webView2Lock.sha256 -or $webView2Size -ne [int64]$webView2Lock.size) {
        throw 'WebView2 Evergreen Bootstrapper does not match the repository lock.'
    }
    $webView2Signature = Get-AuthenticodeSignature -LiteralPath $WebView2Bootstrapper
    if ($webView2Signature.Status -ne 'Valid' -or
        $null -eq $webView2Signature.SignerCertificate -or
        $webView2Signature.SignerCertificate.Subject -ne $webView2Lock.authenticodeSigner) {
        throw 'WebView2 Evergreen Bootstrapper Authenticode identity is invalid.'
    }
}

$arguments = @('run', './cmd/releasebuilder', '-version', $Version, '-output', $Output, '-source', $Source)
if (-not $VerifyOnly) { $arguments += @('-webview2-bootstrapper', $WebView2Bootstrapper) }
if ($AllowDirty) { $arguments += '-allow-dirty' }
if ($VerifyOnly) { $arguments += '-verify-only' }
& go @arguments
if ($LASTEXITCODE -ne 0) {
    throw "Release builder failed with exit code $LASTEXITCODE."
}
