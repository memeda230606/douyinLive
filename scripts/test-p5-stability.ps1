[CmdletBinding()]
param(
    [switch]$Run60Minutes,
    [string]$ResultRoot = ''
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

function Invoke-P5Native {
    param(
        [Parameter(Mandatory)]
        [string[]]$Arguments,
        [Parameter(Mandatory)]
        [int]$TimeoutSeconds
    )

    $goCommand = Get-Command go -ErrorAction Stop
    $startInfo = New-Object System.Diagnostics.ProcessStartInfo
    $startInfo.FileName = $goCommand.Source
    $startInfo.Arguments = [string]::Join(' ', $Arguments)
    $startInfo.WorkingDirectory = $script:RepositoryRoot
    $startInfo.UseShellExecute = $false
    $startInfo.CreateNoWindow = $true
    $startInfo.RedirectStandardOutput = $true
    $startInfo.RedirectStandardError = $true
    $process = New-Object System.Diagnostics.Process
    $process.StartInfo = $startInfo

    if (-not $process.Start()) {
        throw 'P5STB_PROCESS_START_FAILED'
    }
    $stdoutTask = $process.StandardOutput.ReadToEndAsync()
    $stderrTask = $process.StandardError.ReadToEndAsync()
    if (-not $process.WaitForExit($TimeoutSeconds * 1000)) {
        try { $process.Kill() } catch {}
        $process.WaitForExit()
        throw 'P5STB_PROCESS_TIMEOUT'
    }
    $stdout = $stdoutTask.GetAwaiter().GetResult()
    $stderr = $stderrTask.GetAwaiter().GetResult()
    $exitCode = $process.ExitCode
    $process.Dispose()

    if (-not [string]::IsNullOrWhiteSpace($stdout)) {
        [Console]::Out.Write($stdout)
    }
    if (-not [string]::IsNullOrWhiteSpace($stderr)) {
        [Console]::Error.Write($stderr)
    }
    if ($exitCode -ne 0) {
        throw ('P5STB_PROCESS_FAILED:{0}' -f $exitCode)
    }
}

function Resolve-P5ResultRoot {
    param([Parameter(Mandatory)][string]$Value)

    if ([string]::IsNullOrWhiteSpace($Value)) {
        throw 'P5STB_RESULT_ROOT_REQUIRED'
    }
    $resolved = [IO.Path]::GetFullPath($Value)
    if (-not [IO.Path]::IsPathRooted($resolved)) {
        throw 'P5STB_RESULT_ROOT_INVALID'
    }
    if (-not [IO.Directory]::Exists($resolved)) {
        [IO.Directory]::CreateDirectory($resolved) | Out-Null
    }
    $item = Get-Item -LiteralPath $resolved -Force
    if (-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
        throw 'P5STB_RESULT_ROOT_INVALID'
    }
    if (@(Get-ChildItem -LiteralPath $resolved -Force).Count -ne 0) {
        throw 'P5STB_RESULT_ROOT_NOT_EMPTY'
    }
    return $resolved
}

$RepositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
if (-not [IO.File]::Exists((Join-Path $RepositoryRoot 'go.mod'))) {
    throw 'P5STB_REPOSITORY_ROOT_INVALID'
}
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw 'P5STB_GO_UNAVAILABLE'
}

Invoke-P5Native -TimeoutSeconds 180 -Arguments @(
    'test', '-run', '^TestClassifyRecorderExitWindowsNetworkErrors$', '-count=20', './internal/capture'
)
Invoke-P5Native -TimeoutSeconds 180 -Arguments @(
    'test', '-run', '^TestCoordinatorRecorderRecoveryPermanentErrorsDoNotRetry$', '-count=20', './internal/capture'
)
Invoke-P5Native -TimeoutSeconds 180 -Arguments @(
    'test', '-tags', 'p5stbacceptance', '-run', '^TestP5STBStabilitySmoke$',
    '-count=1', '-timeout', '2m', '-v', './internal/app'
)
Invoke-P5Native -TimeoutSeconds 300 -Arguments @(
    'vet', '-tags', 'p5stbacceptance', './internal/app', './internal/capture', './internal/eventstore'
)

if (-not $Run60Minutes) {
    [Console]::Out.WriteLine('P5STB_SMOKE_PASSED')
    exit 0
}

$formalRoot = Resolve-P5ResultRoot -Value $ResultRoot
$resultPath = Join-Path $formalRoot 'p5-stability-result.json'
$oldRun = [Environment]::GetEnvironmentVariable('P5STB_RUN_60M', 'Process')
$oldRoot = [Environment]::GetEnvironmentVariable('P5STB_ROOT', 'Process')
$oldResult = [Environment]::GetEnvironmentVariable('P5STB_RESULT_PATH', 'Process')
try {
    [Environment]::SetEnvironmentVariable('P5STB_RUN_60M', '1', 'Process')
    [Environment]::SetEnvironmentVariable('P5STB_ROOT', $formalRoot, 'Process')
    [Environment]::SetEnvironmentVariable('P5STB_RESULT_PATH', $resultPath, 'Process')
    Invoke-P5Native -TimeoutSeconds 3960 -Arguments @(
        'test', '-tags', 'p5stbacceptance', '-run', '^TestP5STB60MinuteStability$',
        '-count=1', '-timeout', '65m', '-v', './internal/app'
    )
} finally {
    [Environment]::SetEnvironmentVariable('P5STB_RUN_60M', $oldRun, 'Process')
    [Environment]::SetEnvironmentVariable('P5STB_ROOT', $oldRoot, 'Process')
    [Environment]::SetEnvironmentVariable('P5STB_RESULT_PATH', $oldResult, 'Process')
}

if (-not [IO.File]::Exists($resultPath)) {
    throw 'P5STB_RESULT_MISSING'
}
$result = Get-Content -LiteralPath $resultPath -Raw | ConvertFrom-Json
if ($result.schema -ne 'P5-STB-001/v1' -or -not $result.passed -or
    [int64]$result.durationSeconds -ne 3600 -or @($result.samples).Count -ne 61) {
    throw 'P5STB_RESULT_INVALID'
}
[Console]::Out.WriteLine('P5STB_60M_PASSED')
