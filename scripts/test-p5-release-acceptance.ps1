[CmdletBinding()]
param(
    [string]$ReleaseDirectory = 'release/v0.1.0',
    [string]$Version = '0.1.0',
    [string]$ExpectedCommit = '',
    [string]$AntivirusEvidence = '',
    [string]$Windows10Evidence = '',
    [ValidateSet('internal-runnable', 'public-signed')]
    [string]$DistributionTarget = 'internal-runnable',
    [string]$Output = 'release/p5-release-acceptance.json'
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$releaseRoot = [IO.Path]::GetFullPath([IO.Path]::Combine($repoRoot, $ReleaseDirectory))
$outputPath = [IO.Path]::GetFullPath([IO.Path]::Combine($repoRoot, $Output))
if (-not $releaseRoot.StartsWith($repoRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw 'Release directory must be inside the repository.'
}
if (-not $outputPath.StartsWith($repoRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw 'Acceptance output must be inside the repository.'
}
if ([string]::IsNullOrWhiteSpace($ExpectedCommit)) {
    $ExpectedCommit = (& git -C $repoRoot rev-parse HEAD).Trim()
    if ($LASTEXITCODE -ne 0) { throw 'Unable to resolve expected Git commit.' }
}
if ($ExpectedCommit -notmatch '^[0-9a-f]{40}$') { throw 'ExpectedCommit must be a full lowercase Git commit.' }

$checks = New-Object System.Collections.Generic.List[object]
$blockers = New-Object System.Collections.Generic.List[string]
$warnings = New-Object System.Collections.Generic.List[string]
function Add-Check {
    param([string]$Name, [bool]$Passed, [string]$Detail, [string]$Blocker = '', [bool]$Required = $true)
    $checks.Add([ordered]@{ name = $Name; required = $Required; passed = $Passed; detail = $Detail })
    if (-not $Passed -and -not [string]::IsNullOrWhiteSpace($Blocker) -and -not $blockers.Contains($Blocker)) {
        if ($Required) {
            $blockers.Add($Blocker)
        } elseif (-not $warnings.Contains($Blocker)) {
            $warnings.Add($Blocker)
        }
    }
}
function Get-SHA256Lower {
    param([Parameter(Mandatory)][string]$Path)
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}
function Read-Evidence {
    param([string]$Path, [string]$Schema, [string]$MissingBlocker, [bool]$Required = $true)
    if ([string]::IsNullOrWhiteSpace($Path)) {
        Add-Check -Name $MissingBlocker -Passed $false -Detail 'evidence path was not supplied' -Blocker $MissingBlocker -Required $Required
        return $null
    }
    $absolute = [IO.Path]::GetFullPath([IO.Path]::Combine($repoRoot, $Path))
    if (-not [IO.File]::Exists($absolute)) {
        Add-Check -Name $MissingBlocker -Passed $false -Detail 'evidence file is missing' -Blocker $MissingBlocker -Required $Required
        return $null
    }
    $evidence = Get-Content -LiteralPath $absolute -Raw | ConvertFrom-Json
    if ($evidence.schema -ne $Schema) {
        Add-Check -Name $MissingBlocker -Passed $false -Detail 'evidence schema is invalid' -Blocker $MissingBlocker -Required $Required
        return $null
    }
    return $evidence
}

$manifestPath = [IO.Path]::Combine($releaseRoot, 'release-manifest.json')
if (-not [IO.File]::Exists($manifestPath)) { throw 'release-manifest.json is missing.' }
$manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
$manifestHash = Get-SHA256Lower -Path $manifestPath
Add-Check 'manifest-schema' ($manifest.schema -eq 'douyinlive-release-manifest/v1') ([string]$manifest.schema) 'MANIFEST_INVALID'
Add-Check 'manifest-version' ($manifest.version -eq $Version) ([string]$manifest.version) 'VERSION_MISMATCH'
Add-Check 'manifest-commit' ($manifest.gitCommit -eq $ExpectedCommit) ([string]$manifest.gitCommit) 'COMMIT_MISMATCH'
Add-Check 'manifest-clean-reproducible' (($manifest.dirty -eq $false) -and ($manifest.reproducible -eq $true)) ("dirty=$($manifest.dirty); reproducible=$($manifest.reproducible)") 'BUILD_NOT_CLEAN_REPRODUCIBLE'
Add-Check 'manifest-platform' ($manifest.platform -eq 'windows/amd64') ([string]$manifest.platform) 'PLATFORM_MISMATCH'
Add-Check 'sensitive-scan' ($manifest.sensitiveScan.findingCount -eq 0) ("findings=$($manifest.sensitiveScan.findingCount)") 'SENSITIVE_SCAN_FAILED'

$requiredFiles = @(
    "douyin-live-desktop-$Version-windows-amd64.exe",
    "douyin-live-dbrollback-$Version-windows-amd64.exe",
    "douyin-live-desktop-$Version-windows-amd64-installer.exe",
    'release-manifest.json', 'sbom.spdx.json', 'licenses.json', 'THIRD-PARTY-NOTICES.txt',
    'ffmpeg-windows-amd64.lock.json', 'webview2-bootstrapper-windows.lock.json',
    'sensitive-scan.json', 'LICENSE.txt', 'INSTALLATION.md',
    'USER-GUIDE.md', 'PRIVACY.md', 'KNOWN-LIMITATIONS.md', 'RELEASE-CHECKLIST.md'
)
foreach ($name in $requiredFiles) {
    Add-Check ("required-file:" + $name) ([IO.File]::Exists([IO.Path]::Combine($releaseRoot, $name))) $name 'RELEASE_FILE_MISSING'
}

$manifestEntries = @($manifest.files)
if ($manifestEntries.Count -eq 0 -or $manifestEntries.Count -gt 256) {
    Add-Check 'manifest-file-count-bound' $false ("registered=$($manifestEntries.Count)") 'MANIFEST_INVALID'
}
$seen = @{}
foreach ($entry in $manifestEntries) {
    $name = [string]$entry.path
    $safeName = [IO.Path]::GetFileName($name)
    if ($name -ne $safeName -or $name -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$' -or $seen.ContainsKey($name)) {
        Add-Check 'manifest-file-name-safety' $false 'unsafe or duplicate manifest file name' 'MANIFEST_INVALID'
        continue
    }
    $seen[$name] = $true
    $path = [IO.Path]::Combine($releaseRoot, $name)
    if (-not [IO.File]::Exists($path)) {
        Add-Check ("manifest-file:" + $name) $false 'file is missing' 'MANIFEST_FILE_MISMATCH'
        continue
    }
    $info = Get-Item -LiteralPath $path
    $hash = Get-SHA256Lower -Path $path
    $matches = ($info.Length -eq [int64]$entry.size) -and ($hash -eq [string]$entry.sha256)
    Add-Check ("manifest-file:" + $name) $matches ("size=$($info.Length); sha256=$hash") 'MANIFEST_FILE_MISMATCH'
}
foreach ($name in $requiredFiles | Where-Object { $_ -ne 'release-manifest.json' }) {
    Add-Check ("manifest-registers:" + $name) $seen.ContainsKey($name) $name 'MANIFEST_FILE_MISMATCH'
}

$actualNames = @(Get-ChildItem -LiteralPath $releaseRoot -File | ForEach-Object Name)
foreach ($name in $actualNames | Where-Object { $_ -ne 'release-manifest.json' }) {
    Add-Check ("manifest-exact-set:" + $name) $seen.ContainsKey($name) $name 'MANIFEST_FILE_MISMATCH'
}
$exactCount = $actualNames.Count -eq ($manifestEntries.Count + 1)
Add-Check 'manifest-exact-file-count' $exactCount ("actual=$($actualNames.Count); registered=$($manifestEntries.Count)") 'MANIFEST_FILE_MISMATCH'

$publicSigned = $DistributionTarget -eq 'public-signed'
$signatureFiles = @(
    "douyin-live-desktop-$Version-windows-amd64.exe",
    "douyin-live-dbrollback-$Version-windows-amd64.exe",
    "douyin-live-desktop-$Version-windows-amd64-installer.exe"
)
foreach ($name in $signatureFiles) {
    $path = [IO.Path]::Combine($releaseRoot, $name)
    if (-not [IO.File]::Exists($path)) { continue }
    $signature = Get-AuthenticodeSignature -LiteralPath $path
    $valid = $signature.Status -eq [System.Management.Automation.SignatureStatus]::Valid
    $timestamped = $valid -and $null -ne $signature.TimeStamperCertificate
    Add-Check ("signature:" + $name) ($valid -and $timestamped) ("status=$($signature.Status); timestamped=$timestamped") 'CODE_SIGNING_MISSING' $publicSigned
}

$antivirus = Read-Evidence -Path $AntivirusEvidence -Schema 'douyinlive-antivirus-evidence/v1' -MissingBlocker 'ANTIVIRUS_EVIDENCE_MISSING' -Required $publicSigned
if ($null -ne $antivirus) {
    $engines = @($antivirus.engines)
    $engineNames = @{}
    $enginesValid = $engines.Count -ge 2
    foreach ($engine in $engines) {
        $name = [string]$engine.name
        $validTime = [DateTimeOffset]::MinValue
        $timeValid = [DateTimeOffset]::TryParse([string]$engine.scannedAtUtc, [ref]$validTime)
        $engineValid = -not [string]::IsNullOrWhiteSpace($name) -and -not $engineNames.ContainsKey($name) -and -not [string]::IsNullOrWhiteSpace([string]$engine.engineVersion) -and -not [string]::IsNullOrWhiteSpace([string]$engine.signatureVersion) -and $timeValid -and ($engine.result -eq 'passed')
        if (-not $engineValid) { $enginesValid = $false } else { $engineNames[$name] = $true }
    }
    $passed = ($antivirus.result -eq 'passed') -and ($antivirus.findingCount -eq 0) -and ($antivirus.targetManifestSHA256 -eq $manifestHash) -and $enginesValid
    Add-Check 'antivirus-evidence' $passed ("engines=$(@($antivirus.engines).Count); findings=$($antivirus.findingCount)") 'ANTIVIRUS_EVIDENCE_INVALID' $publicSigned
}
$windows10 = Read-Evidence -Path $Windows10Evidence -Schema 'douyinlive-windows10-evidence/v1' -MissingBlocker 'WINDOWS10_EVIDENCE_MISSING' -Required $publicSigned
if ($null -ne $windows10) {
    $passed = ($windows10.result -eq 'passed') -and ($windows10.targetManifestSHA256 -eq $manifestHash) -and ($windows10.architecture -eq 'amd64') -and ($windows10.build -match '^1[0-9]{4}$')
    Add-Check 'windows10-evidence' $passed ("build=$($windows10.build); result=$($windows10.result)") 'WINDOWS10_EVIDENCE_INVALID' $publicSigned
}

[string[]]$blockerArray = $blockers
[string[]]$warningArray = $warnings
[object[]]$checkArray = $checks
$report = [ordered]@{
    schema = 'P5-ACC-001/v2'
    distributionTarget = $DistributionTarget
    version = $Version
    gitCommit = $ExpectedCommit
    manifestSHA256 = $manifestHash
    passed = $blockers.Count -eq 0
    blockerCodes = $blockerArray
    warningCodes = $warningArray
    checks = $checkArray
}
$outputDirectory = [IO.Path]::GetDirectoryName($outputPath)
[IO.Directory]::CreateDirectory($outputDirectory) | Out-Null
$temporary = $outputPath + '.tmp-' + [Guid]::NewGuid().ToString('N')
$utf8 = New-Object Text.UTF8Encoding($false)
[IO.File]::WriteAllText($temporary, (($report | ConvertTo-Json -Depth 8) + "`n"), $utf8)
Move-Item -LiteralPath $temporary -Destination $outputPath -Force
if ($blockers.Count -ne 0) {
    [Console]::WriteLine('P5_RELEASE_ACCEPTANCE_BLOCKED')
    [Console]::WriteLine(('blockers=' + ($blockers -join ',')))
    exit 3
}
[Console]::WriteLine('P5_RELEASE_ACCEPTANCE_PASSED')
[Console]::WriteLine(('distributionTarget=' + $DistributionTarget))
[Console]::WriteLine(('manifestSHA256=' + $manifestHash))
if ($warnings.Count -ne 0) {
    [Console]::WriteLine(('warnings=' + ($warnings -join ',')))
}
