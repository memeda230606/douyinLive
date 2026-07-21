[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$AppExecutable,
    [Parameter(Mandatory = $true)][string]$LauncherExecutable,
    [Parameter(Mandatory = $true)][string]$RelayExecutable,
    [Parameter(Mandatory = $true)][string]$ControllerRoot,
    [Parameter(Mandatory = $true)][string]$LiveUrlFile,
    [Parameter(Mandatory = $true)][string]$ClashUpstream,
    [int]$StartupTimeoutSeconds = 180,
    [int]$PreFaultTimeoutSeconds = 1800,
    [int]$FaultDetectionTimeoutSeconds = 300,
    [int]$RecoveryTimeoutSeconds = 600,
    [int]$FinalizationTimeoutSeconds = 21600,
    [int]$CloseTimeoutSeconds = 30,
    [int]$EvidenceAckTimeoutSeconds = 300,
    [int]$ProbeTimeoutSeconds = 10,
    [int]$PollIntervalMilliseconds = 1000
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
Remove-Item Env:P3ACC_LIVE_URL -ErrorAction SilentlyContinue

function New-P3AccFallbackMetric {
    return [ordered]@{ baseline = 0; peak = 0; latest = 0; delta = 0; latterHalfDelta = 0; latterHalfTrend = 'INSUFFICIENT' }
}

function New-P3AccFallbackReport {
    return [ordered]@{
        schema = 'P3-ACC-CONTROLLER/v1'; passed = $false; code = 'P3ACC_CONTROLLER_INTERNAL_ERROR'
        rootValidated = $false; secretValidated = $false; secretRemoved = $false
        relayDynamicPort = $false; relayBaselineProbe = $false; relayFaultProven = $false; relaySamePortRestored = $false
        appInteractiveLaunch = $false; snapshotContract = $false; stableWindowObserved = $false
        uiBaselineObserved = $false; crashRecoveryObserved = $false; networkFaultArmedObserved = $false
        networkRecoveryObserved = $false; finalizationObserved = $false
        topology = [ordered]@{ sampleCount = 0; beforeFaultSampleCount = 0; afterRecoverySampleCount = 0; appOnlyRelay = $false; ffmpegOnlyRelay = $false; relayOnlyUpstream = $false; noUdpBypass = $false }
        visual = [ordered]@{ safeCropCaptured = $false; sha256 = ''; width = 0; height = 0; nonUniform = $false; evidenceAcknowledged = $false; wmCloseSent = $false; appExitCodeZero = $false; naturalAppTreeExited = $false }
        database = [ordered]@{ quickCheckPassed = $false; unlocked = $false }
        metrics = [ordered]@{
            resources = [ordered]@{
                sampleCount = 0; windowMs = 0; ownedProcessTreeCPUAvgPct = 0.0
                ownedProcessTreeLatterHalfCPUAvgPct = 0.0; cpuTrend = 'INSUFFICIENT'
                databaseWalObserved = $false; diskIoObserved = $false; eventQueueObserved = $false
                averageProcessReadBytesPerSecond = 0.0; averageProcessWriteBytesPerSecond = 0.0
                latterHalfProcessReadBytesPerSecond = 0.0; latterHalfProcessWriteBytesPerSecond = 0.0
                averageDiskWriteBytesPerSecond = 0.0; latterHalfDiskWriteBytesPerSecond = 0.0
                processCount = New-P3AccFallbackMetric; workingSet = New-P3AccFallbackMetric
                privateBytes = New-P3AccFallbackMetric; threads = New-P3AccFallbackMetric
                handles = New-P3AccFallbackMetric; goroutines = New-P3AccFallbackMetric
                heapAlloc = New-P3AccFallbackMetric; heapInUse = New-P3AccFallbackMetric
                system = New-P3AccFallbackMetric
                databaseWalBytes = New-P3AccFallbackMetric; processReadBytes = New-P3AccFallbackMetric
                processWriteBytes = New-P3AccFallbackMetric; dataRootPhysicalBytes = New-P3AccFallbackMetric
                eventQueueCount = New-P3AccFallbackMetric
                eventQueueItems = New-P3AccFallbackMetric; eventQueueBytes = New-P3AccFallbackMetric
                eventQueueItemCapacity = New-P3AccFallbackMetric; eventQueueByteCapacity = New-P3AccFallbackMetric
            }
            uiLatency = [ordered]@{ sampleCount = 0; p95Ms = 0; maxMs = 0 }
            lineage = [ordered]@{
                runtimeAttemptCount = 0; progressRestartCount = 0; mediaAttemptCount = 0
                committedAttemptCount = 0; segmentCount = 0; artifactCount = 0
                processCrashGapCount = 0; recordingRestartGapCount = 0; messageDisconnectGapCount = 0
            }
        }
        cleanup = [ordered]@{ taskRemoved = $false; appStopped = $false; relayStopped = $false; secretRemoved = $false; ephemeralRootRemoved = $false; controlRootRemoved = $false; zeroResidual = $false }
    }
}

$modulePath = Join-Path $PSScriptRoot 'p3-acc\P3AccController.psm1'
try {
    Import-Module -Name $modulePath -Force -ErrorAction Stop
    $report = New-P3AccControllerReport
} catch {
    $report = New-P3AccFallbackReport
    $report | ConvertTo-Json -Depth 5 -Compress
    exit 1
}

$configuration = $null
$appState = $null
$activeRelay = $null
$relayStates = [Collections.ArrayList]::new()
$topology = New-P3AccTopologyTracker
$exitCode = 1

try {
    if (Test-Path Env:P3ACC_LIVE_URL) { throw 'P3ACC_CONTROLLER_CONFIG_INVALID' }
    if ($EvidenceAckTimeoutSeconds -lt 30 -or $EvidenceAckTimeoutSeconds -gt 1800) { throw 'P3ACC_CONTROLLER_CONFIG_INVALID' }
    $configuration = New-P3AccControllerConfiguration `
        -AppExecutable $AppExecutable -LauncherExecutable $LauncherExecutable -RelayExecutable $RelayExecutable `
        -Root $ControllerRoot -LiveUrlFile $LiveUrlFile -ClashUpstream $ClashUpstream `
        -StartupTimeoutSeconds $StartupTimeoutSeconds -PreFaultTimeoutSeconds $PreFaultTimeoutSeconds `
        -FaultDetectionTimeoutSeconds $FaultDetectionTimeoutSeconds -RecoveryTimeoutSeconds $RecoveryTimeoutSeconds `
        -FinalizationTimeoutSeconds $FinalizationTimeoutSeconds -CloseTimeoutSeconds $CloseTimeoutSeconds `
        -ProbeTimeoutSeconds $ProbeTimeoutSeconds -PollIntervalMilliseconds $PollIntervalMilliseconds
    $report.rootValidated = $true
    $report.secretValidated = $true

    $activeRelay = Start-P3AccRelay -Configuration $configuration -ListenPort 0 -Phase 'initial'
    [void]$relayStates.Add($activeRelay)
    $report.relayDynamicPort = $activeRelay.Port -ge 1 -and $activeRelay.Port -le 65535
    if (-not $report.relayDynamicPort -or -not (Test-P3AccRelayProbe -Port $activeRelay.Port -TimeoutSeconds $configuration.ProbeTimeoutSeconds)) {
        throw 'P3ACC_CONTROLLER_PROBE_FAILED'
    }
    $report.relayBaselineProbe = $true

    $appState = Start-P3AccInteractiveApp -Configuration $configuration -RelayPort $activeRelay.Port
    if (-not $appState.TaskRemoved) { throw 'P3ACC_CONTROLLER_APP_LAUNCH_FAILED' }
    $report.appInteractiveLaunch = $true
    $report.secretRemoved = -not (Test-Path -LiteralPath $configuration.SecretPath)
    if (-not $report.secretRemoved) { throw 'P3ACC_CONTROLLER_APP_LAUNCH_FAILED' }

    $preFaultDeadline = [DateTime]::UtcNow.AddSeconds($configuration.PreFaultTimeoutSeconds)
    $armedSnapshot = $null
    while ([DateTime]::UtcNow -lt $preFaultDeadline) {
        $snapshot = Read-P3AccSnapshot $configuration
        if ($null -ne $snapshot) {
            $report.snapshotContract = $true
            if ($snapshot.stage -ceq 'ERROR' -or ($snapshot.stage -ceq 'OFFLINE' -and -not $snapshot.runtime.networkFaultArmed)) {
                throw 'P3ACC_CONTROLLER_FINAL_INVALID'
            }
            if (Test-P3AccTopologySnapshotEligible $snapshot) {
                $sample = Get-P3AccTopologySample -Configuration $configuration -AppState $appState -RelayState $activeRelay
                $confirmedSnapshot = Read-P3AccSnapshot $configuration
                [void](Add-P3AccTopologySample -Tracker $topology -Sample $sample -Snapshot $snapshot -ConfirmedSnapshot $confirmedSnapshot)
                Copy-P3AccTopologyTrackerToReport -Tracker $topology -Report $report
                if ((Test-P3AccPreFaultReady $confirmedSnapshot) -and (Test-P3AccTopologyPhaseReady $topology)) {
                    $armedSnapshot = $confirmedSnapshot
                    break
                }
            } elseif ($topology.BeforeFaultSampleCount -gt 0) {
                Reset-P3AccTopologyPhase -Tracker $topology
                Copy-P3AccTopologyTrackerToReport -Tracker $topology -Report $report
            }
        }
        Start-Sleep -Milliseconds $configuration.PollIntervalMilliseconds
    }
    if ($null -eq $armedSnapshot) { throw 'P3ACC_CONTROLLER_TIMEOUT' }
    $report.stableWindowObserved = $armedSnapshot.resources.stableWindowProven -and $armedSnapshot.resources.cpuWithinTarget -and [int64]$armedSnapshot.resources.windowDurationMs -ge 600000
    $report.uiBaselineObserved = $armedSnapshot.ui.ready -and $armedSnapshot.ui.recordingSeen -and $armedSnapshot.ui.progressAdvanced -and $armedSnapshot.ui.timelineSeen
    $report.crashRecoveryObserved = $armedSnapshot.runtime.crashInjected -and $armedSnapshot.runtime.recoveryProven -and $armedSnapshot.ui.reconnectingSeen -and $armedSnapshot.ui.recoveredSeen -and $armedSnapshot.gaps.crashRecoveryMatched
    $report.networkFaultArmedObserved = $armedSnapshot.runtime.networkFaultArmed
    if (-not $report.stableWindowObserved -or -not $report.uiBaselineObserved -or -not $report.crashRecoveryObserved -or -not $report.networkFaultArmedObserved) {
        throw 'P3ACC_CONTROLLER_SNAPSHOT_INVALID'
    }
    if (-not (Test-P3AccRelayProbe -Port $activeRelay.Port -TimeoutSeconds $configuration.ProbeTimeoutSeconds)) {
        throw 'P3ACC_CONTROLLER_PROBE_FAILED'
    }
    if (-not (Test-P3AccProcessIdentity $activeRelay.Identity)) { throw 'P3ACC_CONTROLLER_RELAY_FAILED' }
    if (-not (Stop-P3AccExactProcess $activeRelay)) { throw 'P3ACC_CONTROLLER_RELAY_FAILED' }
    if (Test-P3AccRelayProbe -Port $activeRelay.Port -TimeoutSeconds $configuration.ProbeTimeoutSeconds) {
        throw 'P3ACC_CONTROLLER_PROBE_FAILED'
    }
    $report.relayFaultProven = $true

    $faultSnapshot = Wait-P3AccSnapshot -Configuration $configuration -TimeoutSeconds $configuration.FaultDetectionTimeoutSeconds -Predicate { param($value) Test-P3AccFaultObserved $value }
    if ($null -eq $faultSnapshot) { throw 'P3ACC_CONTROLLER_TIMEOUT' }

    $restartPort = $activeRelay.Port
    $activeRelay = Start-P3AccRelay -Configuration $configuration -ListenPort $restartPort -Phase 'restored'
    [void]$relayStates.Add($activeRelay)
    if ($activeRelay.Port -ne $restartPort -or -not (Test-P3AccRelayProbe -Port $activeRelay.Port -TimeoutSeconds $configuration.ProbeTimeoutSeconds)) {
        throw 'P3ACC_CONTROLLER_PROBE_FAILED'
    }
    $report.relaySamePortRestored = $true

    $recoveryDeadline = [DateTime]::UtcNow.AddSeconds($configuration.RecoveryTimeoutSeconds)
    $recoveredSnapshot = $null
    while ([DateTime]::UtcNow -lt $recoveryDeadline) {
        $snapshot = Read-P3AccSnapshot $configuration
        if ($null -ne $snapshot -and (Test-P3AccTopologySnapshotEligible $snapshot -AfterRecovery)) {
            $sample = Get-P3AccTopologySample -Configuration $configuration -AppState $appState -RelayState $activeRelay
            $confirmedSnapshot = Read-P3AccSnapshot $configuration
            [void](Add-P3AccTopologySample -Tracker $topology -Sample $sample -Snapshot $snapshot -ConfirmedSnapshot $confirmedSnapshot -AfterRecovery)
            Copy-P3AccTopologyTrackerToReport -Tracker $topology -Report $report
            if ((Test-P3AccNetworkRecovered $confirmedSnapshot) -and (Test-P3AccTopologyPhaseReady $topology -AfterRecovery)) { $recoveredSnapshot = $confirmedSnapshot; break }
        } elseif ($topology.AfterRecoverySampleCount -gt 0) {
            Reset-P3AccTopologyPhase -Tracker $topology -AfterRecovery
            Copy-P3AccTopologyTrackerToReport -Tracker $topology -Report $report
        }
        Start-Sleep -Milliseconds $configuration.PollIntervalMilliseconds
    }
    if ($null -eq $recoveredSnapshot) { throw 'P3ACC_CONTROLLER_TIMEOUT' }
    $report.networkRecoveryObserved = $true

    $finalSnapshot = Wait-P3AccSnapshot -Configuration $configuration -TimeoutSeconds $configuration.FinalizationTimeoutSeconds -Predicate { param($value) Test-P3AccFinalContract $value }
    if ($null -eq $finalSnapshot -or -not (Test-P3AccFinalContract $finalSnapshot)) { throw 'P3ACC_CONTROLLER_FINAL_INVALID' }
    $allUI = $finalSnapshot.ui.ready -and $finalSnapshot.ui.recordingSeen -and $finalSnapshot.ui.progressAdvanced -and $finalSnapshot.ui.timelineSeen -and $finalSnapshot.ui.reconnectingSeen -and $finalSnapshot.ui.recoveredSeen -and $finalSnapshot.ui.networkReconnectingSeen -and $finalSnapshot.ui.networkRecoveredSeen -and $finalSnapshot.ui.offlineSeen -and $finalSnapshot.ui.finalizedSeen
    if (-not $allUI -or -not $finalSnapshot.runtime.finalizationProven -or -not $finalSnapshot.resources.stableWindowProven -or -not $finalSnapshot.resources.cpuWithinTarget -or
        -not $finalSnapshot.resources.databaseWalObserved -or -not $finalSnapshot.resources.diskIoObserved -or -not $finalSnapshot.resources.eventQueueObserved) {
        throw 'P3ACC_CONTROLLER_FINAL_INVALID'
    }
    $report.finalizationObserved = $true

    $report.metrics.resources.sampleCount = [int]$finalSnapshot.resources.sampleCount
    $report.metrics.resources.windowMs = [int64]$finalSnapshot.resources.windowDurationMs
    $report.metrics.resources.ownedProcessTreeCPUAvgPct = [double]$finalSnapshot.resources.averageCpuPercent
    $report.metrics.resources.ownedProcessTreeLatterHalfCPUAvgPct = [double]$finalSnapshot.resources.latterHalfAverageCpuPercent
    $report.metrics.resources.cpuTrend = [string]$finalSnapshot.resources.cpuTrend
    foreach ($name in @('databaseWalObserved','diskIoObserved','eventQueueObserved')) {
        $report.metrics.resources.$name = [bool]$finalSnapshot.resources.$name
    }
    foreach ($name in @('averageProcessReadBytesPerSecond','averageProcessWriteBytesPerSecond','latterHalfProcessReadBytesPerSecond','latterHalfProcessWriteBytesPerSecond','averageDiskWriteBytesPerSecond','latterHalfDiskWriteBytesPerSecond')) {
        $report.metrics.resources.$name = [double]$finalSnapshot.resources.$name
    }
    foreach ($name in @('processCount','workingSet','privateBytes','threads','handles','goroutines','heapAlloc','heapInUse','system','databaseWalBytes','processReadBytes','processWriteBytes','dataRootPhysicalBytes','eventQueueCount','eventQueueItems','eventQueueBytes','eventQueueItemCapacity','eventQueueByteCapacity')) {
        $sourceMetric = $finalSnapshot.resources.$name
        $targetMetric = $report.metrics.resources.$name
        $targetMetric.baseline = [int64]$sourceMetric.baseline
        $targetMetric.peak = [int64]$sourceMetric.peak
        $targetMetric.latest = [int64]$sourceMetric.latest
        $targetMetric.delta = [int64]$sourceMetric.delta
        $targetMetric.latterHalfDelta = [int64]$sourceMetric.latterHalfDelta
        $targetMetric.latterHalfTrend = [string]$sourceMetric.latterHalfTrend
    }
    $report.metrics.uiLatency.sampleCount = [int]$finalSnapshot.ui.latencySampleCount
    $report.metrics.uiLatency.p95Ms = [int64]$finalSnapshot.ui.latencyP95Ms
    $report.metrics.uiLatency.maxMs = [int64]$finalSnapshot.ui.latencyMaxMs
    $report.metrics.lineage.runtimeAttemptCount = [int]$finalSnapshot.runtime.attemptCount
    $report.metrics.lineage.progressRestartCount = [int]$finalSnapshot.progress.restartCount
    $report.metrics.lineage.mediaAttemptCount = [int]$finalSnapshot.mediaManifest.attemptCount
    $report.metrics.lineage.committedAttemptCount = [int]$finalSnapshot.mediaManifest.committedAttemptCount
    $report.metrics.lineage.segmentCount = [int]$finalSnapshot.mediaManifest.segmentCount
    $report.metrics.lineage.artifactCount = [int]$finalSnapshot.mediaManifest.artifactCount
    $report.metrics.lineage.processCrashGapCount = [int]$finalSnapshot.gaps.processCrash
    $report.metrics.lineage.recordingRestartGapCount = [int]$finalSnapshot.gaps.recordingRestart
    $report.metrics.lineage.messageDisconnectGapCount = [int]$finalSnapshot.gaps.messageDisconnect
    if ([int]$report.metrics.lineage.runtimeAttemptCount -lt 3 -or [int]$report.metrics.lineage.progressRestartCount -lt 2 -or
        [int]$report.metrics.lineage.mediaAttemptCount -lt 3 -or [int]$report.metrics.lineage.committedAttemptCount -lt 3 -or
        [int]$report.metrics.lineage.processCrashGapCount -lt 1 -or [int]$report.metrics.lineage.recordingRestartGapCount -lt 1 -or
        [int]$report.metrics.lineage.messageDisconnectGapCount -lt 1) { throw 'P3ACC_CONTROLLER_FINAL_INVALID' }

    if ([int]$report.metrics.resources.sampleCount -lt 30 -or [int64]$report.metrics.resources.windowMs -lt 600000 -or
        [double]$report.metrics.resources.ownedProcessTreeCPUAvgPct -ge 10 -or [double]$report.metrics.resources.ownedProcessTreeLatterHalfCPUAvgPct -ge 10 -or
        @('STABLE','FALLING') -cnotcontains $report.metrics.resources.cpuTrend -or [int]$report.metrics.uiLatency.sampleCount -lt 1 -or
        [int64]$report.metrics.uiLatency.p95Ms -ge 1000 -or [int64]$report.metrics.uiLatency.p95Ms -gt [int64]$report.metrics.uiLatency.maxMs) { throw 'P3ACC_CONTROLLER_FINAL_INVALID' }
    foreach ($name in @('processCount','workingSet','privateBytes','threads','handles','goroutines','heapAlloc','heapInUse','system')) {
        if (@('STABLE','FALLING') -cnotcontains $report.metrics.resources.$name.latterHalfTrend) { throw 'P3ACC_CONTROLLER_FINAL_INVALID' }
    }

    [void](Register-P3AccCurrentDescendantIdentities -AppState $appState)
    $visual = Invoke-P3AccInteractiveVisualAcceptance -Configuration $configuration -AppState $appState `
        -EvidenceTimeoutSeconds $EvidenceAckTimeoutSeconds -CloseTimeoutSeconds $configuration.CloseTimeoutSeconds
    if (Test-P3AccProcessIdentity $appState.Identity) { throw 'P3ACC_CONTROLLER_VISUAL_FAILED' }
    $report.visual.safeCropCaptured = $true
    $report.visual.naturalAppTreeExited = Wait-P3AccNaturalAppTreeExit -AppState $appState -TimeoutSeconds $configuration.CloseTimeoutSeconds
    if (-not $report.visual.naturalAppTreeExited) { throw 'P3ACC_CONTROLLER_VISUAL_FAILED' }

    $report.visual.sha256 = [string]$visual.sha256
    $report.visual.width = [int]$visual.width
    $report.visual.height = [int]$visual.height
    $report.visual.nonUniform = [bool]$visual.nonUniform
    $report.visual.evidenceAcknowledged = [bool]$visual.evidenceAcknowledged
    $report.visual.wmCloseSent = [bool]$visual.wmCloseSent
    $report.visual.appExitCodeZero = [bool]$visual.appExitCodeZero

    $database = Test-P3AccDatabaseAfterClose -Configuration $configuration
    $report.database.quickCheckPassed = [bool]$database.QuickCheckPassed
    $report.database.unlocked = [bool]$database.Unlocked
    if (-not $report.database.quickCheckPassed -or -not $report.database.unlocked) { throw 'P3ACC_CONTROLLER_FINAL_INVALID' }

    Copy-P3AccTopologyTrackerToReport -Tracker $topology -Report $report
    if ($topology.BeforeFaultSampleCount -lt 3 -or $topology.AfterRecoverySampleCount -lt 3 -or -not $topology.AppOnlyRelay -or -not $topology.FfmpegOnlyRelay -or -not $topology.RelayOnlyUpstream -or -not $topology.NoUdpBypass) {
        throw 'P3ACC_CONTROLLER_TOPOLOGY_INVALID'
    }
    $report.passed = $true
    $report.code = 'OK'
    $exitCode = 0
} catch {
    try { Copy-P3AccTopologyTrackerToReport -Tracker $topology -Report $report } catch { }
    $report.passed = $false
    $report.code = Get-P3AccFailureCode $_
    $exitCode = 1
} finally {
    try { Copy-P3AccTopologyTrackerToReport -Tracker $topology -Report $report } catch { }
    try {
        $clean = Complete-P3AccControllerCleanup -Configuration $configuration -AppState $appState -RelayStates @($relayStates) -Report $report
    } catch { $clean = $false }
    if ($clean -ne $true) {
        $report.passed = $false
        $report.code = 'P3ACC_CONTROLLER_CLEANUP_FAILED'
        $exitCode = 1
    }
}

try { [void](Assert-P3AccControllerReport $report) }
catch {
    $report = New-P3AccFallbackReport
    $exitCode = 1
}
$report | ConvertTo-Json -Depth 5 -Compress
exit $exitCode
