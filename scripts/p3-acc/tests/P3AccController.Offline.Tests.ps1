[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$script:PassedCount = 0
$script:Failures = [Collections.ArrayList]::new()
$script:CreatedJunctions = [Collections.ArrayList]::new()
$script:MockNames = @(
    'New-ScheduledTaskAction','New-ScheduledTaskPrincipal','New-ScheduledTaskSettingsSet',
    'Register-ScheduledTask','Start-ScheduledTask','Stop-ScheduledTask',
    'Unregister-ScheduledTask','Get-ScheduledTask'
)

function Assert-P3AccOfflineTrue {
    param([Parameter(Mandatory)][bool]$Condition, [Parameter(Mandatory)][string]$Code)
    if (-not $Condition) { throw "P3ACC_OFFLINE_ASSERT_FAILED:$Code" }
}

function Assert-P3AccOfflineEqual {
    param($Actual, $Expected, [Parameter(Mandatory)][string]$Code)
    if ($Actual -cne $Expected) { throw "P3ACC_OFFLINE_ASSERT_FAILED:${Code}:actual=${Actual}:expected=${Expected}" }
}

function Assert-P3AccOfflineThrows {
    param([Parameter(Mandatory)][string]$Code, [Parameter(Mandatory)][scriptblock]$Body)
    $caught = $null
    try { & $Body | Out-Null }
    catch { $caught = $_.Exception.Message }
    if ($caught -cne $Code) { throw "P3ACC_OFFLINE_EXPECTED_FAILURE:$Code" }
}

function Assert-P3AccOfflineThrowsOneOf {
    param([Parameter(Mandatory)][string[]]$Codes, [Parameter(Mandatory)][scriptblock]$Body)
    $caught = $null
    try { & $Body | Out-Null }
    catch { $caught = $_.Exception.Message }
    if ($Codes -cnotcontains $caught) { throw "P3ACC_OFFLINE_EXPECTED_SAFE_FAILURE:actual=$caught" }
}

function Invoke-P3AccOfflineTest {
    param([Parameter(Mandatory)][string]$Name, [Parameter(Mandatory)][scriptblock]$Body)
    try {
        & $Body
        $script:PassedCount++
        Write-Output "PASS $Name"
    } catch {
        [void]$script:Failures.Add("$Name [$($_.Exception.Message)]")
        Write-Output "FAIL $Name"
    }
}

function New-P3AccOfflineRunRoot {
    param([Parameter(Mandatory)][string]$Parent, [string]$Name = ('run-' + [Guid]::NewGuid().ToString('N')))
    $path = [IO.Path]::Combine($Parent, $Name)
    [void][IO.Directory]::CreateDirectory($path)
    [IO.File]::WriteAllText(
        [IO.Path]::Combine($path, '.p3acc-controller-owned'),
        "P3ACC-CONTROLLER/v1`n",
        [Text.UTF8Encoding]::new($false)
    )
    return $path
}

function New-P3AccOfflinePrivateFile {
    param([Parameter(Mandatory)][string]$Path, [string]$Text = 'fixture-input')
    [IO.File]::WriteAllText($Path, $Text, [Text.UTF8Encoding]::new($false))
    $currentSid = [Security.Principal.WindowsIdentity]::GetCurrent().User
    $systemSid = [Security.Principal.SecurityIdentifier]::new('S-1-5-18')
    $security = [Security.AccessControl.FileSecurity]::new()
    $security.SetOwner($currentSid)
    $security.SetAccessRuleProtection($true, $false)
    $allow = [Security.AccessControl.AccessControlType]::Allow
    $currentRule = [Security.AccessControl.FileSystemAccessRule]::new($currentSid, [Security.AccessControl.FileSystemRights]::FullControl, $allow)
    $systemRule = [Security.AccessControl.FileSystemAccessRule]::new($systemSid, [Security.AccessControl.FileSystemRights]::FullControl, $allow)
    [void]$security.AddAccessRule($currentRule)
    [void]$security.AddAccessRule($systemRule)
    [IO.File]::SetAccessControl($Path, $security)
}

function Remove-P3AccOfflineJunction {
    param([Parameter(Mandatory)][string]$Path)
    if (-not (Test-Path -LiteralPath $Path)) { return }
    $item = Get-Item -LiteralPath $Path -Force -ErrorAction Stop
    if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -eq 0) {
        throw 'P3ACC_OFFLINE_REFUSED_NON_REPARSE_DELETE'
    }
    [IO.Directory]::Delete($Path)
    if (Test-Path -LiteralPath $Path) { throw 'P3ACC_OFFLINE_JUNCTION_DELETE_FAILED' }
}

function Get-P3AccOfflineExactProcesses {
    param([Parameter(Mandatory)][string]$ExecutablePath)
    return @(Get-CimInstance Win32_Process -ErrorAction Stop | Where-Object {
        -not [string]::IsNullOrWhiteSpace([string]$_.ExecutablePath) -and
        [string]::Equals([string]$_.ExecutablePath, $ExecutablePath, [StringComparison]::OrdinalIgnoreCase)
    })
}

function Stop-P3AccOfflineExactProcesses {
    param([Parameter(Mandatory)][string]$ExecutablePath)
    foreach ($process in @(Get-P3AccOfflineExactProcesses $ExecutablePath)) {
        try { Stop-Process -Id ([int]$process.ProcessId) -Force -ErrorAction Stop }
        catch { }
    }
}

function Invoke-P3AccOfflineProcess {
    param(
        [Parameter(Mandatory)][string]$FilePath,
        [Parameter(Mandatory)][string[]]$ArgumentList,
        [Parameter(Mandatory)][string]$OutputDirectory,
        [int]$TimeoutSeconds = 30
    )
    $process = $null
    try {
        $startInfo = [Diagnostics.ProcessStartInfo]::new()
        $startInfo.FileName = $FilePath
        $startInfo.Arguments = $ArgumentList -join ' '
        $startInfo.UseShellExecute = $false
        $startInfo.CreateNoWindow = $true
        $startInfo.RedirectStandardOutput = $true
        $startInfo.RedirectStandardError = $true
        $process = [Diagnostics.Process]::new()
        $process.StartInfo = $startInfo
        if (-not $process.Start()) { throw 'P3ACC_OFFLINE_PROCESS_START_FAILED' }
        $stdoutTask = $process.StandardOutput.ReadToEndAsync()
        $stderrTask = $process.StandardError.ReadToEndAsync()
        if (-not $process.WaitForExit($TimeoutSeconds * 1000)) {
            try { $process.Kill(); [void]$process.WaitForExit(5000) } catch { }
            throw 'P3ACC_OFFLINE_PROCESS_TIMEOUT'
        }
        $process.WaitForExit()
        $process.Refresh()
        $outText = [string]$stdoutTask.Result
        $errText = [string]$stderrTask.Result
        return [pscustomobject]@{ ExitCode = [int]$process.ExitCode; Stdout = $outText; Stderr = $errText }
    } finally {
        if ($null -ne $process) { try { $process.Dispose() } catch { } }
    }
}

function Copy-P3AccOfflineValue {
    param([Parameter(Mandatory)]$Value)
    return ($Value | ConvertTo-Json -Depth 32 -Compress | ConvertFrom-Json -ErrorAction Stop)
}

function New-P3AccOfflineMetric {
    return [pscustomobject][ordered]@{
        baseline = 1; peak = 1; latest = 1; delta = 0; latterHalfDelta = 0; latterHalfTrend = 'STABLE'
    }
}

function New-P3AccOfflineMetricValue {
    param(
        [Parameter(Mandatory)][int64]$Baseline,
        [Parameter(Mandatory)][int64]$Peak,
        [Parameter(Mandatory)][int64]$Latest,
        [Parameter(Mandatory)][int64]$LatterHalfDelta,
        [Parameter(Mandatory)][string]$LatterHalfTrend
    )
    return [pscustomobject][ordered]@{
        baseline = $Baseline; peak = $Peak; latest = $Latest; delta = $Latest - $Baseline
        latterHalfDelta = $LatterHalfDelta; latterHalfTrend = $LatterHalfTrend
    }
}

function New-P3AccOfflineFinalSnapshot {
    $resources = [pscustomobject][ordered]@{
        sampleCount = 30; windowDurationMs = 600000; sampleComplete = $true
        stableWindowProven = $true; frozen = $true
        averageCpuPercent = [double]1; latterHalfAverageCpuPercent = [double]1
        cpuWithinTarget = $true; cpuTrend = 'STABLE'
        databaseWalObserved = $true; diskIoObserved = $true; eventQueueObserved = $true
        averageProcessReadBytesPerSecond = [double]128; averageProcessWriteBytesPerSecond = [double]256
        latterHalfProcessReadBytesPerSecond = [double]96; latterHalfProcessWriteBytesPerSecond = [double]192
        averageDiskWriteBytesPerSecond = [double]512; latterHalfDiskWriteBytesPerSecond = [double]384
        processCount = New-P3AccOfflineMetric
        workingSet = New-P3AccOfflineMetric
        privateBytes = New-P3AccOfflineMetric
        threads = New-P3AccOfflineMetric
        handles = New-P3AccOfflineMetric
        goroutines = New-P3AccOfflineMetric
        heapAlloc = New-P3AccOfflineMetric
        heapInUse = New-P3AccOfflineMetric
        system = New-P3AccOfflineMetric
        databaseWalBytes = New-P3AccOfflineMetricValue -Baseline 4194304 -Peak 4194304 -Latest 0 -LatterHalfDelta -2097152 -LatterHalfTrend 'FALLING'
        processReadBytes = New-P3AccOfflineMetricValue -Baseline 1024 -Peak 2048 -Latest 2048 -LatterHalfDelta 512 -LatterHalfTrend 'RISING'
        processWriteBytes = New-P3AccOfflineMetricValue -Baseline 2048 -Peak 4096 -Latest 4096 -LatterHalfDelta 1024 -LatterHalfTrend 'RISING'
        dataRootPhysicalBytes = New-P3AccOfflineMetricValue -Baseline 4096 -Peak 8192 -Latest 8192 -LatterHalfDelta 2048 -LatterHalfTrend 'RISING'
        eventQueueCount = New-P3AccOfflineMetricValue -Baseline 1 -Peak 1 -Latest 1 -LatterHalfDelta 0 -LatterHalfTrend 'STABLE'
        eventQueueItems = New-P3AccOfflineMetricValue -Baseline 1 -Peak 4 -Latest 2 -LatterHalfDelta -2 -LatterHalfTrend 'STABLE'
        eventQueueBytes = New-P3AccOfflineMetricValue -Baseline 128 -Peak 4096 -Latest 512 -LatterHalfDelta -3584 -LatterHalfTrend 'STABLE'
        eventQueueItemCapacity = New-P3AccOfflineMetricValue -Baseline 256 -Peak 256 -Latest 256 -LatterHalfDelta 0 -LatterHalfTrend 'STABLE'
        eventQueueByteCapacity = New-P3AccOfflineMetricValue -Baseline 1048576 -Peak 1048576 -Latest 1048576 -LatterHalfDelta 0 -LatterHalfTrend 'STABLE'
    }
    return [pscustomobject][ordered]@{
        schema = 'P3-ACC-001/v1'
        stage = 'FINALIZED'
        capturedAt = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
        ui = [pscustomobject][ordered]@{
            ready = $true; recordingSeen = $true; progressAdvanced = $true; timelineSeen = $true
            reconnectingSeen = $true; recoveredSeen = $true; networkReconnectingSeen = $true
            networkRecoveredSeen = $true; offlineSeen = $true; finalizedSeen = $true
            observationCount = 10; latencySampleCount = 1; latencyPendingCount = 0
            latencyP95Ms = 1; latencyMaxMs = 1; latencyWithinTarget = $true
        }
        runtime = [pscustomobject][ordered]@{
            state = 'WAITING'; recordingStatus = ''; revision = 12; errorCode = 'ROOM_OFFLINE'
            hasSession = $true; sessionFenceStable = $true; currentAttemptCommitted = $true
            attemptAdvanced = $true; attemptCount = 3; recorderTargetMatched = $true
            crashInjected = $true; recoveryProven = $true; networkFaultArmed = $true
            networkRecoveryProven = $true; finalizationProven = $true
        }
        progress = [pscustomobject][ordered]@{
            sampleCount = 30; liveBatchCount = 1; liveEventCount = 1; elapsedMs = 600000
            bytesWritten = 4096; segmentCount = 3; restartCount = 2
            steadyRecordingMs = 600000; steadySampleCount = 30
        }
        database = [pscustomobject][ordered]@{
            sessionCount = 1; activeSessionCount = 0; eventCount = 1; sourceEventCount = 1
            publishedEventCount = 1; publishedEventsPersisted = $true
            segmentCount = 3; completeSegmentCount = 3; artifactCount = 0; completeArtifactCount = 0
        }
        sessionManifest = [pscustomobject][ordered]@{
            exists = $true; matchesDatabase = $true; canonicalHashMatches = $true
            manifestClean = $true; ended = $true; status = 'completed'; recordingStatus = 'incomplete'
        }
        mediaManifest = [pscustomobject][ordered]@{
            exists = $true; matchesDatabase = $true; canonicalHashMatches = $true; manifestClean = $true
            state = 'completed'; revision = 12; attemptCount = 3; committedAttemptCount = 3
            cleanAttemptCount = 3; segmentCount = 3; completeSegmentCount = 3
            artifactCount = 0; completeArtifactCount = 0; fileCheckCount = 3; fileFailureCount = 0
            incompleteEntryCount = 0; incompleteSegmentCount = 0; allFilesMatch = $true
            sequenceContinuous = $true; attemptReferencesValid = $true; faultPhaseSegmentsProven = $true
        }
        gaps = [pscustomobject][ordered]@{
            total = 3; open = 0; recovered = 3; recordingRestart = 2
            openRecordingRestart = 0; openMessageDisconnect = 0; processCrash = 1; messageDisconnect = 1
            crashRecoveryMatched = $true; networkMessageMatched = $true; networkRecorderMatched = $true
            latestKind = 'message_disconnect'; latestReasonCode = 'MESSAGE_RECONNECTED'
            latestOpen = $false; latestRecovered = $true
        }
        checkpoint = [pscustomobject][ordered]@{
            exists = $true; state = 'closed'; committedSequence = 1; maxSourceSequence = 1
            coversSourceEvents = $true; openGiftFoldCount = 0; giftFoldsClosed = $true
        }
        resources = $resources
    }
}

