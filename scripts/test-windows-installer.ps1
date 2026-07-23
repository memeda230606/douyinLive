[CmdletBinding()]
param(
    [string]$ReleaseDirectory = 'release/v0.1.0',
    [string]$CurrentVersion = '0.1.0',
    [string]$WebView2Bootstrapper
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$releaseRoot = [IO.Path]::GetFullPath([IO.Path]::Combine($repoRoot, $ReleaseDirectory))
$installerScript = [IO.Path]::Combine($repoRoot, 'cmd', 'desktop', 'build', 'windows', 'installer', 'project.nsi')
$lockPath = [IO.Path]::Combine($releaseRoot, 'ffmpeg-windows-amd64.lock.json')
$lock = Get-Content -LiteralPath $lockPath -Raw | ConvertFrom-Json
$webView2LockPath = [IO.Path]::Combine($releaseRoot, 'webview2-bootstrapper-windows.lock.json')
$webView2Lock = Get-Content -LiteralPath $webView2LockPath -Raw | ConvertFrom-Json
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
if (-not [IO.File]::Exists($WebView2Bootstrapper) -or
    (Get-FileHash -LiteralPath $WebView2Bootstrapper -Algorithm SHA256).Hash.ToLowerInvariant() -ne $webView2Lock.sha256 -or
    (Get-Item -LiteralPath $WebView2Bootstrapper).Length -ne [int64]$webView2Lock.size) {
    throw 'Matrix WebView2 bootstrapper does not match the release lock.'
}
$ffmpegPath = (Get-Command ffmpeg.exe -ErrorAction Stop).Source
$ffprobePath = (Get-Command ffprobe.exe -ErrorAction Stop).Source
$makensisPath = (Get-Command makensis.exe -ErrorAction Stop).Source
$desktopPath = [IO.Path]::Combine($releaseRoot, "douyin-live-desktop-$CurrentVersion-windows-amd64.exe")
$rollbackPath = [IO.Path]::Combine($releaseRoot, "douyin-live-dbrollback-$CurrentVersion-windows-amd64.exe")

foreach ($required in @(
    $installerScript, $desktopPath, $rollbackPath, $ffmpegPath, $ffprobePath, $WebView2Bootstrapper,
    [IO.Path]::Combine($releaseRoot, 'LICENSE.txt'),
    [IO.Path]::Combine($releaseRoot, 'licenses.json'),
    [IO.Path]::Combine($releaseRoot, 'THIRD-PARTY-NOTICES.txt'),
    [IO.Path]::Combine($releaseRoot, 'sbom.spdx.json'),
    $lockPath, $webView2LockPath, [IO.Path]::Combine($releaseRoot, 'INSTALLATION.md'),
    [IO.Path]::Combine($releaseRoot, 'USER-GUIDE.md'), [IO.Path]::Combine($releaseRoot, 'PRIVACY.md'),
    [IO.Path]::Combine($releaseRoot, 'KNOWN-LIMITATIONS.md'), [IO.Path]::Combine($releaseRoot, 'RELEASE-CHECKLIST.md')
)) {
    if (-not [IO.File]::Exists($required)) { throw "Required matrix input is missing: $required" }
}
if ((Get-FileHash -LiteralPath $ffmpegPath -Algorithm SHA256).Hash.ToLowerInvariant() -ne $lock.binaries.'ffmpeg.exe') {
    throw 'Matrix FFmpeg checksum does not match the release lock.'
}
if ((Get-FileHash -LiteralPath $ffprobePath -Algorithm SHA256).Hash.ToLowerInvariant() -ne $lock.binaries.'ffprobe.exe') {
    throw 'Matrix ffprobe checksum does not match the release lock.'
}

$nonce = [Guid]::NewGuid().ToString('N')
$testRoot = [IO.Path]::Combine([IO.Path]::GetTempPath(), "DouyinLiveInstallerMatrix-$nonce")
$installRoot = [IO.Path]::Combine($testRoot, 'installed')
$dataRoot = [IO.Path]::Combine($testRoot, 'data')
$outputRoot = [IO.Path]::Combine($testRoot, 'packages')
$missingRoot = [IO.Path]::Combine($testRoot, 'missing')
$productName = "DouyinLiveMatrix$nonce"
$uninstallKeyName = "DouyinLiveMatrix$nonce"
$uninstallRegistryPath = "HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\$uninstallKeyName"
$uninstallRegistrySubKey = "Software\Microsoft\Windows\CurrentVersion\Uninstall\$uninstallKeyName"
$oldInstaller = [IO.Path]::Combine($outputRoot, 'old-installer.exe')
$currentInstaller = [IO.Path]::Combine($outputRoot, 'current-installer.exe')
$autoWebView2Installer = [IO.Path]::Combine($outputRoot, 'auto-webview2-installer.exe')
$failedWebView2Installer = [IO.Path]::Combine($outputRoot, 'failed-webview2-installer.exe')

function Invoke-NativeChecked {
    param([Parameter(Mandatory)][string]$FilePath, [Parameter(Mandatory)][string[]]$Arguments)
    & $FilePath @Arguments
    if ($LASTEXITCODE -ne 0) { throw "$FilePath failed with exit code $LASTEXITCODE." }
}

function Invoke-BoundedProcess {
    param(
        [Parameter(Mandatory)][string]$FilePath,
        [Parameter(Mandatory)][string[]]$Arguments,
        [int]$TimeoutSeconds = 120
    )
    $startInfo = New-Object System.Diagnostics.ProcessStartInfo
    $startInfo.FileName = $FilePath
    $startInfo.UseShellExecute = $false
    $quoted = foreach ($argument in $Arguments) {
        if ($argument -match '[\s"]') { '"' + $argument.Replace('"', '\"') + '"' } else { $argument }
    }
    $startInfo.Arguments = $quoted -join ' '
    $process = New-Object System.Diagnostics.Process
    $process.StartInfo = $startInfo
    if (-not $process.Start()) { throw "Failed to start $FilePath." }
    try {
        if (-not $process.WaitForExit($TimeoutSeconds * 1000)) {
            try { $process.Kill() } catch {}
            throw "$FilePath exceeded $TimeoutSeconds seconds."
        }
        $process.WaitForExit()
        return [int]$process.ExitCode
    } finally {
        $process.Dispose()
    }
}

function New-MatrixInstaller {
    param(
        [Parameter(Mandatory)][string]$Version,
        [Parameter(Mandatory)][string]$Output,
        [switch]$ForceMissingWebView2,
        [switch]$AssumeWebView2InstallSuccess,
        [int]$ForcedWebView2InstallExit = -1
    )
    $defines = [ordered]@{
        ARG_WAILS_AMD64_BINARY = $desktopPath
        ARG_FFMPEG_BINARY = $ffmpegPath
        ARG_FFPROBE_BINARY = $ffprobePath
        ARG_WEBVIEW2_BOOTSTRAPPER = $WebView2Bootstrapper
        ARG_WEBVIEW2_LOCK = $webView2LockPath
        ARG_DBROLLBACK_BINARY = $rollbackPath
        ARG_LICENSE_FILE = [IO.Path]::Combine($releaseRoot, 'LICENSE.txt')
        ARG_LICENSE_MANIFEST = [IO.Path]::Combine($releaseRoot, 'licenses.json')
        ARG_NOTICES_FILE = [IO.Path]::Combine($releaseRoot, 'THIRD-PARTY-NOTICES.txt')
        ARG_SBOM_FILE = [IO.Path]::Combine($releaseRoot, 'sbom.spdx.json')
        ARG_FFMPEG_LOCK = $lockPath
        ARG_INSTALLATION_GUIDE = [IO.Path]::Combine($releaseRoot, 'INSTALLATION.md')
        ARG_USER_GUIDE = [IO.Path]::Combine($releaseRoot, 'USER-GUIDE.md')
        ARG_PRIVACY_GUIDE = [IO.Path]::Combine($releaseRoot, 'PRIVACY.md')
        ARG_LIMITATIONS_GUIDE = [IO.Path]::Combine($releaseRoot, 'KNOWN-LIMITATIONS.md')
        ARG_RELEASE_CHECKLIST = [IO.Path]::Combine($releaseRoot, 'RELEASE-CHECKLIST.md')
        ARG_INSTALLER_OUTPUT = $Output
        DOUYINLIVE_DATA_ROOT = $dataRoot
        INFO_PROJECTNAME = $productName
        INFO_COMPANYNAME = 'DouyinLiveMatrix'
        INFO_PRODUCTNAME = $productName
        INFO_PRODUCTVERSION = $Version
        PRODUCT_EXECUTABLE = 'douyin-live-desktop.exe'
        UNINST_KEY_NAME = $uninstallKeyName
    }
    $arguments = @('/WX', '/INPUTCHARSET', 'UTF8')
    foreach ($entry in $defines.GetEnumerator()) { $arguments += "-D$($entry.Key)=$($entry.Value)" }
    if ($ForceMissingWebView2) { $arguments += '-DDOUYINLIVE_FORCE_WEBVIEW2_MISSING=1' }
    if ($AssumeWebView2InstallSuccess) { $arguments += '-DDOUYINLIVE_WEBVIEW2_INSTALL_TEST_SUCCESS=1' }
    if ($ForcedWebView2InstallExit -ge 0) { $arguments += "-DDOUYINLIVE_WEBVIEW2_INSTALL_TEST_EXIT=$ForcedWebView2InstallExit" }
    $arguments += '-DDOUYINLIVE_MANAGED_PURGE_TEST=1'
    $arguments += $installerScript
    Invoke-NativeChecked -FilePath $makensisPath -Arguments $arguments
    if (-not [IO.File]::Exists($Output)) { throw "NSIS did not create $Output." }
}

function Assert-InstalledPayload {
    param([Parameter(Mandatory)][string]$ExpectedVersion)
    foreach ($relative in @(
        'douyin-live-desktop.exe', 'douyin-live-dbrollback.exe', 'uninstall.exe',
        'ffmpeg\ffmpeg.exe', 'ffmpeg\ffprobe.exe', 'licenses\LICENSE.txt',
        'licenses\licenses.json', 'licenses\THIRD-PARTY-NOTICES.txt',
        'licenses\sbom.spdx.json', 'licenses\ffmpeg-windows-amd64.lock.json',
        'licenses\webview2-bootstrapper-windows.lock.json',
        'licenses\INSTALLATION.md', 'licenses\USER-GUIDE.md', 'licenses\PRIVACY.md',
        'licenses\KNOWN-LIMITATIONS.md', 'licenses\RELEASE-CHECKLIST.md'
    )) {
        if (-not [IO.File]::Exists([IO.Path]::Combine($installRoot, $relative))) {
            throw "Installed payload is missing $relative."
        }
    }
    $displayVersion = (Get-ItemProperty -LiteralPath $uninstallRegistryPath -Name DisplayVersion).DisplayVersion
    if ($displayVersion -ne $ExpectedVersion) { throw "DisplayVersion is $displayVersion, expected $ExpectedVersion." }
    if ((Get-FileHash -LiteralPath ([IO.Path]::Combine($installRoot, 'ffmpeg', 'ffmpeg.exe')) -Algorithm SHA256).Hash.ToLowerInvariant() -ne $lock.binaries.'ffmpeg.exe') {
        throw 'Installed FFmpeg checksum mismatch.'
    }
}

function Invoke-Install {
    param([Parameter(Mandatory)][string]$Installer, [Parameter(Mandatory)][string]$Target)
    $exitCode = Invoke-BoundedProcess -FilePath $Installer -Arguments @('/S', "/D=$Target")
    if ($exitCode -ne 0) { throw "Installer exited with $exitCode." }
}

function Test-UninstallKeyExists {
    foreach ($view in @([Microsoft.Win32.RegistryView]::Registry64, [Microsoft.Win32.RegistryView]::Registry32)) {
        $baseKey = [Microsoft.Win32.RegistryKey]::OpenBaseKey([Microsoft.Win32.RegistryHive]::CurrentUser, $view)
        try {
            $key = $baseKey.OpenSubKey($uninstallRegistrySubKey)
            if ($null -ne $key) {
                $key.Dispose()
                return $true
            }
        } finally {
            $baseKey.Dispose()
        }
    }
    return $false
}

function Wait-UninstallCleanup {
    $deadline = [DateTime]::UtcNow.AddSeconds(15)
    do {
        if (-not [IO.Directory]::Exists($installRoot) -and -not (Test-UninstallKeyExists)) { return }
        Start-Sleep -Milliseconds 100
    } while ([DateTime]::UtcNow -lt $deadline)
    throw 'Uninstaller cleanup did not converge within 15 seconds.'
}

$passed = New-Object System.Collections.Generic.List[string]
try {
    [IO.Directory]::CreateDirectory($outputRoot) | Out-Null
    New-MatrixInstaller -Version '0.0.9' -Output $oldInstaller
    New-MatrixInstaller -Version $CurrentVersion -Output $currentInstaller
    New-MatrixInstaller -Version $CurrentVersion -Output $autoWebView2Installer `
        -ForceMissingWebView2 -AssumeWebView2InstallSuccess -ForcedWebView2InstallExit 0
    New-MatrixInstaller -Version $CurrentVersion -Output $failedWebView2Installer `
        -ForceMissingWebView2 -ForcedWebView2InstallExit 23

    Invoke-Install -Installer $oldInstaller -Target $installRoot
    Assert-InstalledPayload -ExpectedVersion '0.0.9'
    $passed.Add('fresh-install')

    [IO.Directory]::CreateDirectory($dataRoot) | Out-Null
    $sentinel = [IO.Path]::Combine($dataRoot, 'retention-sentinel.txt')
    [IO.File]::WriteAllText($sentinel, 'matrix-data')
    Invoke-Install -Installer $currentInstaller -Target $installRoot
    Assert-InstalledPayload -ExpectedVersion $CurrentVersion
    if (-not [IO.File]::Exists($sentinel)) { throw 'Upgrade removed application data.' }
    $passed.Add('in-place-upgrade')

    $uninstaller = [IO.Path]::Combine($installRoot, 'uninstall.exe')
    $exitCode = Invoke-BoundedProcess -FilePath $uninstaller -Arguments @('/S')
    Wait-UninstallCleanup
    $sentinelExists = [IO.File]::Exists($sentinel)
    $registryExists = Test-UninstallKeyExists
    if ($exitCode -ne 0 -or -not $sentinelExists -or $registryExists) {
        throw "Default uninstall retention failed: exit=$exitCode sentinel=$sentinelExists registry=$registryExists."
    }
    $passed.Add('uninstall-preserves-data')

    Invoke-Install -Installer $currentInstaller -Target $installRoot
    $uninstaller = [IO.Path]::Combine($installRoot, 'uninstall.exe')
    $directUninstaller = [IO.Path]::Combine($outputRoot, 'direct-uninstaller.exe')
    [IO.File]::Copy($uninstaller, $directUninstaller, $true)
    $env:DOUYINLIVE_PURGE_DATA = '1'
    Remove-Item Env:DOUYINLIVE_CONFIRM_PURGE -ErrorAction SilentlyContinue
    try {
        $exitCode = Invoke-BoundedProcess -FilePath $directUninstaller -Arguments @('/S', "_?=$installRoot")
    } finally {
        Remove-Item Env:DOUYINLIVE_PURGE_DATA -ErrorAction SilentlyContinue
    }
    if ($exitCode -ne 75 -or -not [IO.File]::Exists($sentinel) -or -not [IO.File]::Exists($uninstaller)) {
        throw "Single-confirmation purge did not fail closed: exit=$exitCode."
    }
    $passed.Add('purge-needs-second-confirmation')

    Invoke-Install -Installer $currentInstaller -Target $installRoot
    $uninstaller = [IO.Path]::Combine($installRoot, 'uninstall.exe')
    [IO.File]::Copy($uninstaller, $directUninstaller, $true)
    $env:DOUYINLIVE_PURGE_DATA = '1'
    $env:DOUYINLIVE_CONFIRM_PURGE = '1'
    try {
        $exitCode = Invoke-BoundedProcess -FilePath $directUninstaller -Arguments @('/S', "_?=$installRoot")
    } finally {
        Remove-Item Env:DOUYINLIVE_PURGE_DATA -ErrorAction SilentlyContinue
        Remove-Item Env:DOUYINLIVE_CONFIRM_PURGE -ErrorAction SilentlyContinue
    }
    Wait-UninstallCleanup
    if ($exitCode -ne 0 -or [IO.Directory]::Exists($dataRoot)) {
        throw "Confirmed purge failed: exit=$exitCode."
    }
    $passed.Add('confirmed-purge')

    Invoke-Install -Installer $autoWebView2Installer -Target $installRoot
    Assert-InstalledPayload -ExpectedVersion $CurrentVersion
    $passed.Add('webview2-auto-install')
    $uninstaller = [IO.Path]::Combine($installRoot, 'uninstall.exe')
    [void](Invoke-BoundedProcess -FilePath $uninstaller -Arguments @('/S'))
    Wait-UninstallCleanup

    $exitCode = Invoke-BoundedProcess -FilePath $failedWebView2Installer -Arguments @('/S', "/D=$missingRoot")
    if ($exitCode -ne 74 -or [IO.Directory]::Exists($missingRoot) -or (Test-UninstallKeyExists)) {
        throw "WebView2 automatic install failure did not fail closed: exit=$exitCode."
    }
    $passed.Add('webview2-auto-install-failure')

    Write-Output 'WINDOWS_INSTALLER_MATRIX_PASSED'
    Write-Output ("checks=" + $passed.Count)
    foreach ($item in $passed) { Write-Output ("passed=" + $item) }
} finally {
    Remove-Item Env:DOUYINLIVE_PURGE_DATA -ErrorAction SilentlyContinue
    Remove-Item Env:DOUYINLIVE_CONFIRM_PURGE -ErrorAction SilentlyContinue
    $uninstaller = [IO.Path]::Combine($installRoot, 'uninstall.exe')
    if ([IO.File]::Exists($uninstaller)) {
        try {
            [void](Invoke-BoundedProcess -FilePath $uninstaller -Arguments @('/S'))
            Wait-UninstallCleanup
        } catch {}
    }
    if (Test-Path -LiteralPath $uninstallRegistryPath) { Remove-Item -LiteralPath $uninstallRegistryPath -Recurse -Force }
    foreach ($shortcut in @(
        [IO.Path]::Combine([Environment]::GetFolderPath('Desktop'), "$productName.lnk"),
        [IO.Path]::Combine([Environment]::GetFolderPath('Programs'), "$productName.lnk")
    )) {
        $deadline = [DateTime]::UtcNow.AddSeconds(10)
        while ([IO.File]::Exists($shortcut)) {
            try { [IO.File]::Delete($shortcut) } catch [UnauthorizedAccessException] {}
            if ([DateTime]::UtcNow -ge $deadline) { throw "Shortcut cleanup did not converge: $shortcut" }
            Start-Sleep -Milliseconds 100
        }
    }
    $expectedParent = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\')
    if ([IO.Path]::GetDirectoryName($testRoot).TrimEnd('\') -ne $expectedParent -or
        [IO.Path]::GetFileName($testRoot) -notmatch '^DouyinLiveInstallerMatrix-[0-9a-f]{32}$') {
        throw 'Refusing to clean an unexpected installer matrix root.'
    }
    if ([IO.Directory]::Exists($testRoot)) { [IO.Directory]::Delete($testRoot, $true) }
    if ([IO.Directory]::Exists($testRoot) -or (Test-UninstallKeyExists)) {
        throw 'Installer matrix cleanup left residual state.'
    }
}
