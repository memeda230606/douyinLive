[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

function Assert-CleanupRace {
    param([Parameter(Mandatory)][bool]$Condition, [Parameter(Mandatory)][string]$Code)
    if (-not $Condition) { throw ('P3ACC_CLEANUP_RACE_ASSERT_FAILED:' + $Code) }
}

function Wait-CleanupRaceFile {
    param([Parameter(Mandatory)][string]$Path, [int]$TimeoutSeconds = 10)
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        if (Test-Path -LiteralPath $Path -PathType Leaf) { return }
        Start-Sleep -Milliseconds 25
    }
    throw ('P3ACC_CLEANUP_RACE_TIMEOUT:' + [IO.Path]::GetFileName($Path))
}

function Get-CleanupRaceExactProcesses {
    param([Parameter(Mandatory)][string]$ExecutablePath)
    return @(Get-CimInstance Win32_Process -ErrorAction Stop | Where-Object {
        -not [string]::IsNullOrWhiteSpace([string]$_.ExecutablePath) -and
        [string]::Equals([string]$_.ExecutablePath, $ExecutablePath, [StringComparison]::OrdinalIgnoreCase)
    })
}

function Stop-CleanupRaceExactProcesses {
    param([Parameter(Mandatory)][string]$ExecutablePath)
    $deadline = [DateTime]::UtcNow.AddSeconds(10)
    do {
        $matches = @(Get-CleanupRaceExactProcesses $ExecutablePath)
        foreach ($record in $matches) {
            try { Stop-Process -Id ([int]$record.ProcessId) -Force -ErrorAction Stop } catch { }
        }
        if ($matches.Count -eq 0) { return }
        Start-Sleep -Milliseconds 50
    } while ([DateTime]::UtcNow -lt $deadline)
    throw 'P3ACC_CLEANUP_RACE_RESIDUAL_PROCESS'
}

function New-CleanupRaceState {
    param([Parameter(Mandatory)]$Process, [Parameter(Mandatory)]$JobState)
    return [pscustomobject]@{
        Process = $Process
        Identity = [pscustomobject]@{
            ProcessId = [int]$Process.Id
            StartedAtUtcTicks = [int64]$Process.StartTime.ToUniversalTime().Ticks
        }
        LauncherIdentity = $null
        JobState = $JobState
        ObservedDescendantIdentities = [Collections.ArrayList]::new()
        ObservedFfmpegIdentities = [Collections.ArrayList]::new()
    }
}

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..\..'))
$modulePath = [IO.Path]::Combine($repositoryRoot, 'scripts\p3-acc\P3AccController.psm1')
$testRoot = [IO.Path]::Combine($env:TEMP, 'p3acc-cleanup-race-' + [Guid]::NewGuid().ToString('N'))
[void][IO.Directory]::CreateDirectory($testRoot)
$fixture = [IO.Path]::Combine($testRoot, 'P3AccCleanupRace.exe')

$source = @'
using System;
using System.Diagnostics;
using System.IO;
using System.Threading;
public static class P3AccCleanupRace {
    private static void StartChild(string executable, string readyPath) {
        ProcessStartInfo info = new ProcessStartInfo(executable, "child \"" + readyPath + "\"");
        info.UseShellExecute = false;
        info.CreateNoWindow = true;
        Process.Start(info).Dispose();
    }
    private static void WaitForFile(string path) {
        for (int attempt = 0; attempt < 400 && !File.Exists(path); attempt++) Thread.Sleep(25);
        if (!File.Exists(path)) Environment.Exit(4);
    }
    public static int Main(string[] args) {
        if (args.Length == 2 && args[0] == "child") {
            File.WriteAllText(args[1], "READY");
            Thread.Sleep(Timeout.Infinite);
            return 0;
        }
        if (args.Length != 4 || args[0] != "root") return 2;
        string executable = Process.GetCurrentProcess().MainModule.FileName;
        File.WriteAllText(args[1], "READY");
        WaitForFile(args[2]);
        StartChild(executable, args[3]);
        Thread.Sleep(Timeout.Infinite);
        return 0;
    }
}
'@