function New-P3AccOfflinePassedReport {
    $report = New-P3AccControllerReport
    foreach ($name in @(
        'rootValidated','secretValidated','secretRemoved','relayDynamicPort','relayBaselineProbe',
        'relayFaultProven','relaySamePortRestored','appInteractiveLaunch','snapshotContract',
        'stableWindowObserved','uiBaselineObserved','crashRecoveryObserved',
        'networkFaultArmedObserved','networkRecoveryObserved','finalizationObserved'
    )) { $report[$name] = $true }
    $report.passed = $true
    $report.code = 'OK'
    $report.topology.sampleCount = 6
    $report.topology.beforeFaultSampleCount = 3
    $report.topology.afterRecoverySampleCount = 3
    $report.topology.appOnlyRelay = $true
    $report.topology.ffmpegOnlyRelay = $true
    $report.topology.relayOnlyUpstream = $true
    $report.topology.noUdpBypass = $true
    $report.visual.safeCropCaptured = $true
    $report.visual.sha256 = ('a' * 64)
    $report.visual.width = 320
    $report.visual.height = 180
    $report.visual.nonUniform = $true
    $report.visual.evidenceAcknowledged = $true
    $report.visual.wmCloseSent = $true
    $report.visual.appExitCodeZero = $true
    $report.visual.naturalAppTreeExited = $true
    $report.database.quickCheckPassed = $true
    $report.database.unlocked = $true
    $report.metrics.resources.sampleCount = 30
    $report.metrics.resources.windowMs = 600000
    $report.metrics.resources.ownedProcessTreeCPUAvgPct = [double]1
    $report.metrics.resources.ownedProcessTreeLatterHalfCPUAvgPct = [double]1
    $report.metrics.resources.cpuTrend = 'STABLE'
    foreach ($name in @('processCount','workingSet','privateBytes','threads','handles','goroutines','heapAlloc','heapInUse','system')) {
        $report.metrics.resources[$name].baseline = 1
        $report.metrics.resources[$name].peak = 1
        $report.metrics.resources[$name].latest = 1
        $report.metrics.resources[$name].delta = 0
        $report.metrics.resources[$name].latterHalfTrend = 'STABLE'
    }
    $fixtureResources = (New-P3AccOfflineFinalSnapshot).resources
    foreach ($name in @('databaseWalObserved','diskIoObserved','eventQueueObserved','averageProcessReadBytesPerSecond','averageProcessWriteBytesPerSecond','latterHalfProcessReadBytesPerSecond','latterHalfProcessWriteBytesPerSecond','averageDiskWriteBytesPerSecond','latterHalfDiskWriteBytesPerSecond')) {
        $report.metrics.resources[$name] = $fixtureResources.$name
    }
    foreach ($name in @('databaseWalBytes','processReadBytes','processWriteBytes','dataRootPhysicalBytes','eventQueueCount','eventQueueItems','eventQueueBytes','eventQueueItemCapacity','eventQueueByteCapacity')) {
        foreach ($field in @('baseline','peak','latest','delta','latterHalfDelta','latterHalfTrend')) {
            $report.metrics.resources[$name][$field] = $fixtureResources.$name.$field
        }
    }
    $report.metrics.uiLatency.sampleCount = 1
    $report.metrics.uiLatency.p95Ms = 1
    $report.metrics.uiLatency.maxMs = 1
    $report.metrics.lineage.runtimeAttemptCount = 3
    $report.metrics.lineage.progressRestartCount = 2
    $report.metrics.lineage.mediaAttemptCount = 3
    $report.metrics.lineage.committedAttemptCount = 3
    $report.metrics.lineage.segmentCount = 3
    $report.metrics.lineage.artifactCount = 0
    $report.metrics.lineage.processCrashGapCount = 1
    $report.metrics.lineage.recordingRestartGapCount = 2
    $report.metrics.lineage.messageDisconnectGapCount = 1
    foreach ($name in @('taskRemoved','appStopped','relayStopped','secretRemoved','ephemeralRootRemoved','controlRootRemoved','zeroResidual')) {
        $report.cleanup[$name] = $true
    }
    return $report
}

function New-P3AccOfflineControllerConfiguration {
    param(
        [Parameter(Mandatory)][string]$Parent,
        [Parameter(Mandatory)][string]$AppExecutable,
        [Parameter(Mandatory)][string]$LauncherExecutable,
        [Parameter(Mandatory)][string]$RelayExecutable
    )
    $root = New-P3AccOfflineRunRoot $Parent
    $secret = [IO.Path]::Combine($Parent, 'input-' + [Guid]::NewGuid().ToString('N') + '.txt')
    New-P3AccOfflinePrivateFile $secret
    $configuration = New-P3AccControllerConfiguration `
        -AppExecutable $AppExecutable -LauncherExecutable $LauncherExecutable -RelayExecutable $RelayExecutable `
        -Root $root -LiveUrlFile $secret -ClashUpstream '127.0.0.1:1' `
        -StartupTimeoutSeconds 30 -PreFaultTimeoutSeconds 660 `
        -FaultDetectionTimeoutSeconds 30 -RecoveryTimeoutSeconds 30 `
        -FinalizationTimeoutSeconds 60 -CloseTimeoutSeconds 5 `
        -ProbeTimeoutSeconds 2 -PollIntervalMilliseconds 250
    return $configuration
}

