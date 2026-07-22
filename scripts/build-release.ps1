[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$')]
    [string]$Version,
    [string]$Output = 'release',
    [string]$Source = 'local-release',
    [switch]$AllowDirty,
    [switch]$VerifyOnly
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location -LiteralPath $repoRoot
& where.exe go
if ($LASTEXITCODE -ne 0) { throw 'Go toolchain is unavailable in this session.' }

$arguments = @('run', './cmd/releasebuilder', '-version', $Version, '-output', $Output, '-source', $Source)
if ($AllowDirty) { $arguments += '-allow-dirty' }
if ($VerifyOnly) { $arguments += '-verify-only' }
& go @arguments
if ($LASTEXITCODE -ne 0) {
    throw "Release builder failed with exit code $LASTEXITCODE."
}