$rootProcess = $null
$uncertainProcess = $null
$outsideProcess = $null
$rootJob = $null
$uncertainJob = $null
try {
    Add-Type -TypeDefinition $source -OutputAssembly $fixture -OutputType ConsoleApplication
    Remove-Module -Name P3AccController -Force -ErrorAction SilentlyContinue
    Import-Module -Name $modulePath -Force -ErrorAction Stop
    $module = Get-Module -Name P3AccController
    Assert-CleanupRace ($null -ne $module) 'module-import'

    $rootReady = [IO.Path]::Combine($testRoot, 'root.ready')
    $rootGate = [IO.Path]::Combine($testRoot, 'root.gate')
    $firstReady = [IO.Path]::Combine($testRoot, 'first.ready')
    $rootJob = & $module { New-P3AccAppJobState }
    $rootProcess = Start-Process -FilePath $fixture -ArgumentList @('root',('"' + $rootReady + '"'),('"' + $rootGate + '"'),('"' + $firstReady + '"')) -WindowStyle Hidden -PassThru -ErrorAction Stop
    Wait-CleanupRaceFile $rootReady
    [P3Acc.NativeJob]::AssignProcess($rootJob.Handle, $rootProcess.Handle)
    [IO.File]::WriteAllText($rootGate, 'GO', [Text.UTF8Encoding]::new($false))
    Wait-CleanupRaceFile $firstReady
    $activeDeadline = [DateTime]::UtcNow.AddSeconds(5)
    while ([DateTime]::UtcNow -lt $activeDeadline -and [P3Acc.NativeJob]::GetActiveProcesses($rootJob.Handle) -lt 2) { Start-Sleep -Milliseconds 25 }
    Assert-CleanupRace ([P3Acc.NativeJob]::GetActiveProcesses($rootJob.Handle) -ge 2) 'nested-active-accounting'
    $state = New-CleanupRaceState -Process $rootProcess -JobState $rootJob
    $stopped = & $module { param($value) Stop-P3AccAppTree -AppState $value } $state
    $rootProcess = $null
    Assert-CleanupRace ([bool]$stopped) 'job-tree-stop'
    Assert-CleanupRace ($rootJob.Closed -and $rootJob.ForcedEmptyConfirmed) 'job-tree-close-state'
    Assert-CleanupRace (@(Get-CleanupRaceExactProcesses $fixture).Count -eq 0) 'job-tree-process-residual'

    $uncertainReady = [IO.Path]::Combine($testRoot, 'uncertain-root.ready')
    $outsideReady = [IO.Path]::Combine($testRoot, 'outside.ready')
    $uncertainJob = & $module { New-P3AccAppJobState }
    $uncertainProcess = Start-Process -FilePath $fixture -ArgumentList @('child',('"' + $uncertainReady + '"')) -WindowStyle Hidden -PassThru -ErrorAction Stop
    $outsideProcess = Start-Process -FilePath $fixture -ArgumentList @('child',('"' + $outsideReady + '"')) -WindowStyle Hidden -PassThru -ErrorAction Stop
    Wait-CleanupRaceFile $uncertainReady
    Wait-CleanupRaceFile $outsideReady
    [P3Acc.NativeJob]::AssignProcess($uncertainJob.Handle, $uncertainProcess.Handle)
    $uncertainState = New-CleanupRaceState -Process $uncertainProcess -JobState $uncertainJob
    [void]$uncertainState.ObservedDescendantIdentities.Add([pscustomobject]@{
        ProcessId = [int]$outsideProcess.Id
        StartedAtUtcTicks = [int64]$outsideProcess.StartTime.ToUniversalTime().Ticks
    })
    $uncertainStopped = & $module { param($value) Stop-P3AccAppTree -AppState $value } $uncertainState
    $uncertainProcess = $null
    Assert-CleanupRace ([bool]$uncertainStopped) 'historical-identity-blocked-job-cleanup'
    Assert-CleanupRace ($uncertainJob.Closed -and $uncertainJob.ForcedEmptyConfirmed) 'historical-job-close-state'
    $outsideIdentity = [pscustomobject]@{ ProcessId = [int]$outsideProcess.Id; StartedAtUtcTicks = [int64]$outsideProcess.StartTime.ToUniversalTime().Ticks }
    Assert-CleanupRace (Test-P3AccProcessIdentity $outsideIdentity) 'historical-outside-process-killed'
    $outsideProcess.Kill()
    [void]$outsideProcess.WaitForExit(5000)
    $outsideProcess.Dispose()
    $outsideProcess = $null
    Assert-CleanupRace (@(Get-CleanupRaceExactProcesses $fixture).Count -eq 0) 'historical-process-residual'

    Write-Output 'P3ACC_CLEANUP_RACE_TESTS_OK passed=2'
} finally {
    foreach ($process in @($rootProcess,$uncertainProcess,$outsideProcess)) {
        if ($null -eq $process) { continue }
        try { if (-not $process.HasExited) { $process.Kill(); [void]$process.WaitForExit(5000) } } catch { }
        try { $process.Dispose() } catch { }
    }
    foreach ($job in @($rootJob,$uncertainJob)) {
        if ($null -ne $job -and -not $job.Closed) { try { $job.Handle.Dispose() } catch { } }
    }
    if (Test-Path -LiteralPath $fixture -PathType Leaf) {
        try { Stop-CleanupRaceExactProcesses $fixture } catch { }
    }
    if (Test-Path -LiteralPath $testRoot -PathType Container) {
        Remove-Item -LiteralPath $testRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
}