function Remove-P3AccOfflineConfigurationFixture {
    param($Configuration)
    if ($null -eq $Configuration) { return }
    foreach ($path in @($Configuration.SecretPath, $Configuration.Root, $Configuration.ControlRoot)) {
        if ([string]::IsNullOrWhiteSpace([string]$path) -or -not (Test-Path -LiteralPath $path)) { continue }
        $item = Get-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue
        if ($null -ne $item -and ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
            Remove-P3AccOfflineJunction $path
        } else {
            Remove-Item -LiteralPath $path -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}

function Install-P3AccOfflineScheduledTaskMocks {
    foreach ($name in $script:MockNames) {
        if (Test-Path -LiteralPath ("Function:\global:$name")) { throw 'P3ACC_OFFLINE_MOCK_COLLISION' }
    }
    function global:New-ScheduledTaskAction {
        [CmdletBinding()] param($Execute, $Argument)
        return [pscustomobject]@{ Execute = $Execute; Argument = $Argument }
    }
    function global:New-ScheduledTaskPrincipal {
        [CmdletBinding()] param($UserId, $LogonType, $RunLevel)
        return [pscustomobject]@{ UserId = $UserId }
    }
    function global:New-ScheduledTaskSettingsSet {
        [CmdletBinding()] param($ExecutionTimeLimit, $MultipleInstances)
        return [pscustomobject]@{ ExecutionTimeLimit = $ExecutionTimeLimit }
    }
    function global:Register-ScheduledTask {
        [CmdletBinding()] param($TaskName, $Action, $Principal, $Settings, [switch]$Force)
        $global:P3AccOfflineTaskRegistered = $true
        $global:P3AccOfflineTaskName = [string]$TaskName
        return [pscustomobject]@{ TaskName = $TaskName }
    }
    function global:Start-ScheduledTask {
        [CmdletBinding()] param($TaskName)
        $global:P3AccOfflineTaskStartCount = [int]$global:P3AccOfflineTaskStartCount + 1
        switch ($global:P3AccOfflineTaskMode) {
            'MissingIdentity' { return }
            'InvalidIdentity' {
                [IO.File]::WriteAllText($global:P3AccOfflinePreIdentityPath, '{', [Text.UTF8Encoding]::new($false))
                return
            }
            'ExitedIdentity' {
                $dead = Start-Process -FilePath $env:ComSpec -ArgumentList @('/d','/c','exit','0') -WindowStyle Hidden -PassThru
                $ticks = $dead.StartTime.ToUniversalTime().Ticks
                $identifier = $dead.Id
                [void]$dead.WaitForExit(5000)
                $dead.Dispose()
                $launchText = Get-Content -LiteralPath $global:P3AccOfflineLaunchPath -Raw
                $nonceMatch = [regex]::Match($launchText, "jobNonce = '([0-9a-f]{32})'")
                if (-not $nonceMatch.Success) { throw 'P3ACC_OFFLINE_NONCE_MISSING' }
                $payload = [ordered]@{ schema = 'P3ACC-LAUNCHER-IDENTITY/v1'; jobNonce = $nonceMatch.Groups[1].Value; launcherProcessId = $identifier; launcherStartedAtUtcTicks = $ticks } | ConvertTo-Json -Compress
                [IO.File]::WriteAllText($global:P3AccOfflinePreIdentityPath, $payload, [Text.UTF8Encoding]::new($false))
                return
            }
            'HelperIdentityWriteFailure' {
                $identityTemporary = $global:P3AccOfflinePreIdentityPath + '.tmp'
                [IO.File]::WriteAllText($identityTemporary, 'occupied', [Text.UTF8Encoding]::new($false))
                [IO.File]::SetAttributes($identityTemporary, [IO.FileAttributes]::ReadOnly)
                $stdout = [IO.Path]::Combine([IO.Path]::GetDirectoryName($global:P3AccOfflineLaunchPath), 'mock-launch.stdout')
                $stderr = [IO.Path]::Combine([IO.Path]::GetDirectoryName($global:P3AccOfflineLaunchPath), 'mock-launch.stderr')
                $child = Start-Process -FilePath 'powershell.exe' -ArgumentList @('-NoProfile','-NonInteractive','-ExecutionPolicy','Bypass','-File',('"' + $global:P3AccOfflineLaunchPath + '"')) -RedirectStandardOutput $stdout -RedirectStandardError $stderr -WindowStyle Hidden -PassThru
                [void]$child.WaitForExit(15000)
                $child.Dispose()
                Remove-Item -LiteralPath $stdout -Force -ErrorAction SilentlyContinue
                Remove-Item -LiteralPath $stderr -Force -ErrorAction SilentlyContinue
                return
            }
            default { throw 'P3ACC_OFFLINE_UNKNOWN_TASK_MODE' }
        }
    }
    function global:Stop-ScheduledTask {
        [CmdletBinding()] param($TaskName)
    }
    function global:Unregister-ScheduledTask {
        [CmdletBinding()] param($TaskName, [switch]$Confirm)
        $global:P3AccOfflineTaskRegistered = $false
    }
    function global:Get-ScheduledTask {
        [CmdletBinding()] param($TaskName)
        if ($global:P3AccOfflineTaskRegistered) { return [pscustomobject]@{ TaskName = $TaskName } }
        return $null
    }
}

function Remove-P3AccOfflineScheduledTaskMocks {
    foreach ($name in $script:MockNames) {
        Remove-Item -LiteralPath ("Function:\global:$name") -Force -ErrorAction SilentlyContinue
    }
}

function Get-P3AccOfflineFreeTcpPort {
    $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
    try {
        $listener.Start()
        return [int]([Net.IPEndPoint]$listener.LocalEndpoint).Port
    } finally { $listener.Stop() }
}

function Wait-P3AccOfflineReadyFile {
    param([Parameter(Mandatory)][string]$Path, [int]$TimeoutSeconds = 10)
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        if (Test-Path -LiteralPath $Path -PathType Leaf) { return }
        Start-Sleep -Milliseconds 50
    }
    throw ('offline fixture timeout: ' + [IO.Path]::GetFileName($Path))
}

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..\..'))
$modulePath = [IO.Path]::Combine($repositoryRoot, 'scripts\p3-acc\P3AccController.psm1')
$runnerPath = [IO.Path]::Combine($repositoryRoot, 'scripts\p3-acc-controller.ps1')
$testRoot = [IO.Path]::Combine($env:TEMP, 'p3acc-offline-' + [Guid]::NewGuid().ToString('N'))
[void][IO.Directory]::CreateDirectory($testRoot)

$relayFixture = [IO.Path]::Combine($testRoot, 'P3AccOfflineBadRelay.exe')
$silentFixture = [IO.Path]::Combine($testRoot, 'P3AccOfflineSilent.exe')
$launcherFixture = [IO.Path]::Combine($testRoot, 'P3AccOfflineLauncherStub.exe')
$sleeperFixture = [IO.Path]::Combine($testRoot, 'P3AccOfflineSleeper.exe')
$treeFixture = [IO.Path]::Combine($testRoot, 'P3AccOfflineTree.exe')
$socketBaseFixture = [IO.Path]::Combine($testRoot, 'P3AccOfflineSocketBase.exe')
$socketUpstreamFixture = [IO.Path]::Combine($testRoot, 'P3AccOfflineSocketUpstream.exe')
$socketRelayFixture = [IO.Path]::Combine($testRoot, 'P3AccOfflineSocketRelay.exe')
$socketFfmpegFixture = [IO.Path]::Combine($testRoot, 'ffmpeg.exe')
$socketHelperFixture = [IO.Path]::Combine($testRoot, 'P3AccOfflineSocketHelper.exe')

try {
    $badRelaySource = @'
using System;
using System.Threading;
public static class P3AccOfflineBadRelay {
    public static int Main(string[] args) {
        Console.WriteLine("invalid");
        Console.Out.Flush();
        Thread.Sleep(60000);
        return 0;
    }
}
'@
    $silentSource = @'
public static class P3AccOfflineSilent {
    public static int Main(string[] args) { return 0; }
}
'@
    $launcherSource = @'
using System.Threading;
public static class P3AccOfflineLauncherStub {
    public static int Main(string[] args) { Thread.Sleep(60000); return 0; }
}
'@
    $sleeperSource = @'
using System.Threading;
public static class P3AccOfflineSleeper {
    public static int Main(string[] args) { Thread.Sleep(60000); return 0; }
}
'@
    $treeSource = @'
using System.Diagnostics;
using System.Threading;
public static class P3AccOfflineTree {
    public static int Main(string[] args) {
        if (args.Length == 1 && args[0] == "child") { Thread.Sleep(60000); return 0; }
        Thread.Sleep(1000);
        string executable = Process.GetCurrentProcess().MainModule.FileName;
        ProcessStartInfo child = new ProcessStartInfo(executable, "child");
        child.UseShellExecute = false;
        child.CreateNoWindow = true;
        Process.Start(child);
        Thread.Sleep(60000);
        return 0;
    }
}
'@
    $socketSource = @'
using System;
using System.Collections.Generic;
using System.IO;
using System.Net;
using System.Net.Sockets;
using System.Threading;
public static class P3AccOfflineSocketBase {
    private static readonly List<TcpClient> Clients = new List<TcpClient>();
    private static readonly List<UdpClient> UdpClients = new List<UdpClient>();
    private static void Ready(string path) { File.WriteAllText(path, "READY"); }
    private static UdpClient OpenUdp(int remotePort) {
        UdpClient udp = new UdpClient(new IPEndPoint(IPAddress.Loopback, 0));
        udp.Connect(IPAddress.Loopback, remotePort);
        return udp;
    }
    private static void ArmUdp(string triggerPath, string readyPath, int remotePort) {
        Thread worker = new Thread(delegate() {
            while (!File.Exists(triggerPath)) Thread.Sleep(20);
            UdpClient udp = OpenUdp(remotePort);
            UdpClients.Add(udp);
            Ready(readyPath);
            Thread.Sleep(Timeout.Infinite);
        });
        worker.IsBackground = true;
        worker.Start();
    }
    public static int Main(string[] args) {
        if (args.Length < 3) return 2;
        if (args[0] == "server") {
            TcpListener listener = new TcpListener(IPAddress.Loopback, Int32.Parse(args[1]));
            listener.Start();
            Ready(args[2]);
            while (true) Clients.Add(listener.AcceptTcpClient());
        }
        if (args[0] == "relay" && (args.Length == 4 || args.Length == 6)) {
            TcpListener listener = new TcpListener(IPAddress.Loopback, Int32.Parse(args[1]));
            listener.Start();
            TcpClient upstream = new TcpClient();
            upstream.Connect(IPAddress.Loopback, Int32.Parse(args[2]));
            Clients.Add(upstream);
            if (args.Length == 6) ArmUdp(args[4], args[5], Int32.Parse(args[2]));
            Ready(args[3]);
            while (true) Clients.Add(listener.AcceptTcpClient());
        }
        if (args[0] == "udp") {
            UdpClients.Add(OpenUdp(Int32.Parse(args[1])));
            Ready(args[2]);
            Thread.Sleep(Timeout.Infinite);
        }
        if (args[0] == "client") {
            TcpClient client = new TcpClient();
            client.Connect(IPAddress.Loopback, Int32.Parse(args[1]));
            Clients.Add(client);
            Ready(args[2]);
            Thread.Sleep(Timeout.Infinite);
        }
        return 3;
    }
}
'@
    Add-Type -TypeDefinition $badRelaySource -OutputAssembly $relayFixture -OutputType ConsoleApplication
    Add-Type -TypeDefinition $silentSource -OutputAssembly $silentFixture -OutputType ConsoleApplication
    Add-Type -TypeDefinition $launcherSource -OutputAssembly $launcherFixture -OutputType ConsoleApplication
    Add-Type -TypeDefinition $sleeperSource -OutputAssembly $sleeperFixture -OutputType ConsoleApplication
    Add-Type -TypeDefinition $treeSource -OutputAssembly $treeFixture -OutputType ConsoleApplication
    Add-Type -TypeDefinition $socketSource -OutputAssembly $socketBaseFixture -OutputType ConsoleApplication
    Copy-Item -LiteralPath $socketBaseFixture -Destination $socketUpstreamFixture
    Copy-Item -LiteralPath $socketBaseFixture -Destination $socketRelayFixture
    Copy-Item -LiteralPath $socketBaseFixture -Destination $socketFfmpegFixture
    Copy-Item -LiteralPath $socketBaseFixture -Destination $socketHelperFixture

    Invoke-P3AccOfflineTest 'parser-and-fresh-import' {
        foreach ($path in @($modulePath, $runnerPath)) {
            $tokens = $null
            $errors = $null
            [void][Management.Automation.Language.Parser]::ParseFile($path, [ref]$tokens, [ref]$errors)
            Assert-P3AccOfflineEqual @($errors).Count 0 'parser-errors'
        }
        $freshScript = [IO.Path]::Combine($testRoot, 'fresh-import.ps1')
        $freshContent = @'
param([string]$ModulePath)
$ErrorActionPreference = 'Stop'
Import-Module -Name $ModulePath -Force -ErrorAction Stop
$required = @('Assert-P3AccSnapshotContract','Test-P3AccFinalContract','Assert-P3AccControllerReport','Wait-P3AccEvidenceAcknowledgement','Test-P3AccDatabaseAfterClose','Register-P3AccCurrentDescendantIdentities','Wait-P3AccNaturalAppTreeExit','Test-P3AccTopologySnapshotEligible','Test-P3AccTopologyFenceStable','Test-P3AccTopologyPhaseReady','Copy-P3AccTopologyTrackerToReport')
$module = Get-Module -Name P3AccController
if ($null -eq $module) { exit 21 }
foreach ($name in $required) { if ($module.ExportedFunctions.Keys -cnotcontains $name) { exit 22 } }
Write-Output 'IMPORT_OK'
'@
        [IO.File]::WriteAllText($freshScript, $freshContent, [Text.UTF8Encoding]::new($false))
        $fresh = Invoke-P3AccOfflineProcess -FilePath 'powershell.exe' -ArgumentList @('-NoProfile','-NonInteractive','-ExecutionPolicy','Bypass','-File',('"' + $freshScript + '"'),('"' + $modulePath + '"')) -OutputDirectory $testRoot
        Assert-P3AccOfflineEqual $fresh.ExitCode 0 'fresh-import-exit'
        Assert-P3AccOfflineEqual $fresh.Stdout "IMPORT_OK`r`n" 'fresh-import-output'
        Assert-P3AccOfflineEqual $fresh.Stderr '' 'fresh-import-stderr'
    }

    Remove-Module -Name P3AccController -Force -ErrorAction SilentlyContinue
    Import-Module -Name $modulePath -Force -ErrorAction Stop

    Invoke-P3AccOfflineTest 'outer-job-name-dacl-limits-accounting-and-collision' {
        $module = Get-Module P3AccController -ErrorAction Stop
        $job = $null
        try {
            $job = & $module { New-P3AccAppJobState }
            $sid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
            Assert-P3AccOfflineTrue ($job.Name -cmatch '^Global\\DouyinLive\.P3ACC\.App\.[0-9a-f]{32}$') 'job-global-name'
            Assert-P3AccOfflineTrue ($job.Nonce -cmatch '^[0-9a-f]{32}$') 'job-nonce'
            Assert-P3AccOfflineEqual ([P3Acc.NativeJob]::GetLimitFlags($job.Handle)) ([uint32]0x2000) 'job-limit-flags'
            Assert-P3AccOfflineTrue ([P3Acc.NativeJob]::HasExactProtectedDacl($job.Handle, $sid)) 'job-exact-dacl'
            Assert-P3AccOfflineTrue ([P3Acc.NativeJob]::HasExactMediumMandatoryLabel($job.Handle)) 'job-exact-medium-label'
            Assert-P3AccOfflineEqual ([P3Acc.NativeJob]::GetActiveProcesses($job.Handle)) ([uint32]0) 'job-initial-accounting'
            $collision = $false
            $other = $null
            try { $other = [P3Acc.NativeJob]::CreateOwned($job.Name, $sid) }
            catch { $collision = $_.Exception.Message -match 'P3ACC_JOB_NAME_COLLISION' }
            finally { if ($null -ne $other) { $other.Dispose() } }
            Assert-P3AccOfflineTrue $collision 'job-name-collision-not-rejected'
            $twoZero = & $module { param($value) Wait-P3AccJobEmpty -JobState $value -TimeoutSeconds 2 -ConsecutiveZeroSamples 2 -PollMilliseconds 50 } $job
            Assert-P3AccOfflineTrue ([bool]$twoZero) 'job-two-zero-samples'
            $closed = & $module {
                param($value)
                $value.GracefulExitConfirmed = $true
                Close-P3AccJobState $value
            } $job
            Assert-P3AccOfflineTrue ([bool]$closed) 'job-close-failed'
            Assert-P3AccOfflineTrue ($job.Closed -and $job.GracefulExitConfirmed -and -not $job.ForcedEmptyConfirmed) 'job-close-state-invalid'
        } finally {
            if ($null -ne $job -and -not $job.Closed) { $job.Handle.Dispose() }
        }
    }

    Invoke-P3AccOfflineTest 'topology-stage-isolation-continuity-and-report-sync' {
        $before = New-P3AccOfflineFinalSnapshot
        $before.stage = 'RECOVERED'
        $before.runtime.state = 'RECORDING'
        $before.runtime.recordingStatus = 'recording'
        $before.runtime.errorCode = ''
        $before.runtime.revision = 12
        $before.runtime.attemptCount = 2
        $before.runtime.finalizationProven = $false
        $before.runtime.networkRecoveryProven = $false
        $before.progress.restartCount = 1
        $before.ui.networkReconnectingSeen = $false
        $before.ui.networkRecoveredSeen = $false
        $before.ui.offlineSeen = $false
        $before.ui.finalizedSeen = $false
        $before.ui.observationCount = 6
        $before.gaps.networkMessageMatched = $false
        $before.gaps.networkRecorderMatched = $false
        $before.capturedAt = 1000
        Assert-P3AccOfflineTrue ([bool](Assert-P3AccSnapshotContract $before)) 'before-contract-valid'
        Assert-P3AccOfflineTrue (Test-P3AccPreFaultReady $before) 'before-resource-observation-ready'
        foreach ($name in @('databaseWalObserved','diskIoObserved','eventQueueObserved')) {
            $unobserved = Copy-P3AccOfflineValue $before
            $unobserved.resources.$name = $false
            Assert-P3AccOfflineTrue (-not (Test-P3AccPreFaultReady $unobserved)) ('before-resource-unobserved-' + $name)
        }
        $readOnlyDisk = Copy-P3AccOfflineValue $before
        $readOnlyDisk.resources.dataRootPhysicalBytes.baseline = 8192
        $readOnlyDisk.resources.dataRootPhysicalBytes.peak = 8192
        $readOnlyDisk.resources.dataRootPhysicalBytes.latest = 8192
        $readOnlyDisk.resources.dataRootPhysicalBytes.delta = 0
        $readOnlyDisk.resources.dataRootPhysicalBytes.latterHalfDelta = 0
        $readOnlyDisk.resources.averageDiskWriteBytesPerSecond = [double]0
        $readOnlyDisk.resources.latterHalfDiskWriteBytesPerSecond = [double]0
        Assert-P3AccOfflineTrue (-not (Test-P3AccPreFaultReady $readOnlyDisk)) 'before-write-delta-required'
        Assert-P3AccOfflineTrue (Test-P3AccTopologySnapshotEligible $before) 'before-eligible'

        $oldAttempt = Copy-P3AccOfflineValue $before
        $oldAttempt.runtime.attemptCount = 1
        $oldAttempt.progress.restartCount = 0
        Assert-P3AccOfflineTrue (-not (Test-P3AccTopologySnapshotEligible $oldAttempt)) 'pre-crash-ineligible'
        $wrongStage = Copy-P3AccOfflineValue $before
        $wrongStage.stage = 'RECORDING'
        Assert-P3AccOfflineTrue (-not (Test-P3AccTopologySnapshotEligible $wrongStage)) 'wrong-stage-ineligible'

        $sample = [pscustomobject]@{
            Complete = $true; RetryableUnstable = $false
            AppOnlyRelay = $true; FfmpegOnlyRelay = $true; RelayOnlyUpstream = $true; NoUdpBypass = $true
            AppIdentityKey = 'app-a'; RelayIdentityKey = 'relay-a'; FfmpegIdentityKey = 'ffmpeg-a'
        }
        $retry = [pscustomobject]@{
            Complete = $false; RetryableUnstable = $true
            AppOnlyRelay = $false; FfmpegOnlyRelay = $false; RelayOnlyUpstream = $false; NoUdpBypass = $true
            AppIdentityKey = ''; RelayIdentityKey = ''; FfmpegIdentityKey = ''
        }
        $tracker = New-P3AccTopologyTracker
        for ($index = 0; $index -lt 2; $index++) {
            $start = Copy-P3AccOfflineValue $before
            $finish = Copy-P3AccOfflineValue $before
            $start.capturedAt = 1000 + ($index * 1000)
            $finish.capturedAt = $start.capturedAt + 1
            Assert-P3AccOfflineTrue (Add-P3AccTopologySample -Tracker $tracker -Sample $sample -Snapshot $start -ConfirmedSnapshot $finish) ('before-add-' + $index)
        }
        Assert-P3AccOfflineEqual $tracker.BeforeFaultSampleCount 2 'before-two'
        $retryStart = Copy-P3AccOfflineValue $before
        $retryFinish = Copy-P3AccOfflineValue $before
        $retryStart.capturedAt = 3000
        $retryFinish.capturedAt = 3001
        Assert-P3AccOfflineTrue (-not (Add-P3AccTopologySample -Tracker $tracker -Sample $retry -Snapshot $retryStart -ConfirmedSnapshot $retryFinish)) 'retry-not-counted'
        Assert-P3AccOfflineEqual $tracker.BeforeFaultSampleCount 0 'retry-resets-window'

        for ($index = 0; $index -lt 3; $index++) {
            $start = Copy-P3AccOfflineValue $before
            $finish = Copy-P3AccOfflineValue $before
            $start.capturedAt = 4000 + ($index * 1000)
            $finish.capturedAt = $start.capturedAt + 1
            [void](Add-P3AccTopologySample -Tracker $tracker -Sample $sample -Snapshot $start -ConfirmedSnapshot $finish)
        }
        Assert-P3AccOfflineTrue (Test-P3AccTopologyPhaseReady $tracker) 'before-three-contiguous'

        $after = Copy-P3AccOfflineValue $before
        $after.runtime.revision = 13
        $after.runtime.attemptCount = 3
        $after.progress.restartCount = 2
        $after.runtime.networkRecoveryProven = $true
        $after.ui.networkReconnectingSeen = $true
        $after.ui.networkRecoveredSeen = $true
        $after.ui.observationCount = 8
        $after.gaps.networkMessageMatched = $true
        $after.gaps.networkRecorderMatched = $true
        Assert-P3AccOfflineTrue ([bool](Assert-P3AccSnapshotContract $after)) 'after-contract-valid'
        Assert-P3AccOfflineTrue (Test-P3AccTopologySnapshotEligible $after -AfterRecovery) 'after-eligible'
        $weakAfter = Copy-P3AccOfflineValue $after
        $weakAfter.runtime.attemptCount = 2
        $weakAfter.progress.restartCount = 1
        Assert-P3AccOfflineTrue (-not (Test-P3AccTopologySnapshotEligible $weakAfter -AfterRecovery)) 'after-threshold-ineligible'
        $afterSample = Copy-P3AccOfflineValue $sample
        $afterSample.RelayIdentityKey = 'relay-b'
        $afterSample.FfmpegIdentityKey = 'ffmpeg-b'
        for ($index = 0; $index -lt 2; $index++) {
            $start = Copy-P3AccOfflineValue $after
            $finish = Copy-P3AccOfflineValue $after
            $start.capturedAt = 10000 + ($index * 1000)
            $finish.capturedAt = $start.capturedAt + 1
            [void](Add-P3AccTopologySample -Tracker $tracker -Sample $afterSample -Snapshot $start -ConfirmedSnapshot $finish -AfterRecovery)
        }
        Assert-P3AccOfflineEqual $tracker.AfterRecoverySampleCount 2 'after-two'
        $retryStart = Copy-P3AccOfflineValue $after
        $retryFinish = Copy-P3AccOfflineValue $after
        $retryStart.capturedAt = 12000
        $retryFinish.capturedAt = 12001
        [void](Add-P3AccTopologySample -Tracker $tracker -Sample $retry -Snapshot $retryStart -ConfirmedSnapshot $retryFinish -AfterRecovery)
        Assert-P3AccOfflineEqual $tracker.BeforeFaultSampleCount 3 'after-retry-preserves-before'
        Assert-P3AccOfflineEqual $tracker.AfterRecoverySampleCount 0 'after-retry-resets-after'
        for ($index = 0; $index -lt 3; $index++) {
            $start = Copy-P3AccOfflineValue $after
            $finish = Copy-P3AccOfflineValue $after
            $start.capturedAt = 13000 + ($index * 1000)
            $finish.capturedAt = $start.capturedAt + 1
            [void](Add-P3AccTopologySample -Tracker $tracker -Sample $afterSample -Snapshot $start -ConfirmedSnapshot $finish -AfterRecovery)
        }
        Assert-P3AccOfflineTrue (Test-P3AccTopologyPhaseReady $tracker -AfterRecovery) 'after-three-contiguous'
        Assert-P3AccOfflineEqual $tracker.SampleCount 6 'phase-sample-total'
        $report = New-P3AccControllerReport
        Copy-P3AccTopologyTrackerToReport -Tracker $tracker -Report $report
        Assert-P3AccOfflineEqual $report.topology.sampleCount 6 'report-sample-total'
        Assert-P3AccOfflineTrue ($report.topology.appOnlyRelay -and $report.topology.ffmpegOnlyRelay -and $report.topology.relayOnlyUpstream -and $report.topology.noUdpBypass) 'report-topology-proven'

        $changedIdentity = Copy-P3AccOfflineValue $afterSample
        $changedIdentity.FfmpegIdentityKey = 'ffmpeg-c'
        $start = Copy-P3AccOfflineValue $after
        $finish = Copy-P3AccOfflineValue $after
        $start.capturedAt = 17000
        $finish.capturedAt = 17001
        [void](Add-P3AccTopologySample -Tracker $tracker -Sample $changedIdentity -Snapshot $start -ConfirmedSnapshot $finish -AfterRecovery)
        Assert-P3AccOfflineEqual $tracker.AfterRecoverySampleCount 1 'identity-change-resets-window'
        Assert-P3AccOfflineTrue (-not $tracker.AppOnlyRelay) 'aggregate-cleared-after-reset'

        $invalid = Copy-P3AccOfflineValue $sample
        $invalid.NoUdpBypass = $false
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_TOPOLOGY_INVALID' { Add-P3AccTopologySample -Tracker $tracker -Sample $invalid -Snapshot $before -ConfirmedSnapshot $before }
        $drifted = Copy-P3AccOfflineValue $before
        $drifted.runtime.recorderTargetMatched = $false
        Assert-P3AccOfflineTrue (-not (Test-P3AccTopologyFenceStable $before $drifted)) 'fence-drift-rejected'
        $timeRegressed = Copy-P3AccOfflineValue $before
        $timeRegressed.capturedAt = $before.capturedAt - 1
        Assert-P3AccOfflineTrue (-not (Test-P3AccTopologyFenceStable $before $timeRegressed)) 'fence-time-regression-rejected'
    }

    Invoke-P3AccOfflineTest 'topology-sampler-established-pairs-exit-and-bypass' {
        $module = Get-Module -Name P3AccController
        $fakeClientConnection = [pscustomobject]@{ State='Established'; LocalPort=30000; LocalAddress='127.0.0.1'; RemoteAddress='127.0.0.1' }
        $emptyPeerResult = & $module { param($connection) Test-P3AccRelayPeerRow -ClientConnection $connection -RelayConnections @() -RelayPort 31000 } $fakeClientConnection
        Assert-P3AccOfflineTrue (-not $emptyPeerResult) 'empty-relay-peer-is-retryable-false'
        $udpOwner = [Diagnostics.Process]::GetCurrentProcess().Id
        $udpBypassResult = & $module { param($owner) Test-P3AccUdpBypass -UdpEndpoints @([pscustomobject]@{ OwningProcess=$owner }) -ClientIds @($owner) } $udpOwner
        Assert-P3AccOfflineTrue $udpBypassResult 'synthetic-client-udp-is-bypass'

        $clientScope = & $module {
            Get-P3AccTopologyClientScope `
                -RootIdentity ([pscustomobject]@{ ProcessId=101 }) `
                -Descendants @(
                    [pscustomobject]@{ Name='msedgewebview2.exe'; ProcessId=202 },
                    [pscustomobject]@{ Name='ffmpeg.exe'; ProcessId=303 }
                ) `
                -RelayIdentity ([pscustomobject]@{ ProcessId=404 })
        }
        Assert-P3AccOfflineEqual (@($clientScope.AppClientIds) -join ',') '101,202' 'topology-scope-includes-non-ffmpeg-descendant'
        Assert-P3AccOfflineEqual (@($clientScope.FfmpegClientIds) -join ',') '303' 'topology-scope-classifies-ffmpeg-descendant'
        Assert-P3AccOfflineEqual (@($clientScope.UdpForbiddenIds) -join ',') '101,202,303,404' 'topology-scope-includes-relay-in-udp-forbidden-set'
        $descendantUdpBypass = & $module { param($scope) Test-P3AccUdpBypass -UdpEndpoints @([pscustomobject]@{ OwningProcess=202 }) -ClientIds @($scope.UdpForbiddenIds) } $clientScope
        $relayUdpBypass = & $module { param($scope) Test-P3AccUdpBypass -UdpEndpoints @([pscustomobject]@{ OwningProcess=404 }) -ClientIds @($scope.UdpForbiddenIds) } $clientScope
        $foreignUdpBypass = & $module { param($scope) Test-P3AccUdpBypass -UdpEndpoints @([pscustomobject]@{ OwningProcess=505 }) -ClientIds @($scope.UdpForbiddenIds) } $clientScope
        Assert-P3AccOfflineTrue $descendantUdpBypass 'synthetic-non-ffmpeg-descendant-udp-is-bypass'
        Assert-P3AccOfflineTrue $relayUdpBypass 'synthetic-relay-udp-is-bypass'
        Assert-P3AccOfflineTrue (-not $foreignUdpBypass) 'synthetic-foreign-udp-is-allowed'

        $upstreamPort = Get-P3AccOfflineFreeTcpPort
        do { $relayPort = Get-P3AccOfflineFreeTcpPort } while ($relayPort -eq $upstreamPort)
        $upstreamReady = [IO.Path]::Combine($testRoot, 'socket-upstream.ready')
        $relayReady = [IO.Path]::Combine($testRoot, 'socket-relay.ready')
        $topologyChildScript = [IO.Path]::Combine($testRoot, 'topology-sampler-child.ps1')
        $upstreamProcess = $null
        $relayProcess = $null
        try {
            $upstreamProcess = Start-Process -FilePath $socketUpstreamFixture -ArgumentList @('server',[string]$upstreamPort,('"' + $upstreamReady + '"')) -WindowStyle Hidden -PassThru
            Wait-P3AccOfflineReadyFile $upstreamReady
            $relayProcess = Start-Process -FilePath $socketRelayFixture -ArgumentList @('relay',[string]$relayPort,[string]$upstreamPort,('"' + $relayReady + '"')) -WindowStyle Hidden -PassThru
            Wait-P3AccOfflineReadyFile $relayReady
            $childContent = @'
param(
    [string]$ModulePath, [string]$FfmpegFixture, [string]$HelperFixture,
    [string]$ControlRoot, [int]$RelayPort, [int]$UpstreamPort,
    [int]$RelayProcessId, [int64]$RelayStartedAtUtcTicks
)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
Import-Module -Name $ModulePath -Force -ErrorAction Stop
function Assert-ChildTrue { param([bool]$Condition, [string]$Code); if (-not $Condition) { throw ('P3ACC_CHILD_ASSERT:' + $Code) } }
function Assert-ChildThrows {
    param([scriptblock]$Body)
    $caught = ''
    try { & $Body | Out-Null } catch { $caught = [string]$_.Exception.Message }
    if ($caught -cne 'P3ACC_CONTROLLER_TOPOLOGY_INVALID') { throw 'P3ACC_CHILD_EXPECTED_TOPOLOGY_INVALID' }
}
function Wait-ChildReady {
    param([string]$Path)
    $deadline = [DateTime]::UtcNow.AddSeconds(10)
    while ([DateTime]::UtcNow -lt $deadline) {
        if (Test-Path -LiteralPath $Path -PathType Leaf) { return }
        Start-Sleep -Milliseconds 50
    }
    throw 'P3ACC_CHILD_READY_TIMEOUT'
}
function Wait-ChildComplete {
    param($Configuration, $AppState, $RelayState)
    $sample = $null
    $deadline = [DateTime]::UtcNow.AddSeconds(10)
    while ([DateTime]::UtcNow -lt $deadline) {
        try {
            $sample = Get-P3AccTopologySample -Configuration $Configuration -AppState $AppState -RelayState $RelayState
        } catch {
            if ($_.Exception.Message -cne 'P3ACC_CONTROLLER_TOPOLOGY_INVALID') { throw }
            $sample = $null
            Start-Sleep -Milliseconds 100
            continue
        }
        if ($sample.Complete) { return $sample }
        Start-Sleep -Milliseconds 100
    }
    throw 'P3ACC_CHILD_TOPOLOGY_TIMEOUT'
}
$ffmpegReady = [IO.Path]::Combine($ControlRoot, 'socket-ffmpeg.ready')
$secondFfmpegReady = [IO.Path]::Combine($ControlRoot, 'socket-ffmpeg-second.ready')
$restartedFfmpegReady = [IO.Path]::Combine($ControlRoot, 'socket-ffmpeg-restarted.ready')
$helperTcpReady = [IO.Path]::Combine($ControlRoot, 'socket-helper-tcp.ready')
$ffmpegProcess = $null
$secondFfmpegProcess = $null
$helperProcess = $null
$appClient = $null
try {
    $appClient = [Net.Sockets.TcpClient]::new()
    $appClient.Connect([Net.IPAddress]::Loopback, $RelayPort)
    $ffmpegProcess = Start-Process -FilePath $FfmpegFixture -ArgumentList @('client',[string]$RelayPort,('"' + $ffmpegReady + '"')) -WindowStyle Hidden -PassThru
    Wait-ChildReady $ffmpegReady
    $current = [Diagnostics.Process]::GetCurrentProcess()
    $appState = [pscustomobject]@{
        Identity = [pscustomobject]@{ ProcessId=$current.Id; StartedAtUtcTicks=$current.StartTime.ToUniversalTime().Ticks }
        ObservedDescendantIdentities = [Collections.ArrayList]::new()
        ObservedFfmpegIdentities = [Collections.ArrayList]::new()
    }
    $relayState = [pscustomobject]@{ Identity=[pscustomobject]@{ ProcessId=$RelayProcessId; StartedAtUtcTicks=$RelayStartedAtUtcTicks }; Port=$RelayPort }
    $configuration = [pscustomobject]@{ Upstream=[pscustomobject]@{ Host='127.0.0.1'; Port=$UpstreamPort } }

    $sample = Wait-ChildComplete $configuration $appState $relayState
    Assert-ChildTrue ($sample.AppOnlyRelay -and $sample.FfmpegOnlyRelay -and $sample.RelayOnlyUpstream -and $sample.NoUdpBypass) 'baseline-legs'
    Write-Output 'BASELINE_OK'

    $secondFfmpegProcess = Start-Process -FilePath $FfmpegFixture -ArgumentList @('client',[string]$RelayPort,('"' + $secondFfmpegReady + '"')) -WindowStyle Hidden -PassThru
    Wait-ChildReady $secondFfmpegReady
    $multipleSample = Get-P3AccTopologySample -Configuration $configuration -AppState $appState -RelayState $relayState
    Assert-ChildTrue (-not $multipleSample.Complete -and $multipleSample.RetryableUnstable) 'overlapping-ffmpeg'
    Write-Output 'OVERLAP_OK'
    $secondFfmpegProcess.Kill(); [void]$secondFfmpegProcess.WaitForExit(5000); $secondFfmpegProcess.Dispose(); $secondFfmpegProcess = $null

    $ffmpegProcess.Kill(); [void]$ffmpegProcess.WaitForExit(5000); $ffmpegProcess.Dispose(); $ffmpegProcess = $null
    $exitSample = Get-P3AccTopologySample -Configuration $configuration -AppState $appState -RelayState $relayState
    Assert-ChildTrue (-not $exitSample.Complete -and $exitSample.RetryableUnstable) 'exited-ffmpeg'
    Write-Output 'EXIT_OK'
    $ffmpegProcess = Start-Process -FilePath $FfmpegFixture -ArgumentList @('client',[string]$RelayPort,('"' + $restartedFfmpegReady + '"')) -WindowStyle Hidden -PassThru
    Wait-ChildReady $restartedFfmpegReady
    [void](Wait-ChildComplete $configuration $appState $relayState)
    Write-Output 'RESTART_OK'

    $helperProcess = Start-Process -FilePath $HelperFixture -ArgumentList @('client',[string]$UpstreamPort,('"' + $helperTcpReady + '"')) -WindowStyle Hidden -PassThru
    Wait-ChildReady $helperTcpReady
    Assert-ChildThrows { Get-P3AccTopologySample -Configuration $configuration -AppState $appState -RelayState $relayState }
    Write-Output 'DESC_TCP_DETECTED'
    $helperProcess.Kill(); [void]$helperProcess.WaitForExit(5000); $helperProcess.Dispose(); $helperProcess = $null
    [void](Wait-ChildComplete $configuration $appState $relayState)
    Write-Output 'DESC_TCP_OK'

} finally {
    if ($null -ne $appClient) { try { $appClient.Dispose() } catch { } }
    foreach ($process in @($helperProcess,$secondFfmpegProcess,$ffmpegProcess)) {
        if ($null -eq $process) { continue }
        try { if (-not $process.HasExited) { $process.Kill(); [void]$process.WaitForExit(5000) } } catch { }
        try { $process.Dispose() } catch { }
    }
}
'@
            [IO.File]::WriteAllText($topologyChildScript, $childContent, [Text.UTF8Encoding]::new($false))
            $childArguments = @(
                '-NoProfile','-NonInteractive','-ExecutionPolicy','Bypass','-File',('"' + $topologyChildScript + '"'),
                '-ModulePath',('"' + $modulePath + '"'),'-FfmpegFixture',('"' + $socketFfmpegFixture + '"'),
                '-HelperFixture',('"' + $socketHelperFixture + '"'),'-ControlRoot',('"' + $testRoot + '"'),
                '-RelayPort',[string]$relayPort,'-UpstreamPort',[string]$upstreamPort,
                '-RelayProcessId',[string]$relayProcess.Id,'-RelayStartedAtUtcTicks',[string]$relayProcess.StartTime.ToUniversalTime().Ticks
            )
            $child = Invoke-P3AccOfflineProcess -FilePath 'powershell.exe' -ArgumentList $childArguments -OutputDirectory $testRoot -TimeoutSeconds 180
            if ($child.ExitCode -ne 0) {
                $safeStage = (([string]$child.Stdout).Trim() -replace '[^A-Z0-9_\r\n]', '?') -replace '[\r\n]+', ','
                throw ('P3ACC_OFFLINE_TOPOLOGY_CHILD_FAILED:' + $safeStage)
            }
            Assert-P3AccOfflineEqual $child.ExitCode 0 'topology-child-exit'
            $expectedStages = "BASELINE_OK`r`nOVERLAP_OK`r`nEXIT_OK`r`nRESTART_OK`r`nDESC_TCP_DETECTED`r`nDESC_TCP_OK`r`n"
            Assert-P3AccOfflineEqual $child.Stdout $expectedStages 'topology-child-output'
            Assert-P3AccOfflineEqual $child.Stderr '' 'topology-child-stderr'
        } finally {
            foreach ($process in @($relayProcess,$upstreamProcess)) {
                if ($null -eq $process) { continue }
                try { if (-not $process.HasExited) { $process.Kill(); [void]$process.WaitForExit(5000) } } catch { }
                try { $process.Dispose() } catch { }
            }
        }
    }

    Invoke-P3AccOfflineTest 'endpoint-and-relay-announcement-contract' {
        $ipv4 = ConvertFrom-P3AccLoopbackEndpoint '127.0.0.1:7890'
        Assert-P3AccOfflineEqual $ipv4.Canonical '127.0.0.1:7890' 'ipv4-canonical'
        $ipv6 = ConvertFrom-P3AccLoopbackEndpoint '[::1]:1'
        Assert-P3AccOfflineEqual $ipv6.Port 1 'ipv6-port'
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_CONFIG_INVALID' { ConvertFrom-P3AccLoopbackEndpoint '127.0.0.1:0' }
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_CONFIG_INVALID' { ConvertFrom-P3AccLoopbackEndpoint '0.0.0.0:1234' }
        Assert-P3AccOfflineEqual (Read-P3AccRelayAnnouncementText "1234`r`n") 1234 'announcement-valid'
        Assert-P3AccOfflineTrue ($null -eq (Read-P3AccRelayAnnouncementText $null)) 'announcement-null-not-ready'
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_RELAY_FAILED' { Read-P3AccRelayAnnouncementText '1234' }
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_RELAY_FAILED' { Read-P3AccRelayAnnouncementText "0`n" }
    }

    Invoke-P3AccOfflineTest 'snapshot-schema-and-read-fail-closed' {
        $snapshot = New-P3AccOfflineFinalSnapshot
        Assert-P3AccOfflineTrue ([bool](Assert-P3AccSnapshotContract $snapshot)) 'snapshot-valid'
        Assert-P3AccOfflineTrue ([bool](Test-P3AccFinalContract $snapshot)) 'final-contract-valid'

        $singleSample = Copy-P3AccOfflineValue $snapshot
        $singleSample.resources.sampleCount = 1
        $singleSample.resources.windowDurationMs = 0
        $singleSample.resources.stableWindowProven = $false
        $singleSample.resources.cpuWithinTarget = $false
        $singleSample.resources.cpuTrend = 'INSUFFICIENT'
        $singleSample.resources.diskIoObserved = $false
        foreach ($name in @(
            'averageProcessReadBytesPerSecond','averageProcessWriteBytesPerSecond',
            'latterHalfProcessReadBytesPerSecond','latterHalfProcessWriteBytesPerSecond',
            'averageDiskWriteBytesPerSecond','latterHalfDiskWriteBytesPerSecond'
        )) { $singleSample.resources.$name = [double]0 }
        foreach ($name in @(
            'processCount','workingSet','privateBytes','threads','handles','goroutines',
            'heapAlloc','heapInUse','system','databaseWalBytes','processReadBytes','processWriteBytes',
            'dataRootPhysicalBytes','eventQueueCount','eventQueueItems','eventQueueBytes',
            'eventQueueItemCapacity','eventQueueByteCapacity'
        )) {
            $singleSample.resources.$name.peak = $singleSample.resources.$name.baseline
            $singleSample.resources.$name.latest = $singleSample.resources.$name.baseline
            $singleSample.resources.$name.delta = 0
            $singleSample.resources.$name.latterHalfDelta = 0
            $singleSample.resources.$name.latterHalfTrend = 'INSUFFICIENT'
        }
        Assert-P3AccOfflineTrue ([bool](Assert-P3AccSnapshotContract $singleSample)) 'snapshot-single-sample-trends-valid'
        Assert-P3AccOfflineTrue (-not (Test-P3AccPreFaultReady $singleSample)) 'snapshot-single-sample-prefault-not-ready'
        $singleSampleStable = Copy-P3AccOfflineValue $singleSample
        $singleSampleStable.resources.stableWindowProven = $true
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $singleSampleStable }
        $singleSampleCpu = Copy-P3AccOfflineValue $singleSample
        $singleSampleCpu.resources.cpuWithinTarget = $true
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $singleSampleCpu }
        $singleSampleDisk = Copy-P3AccOfflineValue $singleSample
        $singleSampleDisk.resources.diskIoObserved = $true
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $singleSampleDisk }
        $singleSampleLatterDelta = Copy-P3AccOfflineValue $singleSample
        $singleSampleLatterDelta.resources.workingSet.latterHalfDelta = 1
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $singleSampleLatterDelta }
        $singleSampleWrongTrend = Copy-P3AccOfflineValue $singleSample
        $singleSampleWrongTrend.resources.workingSet.latterHalfTrend = 'STABLE'
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $singleSampleWrongTrend }

        $extra = Copy-P3AccOfflineValue $snapshot
        Add-Member -InputObject $extra -NotePropertyName unexpected -NotePropertyValue $true
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $extra }

        $missing = Copy-P3AccOfflineValue $snapshot
        $missing.PSObject.Properties.Remove('checkpoint')
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $missing }

        $wrongType = Copy-P3AccOfflineValue $snapshot
        $wrongType.ui.ready = 1
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $wrongType }

        $wrongCount = Copy-P3AccOfflineValue $snapshot
        $wrongCount.ui.observationCount = 9
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $wrongCount }

        $wrongRelation = Copy-P3AccOfflineValue $snapshot
        $wrongRelation.runtime.crashInjected = $false
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $wrongRelation }

        $wrongMetric = Copy-P3AccOfflineValue $snapshot
        $wrongMetric.resources.workingSet.delta = 2
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $wrongMetric }

        $latestAbovePeak = Copy-P3AccOfflineValue $snapshot
        $latestAbovePeak.resources.workingSet.latest = 2
        $latestAbovePeak.resources.workingSet.delta = 1
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $latestAbovePeak }

        $thresholdBoundary = Copy-P3AccOfflineValue $snapshot
        $thresholdBoundary.resources.workingSet.latterHalfDelta = 1048576
        Assert-P3AccOfflineTrue ([bool](Assert-P3AccSnapshotContract $thresholdBoundary)) 'snapshot-trend-threshold-boundary-valid'
        $thresholdExceeded = Copy-P3AccOfflineValue $thresholdBoundary
        $thresholdExceeded.resources.workingSet.latterHalfDelta = 1048577
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $thresholdExceeded }
        $thresholdExceeded.resources.workingSet.latterHalfTrend = 'RISING'
        Assert-P3AccOfflineTrue ([bool](Assert-P3AccSnapshotContract $thresholdExceeded)) 'snapshot-trend-threshold-rising-valid'

        $wrongQueueTrend = Copy-P3AccOfflineValue $snapshot
        $wrongQueueTrend.resources.eventQueueItems.latterHalfTrend = 'FALLING'
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $wrongQueueTrend }

        $wrongWalTrend = Copy-P3AccOfflineValue $snapshot
        $wrongWalTrend.resources.databaseWalBytes.latterHalfTrend = 'STABLE'
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $wrongWalTrend }

        $legacyDiskMetric = Copy-P3AccOfflineValue $snapshot
        Add-Member -InputObject $legacyDiskMetric.resources -NotePropertyName diskWriteBytes -NotePropertyValue (New-P3AccOfflineMetric)
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $legacyDiskMetric }

        $missingCapacity = Copy-P3AccOfflineValue $snapshot
        $missingCapacity.resources.PSObject.Properties.Remove('eventQueueItemCapacity')
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $missingCapacity }

        $wrongObservationType = Copy-P3AccOfflineValue $snapshot
        $wrongObservationType.resources.databaseWalObserved = 1
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $wrongObservationType }

        $queueObservationMissing = Copy-P3AccOfflineValue $snapshot
        $queueObservationMissing.resources.eventQueueObserved = $false
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $queueObservationMissing }

        $stableWindowMissing = Copy-P3AccOfflineValue $snapshot
        $stableWindowMissing.resources.stableWindowProven = $false
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $stableWindowMissing }

        $cpuTargetMissing = Copy-P3AccOfflineValue $snapshot
        $cpuTargetMissing.resources.cpuWithinTarget = $false
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $cpuTargetMissing }

        $cpuTargetForged = Copy-P3AccOfflineValue $snapshot
        $cpuTargetForged.resources.averageCpuPercent = [double]10
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $cpuTargetForged }

        $wrongRate = Copy-P3AccOfflineValue $snapshot
        $wrongRate.resources.averageDiskWriteBytesPerSecond = [double]::NaN
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $wrongRate }

        $missingProcessRate = Copy-P3AccOfflineValue $snapshot
        $missingProcessRate.resources.averageProcessWriteBytesPerSecond = [double]0
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $missingProcessRate }

        $missingProcessLatterRate = Copy-P3AccOfflineValue $snapshot
        $missingProcessLatterRate.resources.latterHalfProcessReadBytesPerSecond = [double]0
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $missingProcessLatterRate }

        $missingPhysicalRate = Copy-P3AccOfflineValue $snapshot
        $missingPhysicalRate.resources.averageDiskWriteBytesPerSecond = [double]0
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $missingPhysicalRate }

        $missingPhysicalLatterRate = Copy-P3AccOfflineValue $snapshot
        $missingPhysicalLatterRate.resources.latterHalfDiskWriteBytesPerSecond = [double]0
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $missingPhysicalLatterRate }

        $negativeDiskDelta = Copy-P3AccOfflineValue $snapshot
        $negativeDiskDelta.resources.dataRootPhysicalBytes.baseline = 8192
        $negativeDiskDelta.resources.dataRootPhysicalBytes.peak = 8192
        $negativeDiskDelta.resources.dataRootPhysicalBytes.latest = 4096
        $negativeDiskDelta.resources.dataRootPhysicalBytes.delta = -4096
        $negativeDiskDelta.resources.dataRootPhysicalBytes.latterHalfDelta = 0
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $negativeDiskDelta }

        $negativeDiskLatterHalf = Copy-P3AccOfflineValue $snapshot
        $negativeDiskLatterHalf.resources.dataRootPhysicalBytes.latterHalfDelta = -1
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $negativeDiskLatterHalf }

        $diskPeakNotLatest = Copy-P3AccOfflineValue $snapshot
        $diskPeakNotLatest.resources.dataRootPhysicalBytes.peak = 8193
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $diskPeakNotLatest }

        $readOnlyDisk = Copy-P3AccOfflineValue $snapshot
        $readOnlyDisk.resources.dataRootPhysicalBytes.baseline = 8192
        $readOnlyDisk.resources.dataRootPhysicalBytes.peak = 8192
        $readOnlyDisk.resources.dataRootPhysicalBytes.latest = 8192
        $readOnlyDisk.resources.dataRootPhysicalBytes.delta = 0
        $readOnlyDisk.resources.dataRootPhysicalBytes.latterHalfDelta = 0
        $readOnlyDisk.resources.dataRootPhysicalBytes.latterHalfTrend = 'STABLE'
        $readOnlyDisk.resources.averageDiskWriteBytesPerSecond = [double]0
        $readOnlyDisk.resources.latterHalfDiskWriteBytesPerSecond = [double]0
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $readOnlyDisk }

        $queueCountMissing = Copy-P3AccOfflineValue $snapshot
        $queueCountMissing.resources.eventQueueCount.baseline = 0
        $queueCountMissing.resources.eventQueueCount.delta = 1
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $queueCountMissing }

        $capacityMissing = Copy-P3AccOfflineValue $snapshot
        foreach ($field in @('baseline','peak','latest')) { $capacityMissing.resources.eventQueueItemCapacity.$field = 0 }
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $capacityMissing }

        $capacityDrift = Copy-P3AccOfflineValue $snapshot
        $capacityDrift.resources.eventQueueItemCapacity.peak = 512
        $capacityDrift.resources.eventQueueItemCapacity.latest = 512
        $capacityDrift.resources.eventQueueItemCapacity.delta = 256
        $capacityDrift.resources.eventQueueItemCapacity.latterHalfDelta = 256
        $capacityDrift.resources.eventQueueItemCapacity.latterHalfTrend = 'RISING'
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $capacityDrift }

        $itemsOverflow = Copy-P3AccOfflineValue $snapshot
        $itemsOverflow.resources.eventQueueItems.peak = 257
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $itemsOverflow }

        $bytesOverflow = Copy-P3AccOfflineValue $snapshot
        $bytesOverflow.resources.eventQueueBytes.peak = 1048577
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Assert-P3AccSnapshotContract $bytesOverflow }

        $readRoot = New-P3AccOfflineRunRoot $testRoot
        try {
            $rootInfo = Assert-P3AccRunRoot $readRoot
            $resultDirectory = [IO.Path]::Combine($readRoot, 'result')
            [void][IO.Directory]::CreateDirectory($resultDirectory)
            $resultPath = [IO.Path]::Combine($resultDirectory, 'p3-acc.snapshot.json')
            $configuration = [pscustomobject]@{
                Root = $rootInfo.Path; RootCanonical = $rootInfo.Canonical; RootIdentity = $rootInfo.Identity
                ResultPath = $resultPath; PollIntervalMilliseconds = 5000
            }
            [IO.File]::WriteAllText($resultPath, ($snapshot | ConvertTo-Json -Depth 32 -Compress), [Text.UTF8Encoding]::new($false))
            $read = Read-P3AccSnapshot $configuration
            Assert-P3AccOfflineEqual $read.schema 'P3-ACC-001/v1' 'snapshot-read-valid'

            $errorSnapshot = Copy-P3AccOfflineValue $snapshot
            $errorSnapshot.stage = 'ERROR'
            $errorSnapshot.runtime.state = 'FINALIZING'
            $errorSnapshot.runtime.errorCode = 'CAPTURE_FINALIZE_FAILED'
            $errorSnapshot.runtime.finalizationProven = $false
            $errorSnapshot.capturedAt = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
            Assert-P3AccOfflineTrue ([bool](Assert-P3AccSnapshotContract $errorSnapshot)) 'terminal-error-snapshot-valid'
            [IO.File]::WriteAllText($resultPath, ($errorSnapshot | ConvertTo-Json -Depth 32 -Compress), [Text.UTF8Encoding]::new($false))
            $watch = [Diagnostics.Stopwatch]::StartNew()
            Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_FINAL_INVALID' {
                Wait-P3AccSnapshot -Configuration $configuration -TimeoutSeconds 10 -Predicate { param($value) $false }
            }
            $watch.Stop()
            Assert-P3AccOfflineTrue ($watch.ElapsedMilliseconds -lt 2000) 'terminal-error-wait-exceeded-immediate-bound'

            $stale = Copy-P3AccOfflineValue $snapshot
            $stale.capturedAt = [DateTimeOffset]::UtcNow.AddMinutes(-3).ToUnixTimeMilliseconds()
            [IO.File]::WriteAllText($resultPath, ($stale | ConvertTo-Json -Depth 32 -Compress), [Text.UTF8Encoding]::new($false))
            Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Read-P3AccSnapshot $configuration }

            [IO.File]::WriteAllBytes($resultPath, [byte[]](0xC3, 0x28))
            Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' { Read-P3AccSnapshot $configuration }
        } finally {
            Remove-Item -LiteralPath $readRoot -Recurse -Force -ErrorAction SilentlyContinue
        }
    }

    Invoke-P3AccOfflineTest 'final-contract-strong-invariants' {
        $mutations = @(
            { param($v) $v.ui.latencyP95Ms = 1000 },
            { param($v) $v.ui.observationCount = 9 },
            { param($v) $v.runtime.finalizationProven = $false },
            { param($v) $v.sessionManifest.status = 'interrupted' },
            { param($v) $v.sessionManifest.status = 'failed' },
            { param($v) $v.sessionManifest.status = 'Completed' },
            { param($v) $v.sessionManifest.status = 1 },
            { param($v) $v.sessionManifest.recordingStatus = 'completed' },
            { param($v) $v.sessionManifest.recordingStatus = 'failed' },
            { param($v) $v.sessionManifest.recordingStatus = 'Incomplete' },
            { param($v) $v.sessionManifest.recordingStatus = @('incomplete') },
            { param($v) $v.sessionManifest.ended = $false },
            { param($v) $v.database.activeSessionCount = 1 },
            { param($v) $v.gaps.networkRecorderMatched = $false },
            { param($v) $v.checkpoint.giftFoldsClosed = $false },
            { param($v) $v.mediaManifest.sequenceContinuous = $false },
            { param($v) $v.mediaManifest.fileCheckCount = 2 },
            { param($v) $v.resources.stableWindowProven = $false },
            { param($v) $v.resources.averageCpuPercent = [double]10 },
            { param($v) $v.resources.databaseWalObserved = $false },
            { param($v) $v.resources.diskIoObserved = $false },
            { param($v) $v.resources.eventQueueObserved = $false },
            { param($v) $v.resources.dataRootPhysicalBytes.delta = 0 },
            { param($v) $v.resources.eventQueueCount.baseline = 0 },
            { param($v) $v.resources.eventQueueItems.peak = 257 },
            { param($v) $v.resources.eventQueueBytes.peak = 1048577 }
        )
        foreach ($mutation in $mutations) {
            $candidate = Copy-P3AccOfflineValue (New-P3AccOfflineFinalSnapshot)
            & $mutation $candidate
            Assert-P3AccOfflineTrue (-not (Test-P3AccFinalContract $candidate)) 'final-contract-mutation-accepted'
        }
    }

    Invoke-P3AccOfflineTest 'report-schema-and-passed-invariants' {
        $default = New-P3AccControllerReport
        Assert-P3AccOfflineTrue ([bool](Assert-P3AccControllerReport $default)) 'default-failure-report'
        $passed = New-P3AccOfflinePassedReport
        Assert-P3AccOfflineTrue ([bool](Assert-P3AccControllerReport $passed)) 'passed-report-valid'

        $extra = Copy-P3AccOfflineValue $passed
        Add-Member -InputObject $extra -NotePropertyName unexpected -NotePropertyValue $false
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_INTERNAL_ERROR' { Assert-P3AccControllerReport $extra }

        $missingCapacity = Copy-P3AccOfflineValue $passed
        $missingCapacity.metrics.resources.PSObject.Properties.Remove('eventQueueByteCapacity')
        Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_INTERNAL_ERROR' { Assert-P3AccControllerReport $missingCapacity }

        $invalidCases = @(
            { param($v) $v.rootValidated = $false },
            { param($v) $v.code = 'P3ACC_CONTROLLER_TIMEOUT' },
            { param($v) $v.topology.beforeFaultSampleCount = 2 },
            { param($v) $v.topology.sampleCount = 7 },
            { param($v) $v.topology.noUdpBypass = $false },
            { param($v) $v.visual.sha256 = ('A' * 64) },
            { param($v) $v.visual.width = 299 },
            { param($v) $v.visual.height = 179 },
            { param($v) $v.visual.naturalAppTreeExited = $false },
            { param($v) $v.database.unlocked = $false },
            { param($v) $v.metrics.resources.sampleCount = 29 },
            { param($v) $v.metrics.resources.ownedProcessTreeCPUAvgPct = [double]10 },
            { param($v) $v.metrics.resources.ownedProcessTreeLatterHalfCPUAvgPct = [double]10 },
            { param($v) $v.metrics.resources.cpuTrend = 'RISING' },
            { param($v) $v.metrics.resources.databaseWalObserved = $false },
            { param($v) $v.metrics.resources.diskIoObserved = $false },
            { param($v) $v.metrics.resources.eventQueueObserved = $false },
            { param($v) $v.metrics.resources.averageDiskWriteBytesPerSecond = [double]-1 },
            { param($v) $v.metrics.resources.dataRootPhysicalBytes.peak = 8193 },
            { param($v) $v.metrics.resources.eventQueueItems.peak = 257 },
            { param($v) $v.metrics.resources.handles.latterHalfTrend = 'RISING' },
            { param($v) $v.metrics.uiLatency.p95Ms = 1000 },
            { param($v) $v.metrics.lineage.runtimeAttemptCount = 2 },
            { param($v) $v.cleanup.controlRootRemoved = $false }
        )
        foreach ($mutation in $invalidCases) {
            $candidate = Copy-P3AccOfflineValue $passed
            & $mutation $candidate
            Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_INTERNAL_ERROR' { Assert-P3AccControllerReport $candidate }
        }
    }

    Invoke-P3AccOfflineTest 'fresh-root-control-root-and-reparse-fail-closed' {
        $secret = $null
        $configuration = $null
        $target = $null
        try {
            $root = New-P3AccOfflineRunRoot $testRoot
            $secret = [IO.Path]::Combine($testRoot, 'config-input-' + [Guid]::NewGuid().ToString('N') + '.txt')
            New-P3AccOfflinePrivateFile $secret
            $configuration = New-P3AccControllerConfiguration `
                -AppExecutable ([IO.Path]::Combine($env:SystemRoot, 'System32\cmd.exe')) `
                -LauncherExecutable ([IO.Path]::Combine($env:SystemRoot, 'System32\whoami.exe')) `
                -RelayExecutable ([IO.Path]::Combine($env:SystemRoot, 'System32\where.exe')) `
                -Root $root -LiveUrlFile $secret -ClashUpstream '127.0.0.1:1' `
                -StartupTimeoutSeconds 30 -PreFaultTimeoutSeconds 660 `
                -FaultDetectionTimeoutSeconds 30 -RecoveryTimeoutSeconds 30 `
                -FinalizationTimeoutSeconds 60 -CloseTimeoutSeconds 5 `
                -ProbeTimeoutSeconds 2 -PollIntervalMilliseconds 250
            Assert-P3AccOfflineTrue (Test-Path -LiteralPath $configuration.ControlRoot -PathType Container) 'control-root-created'
            Assert-P3AccOfflineTrue (-not $configuration.ControlRoot.StartsWith(($configuration.Root.TrimEnd('\') + '\'), [StringComparison]::OrdinalIgnoreCase)) 'control-root-outside-run-root'
            Assert-P3AccOfflineEqual @(Get-ChildItem -LiteralPath $configuration.ControlRoot -Force).Count 0 'control-root-fresh'
            Assert-P3AccOfflineTrue (Get-Acl -LiteralPath $configuration.ControlRoot).AreAccessRulesProtected 'control-root-acl'

            $nonFreshRoot = New-P3AccOfflineRunRoot $testRoot
            try {
                [void][IO.Directory]::CreateDirectory([IO.Path]::Combine($nonFreshRoot, 'data'))
                Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_ROOT_INVALID' {
                    New-P3AccControllerConfiguration `
                        -AppExecutable ([IO.Path]::Combine($env:SystemRoot, 'System32\cmd.exe')) `
                        -LauncherExecutable ([IO.Path]::Combine($env:SystemRoot, 'System32\whoami.exe')) `
                        -RelayExecutable ([IO.Path]::Combine($env:SystemRoot, 'System32\where.exe')) `
                        -Root $nonFreshRoot -LiveUrlFile $secret -ClashUpstream '127.0.0.1:1' `
                        -StartupTimeoutSeconds 30 -PreFaultTimeoutSeconds 660 `
                        -FaultDetectionTimeoutSeconds 30 -RecoveryTimeoutSeconds 30 `
                        -FinalizationTimeoutSeconds 60 -CloseTimeoutSeconds 5 `
                        -ProbeTimeoutSeconds 2 -PollIntervalMilliseconds 250
                }
            } finally { Remove-Item -LiteralPath $nonFreshRoot -Recurse -Force -ErrorAction SilentlyContinue }

            $target = New-P3AccOfflineRunRoot $testRoot ('junction-target-' + [Guid]::NewGuid().ToString('N'))
            $link = [IO.Path]::Combine($testRoot, 'junction-link-' + [Guid]::NewGuid().ToString('N'))
            [void](New-Item -ItemType Junction -Path $link -Target $target -ErrorAction Stop)
            [void]$script:CreatedJunctions.Add($link)
            Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_ROOT_INVALID' { Assert-P3AccRunRoot $link }
            Remove-P3AccOfflineJunction $link
            [void]$script:CreatedJunctions.Remove($link)
            Assert-P3AccOfflineTrue (Test-Path -LiteralPath $target -PathType Container) 'junction-target-preserved'
            Remove-Item -LiteralPath $target -Recurse -Force
            $target = $null

            $target = [IO.Path]::Combine($testRoot, 'control-target-' + [Guid]::NewGuid().ToString('N'))
            [void][IO.Directory]::CreateDirectory($target)
            [IO.File]::WriteAllText([IO.Path]::Combine($target, 'marker.txt'), 'marker', [Text.UTF8Encoding]::new($false))
            Remove-Item -LiteralPath $configuration.ControlRoot -Force
            [void](New-Item -ItemType Junction -Path $configuration.ControlRoot -Target $target -ErrorAction Stop)
            [void]$script:CreatedJunctions.Add($configuration.ControlRoot)
            Assert-P3AccOfflineThrowsOneOf @('P3ACC_CONTROLLER_ROOT_INVALID','P3ACC_CONTROLLER_RELAY_FAILED') {
                Start-P3AccRelay -Configuration $configuration -ListenPort 0 -Phase 'reparse'
            }
            Assert-P3AccOfflineEqual @(Get-ChildItem -LiteralPath $target -Force).Count 1 'control-reparse-no-write-through'

            $report = New-P3AccControllerReport
            $cleaned = Complete-P3AccControllerCleanup -Configuration $configuration -AppState $null -RelayStates @() -Report $report
            Assert-P3AccOfflineTrue (-not $cleaned) 'control-reparse-cleanup-fails-closed'
            Assert-P3AccOfflineTrue (-not $report.cleanup.controlRootRemoved) 'control-reparse-not-removed'
            Assert-P3AccOfflineTrue (Test-Path -LiteralPath ([IO.Path]::Combine($target, 'marker.txt')) -PathType Leaf) 'control-target-not-deleted'
        } finally {
            if ($null -ne $configuration -and (Test-Path -LiteralPath $configuration.ControlRoot)) {
                $item = Get-Item -LiteralPath $configuration.ControlRoot -Force -ErrorAction SilentlyContinue
                if ($null -ne $item -and ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
                    Remove-P3AccOfflineJunction $configuration.ControlRoot
                    [void]$script:CreatedJunctions.Remove($configuration.ControlRoot)
                }
            }
            if ($null -ne $target -and (Test-Path -LiteralPath $target -PathType Container)) { Remove-Item -LiteralPath $target -Recurse -Force }
            Remove-P3AccOfflineConfigurationFixture $configuration
            if ($null -ne $secret -and (Test-Path -LiteralPath $secret)) { Remove-Item -LiteralPath $secret -Force }
        }
    }

    Invoke-P3AccOfflineTest 'relay-failed-start-cleans-process-and-artifact' {
        $configuration = $null
        try {
            $configuration = New-P3AccOfflineControllerConfiguration -Parent $testRoot -AppExecutable $silentFixture -LauncherExecutable $launcherFixture -RelayExecutable $relayFixture
            $configuration.StartupTimeoutSeconds = 2
            Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_RELAY_FAILED' {
                Start-P3AccRelay -Configuration $configuration -ListenPort 0 -Phase 'offline-fail'
            }
            $running = @(Get-P3AccOfflineExactProcesses $relayFixture)
            try { Assert-P3AccOfflineEqual $running.Count 0 'failed-relay-process-residual' }
            finally { Stop-P3AccOfflineExactProcesses $relayFixture }
            Assert-P3AccOfflineTrue (-not (Test-Path -LiteralPath ([IO.Path]::Combine($configuration.ControlRoot, 'relay-offline-fail.port')))) 'failed-relay-announcement-residual'
        } finally {
            Stop-P3AccOfflineExactProcesses $relayFixture
            Remove-P3AccOfflineConfigurationFixture $configuration
        }
    }

    Invoke-P3AccOfflineTest 'interactive-start-failures-clean-task-process-and-files' {
        Install-P3AccOfflineScheduledTaskMocks
        try {
            foreach ($mode in @('MissingIdentity','InvalidIdentity','ExitedIdentity','HelperIdentityWriteFailure')) {
                $configuration = $null
                try {
                    $configuration = New-P3AccOfflineControllerConfiguration -Parent $testRoot -AppExecutable $sleeperFixture -LauncherExecutable $launcherFixture -RelayExecutable $relayFixture
                    $configuration.StartupTimeoutSeconds = 1
                    $global:P3AccOfflineTaskMode = $mode
                    $global:P3AccOfflineTaskRegistered = $false
                    $global:P3AccOfflineTaskStartCount = 0
                    $global:P3AccOfflineTaskName = ''
                    $global:P3AccOfflinePreIdentityPath = [IO.Path]::Combine($configuration.ControlRoot, 'launcher.identity.json')
                    $global:P3AccOfflineHandshakePath = [IO.Path]::Combine($configuration.ControlRoot, 'launcher.handshake.json')
                    $global:P3AccOfflineLaunchPath = [IO.Path]::Combine($configuration.ControlRoot, 'launch.ps1')
                    $failureCode = $null
                    try { Start-P3AccInteractiveApp -Configuration $configuration -RelayPort 12345 | Out-Null }
                    catch { $failureCode = $_.Exception.Message }
                    Assert-P3AccOfflineTrue (@('P3ACC_CONTROLLER_APP_LAUNCH_FAILED','P3ACC_CONTROLLER_CLEANUP_FAILED') -ccontains $failureCode) "interactive-failure-code-$mode"
                    if ($failureCode -ceq 'P3ACC_CONTROLLER_CLEANUP_FAILED') {
                        Assert-P3AccOfflineTrue $configuration.AppLaunchCleanupUncertain "interactive-cleanup-uncertain-$mode"
                    }
                    $running = @(Get-P3AccOfflineExactProcesses $sleeperFixture)
                    try { Assert-P3AccOfflineEqual $running.Count 0 "interactive-process-residual-$mode" }
                    finally { Stop-P3AccOfflineExactProcesses $sleeperFixture }
                    $runningLauncher = @(Get-P3AccOfflineExactProcesses $launcherFixture)
                    try { Assert-P3AccOfflineEqual $runningLauncher.Count 0 "interactive-launcher-residual-$mode" }
                    finally { Stop-P3AccOfflineExactProcesses $launcherFixture }
                    Assert-P3AccOfflineTrue (-not $global:P3AccOfflineTaskRegistered) "interactive-task-residual-$mode"
                    Assert-P3AccOfflineTrue (-not (Test-Path -LiteralPath $global:P3AccOfflineLaunchPath)) "interactive-launch-residual-$mode"
                    Assert-P3AccOfflineTrue (-not (Test-Path -LiteralPath $global:P3AccOfflinePreIdentityPath)) "interactive-preidentity-residual-$mode"
                    Assert-P3AccOfflineTrue (-not (Test-Path -LiteralPath ($global:P3AccOfflinePreIdentityPath + '.tmp'))) "interactive-preidentity-temp-residual-$mode"
                    Assert-P3AccOfflineTrue (-not (Test-Path -LiteralPath $global:P3AccOfflineHandshakePath)) "interactive-handshake-residual-$mode"
                    Assert-P3AccOfflineTrue (-not (Test-Path -LiteralPath ($global:P3AccOfflineHandshakePath + '.tmp'))) "interactive-handshake-temp-residual-$mode"
                    if ($mode -ceq 'HelperIdentityWriteFailure') {
                        Assert-P3AccOfflineTrue (-not (Test-Path -LiteralPath $configuration.SecretPath)) 'helper-secret-not-delete-on-close'
                    }
                } finally {
                    Stop-P3AccOfflineExactProcesses $launcherFixture
                    Stop-P3AccOfflineExactProcesses $sleeperFixture
                    Remove-P3AccOfflineConfigurationFixture $configuration
                }
            }

            $swappedConfiguration = $null
            try {
                $swappedConfiguration = New-P3AccOfflineControllerConfiguration -Parent $testRoot -AppExecutable $sleeperFixture -LauncherExecutable $launcherFixture -RelayExecutable $relayFixture
                Remove-Item -LiteralPath $swappedConfiguration.SecretPath -Force
                New-P3AccOfflinePrivateFile -Path $swappedConfiguration.SecretPath -Text 'replacement-fixture-input'
                $global:P3AccOfflineTaskMode = 'MissingIdentity'
                $global:P3AccOfflineTaskRegistered = $false
                $global:P3AccOfflineTaskStartCount = 0
                $global:P3AccOfflineTaskName = ''
                $global:P3AccOfflinePreIdentityPath = [IO.Path]::Combine($swappedConfiguration.ControlRoot, 'launcher.identity.json')
                $global:P3AccOfflineHandshakePath = [IO.Path]::Combine($swappedConfiguration.ControlRoot, 'launcher.handshake.json')
                $global:P3AccOfflineLaunchPath = [IO.Path]::Combine($swappedConfiguration.ControlRoot, 'launch.ps1')
                Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_APP_LAUNCH_FAILED' {
                    Start-P3AccInteractiveApp -Configuration $swappedConfiguration -RelayPort 12345
                }
                Assert-P3AccOfflineEqual $global:P3AccOfflineTaskStartCount 0 'secret-swap-started-task'
                Assert-P3AccOfflineTrue (-not $global:P3AccOfflineTaskRegistered) 'secret-swap-task-residual'
                Assert-P3AccOfflineTrue (-not (Test-Path -LiteralPath $global:P3AccOfflineLaunchPath)) 'secret-swap-launch-residual'
                Assert-P3AccOfflineTrue (-not $swappedConfiguration.AppLaunchCleanupUncertain) 'secret-swap-cleanup-uncertain'
            } finally {
                Stop-P3AccOfflineExactProcesses $sleeperFixture
                Remove-P3AccOfflineConfigurationFixture $swappedConfiguration
            }

            $module = Get-Module P3AccController -ErrorAction Stop
            $naturalJob = & $module { New-P3AccAppJobState }
            $naturalProcess = Start-Process -FilePath $sleeperFixture -WindowStyle Hidden -PassThru -ErrorAction Stop
            try {
                [P3Acc.NativeJob]::AssignProcess($naturalJob.Handle, $naturalProcess.Handle)
                $naturalState = [pscustomobject]@{
                    Process = $naturalProcess
                    Identity = [pscustomobject]@{ ProcessId = $naturalProcess.Id; StartedAtUtcTicks = $naturalProcess.StartTime.ToUniversalTime().Ticks }
                    LauncherIdentity = $null
                    JobState = $naturalJob
                    ObservedDescendantIdentities = [Collections.ArrayList]::new()
                    ObservedFfmpegIdentities = [Collections.ArrayList]::new()
                }
                Assert-P3AccOfflineTrue ([P3Acc.NativeJob]::ContainsProcess($naturalJob.Handle, $naturalProcess.Handle)) 'job-natural-root-not-member'
                Assert-P3AccOfflineTrue (([P3Acc.NativeJob]::GetActiveProcesses($naturalJob.Handle)) -eq 1) 'job-natural-active-not-one'
                Assert-P3AccOfflineTrue (-not (Wait-P3AccNaturalAppTreeExit -AppState $naturalState -TimeoutSeconds 1)) 'job-natural-live-accepted'
                $naturalProcess.Kill()
                [void]$naturalProcess.WaitForExit(5000)
                Assert-P3AccOfflineTrue (Wait-P3AccNaturalAppTreeExit -AppState $naturalState -TimeoutSeconds 2) 'job-natural-zero-rejected'
                Assert-P3AccOfflineTrue ($naturalJob.Closed -and $naturalJob.GracefulExitConfirmed -and -not $naturalJob.ForcedEmptyConfirmed) 'job-natural-close-state'
            } finally {
                try {
                    $naturalProcess.Refresh()
                    if (-not $naturalProcess.HasExited) { $naturalProcess.Kill(); [void]$naturalProcess.WaitForExit(5000) }
                } catch { }
                $naturalProcess.Dispose()
                if (-not $naturalJob.Closed) { $naturalJob.Handle.Dispose() }
            }

            $treeJob = & $module { New-P3AccAppJobState }
            $treeRoot = Start-Process -FilePath $treeFixture -WindowStyle Hidden -PassThru -ErrorAction Stop
            $outsideProcess = Start-Process -FilePath $sleeperFixture -WindowStyle Hidden -PassThru -ErrorAction Stop
            try {
                [P3Acc.NativeJob]::AssignProcess($treeJob.Handle, $treeRoot.Handle)
                $outsideIdentity = [pscustomobject]@{ ProcessId = $outsideProcess.Id; StartedAtUtcTicks = $outsideProcess.StartTime.ToUniversalTime().Ticks }
                $historical = [Collections.ArrayList]::new()
                [void]$historical.Add($outsideIdentity)
                $treeState = [pscustomobject]@{
                    Process = $treeRoot
                    Identity = [pscustomobject]@{
                        ProcessId = $treeRoot.Id
                        StartedAtUtcTicks = $treeRoot.StartTime.ToUniversalTime().Ticks
                    }
                    LauncherIdentity = $null
                    JobState = $treeJob
                    ObservedDescendantIdentities = $historical
                    ObservedFfmpegIdentities = [Collections.ArrayList]::new()
                }
                $childDeadline = [DateTime]::UtcNow.AddSeconds(8)
                while ([DateTime]::UtcNow -lt $childDeadline -and [P3Acc.NativeJob]::GetActiveProcesses($treeJob.Handle) -lt 2) {
                    Start-Sleep -Milliseconds 100
                }
                Assert-P3AccOfflineTrue ([P3Acc.NativeJob]::GetActiveProcesses($treeJob.Handle) -ge 2) 'job-child-not-inherited'
                Assert-P3AccOfflineTrue (-not (Wait-P3AccNaturalAppTreeExit -AppState $treeState -TimeoutSeconds 1)) 'job-live-tree-accepted'
                $forced = & $module { param($state) Stop-P3AccAppTree $state } $treeState
                Assert-P3AccOfflineTrue ([bool]$forced) 'job-forced-stop-failed'
                Assert-P3AccOfflineTrue ($treeJob.Closed -and $treeJob.ForcedEmptyConfirmed) 'job-forced-close-state'
                Assert-P3AccOfflineTrue (Test-P3AccProcessIdentity $outsideIdentity) 'historical-outside-process-killed'
                Assert-P3AccOfflineEqual @(Get-P3AccOfflineExactProcesses $treeFixture).Count 0 'job-tree-residual'
            } finally {
                try {
                    $treeRoot.Refresh()
                    if (-not $treeRoot.HasExited) { $treeRoot.Kill(); [void]$treeRoot.WaitForExit(5000) }
                } catch { }
                $treeRoot.Dispose()
                try {
                    $outsideProcess.Refresh()
                    if (-not $outsideProcess.HasExited) { $outsideProcess.Kill(); [void]$outsideProcess.WaitForExit(5000) }
                } catch { }
                $outsideProcess.Dispose()
                if (-not $treeJob.Closed) { $treeJob.Handle.Dispose() }
                Stop-P3AccOfflineExactProcesses $treeFixture
            }
        } finally {
            Remove-P3AccOfflineScheduledTaskMocks
            foreach ($name in @('P3AccOfflineTaskMode','P3AccOfflineTaskRegistered','P3AccOfflineTaskStartCount','P3AccOfflineTaskName','P3AccOfflinePreIdentityPath','P3AccOfflineHandshakePath','P3AccOfflineLaunchPath')) {
                Remove-Variable -Name $name -Scope Global -ErrorAction SilentlyContinue
            }
        }
    }

    Invoke-P3AccOfflineTest 'evidence-ready-ack-strict-handshake' {
        $configuration = $null
        try {
            $configuration = New-P3AccOfflineControllerConfiguration -Parent $testRoot -AppExecutable $silentFixture -LauncherExecutable $launcherFixture -RelayExecutable $relayFixture
            $root = $configuration.Root
            $capturePath = [IO.Path]::Combine($root, 'p3-acc-safe-status.png')
            [byte[]]$captureBytes = @((0..255) + (0..255))
            [IO.File]::WriteAllBytes($capturePath, $captureBytes)
            $process = Get-Process -Id $PID
            $appState = [pscustomobject]@{ Identity = [pscustomobject]@{ ProcessId = $PID; StartedAtUtcTicks = $process.StartTime.ToUniversalTime().Ticks } }
            $process.Dispose()
            $capture = [pscustomobject]@{ Path = $capturePath; Width = 320; Height = 180; NonUniform = $true }
            $job = Start-Job -ScriptBlock {
                param($Root)
                $readyPath = [IO.Path]::Combine($Root, 'evidence.ready')
                $deadline = [DateTime]::UtcNow.AddSeconds(10)
                while ([DateTime]::UtcNow -lt $deadline -and -not (Test-Path -LiteralPath $readyPath -PathType Leaf)) { Start-Sleep -Milliseconds 50 }
                if (-not (Test-Path -LiteralPath $readyPath -PathType Leaf)) { return }
                $ready = Get-Content -LiteralPath $readyPath -Raw | ConvertFrom-Json
                $ack = [ordered]@{ schema = 'P3ACC-EVIDENCE-ACK/v1'; sha256 = [string]$ready.sha256 } | ConvertTo-Json -Compress
                [IO.File]::WriteAllText([IO.Path]::Combine($Root, 'evidence.ack'), $ack, [Text.UTF8Encoding]::new($false))
            } -ArgumentList $root
            try { $result = Wait-P3AccEvidenceAcknowledgement -Configuration $configuration -Capture $capture -AppState $appState -TimeoutSeconds 10 }
            finally { [void](Wait-Job -Job $job -Timeout 12); Receive-Job -Job $job -ErrorAction SilentlyContinue | Out-Null; Remove-Job -Job $job -Force -ErrorAction SilentlyContinue }
            Assert-P3AccOfflineTrue $result.Acknowledged 'evidence-acknowledged'
            Assert-P3AccOfflineTrue ($result.SHA256 -cmatch '^[0-9a-f]{64}$') 'evidence-hash-format'
            $ready = Get-Content -LiteralPath ([IO.Path]::Combine($root, 'evidence.ready')) -Raw | ConvertFrom-Json
            Assert-P3AccOfflineEqual @($ready.PSObject.Properties).Count 4 'evidence-ready-property-count'
            Assert-P3AccOfflineEqual $ready.schema 'P3ACC-EVIDENCE/v1' 'evidence-ready-schema'
            Assert-P3AccOfflineEqual $ready.width 320 'evidence-ready-width'
            Assert-P3AccOfflineEqual $ready.height 180 'evidence-ready-height'

            Remove-Item -LiteralPath ([IO.Path]::Combine($root, 'evidence.ready')) -Force
            Remove-Item -LiteralPath ([IO.Path]::Combine($root, 'evidence.ack')) -Force
            $badJob = Start-Job -ScriptBlock {
                param($Root)
                $readyPath = [IO.Path]::Combine($Root, 'evidence.ready')
                $deadline = [DateTime]::UtcNow.AddSeconds(10)
                while ([DateTime]::UtcNow -lt $deadline -and -not (Test-Path -LiteralPath $readyPath -PathType Leaf)) { Start-Sleep -Milliseconds 50 }
                if (-not (Test-Path -LiteralPath $readyPath -PathType Leaf)) { return }
                $ack = [ordered]@{ schema = 'P3ACC-EVIDENCE-ACK/v1'; sha256 = ('0' * 64); unexpected = $true } | ConvertTo-Json -Compress
                [IO.File]::WriteAllText([IO.Path]::Combine($Root, 'evidence.ack'), $ack, [Text.UTF8Encoding]::new($false))
            } -ArgumentList $root
            try {
                Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_VISUAL_FAILED' {
                    Wait-P3AccEvidenceAcknowledgement -Configuration $configuration -Capture $capture -AppState $appState -TimeoutSeconds 10
                }
            } finally { [void](Wait-Job -Job $badJob -Timeout 12); Receive-Job -Job $badJob -ErrorAction SilentlyContinue | Out-Null; Remove-Job -Job $badJob -Force -ErrorAction SilentlyContinue }
        } finally {
            Remove-P3AccOfflineConfigurationFixture $configuration
        }
    }

    Invoke-P3AccOfflineTest 'database-quick-check-empty-output-and-unlock' {
        $configuration = $null
        try {
            $configuration = New-P3AccOfflineControllerConfiguration -Parent $testRoot -AppExecutable $silentFixture -LauncherExecutable $launcherFixture -RelayExecutable $relayFixture
            [void][IO.Directory]::CreateDirectory($configuration.DataPath)
            $database = $configuration.DatabasePath
            $control = $configuration.ControlRoot
            $sqlite = $configuration.SQLiteExecutable
            $dbArgument = '"' + $database + '"'
            $sqlArgument = '"CREATE TABLE offline_fixture(value INTEGER);"'
            $create = Invoke-P3AccOfflineProcess -FilePath $sqlite -ArgumentList @($dbArgument, $sqlArgument) -OutputDirectory $control
            Assert-P3AccOfflineEqual $create.ExitCode 0 'database-create-exit'
            Assert-P3AccOfflineEqual $create.Stderr '' 'database-create-stderr'

            $verified = Test-P3AccDatabaseAfterClose $configuration
            Assert-P3AccOfflineTrue $verified.QuickCheckPassed 'database-quick-check'
            Assert-P3AccOfflineTrue $verified.Unlocked 'database-unlocked'
            Assert-P3AccOfflineTrue (-not (Test-Path -LiteralPath ([IO.Path]::Combine($control, 'sqlite.stdout')))) 'database-stdout-residual'
            Assert-P3AccOfflineTrue (-not (Test-Path -LiteralPath ([IO.Path]::Combine($control, 'sqlite.stderr')))) 'database-stderr-residual'

            $lock = [IO.File]::Open($database, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::ReadWrite)
            try { Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_FINAL_INVALID' { Test-P3AccDatabaseAfterClose $configuration } }
            finally { $lock.Dispose() }

            $configuration.SQLiteExecutable = $silentFixture
            Assert-P3AccOfflineThrows 'P3ACC_CONTROLLER_FINAL_INVALID' { Test-P3AccDatabaseAfterClose $configuration }
            Assert-P3AccOfflineTrue (-not (Test-Path -LiteralPath ([IO.Path]::Combine($control, 'sqlite.stdout')))) 'database-empty-stdout-residual'
            Assert-P3AccOfflineTrue (-not (Test-Path -LiteralPath ([IO.Path]::Combine($control, 'sqlite.stderr')))) 'database-empty-stderr-residual'
        } finally {
            Remove-P3AccOfflineConfigurationFixture $configuration
        }
    }

    Invoke-P3AccOfflineTest 'runner-cleanup-throw-still-emits-fixed-report' {
        $runnerRoot = [IO.Path]::Combine($testRoot, 'runner-' + [Guid]::NewGuid().ToString('N'))
        $stubDirectory = [IO.Path]::Combine($runnerRoot, 'p3-acc')
        [void][IO.Directory]::CreateDirectory($stubDirectory)
        $runnerCopy = [IO.Path]::Combine($runnerRoot, 'p3-acc-controller.ps1')
        Copy-Item -LiteralPath $runnerPath -Destination $runnerCopy
        $stubPath = [IO.Path]::Combine($stubDirectory, 'P3AccController.psm1')
        $stub = @'
function New-P3AccStubMetric { return [ordered]@{baseline=0;peak=0;latest=0;delta=0;latterHalfDelta=0;latterHalfTrend='INSUFFICIENT'} }
function New-P3AccControllerReport {
    return [ordered]@{
        schema='P3-ACC-CONTROLLER/v1';passed=$false;code='P3ACC_CONTROLLER_INTERNAL_ERROR'
        rootValidated=$false;secretValidated=$false;secretRemoved=$false;relayDynamicPort=$false;relayBaselineProbe=$false;relayFaultProven=$false;relaySamePortRestored=$false
        appInteractiveLaunch=$false;snapshotContract=$false;stableWindowObserved=$false;uiBaselineObserved=$false;crashRecoveryObserved=$false;networkFaultArmedObserved=$false;networkRecoveryObserved=$false;finalizationObserved=$false
        topology=[ordered]@{sampleCount=0;beforeFaultSampleCount=0;afterRecoverySampleCount=0;appOnlyRelay=$false;ffmpegOnlyRelay=$false;relayOnlyUpstream=$false;noUdpBypass=$false}
        visual=[ordered]@{safeCropCaptured=$false;sha256='';width=0;height=0;nonUniform=$false;evidenceAcknowledged=$false;wmCloseSent=$false;appExitCodeZero=$false;naturalAppTreeExited=$false}
        database=[ordered]@{quickCheckPassed=$false;unlocked=$false}
        metrics=[ordered]@{
            resources=[ordered]@{
                sampleCount=0;windowMs=0;ownedProcessTreeCPUAvgPct=0.0;ownedProcessTreeLatterHalfCPUAvgPct=0.0;cpuTrend='INSUFFICIENT'
                databaseWalObserved=$false;diskIoObserved=$false;eventQueueObserved=$false;averageProcessReadBytesPerSecond=0.0;averageProcessWriteBytesPerSecond=0.0;latterHalfProcessReadBytesPerSecond=0.0;latterHalfProcessWriteBytesPerSecond=0.0;averageDiskWriteBytesPerSecond=0.0;latterHalfDiskWriteBytesPerSecond=0.0
                processCount=(New-P3AccStubMetric);workingSet=(New-P3AccStubMetric);privateBytes=(New-P3AccStubMetric);threads=(New-P3AccStubMetric);handles=(New-P3AccStubMetric);goroutines=(New-P3AccStubMetric);heapAlloc=(New-P3AccStubMetric);heapInUse=(New-P3AccStubMetric);system=(New-P3AccStubMetric)
                databaseWalBytes=(New-P3AccStubMetric);processReadBytes=(New-P3AccStubMetric);processWriteBytes=(New-P3AccStubMetric);dataRootPhysicalBytes=(New-P3AccStubMetric);eventQueueCount=(New-P3AccStubMetric)
                eventQueueItems=(New-P3AccStubMetric);eventQueueBytes=(New-P3AccStubMetric);eventQueueItemCapacity=(New-P3AccStubMetric);eventQueueByteCapacity=(New-P3AccStubMetric)
            }
            uiLatency=[ordered]@{sampleCount=0;p95Ms=0;maxMs=0}
            lineage=[ordered]@{runtimeAttemptCount=0;progressRestartCount=0;mediaAttemptCount=0;committedAttemptCount=0;segmentCount=0;artifactCount=0;processCrashGapCount=0;recordingRestartGapCount=0;messageDisconnectGapCount=0}
        }
        cleanup=[ordered]@{taskRemoved=$false;appStopped=$false;relayStopped=$false;secretRemoved=$false;ephemeralRootRemoved=$false;controlRootRemoved=$false;zeroResidual=$false}
    }
}
function New-P3AccTopologyTracker {
    return [pscustomobject]@{SampleCount=5;BeforeFaultSampleCount=3;AfterRecoverySampleCount=2;AppOnlyRelay=$false;FfmpegOnlyRelay=$false;RelayOnlyUpstream=$false;NoUdpBypass=$false}
}
function Copy-P3AccTopologyTrackerToReport {
    param($Tracker,$Report)
    $Report.topology.sampleCount=[int]$Tracker.SampleCount
    $Report.topology.beforeFaultSampleCount=[int]$Tracker.BeforeFaultSampleCount
    $Report.topology.afterRecoverySampleCount=[int]$Tracker.AfterRecoverySampleCount
    $Report.topology.appOnlyRelay=[bool]$Tracker.AppOnlyRelay
    $Report.topology.ffmpegOnlyRelay=[bool]$Tracker.FfmpegOnlyRelay
    $Report.topology.relayOnlyUpstream=[bool]$Tracker.RelayOnlyUpstream
    $Report.topology.noUdpBypass=[bool]$Tracker.NoUdpBypass
}
function New-P3AccControllerConfiguration {
    if (Test-Path Env:P3ACC_LIVE_URL) { throw 'P3ACC_OFFLINE_AMBIENT_SECRET_PRESENT' }
    throw 'P3ACC_CONTROLLER_CONFIG_INVALID'
}
function Get-P3AccFailureCode { param($ErrorRecord) return [string]$ErrorRecord.Exception.Message }
function Complete-P3AccControllerCleanup { throw 'P3ACC_CONTROLLER_CLEANUP_FAILED' }
function Assert-P3AccControllerReport { return $true }
Export-ModuleMember -Function New-P3AccControllerReport,New-P3AccTopologyTracker,Copy-P3AccTopologyTrackerToReport,New-P3AccControllerConfiguration,Get-P3AccFailureCode,Complete-P3AccControllerCleanup,Assert-P3AccControllerReport
'@
        [IO.File]::WriteAllText($stubPath, $stub, [Text.UTF8Encoding]::new($false))
        $arguments = @(
            '-NoProfile','-NonInteractive','-ExecutionPolicy','Bypass','-File',('"' + $runnerCopy + '"'),
            '-AppExecutable','fixture-app','-LauncherExecutable','fixture-launcher','-RelayExecutable','fixture-relay','-ControllerRoot','fixture-root',
            '-LiveUrlFile','fixture-input','-ClashUpstream','127.0.0.1:1'
        )
        $ambientName = 'P3ACC_LIVE_URL'
        $ambientBefore = [Environment]::GetEnvironmentVariable($ambientName, [EnvironmentVariableTarget]::Process)
        $ambientCanary = 'P3ACC_AMBIENT_SECRET_CANARY_' + [Guid]::NewGuid().ToString('N')
        try {
            [Environment]::SetEnvironmentVariable($ambientName, $ambientCanary, [EnvironmentVariableTarget]::Process)
            $run = Invoke-P3AccOfflineProcess -FilePath 'powershell.exe' -ArgumentList $arguments -OutputDirectory $runnerRoot
        } finally {
            [Environment]::SetEnvironmentVariable($ambientName, $ambientBefore, [EnvironmentVariableTarget]::Process)
        }
        Assert-P3AccOfflineEqual $run.ExitCode 1 'runner-cleanup-exit'
        Assert-P3AccOfflineEqual $run.Stderr '' 'runner-cleanup-stderr'
        Assert-P3AccOfflineTrue (-not [string]::IsNullOrWhiteSpace($run.Stdout)) 'runner-cleanup-report-missing'
        Assert-P3AccOfflineTrue (-not $run.Stdout.Contains($ambientCanary)) 'runner-ambient-secret-stdout'
        Assert-P3AccOfflineTrue (-not $run.Stderr.Contains($ambientCanary)) 'runner-ambient-secret-stderr'
        $report = $run.Stdout | ConvertFrom-Json -ErrorAction Stop
        Assert-P3AccOfflineTrue (-not $report.passed) 'runner-cleanup-report-passed'
        Assert-P3AccOfflineEqual $report.code 'P3ACC_CONTROLLER_CLEANUP_FAILED' 'runner-cleanup-code'
        Assert-P3AccOfflineEqual $report.topology.sampleCount 5 'runner-topology-preserved-total'
        Assert-P3AccOfflineEqual $report.topology.beforeFaultSampleCount 3 'runner-topology-preserved-before'
        Assert-P3AccOfflineEqual $report.topology.afterRecoverySampleCount 2 'runner-topology-preserved-after'
        Assert-P3AccOfflineTrue ([bool](Assert-P3AccControllerReport $report)) 'runner-cleanup-fixed-schema'
    }
} finally {
    Remove-P3AccOfflineScheduledTaskMocks
    Stop-P3AccOfflineExactProcesses $relayFixture
    Stop-P3AccOfflineExactProcesses $launcherFixture
    Stop-P3AccOfflineExactProcesses $sleeperFixture
    Stop-P3AccOfflineExactProcesses $treeFixture
    Stop-P3AccOfflineExactProcesses $socketUpstreamFixture
    Stop-P3AccOfflineExactProcesses $socketRelayFixture
    Stop-P3AccOfflineExactProcesses $socketFfmpegFixture
    Stop-P3AccOfflineExactProcesses $socketHelperFixture
    foreach ($junction in @($script:CreatedJunctions)) {
        try { Remove-P3AccOfflineJunction $junction } catch { }
    }
    if (Test-Path -LiteralPath $testRoot -PathType Container) {
        $remainingReparse = @(Get-ChildItem -LiteralPath $testRoot -Force -ErrorAction SilentlyContinue | Where-Object { ($_.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0 })
        if ($remainingReparse.Count -eq 0) { Remove-Item -LiteralPath $testRoot -Recurse -Force -ErrorAction SilentlyContinue }
    }
}

if ($script:Failures.Count -gt 0) {
    foreach ($failure in $script:Failures) { Write-Output "DETAIL $failure" }
    Write-Output "P3ACC_OFFLINE_TESTS_FAILED count=$($script:Failures.Count) passed=$script:PassedCount"
    exit 1
}

Write-Output "P3ACC_OFFLINE_TESTS_OK passed=$script:PassedCount"
exit 0
