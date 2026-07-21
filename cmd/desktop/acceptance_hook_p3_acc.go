//go:build p3accacceptance

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	application "github.com/jwwsjlm/douyinLive/v2/internal/app"
	"github.com/jwwsjlm/douyinLive/v2/internal/capture"
	"github.com/jwwsjlm/douyinLive/v2/internal/eventstore"
	"github.com/jwwsjlm/douyinLive/v2/internal/room"
	"github.com/jwwsjlm/douyinLive/v2/internal/settings"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	p3ACCAcceptanceSchema                 = "P3-ACC-001/v1"
	p3ACCAcceptanceResultName             = "p3-acc.snapshot.json"
	p3ACCAcceptanceAlias                  = "P3 在线验收房间"
	p3ACCAcceptanceMaximumResourceSamples = 128
	p3ACCAcceptanceSnapshotInterval       = 2 * time.Second
	p3ACCAcceptanceResourceInterval       = 10 * time.Second
	p3ACCAcceptanceMaximumResourceGap     = 30 * time.Second
	p3ACCAcceptanceQueryTimeout           = 3 * time.Second
	p3ACCAcceptanceMaximumSnapshotBytes   = 1 << 20
	p3ACCAcceptanceMaximumManifestBytes   = 128 << 20
	p3ACCAcceptanceSentinelName           = ".p3acc-controller-owned"
	p3ACCAcceptanceSentinelContent        = "P3ACC-CONTROLLER/v1\n"
	p3ACCAcceptanceProgressFreshness      = 5 * time.Second
	p3ACCAcceptanceStableWindow           = 10 * time.Minute
	p3ACCAcceptanceMinimumStableSamples   = 30
	p3ACCAcceptanceMaximumObservedEvents  = 4096
	p3ACCAcceptanceMaximumMediaSegments   = 4096
	p3ACCAcceptanceMaximumMediaArtifacts  = 8192
	p3ACCAcceptanceMaximumRecoveryGaps    = 256
	p3ACCAcceptanceMaximumLatencyPending  = 256
	p3ACCAcceptanceMaximumLatencySamples  = 4096
	p3ACCAcceptanceMaximumUILatency       = time.Minute
)

//go:embed p3_acc_acceptance.js
var p3ACCAcceptanceScript string

var p3ACCAcceptanceAllowedCodes = map[string]struct{}{
	"": {}, "UNKNOWN": {},
	"P3ACC_CONFIG_INVALID": {}, "HOOK_CONFIG_INVALID": {},
	"HOOK_ISOLATION_INVALID": {}, "PRIVACY_CONFIG_FAILED": {},
	"ROOM_CREATE_FAILED": {}, "MONITOR_START_FAILED": {},
	"P3ACC_NOT_READY": {}, "P3ACC_SNAPSHOT_FAILED": {},
	"P3ACC_SNAPSHOT_INVALID": {}, "P3ACC_RECORDER_FENCE_MISMATCH": {},
	"P3ACC_RECORDER_UNAVAILABLE": {}, "P3ACC_RECORDER_CRASH_FAILED": {},
	"ROOM_OFFLINE": {}, "ROOM_OFFLINE_CONFIRMING": {}, "ROOM_CHECK_FAILED": {},
	"ROOM_NOT_FOUND": {}, "COOKIE_INVALID": {}, "CAPTURE_OPEN_FAILED": {},
	"CAPTURE_REBIND_FAILED": {}, "ROOM_CONNECTION_INTERRUPTED": {},
	"CAPTURE_FINALIZING": {}, "CAPTURE_FINALIZE_FAILED": {},
	"MONITOR_LIMIT_REACHED":         {},
	"MONITOR_MANAGER_SHUTTING_DOWN": {}, "FFMPEG_EXITED": {},
	"FFMPEG_START_FAILED": {}, "FFMPEG_STOP_FAILED": {},
	"FFMPEG_PROGRESS_INVALID": {}, "FFMPEG_PROGRESS_STALLED": {},
	"RECORDING_UNAVAILABLE": {}, "RECORDING_RESTARTED": {},
	"STREAM_UNAVAILABLE": {}, "DISK_FULL": {}, "PROCESS_CRASH": {},
	"MESSAGE_DISCONNECT": {}, "CLOCK_UNCERTAIN": {}, "EVENT_PERSISTENCE": {},
	"MESSAGE_RECONNECTED": {}, "MESSAGE_REBIND_RETRY": {},
	"MESSAGE_SUBSCRIPTION_FAILED": {}, "MESSAGE_REBIND_EXHAUSTED": {},
	"MESSAGE_FINALIZED": {}, "RECORDER_PROCESS_EXITED": {},
	"RECORDER_NETWORK_FAILURE": {},
}

type p3ACCStage string

const (
	p3ACCStageInitializing p3ACCStage = "INITIALIZING"
	p3ACCStageConfigured   p3ACCStage = "CONFIGURED"
	p3ACCStageWaiting      p3ACCStage = "WAITING"
	p3ACCStageStarting     p3ACCStage = "STARTING"
	p3ACCStageLive         p3ACCStage = "LIVE"
	p3ACCStageRecording    p3ACCStage = "RECORDING"
	p3ACCStageReconnecting p3ACCStage = "RECONNECTING"
	p3ACCStageRecovered    p3ACCStage = "RECOVERED"
	p3ACCStageFinalizing   p3ACCStage = "FINALIZING"
	p3ACCStageFinalized    p3ACCStage = "FINALIZED"
	p3ACCStageOffline      p3ACCStage = "OFFLINE"
	p3ACCStageError        p3ACCStage = "ERROR"
)

type p3ACCAcceptancePaths struct {
	Root       string
	DataRoot   string
	ResultPath string
}

// Every exported JSON field below is an explicit acceptance allowlist. IDs,
// process IDs, names, content, URLs, paths, credentials and command lines are
// intentionally absent.
type p3ACCAcceptanceSnapshot struct {
	Schema          string                      `json:"schema"`
	Stage           p3ACCStage                  `json:"stage"`
	CapturedAt      int64                       `json:"capturedAt"`
	UI              p3ACCUIObservationSummary   `json:"ui"`
	Runtime         p3ACCRuntimeSummary         `json:"runtime"`
	Progress        p3ACCProgressSummary        `json:"progress"`
	Database        p3ACCDatabaseSummary        `json:"database"`
	SessionManifest p3ACCSessionManifestSummary `json:"sessionManifest"`
	MediaManifest   p3ACCMediaManifestSummary   `json:"mediaManifest"`
	Gaps            p3ACCGapSummary             `json:"gaps"`
	Checkpoint      p3ACCCheckpointSummary      `json:"checkpoint"`
	Resources       p3ACCResourceSummary        `json:"resources"`
}

type p3ACCUIObservationSummary struct {
	Ready                   bool  `json:"ready"`
	RecordingSeen           bool  `json:"recordingSeen"`
	ProgressAdvanced        bool  `json:"progressAdvanced"`
	TimelineSeen            bool  `json:"timelineSeen"`
	ReconnectingSeen        bool  `json:"reconnectingSeen"`
	RecoveredSeen           bool  `json:"recoveredSeen"`
	NetworkReconnectingSeen bool  `json:"networkReconnectingSeen"`
	NetworkRecoveredSeen    bool  `json:"networkRecoveredSeen"`
	OfflineSeen             bool  `json:"offlineSeen"`
	FinalizedSeen           bool  `json:"finalizedSeen"`
	ObservationCount        int   `json:"observationCount"`
	LatencySampleCount      int   `json:"latencySampleCount"`
	LatencyPendingCount     int   `json:"latencyPendingCount"`
	LatencyP95MS            int64 `json:"latencyP95Ms"`
	LatencyMaxMS            int64 `json:"latencyMaxMs"`
	LatencyWithinTarget     bool  `json:"latencyWithinTarget"`
}

type p3ACCRuntimeSummary struct {
	State                   room.RuntimeState       `json:"state"`
	RecordingStatus         capture.RecordingStatus `json:"recordingStatus"`
	Revision                int64                   `json:"revision"`
	ErrorCode               string                  `json:"errorCode"`
	HasSession              bool                    `json:"hasSession"`
	SessionFenceStable      bool                    `json:"sessionFenceStable"`
	CurrentAttemptCommitted bool                    `json:"currentAttemptCommitted"`
	AttemptAdvanced         bool                    `json:"attemptAdvanced"`
	AttemptCount            int                     `json:"attemptCount"`
	RecorderTargetMatched   bool                    `json:"recorderTargetMatched"`
	CrashInjected           bool                    `json:"crashInjected"`
	RecoveryProven          bool                    `json:"recoveryProven"`
	NetworkFaultArmed       bool                    `json:"networkFaultArmed"`
	NetworkRecoveryProven   bool                    `json:"networkRecoveryProven"`
	FinalizationProven      bool                    `json:"finalizationProven"`
}

type p3ACCProgressSummary struct {
	SampleCount       int   `json:"sampleCount"`
	LiveBatchCount    int   `json:"liveBatchCount"`
	LiveEventCount    int   `json:"liveEventCount"`
	ElapsedMS         int64 `json:"elapsedMs"`
	BytesWritten      int64 `json:"bytesWritten"`
	SegmentCount      int64 `json:"segmentCount"`
	RestartCount      int   `json:"restartCount"`
	SteadyRecordingMS int64 `json:"steadyRecordingMs"`
	SteadySampleCount int   `json:"steadySampleCount"`
}

type p3ACCDatabaseSummary struct {
	SessionCount             int   `json:"sessionCount"`
	ActiveSessionCount       int   `json:"activeSessionCount"`
	EventCount               int64 `json:"eventCount"`
	SourceEventCount         int64 `json:"sourceEventCount"`
	PublishedEventCount      int   `json:"publishedEventCount"`
	PublishedEventsPersisted bool  `json:"publishedEventsPersisted"`
	SegmentCount             int   `json:"segmentCount"`
	CompleteSegmentCount     int   `json:"completeSegmentCount"`
	ArtifactCount            int   `json:"artifactCount"`
	CompleteArtifactCount    int   `json:"completeArtifactCount"`
}

type p3ACCSessionManifestSummary struct {
	Exists               bool                    `json:"exists"`
	MatchesDatabase      bool                    `json:"matchesDatabase"`
	CanonicalHashMatches bool                    `json:"canonicalHashMatches"`
	ManifestClean        bool                    `json:"manifestClean"`
	Ended                bool                    `json:"ended"`
	Status               capture.SessionStatus   `json:"status"`
	RecordingStatus      capture.RecordingStatus `json:"recordingStatus"`
}

type p3ACCMediaManifestSummary struct {
	Exists                   bool                      `json:"exists"`
	MatchesDatabase          bool                      `json:"matchesDatabase"`
	CanonicalHashMatches     bool                      `json:"canonicalHashMatches"`
	ManifestClean            bool                      `json:"manifestClean"`
	State                    capture.SessionMediaState `json:"state"`
	Revision                 int64                     `json:"revision"`
	AttemptCount             int                       `json:"attemptCount"`
	CommittedAttemptCount    int                       `json:"committedAttemptCount"`
	CleanAttemptCount        int                       `json:"cleanAttemptCount"`
	SegmentCount             int                       `json:"segmentCount"`
	CompleteSegmentCount     int                       `json:"completeSegmentCount"`
	ArtifactCount            int                       `json:"artifactCount"`
	CompleteArtifactCount    int                       `json:"completeArtifactCount"`
	FileCheckCount           int                       `json:"fileCheckCount"`
	FileFailureCount         int                       `json:"fileFailureCount"`
	IncompleteEntryCount     int                       `json:"incompleteEntryCount"`
	IncompleteSegmentCount   int                       `json:"incompleteSegmentCount"`
	AllFilesMatch            bool                      `json:"allFilesMatch"`
	SequenceContinuous       bool                      `json:"sequenceContinuous"`
	AttemptReferencesValid   bool                      `json:"attemptReferencesValid"`
	FaultPhaseSegmentsProven bool                      `json:"faultPhaseSegmentsProven"`
}

type p3ACCGapSummary struct {
	Total                  int    `json:"total"`
	Open                   int    `json:"open"`
	Recovered              int    `json:"recovered"`
	RecordingRestart       int    `json:"recordingRestart"`
	OpenRecordingRestart   int    `json:"openRecordingRestart"`
	OpenMessageDisconnect  int    `json:"openMessageDisconnect"`
	ProcessCrash           int    `json:"processCrash"`
	MessageDisconnect      int    `json:"messageDisconnect"`
	CrashRecoveryMatched   bool   `json:"crashRecoveryMatched"`
	NetworkMessageMatched  bool   `json:"networkMessageMatched"`
	NetworkRecorderMatched bool   `json:"networkRecorderMatched"`
	LatestKind             string `json:"latestKind"`
	LatestReasonCode       string `json:"latestReasonCode"`
	LatestOpen             bool   `json:"latestOpen"`
	LatestRecovered        bool   `json:"latestRecovered"`
}

type p3ACCCheckpointSummary struct {
	Exists             bool   `json:"exists"`
	State              string `json:"state"`
	CommittedSequence  int64  `json:"committedSequence"`
	MaxSourceSequence  int64  `json:"maxSourceSequence"`
	CoversSourceEvents bool   `json:"coversSourceEvents"`
	OpenGiftFoldCount  int    `json:"openGiftFoldCount"`
	GiftFoldsClosed    bool   `json:"giftFoldsClosed"`
}

type p3ACCResourceMetric struct {
	Baseline        int64  `json:"baseline"`
	Peak            int64  `json:"peak"`
	Latest          int64  `json:"latest"`
	Delta           int64  `json:"delta"`
	LatterHalfDelta int64  `json:"latterHalfDelta"`
	LatterHalfTrend string `json:"latterHalfTrend"`
}

type p3ACCResourceSummary struct {
	SampleCount                          int                 `json:"sampleCount"`
	WindowDurationMS                     int64               `json:"windowDurationMs"`
	SampleComplete                       bool                `json:"sampleComplete"`
	StableWindowProven                   bool                `json:"stableWindowProven"`
	Frozen                               bool                `json:"frozen"`
	DatabaseWALObserved                  bool                `json:"databaseWalObserved"`
	DiskIOObserved                       bool                `json:"diskIoObserved"`
	EventQueueObserved                   bool                `json:"eventQueueObserved"`
	AverageCPUPercent                    float64             `json:"averageCpuPercent"`
	LatterHalfAverageCPUPercent          float64             `json:"latterHalfAverageCpuPercent"`
	AverageProcessReadBytesPerSecond     float64             `json:"averageProcessReadBytesPerSecond"`
	AverageProcessWriteBytesPerSecond    float64             `json:"averageProcessWriteBytesPerSecond"`
	LatterHalfProcessReadBytesPerSecond  float64             `json:"latterHalfProcessReadBytesPerSecond"`
	LatterHalfProcessWriteBytesPerSecond float64             `json:"latterHalfProcessWriteBytesPerSecond"`
	AverageDiskWriteBytesPerSecond       float64             `json:"averageDiskWriteBytesPerSecond"`
	LatterHalfDiskWriteBytesPerSecond    float64             `json:"latterHalfDiskWriteBytesPerSecond"`
	CPUWithinTarget                      bool                `json:"cpuWithinTarget"`
	CPUTrend                             string              `json:"cpuTrend"`
	ProcessCount                         p3ACCResourceMetric `json:"processCount"`
	WorkingSet                           p3ACCResourceMetric `json:"workingSet"`
	PrivateBytes                         p3ACCResourceMetric `json:"privateBytes"`
	Threads                              p3ACCResourceMetric `json:"threads"`
	Handles                              p3ACCResourceMetric `json:"handles"`
	Goroutines                           p3ACCResourceMetric `json:"goroutines"`
	HeapAlloc                            p3ACCResourceMetric `json:"heapAlloc"`
	HeapInUse                            p3ACCResourceMetric `json:"heapInUse"`
	System                               p3ACCResourceMetric `json:"system"`
	DatabaseWALBytes                     p3ACCResourceMetric `json:"databaseWalBytes"`
	ProcessReadBytes                     p3ACCResourceMetric `json:"processReadBytes"`
	ProcessWriteBytes                    p3ACCResourceMetric `json:"processWriteBytes"`
	DataRootPhysicalBytes                p3ACCResourceMetric `json:"dataRootPhysicalBytes"`
	EventQueueCount                      p3ACCResourceMetric `json:"eventQueueCount"`
	EventQueueItems                      p3ACCResourceMetric `json:"eventQueueItems"`
	EventQueueBytes                      p3ACCResourceMetric `json:"eventQueueBytes"`
	EventQueueItemCapacity               p3ACCResourceMetric `json:"eventQueueItemCapacity"`
	EventQueueByteCapacity               p3ACCResourceMetric `json:"eventQueueByteCapacity"`
}

type p3ACCResourceSample struct {
	CapturedAt             time.Time
	Complete               bool
	ProcessCPU100NS        int64
	ProcessCount           int64
	WorkingSetBytes        int64
	PrivateBytes           int64
	ThreadCount            int64
	HandleCount            int64
	Goroutines             int64
	HeapAllocBytes         int64
	HeapInUseBytes         int64
	SysBytes               int64
	DatabaseWALBytes       int64
	DatabaseWALPresent     bool
	ProcessReadBytes       int64
	ProcessWriteBytes      int64
	DataRootPhysicalBytes  int64
	EventQueueCount        int64
	EventQueueItems        int64
	EventQueueBytes        int64
	EventQueueItemCapacity int64
	EventQueueByteCapacity int64
}

type p3ACCProcessTreeResourceSample struct {
	Complete        bool
	CPU100NS        int64
	ProcessCount    int64
	WorkingSetBytes int64
	PrivateBytes    int64
	ThreadCount     int64
	HandleCount     int64
	ReadBytes       int64
	WriteBytes      int64
}

type p3ACCMediaPathState uint8

const (
	p3ACCMediaPathUnsafe p3ACCMediaPathState = iota
	p3ACCMediaPathMissing
	p3ACCMediaPathRegular
)

type p3ACCObservedStatus struct {
	State           room.RuntimeState
	RecordingStatus capture.RecordingStatus
	Revision        int64
	ErrorCode       string
	RetryAt         int64
	SessionID       string
	OperationID     string
}

type p3ACCUIEventLatencyPending struct {
	emittedAt  time.Time
	eventCount int
}

type p3ACCRecoveryProgressBaseline struct {
	attemptID         string
	updatedAt         int64
	elapsedMS         int64
	frame             int64
	segmentCount      int64
	physicalBytes     int64
	physicalFileCount int
}

type p3ACCAcceptanceState struct {
	ctx   context.Context
	paths p3ACCAcceptancePaths

	mu                            sync.Mutex
	roomID                        string
	status                        p3ACCObservedStatus
	progress                      p3ACCProgressSummary
	progressInternalUpdatedAt     int64
	progressReceivedAt            time.Time
	progressFrame                 int64
	ui                            p3ACCUIObservationSummary
	uiLatencyTracking             bool
	uiLatencyInvalid              bool
	uiLatencyPending              []p3ACCUIEventLatencyPending
	uiLatencySamples              []int64
	crashInjected                 bool
	crashInFlight                 bool
	crashBaselineAttemptID        string
	crashBaselineOperationID      string
	crashBaselineAtMS             int64
	crashBaselineAttemptCount     int
	crashBaselineGapCount         int
	crashBaselineRestartCount     int
	crashBaselineRevision         int64
	crashBaselineProgressAt       int64
	postCrashReconnecting         bool
	postCrashGapOpened            bool
	postCrashGapClosed            bool
	postCrashProgress             bool
	crashRecoveryProgress         p3ACCRecoveryProgressBaseline
	recoveryProven                bool
	networkFaultArmed             bool
	networkArmInFlight            bool
	networkBaselineAttemptID      string
	networkBaselineOperationID    string
	networkBaselineAtMS           int64
	networkBaselineAttemptCount   int
	networkBaselineMessageGap     int
	networkBaselineRecordingGap   int
	networkBaselineRestartCount   int
	networkBaselineRevision       int64
	networkBaselineProgressAt     int64
	postNetworkReconnecting       bool
	postNetworkMessageOpened      bool
	postNetworkMessageClosed      bool
	postNetworkRecordingGapOpened bool
	postNetworkRecordingGapClosed bool
	postNetworkProgress           bool
	networkRecoveryProgress       p3ACCRecoveryProgressBaseline
	networkRecoveryProven         bool
	seenRecording                 bool
	offlineConfirmRevision        int64
	offlineFinalRevision          int64
	offlineSequenceProven         bool
	steadyStartedAt               time.Time
	steadySessionID               string
	steadyOperationID             string
	steadyAttemptID               string
	steadyRestartCount            int
	steadyLastProgressAt          int64
	steadyLastElapsedMS           int64
	steadyLastFrame               int64
	steadyLastSegmentCount        int64
	steadyLastPhysicalBytes       int64
	steadyLastPhysicalFileCount   int
	steadyLastPhysicalGrowthAt    time.Time
	steadySampleCount             int
	resourceSamples               []p3ACCResourceSample
	bestResourceSummary           p3ACCResourceSummary
	lastResourceSampleAt          time.Time
	latestResourceAttemptComplete bool
	resourceSamplingFailed        bool
	observedEventIDs              map[string]struct{}
	observedEventOverflow         bool
	mediaVerificationMu           sync.Mutex
	mediaVerificationRevision     int64
	mediaVerificationUpdatedAt    int64
	mediaVerificationSessionID    string
	mediaVerificationRoot         p3ACCMediaRootIdentity
	mediaVerification             p3ACCMediaManifestSummary
	writeMu                       sync.Mutex
}

var p3ACCAcceptanceRegistry = struct {
	sync.Mutex
	states map[*DesktopApp]*p3ACCAcceptanceState
}{states: make(map[*DesktopApp]*p3ACCAcceptanceState)}

var p3ACCBootstrap = struct {
	sync.Mutex
	liveURL string
	paths   p3ACCAcceptancePaths
	ready   bool
}{}

type p3ACCInternalDatabaseSnapshot struct {
	Found                   bool
	SessionID               string
	OperationID             string
	DataPath                string
	SessionStatus           capture.SessionStatus
	RecordingStatus         capture.RecordingStatus
	EndedAt                 sql.NullInt64
	Session                 capture.LiveSession
	MediaRootID             sql.NullString
	MediaState              capture.SessionMediaState
	MediaRevision           int64
	MediaDirty              bool
	Media                   capture.SessionMedia
	Attempts                []capture.MediaAttempt
	Segments                []capture.MediaSegment
	Artifacts               []capture.MediaArtifact
	CurrentAttemptID        string
	CurrentAttemptCommitted bool
	Database                p3ACCDatabaseSummary
	Gaps                    p3ACCGapSummary
	Checkpoint              p3ACCCheckpointSummary
}

type p3ACCMediaManifestWire struct {
	SchemaVersion int `json:"schemaVersion"`
	Session       struct {
		ID              string                    `json:"id"`
		RecordingRootID *string                   `json:"recordingRootId,omitempty"`
		RelativePath    string                    `json:"relativePath"`
		State           capture.SessionMediaState `json:"state"`
		Revision        int64                     `json:"revision"`
		MediaEpochAt    *int64                    `json:"mediaEpochAt,omitempty"`
		CreatedAt       int64                     `json:"createdAt"`
		UpdatedAt       int64                     `json:"updatedAt"`
	} `json:"session"`
	Attempts  []capture.MediaAttempt  `json:"attempts"`
	Segments  []capture.MediaSegment  `json:"segments"`
	Artifacts []capture.MediaArtifact `json:"artifacts"`
}

type p3ACCRecorderRecoveryGapDetails struct {
	Version           int    `json:"version"`
	SourceAttemptID   string `json:"sourceAttemptId"`
	SourceOperationID string `json:"sourceOperationId"`
	SourceErrorCode   string `json:"sourceErrorCode"`
	RestartAttempts   int    `json:"restartAttempts"`
	LastErrorCode     string `json:"lastErrorCode"`
	LastOccurredAtMS  int64  `json:"lastOccurredAtMs"`
	ClockUncertain    bool   `json:"clockUncertain,omitempty"`
}

type p3ACCMessageRecoveryGapDetails struct {
	Version           int    `json:"version"`
	OpenedOperationID string `json:"openedOperationId"`
	LastOperationID   string `json:"lastOperationId"`
	BeginAttempts     int    `json:"beginAttempts"`
	FirstErrorCode    string `json:"firstErrorCode"`
	LastErrorCode     string `json:"lastErrorCode"`
	LastOccurredAtMS  int64  `json:"lastOccurredAtMs"`
}

type p3ACCRecorderRecoveryGapEvidence struct {
	StartedAt  int64
	EndedAt    sql.NullInt64
	Recovered  bool
	ReasonCode string
	DedupeKey  string
	Details    p3ACCRecorderRecoveryGapDetails
}

type p3ACCMessageRecoveryGapEvidence struct {
	StartedAt         int64
	OpenedOperationID string
}

func desktopInfrastructureOptions() (application.InfrastructureOptions, error) {
	if err := captureP3ACCBootstrap(); err != nil {
		return application.InfrastructureOptions{}, errors.New("P3ACC_CONFIG_INVALID")
	}
	paths, err := peekP3ACCBootstrapPaths()
	if err != nil {
		clearP3ACCBootstrap()
		return application.InfrastructureOptions{}, errors.New("P3ACC_CONFIG_INVALID")
	}
	return application.InfrastructureOptions{DataRoot: paths.DataRoot, DisableDiagnostics: true}, nil
}

func (a *DesktopApp) startAcceptanceHook(ctx context.Context) {
	liveURL, paths, err := takeP3ACCBootstrap()
	if err != nil {
		return
	}
	defer func() { liveURL = "" }()
	if validateP3ACCAcceptanceDataPath(paths.Root, paths.DataRoot) != nil {
		return
	}
	if _, err := os.Stat(paths.ResultPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return
	}
	settingsService, err := a.settingsService()
	if err != nil {
		writeP3ACCAcceptanceFailure(paths, "HOOK_ISOLATION_INVALID")
		return
	}
	currentSettings, err := settingsService.GetSettings(ctx)
	if err != nil || filepath.Clean(currentSettings.StorageRoot) != paths.DataRoot {
		writeP3ACCAcceptanceFailure(paths, "HOOK_ISOLATION_INVALID")
		return
	}
	roomService, err := a.roomService()
	if err != nil {
		writeP3ACCAcceptanceFailure(paths, "HOOK_ISOLATION_INVALID")
		return
	}
	existing, err := roomService.ListRooms(ctx)
	if err != nil || len(existing) != 0 {
		writeP3ACCAcceptanceFailure(paths, "HOOK_ISOLATION_INVALID")
		return
	}
	updatedSettings, err := settingsService.UpdateSettings(
		ctx, p3ACCPrivacySettingsInput(currentSettings, paths.DataRoot),
	)
	if err != nil || updatedSettings.SaveDisplayNames {
		writeP3ACCAcceptanceFailure(paths, "PRIVACY_CONFIG_FAILED")
		return
	}
	if manager := a.application.EventStoreManager(); manager != nil {
		manager.SetStoreDisplayName(false)
	} else {
		writeP3ACCAcceptanceFailure(paths, "HOOK_ISOLATION_INVALID")
		return
	}
	configured, err := roomService.CreateRoom(ctx, room.CreateRoomInput{
		LiveID: liveURL, Alias: p3ACCAcceptanceAlias,
		MonitorEnabled: false, RecordEnabled: true,
		RecordingProfile: room.RecordingProfile{
			Quality: room.QualityAuto, SegmentMinutes: 5,
		},
	})
	liveURL = ""
	if err != nil || !configured.RecordEnabled || configured.MonitorEnabled {
		writeP3ACCAcceptanceFailure(paths, "ROOM_CREATE_FAILED")
		return
	}
	roomID := configured.ID
	configured = room.RoomConfig{}
	state := &p3ACCAcceptanceState{
		ctx: ctx, paths: paths, roomID: roomID,
		status:           p3ACCObservedStatus{State: room.RuntimeStopped},
		observedEventIDs: make(map[string]struct{}),
	}
	p3ACCAcceptanceRegistry.Lock()
	p3ACCAcceptanceRegistry.states[a] = state
	p3ACCAcceptanceRegistry.Unlock()

	if err := a.application.MonitorManager().StartMonitoring(ctx, roomID); err != nil {
		state.mu.Lock()
		state.status.State = room.RuntimeError
		state.status.ErrorCode = "MONITOR_START_FAILED"
		state.mu.Unlock()
		_ = a.writeP3ACCAcceptanceSnapshot()
		return
	}
	_ = a.writeP3ACCAcceptanceSnapshot()
	go a.runP3ACCAcceptanceSnapshotLoop(state)
	go func() {
		timer := time.NewTimer(750 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			runtime.WindowExecJS(ctx, p3ACCAcceptanceScript)
		}
	}()
}

func p3ACCPrivacySettingsInput(current settings.AppSettings, dataRoot string) settings.UpdateSettingsInput {
	return settings.UpdateSettingsInput{
		RecordingDirectory: filepath.Join(dataRoot, "rooms"),
		DefaultQuality:     current.DefaultQuality, DefaultSegmentMinutes: current.DefaultSegmentMinutes,
		MaxConcurrentRecordings: current.MaxConcurrentRecordings,
		MinimumFreeSpaceGiB:     current.MinimumFreeSpaceGiB,
		SaveDisplayNames:        false,
	}
}

func captureP3ACCBootstrap() error {
	value := strings.TrimSpace(os.Getenv("P3ACC_LIVE_URL"))
	rootValue := os.Getenv("P3ACC_ROOT")
	resultValue := os.Getenv("P3ACC_RESULT_PATH")
	_ = os.Unsetenv("P3ACC_LIVE_URL")
	_ = os.Unsetenv("P3ACC_ROOT")
	_ = os.Unsetenv("P3ACC_RESULT_PATH")
	if value == "" {
		return errors.New("P3ACC_CONFIG_INVALID")
	}
	normalized, err := room.NormalizeLiveID(value)
	value = ""
	if err != nil || normalized == "" {
		normalized = ""
		return errors.New("P3ACC_CONFIG_INVALID")
	}
	paths, err := parseP3ACCAcceptancePaths(rootValue, resultValue)
	rootValue, resultValue = "", ""
	if err != nil {
		normalized = ""
		return errors.New("P3ACC_CONFIG_INVALID")
	}
	if err := validateP3ACCAcceptanceRoot(paths.Root); err != nil {
		normalized = ""
		value = ""
		return errors.New("P3ACC_CONFIG_INVALID")
	}
	if err := validateP3ACCAcceptanceFreshLayout(paths); err != nil {
		normalized = ""
		return errors.New("P3ACC_CONFIG_INVALID")
	}
	p3ACCBootstrap.Lock()
	defer p3ACCBootstrap.Unlock()
	if p3ACCBootstrap.ready {
		normalized = ""
		return errors.New("P3ACC_CONFIG_INVALID")
	}
	p3ACCBootstrap.liveURL = normalized
	p3ACCBootstrap.paths = paths
	p3ACCBootstrap.ready = true
	return nil
}

func peekP3ACCBootstrapPaths() (p3ACCAcceptancePaths, error) {
	p3ACCBootstrap.Lock()
	defer p3ACCBootstrap.Unlock()
	if !p3ACCBootstrap.ready || p3ACCBootstrap.paths.Root == "" {
		return p3ACCAcceptancePaths{}, errors.New("P3ACC_CONFIG_INVALID")
	}
	return p3ACCBootstrap.paths, nil
}

func takeP3ACCBootstrap() (string, p3ACCAcceptancePaths, error) {
	p3ACCBootstrap.Lock()
	defer p3ACCBootstrap.Unlock()
	if !p3ACCBootstrap.ready || p3ACCBootstrap.liveURL == "" || p3ACCBootstrap.paths.Root == "" {
		return "", p3ACCAcceptancePaths{}, errors.New("P3ACC_CONFIG_INVALID")
	}
	value := p3ACCBootstrap.liveURL
	paths := p3ACCBootstrap.paths
	p3ACCBootstrap.liveURL = ""
	p3ACCBootstrap.paths = p3ACCAcceptancePaths{}
	p3ACCBootstrap.ready = false
	return value, paths, nil
}

func clearP3ACCBootstrap() {
	p3ACCBootstrap.Lock()
	p3ACCBootstrap.liveURL = ""
	p3ACCBootstrap.paths = p3ACCAcceptancePaths{}
	p3ACCBootstrap.ready = false
	p3ACCBootstrap.Unlock()
}

func loadP3ACCAcceptancePaths() (p3ACCAcceptancePaths, error) {
	return parseP3ACCAcceptancePaths(os.Getenv("P3ACC_ROOT"), os.Getenv("P3ACC_RESULT_PATH"))
}

func parseP3ACCAcceptancePaths(rootValue, resultValue string) (p3ACCAcceptancePaths, error) {
	root, err := cleanP3ACCAcceptanceAbsolutePath(rootValue)
	if err != nil {
		return p3ACCAcceptancePaths{}, err
	}
	result, err := cleanP3ACCAcceptanceAbsolutePath(resultValue)
	if err != nil {
		return p3ACCAcceptancePaths{}, err
	}
	dataRoot := filepath.Join(root, "data")
	volumeRoot := filepath.Clean(filepath.VolumeName(root) + string(filepath.Separator))
	relativeRoot, relativeErr := filepath.Rel(volumeRoot, root)
	rootDepth := 0
	if relativeErr == nil && relativeRoot != "." {
		for _, component := range strings.Split(relativeRoot, string(filepath.Separator)) {
			if component != "" && component != "." {
				rootDepth++
			}
		}
	}
	if strings.HasPrefix(root, `\\`) || relativeErr != nil || rootDepth < 2 ||
		strings.EqualFold(root, volumeRoot) || filepath.Base(result) != p3ACCAcceptanceResultName ||
		!p3ACCAcceptancePathWithin(root, result, false) ||
		p3ACCAcceptancePathWithin(dataRoot, result, true) {
		return p3ACCAcceptancePaths{}, errors.New("P3ACC_CONFIG_INVALID")
	}
	return p3ACCAcceptancePaths{Root: root, DataRoot: dataRoot, ResultPath: result}, nil
}

func validateP3ACCAcceptanceFreshLayout(paths p3ACCAcceptancePaths) error {
	entries, err := os.ReadDir(paths.Root)
	if err != nil || len(entries) != 1 || entries[0].Name() != p3ACCAcceptanceSentinelName ||
		entries[0].IsDir() || entries[0].Type()&os.ModeSymlink != 0 {
		return errors.New("P3ACC_CONFIG_INVALID")
	}
	for _, candidate := range []string{paths.DataRoot, paths.ResultPath, filepath.Dir(paths.ResultPath)} {
		if _, err := os.Lstat(candidate); !errors.Is(err, os.ErrNotExist) {
			return errors.New("P3ACC_CONFIG_INVALID")
		}
	}
	return nil
}

func cleanP3ACCAcceptanceAbsolutePath(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	cleaned := filepath.Clean(trimmed)
	if trimmed == "" || cleaned == "." || !filepath.IsAbs(cleaned) || strings.Contains(cleaned, "%") {
		return "", errors.New("P3ACC_CONFIG_INVALID")
	}
	return cleaned, nil
}

func p3ACCAcceptancePathWithin(root, candidate string, allowRoot bool) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	if relative == "." {
		return allowRoot
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (a *DesktopApp) observeAcceptanceEvent(eventName string, payload any) {
	state := p3ACCAcceptanceStateFor(a)
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	switch eventName {
	case room.StatusEventName:
		status, ok := payload.(room.RoomRuntimeStatus)
		if !ok || status.RoomID != state.roomID || !validP3ACCRuntimeState(status.State) ||
			!validP3ACCRecordingStatus(status.RecordingStatus, true) ||
			status.Revision <= state.status.Revision {
			return
		}
		safeCode := safeP3ACCErrorCode(status.ErrorCode)
		state.status = p3ACCObservedStatus{
			State: status.State, RecordingStatus: status.RecordingStatus,
			Revision: status.Revision, ErrorCode: safeCode,
			RetryAt: status.RetryAt, SessionID: status.SessionID, OperationID: status.OperationID,
		}
		if status.State == room.RuntimeRecording && status.RecordingStatus == capture.RecordingActive {
			state.seenRecording = true
		}
		if (state.crashInjected || state.crashInFlight) && status.Revision > state.crashBaselineRevision &&
			status.State == room.RuntimeReconnecting {
			state.postCrashReconnecting = true
		}
		if state.networkFaultArmed && status.Revision > state.networkBaselineRevision &&
			status.State == room.RuntimeReconnecting {
			state.postNetworkReconnecting = true
		}
		if state.seenRecording && status.State == room.RuntimeReconnecting &&
			safeCode == "ROOM_OFFLINE_CONFIRMING" {
			state.offlineConfirmRevision = status.Revision
			state.offlineFinalRevision = 0
			state.offlineSequenceProven = false
		} else if state.offlineConfirmRevision > 0 && status.Revision > state.offlineConfirmRevision &&
			status.State == room.RuntimeWaiting && safeCode == "ROOM_OFFLINE" &&
			status.SessionID == "" && status.RecordingStatus == "" && validP3ACCUUID(status.OperationID) {
			state.offlineFinalRevision = status.Revision
			state.offlineSequenceProven = true
		} else if status.State == room.RuntimeRecording || status.State == room.RuntimeLive {
			// STARTING is the normal polling state after a terminal offline
			// result. Retain a proven sequence until the room is actually live.
			state.offlineConfirmRevision = 0
			state.offlineFinalRevision = 0
			state.offlineSequenceProven = false
		}
		if status.State != room.RuntimeRecording || status.RecordingStatus != capture.RecordingActive {
			state.resetP3ACCSteadyLocked()
		}
	case capture.RecordingProgressEventName:
		progress, ok := payload.(capture.RecordingProgressDTO)
		if !ok || progress.RoomID != state.roomID || progress.SessionID == "" ||
			progress.SessionID != state.status.SessionID || progress.OperationID == "" ||
			progress.OperationID != state.status.OperationID || progress.UpdatedAt <= 0 ||
			progress.UpdatedAt <= state.progressUpdatedAtLocked() || progress.ElapsedMS < 0 ||
			progress.BytesWritten < 0 || progress.SegmentCount < 0 || progress.Frame < 0 ||
			progress.RestartCount < 0 {
			return
		}
		state.progress.SampleCount++
		state.progress.ElapsedMS = progress.ElapsedMS
		state.progress.BytesWritten = progress.BytesWritten
		state.progress.SegmentCount = progress.SegmentCount
		state.progress.RestartCount = progress.RestartCount
		state.setProgressUpdatedAtLocked(progress.UpdatedAt)
		state.progressFrame = progress.Frame
		state.progressReceivedAt = time.Now()
	case eventstore.LiveEventEventName:
		batch, ok := payload.(eventstore.LiveEventBatchDTO)
		if !ok || batch.SessionID == "" || batch.SessionID != state.status.SessionID ||
			len(batch.Events) == 0 || len(batch.Events) > 100 {
			return
		}
		for _, event := range batch.Events {
			if !validP3ACCUUID(event.ID) {
				return
			}
		}
		if state.uiLatencyTracking {
			if state.uiLatencyInvalid || len(state.uiLatencyPending) >= p3ACCAcceptanceMaximumLatencyPending ||
				len(state.uiLatencySamples)+len(batch.Events) > p3ACCAcceptanceMaximumLatencySamples {
				state.uiLatencyInvalid = true
			} else {
				state.uiLatencyPending = append(state.uiLatencyPending, p3ACCUIEventLatencyPending{
					emittedAt: time.Now(), eventCount: len(batch.Events),
				})
			}
			state.updateP3ACCUIEventLatencyLocked()
		}
		state.progress.LiveBatchCount++
		state.progress.LiveEventCount += len(batch.Events)
		for _, event := range batch.Events {
			if len(state.observedEventIDs) >= p3ACCAcceptanceMaximumObservedEvents {
				if _, exists := state.observedEventIDs[event.ID]; !exists {
					state.observedEventOverflow = true
				}
				continue
			}
			state.observedEventIDs[event.ID] = struct{}{}
		}
	}
}

func (s *p3ACCAcceptanceState) resetP3ACCSteadyLocked() {
	s.steadyStartedAt = time.Time{}
	s.steadySessionID = ""
	s.steadyOperationID = ""
	s.steadyAttemptID = ""
	s.steadyRestartCount = 0
	s.steadyLastProgressAt = 0
	s.steadyLastElapsedMS = 0
	s.steadyLastFrame = 0
	s.steadyLastSegmentCount = 0
	s.steadyLastPhysicalBytes = 0
	s.steadyLastPhysicalFileCount = 0
	s.steadyLastPhysicalGrowthAt = time.Time{}
	s.steadySampleCount = 0
	s.progress.SteadyRecordingMS = 0
	s.progress.SteadySampleCount = 0
	s.resourceSamples = nil
	s.lastResourceSampleAt = time.Time{}
	s.latestResourceAttemptComplete = false
	resetP3ACCProcessTreeResourceTracker()
	resetP3ACCDataRootPhysicalTracker()
}

// UpdatedAt is intentionally kept outside the public progress JSON contract.
func (s *p3ACCAcceptanceState) progressUpdatedAtLocked() int64 {
	return s.progressInternalUpdatedAt
}

func (s *p3ACCAcceptanceState) setProgressUpdatedAtLocked(value int64) {
	s.progressInternalUpdatedAt = value
}

func p3ACCAcceptanceStateFor(a *DesktopApp) *p3ACCAcceptanceState {
	p3ACCAcceptanceRegistry.Lock()
	defer p3ACCAcceptanceRegistry.Unlock()
	return p3ACCAcceptanceRegistry.states[a]
}

func (a *DesktopApp) runP3ACCAcceptanceSnapshotLoop(state *p3ACCAcceptanceState) {
	ticker := time.NewTicker(p3ACCAcceptanceSnapshotInterval)
	defer ticker.Stop()
	for {
		select {
		case <-state.ctx.Done():
			return
		case now := <-ticker.C:
			state.mu.Lock()
			if !state.steadyStartedAt.IsZero() &&
				(state.lastResourceSampleAt.IsZero() || now.Sub(state.lastResourceSampleAt) >= p3ACCAcceptanceResourceInterval) {
				state.appendResourceSampleLocked(readP3ACCResourceSample(a, state.paths.DataRoot))
				state.lastResourceSampleAt = now
			}
			state.mu.Unlock()
			_ = a.writeP3ACCAcceptanceSnapshot()
		}
	}
}

// GetP3ACCAcceptanceSnapshot exists only in the tagged Wails harness.
func (a *DesktopApp) GetP3ACCAcceptanceSnapshot() (p3ACCAcceptanceSnapshot, error) {
	snapshot, err := a.buildP3ACCAcceptanceSnapshot()
	if err != nil {
		return p3ACCAcceptanceSnapshot{}, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	return snapshot, nil
}

// SampleP3ACCAcceptanceResources records one bounded in-memory sample and
// returns only aggregate statistics through later snapshots.
func (a *DesktopApp) SampleP3ACCAcceptanceResources() error {
	state := p3ACCAcceptanceStateFor(a)
	if state == nil {
		return errors.New("P3ACC_NOT_READY")
	}
	state.mu.Lock()
	if !state.steadyStartedAt.IsZero() {
		state.appendResourceSampleLocked(readP3ACCResourceSample(a, state.paths.DataRoot))
		state.lastResourceSampleAt = time.Now()
	}
	state.mu.Unlock()
	return a.writeP3ACCAcceptanceSnapshot()
}

// MarkP3ACCAcceptanceResourceFailure makes any JS sampling failure fail closed
// without accepting error text or other controller-provided data.
func (a *DesktopApp) MarkP3ACCAcceptanceResourceFailure() error {
	state := p3ACCAcceptanceStateFor(a)
	if state == nil {
		return errors.New("P3ACC_NOT_READY")
	}
	state.mu.Lock()
	state.resourceSamplingFailed = true
	state.mu.Unlock()
	return a.writeP3ACCAcceptanceSnapshot()
}

// ObserveP3ACCAcceptanceUI accepts only a fixed phase enum from the embedded
// probe. It never accepts DOM text, identifiers, content, or numeric values.
func (a *DesktopApp) ObserveP3ACCAcceptanceUI(phase string) error {
	state := p3ACCAcceptanceStateFor(a)
	if state == nil {
		return errors.New("P3ACC_NOT_READY")
	}
	state.mu.Lock()
	changed := false
	switch phase {
	case "READY":
		if !state.ui.Ready {
			state.uiLatencyTracking = true
			state.uiLatencyInvalid = false
			state.uiLatencyPending = nil
			state.uiLatencySamples = nil
			state.updateP3ACCUIEventLatencyLocked()
			state.ui.Ready, changed = true, true
		}
	case "RECORDING":
		if !state.ui.Ready {
			state.mu.Unlock()
			return errors.New("P3ACC_SNAPSHOT_INVALID")
		}
		if !state.ui.RecordingSeen {
			state.ui.RecordingSeen, changed = true, true
		}
	case "PROGRESS_ADVANCED":
		if !state.ui.RecordingSeen {
			state.mu.Unlock()
			return errors.New("P3ACC_SNAPSHOT_INVALID")
		}
		if !state.ui.ProgressAdvanced {
			state.ui.ProgressAdvanced, changed = true, true
		}
	case "TIMELINE_VISIBLE":
		if !state.ui.RecordingSeen {
			state.mu.Unlock()
			return errors.New("P3ACC_SNAPSHOT_INVALID")
		}
		if !state.ui.TimelineSeen {
			state.ui.TimelineSeen, changed = true, true
		}
	case "RECONNECTING":
		if !state.crashInjected || !state.ui.RecordingSeen {
			state.mu.Unlock()
			return errors.New("P3ACC_SNAPSHOT_INVALID")
		}
		if !state.ui.ReconnectingSeen {
			state.ui.ReconnectingSeen, changed = true, true
		}
	case "RECOVERED":
		if !state.ui.ReconnectingSeen {
			state.mu.Unlock()
			return errors.New("P3ACC_SNAPSHOT_INVALID")
		}
		if !state.ui.RecoveredSeen {
			state.ui.RecoveredSeen, changed = true, true
		}
	case "NETWORK_RECONNECTING":
		if !state.networkFaultArmed || !state.ui.RecoveredSeen {
			state.mu.Unlock()
			return errors.New("P3ACC_SNAPSHOT_INVALID")
		}
		if !state.ui.NetworkReconnectingSeen {
			state.ui.NetworkReconnectingSeen, changed = true, true
		}
	case "NETWORK_RECOVERED":
		if !state.ui.NetworkReconnectingSeen {
			state.mu.Unlock()
			return errors.New("P3ACC_SNAPSHOT_INVALID")
		}
		if !state.ui.NetworkRecoveredSeen {
			state.ui.NetworkRecoveredSeen, changed = true, true
		}
	case "OFFLINE":
		if !state.ui.NetworkRecoveredSeen {
			state.mu.Unlock()
			return errors.New("P3ACC_SNAPSHOT_INVALID")
		}
		if !state.ui.OfflineSeen {
			state.ui.OfflineSeen, changed = true, true
		}
	case "FINALIZED":
		if !state.ui.OfflineSeen || state.uiLatencyInvalid ||
			state.ui.LatencySampleCount < 1 || state.ui.LatencyPendingCount != 0 ||
			!state.ui.LatencyWithinTarget {
			state.mu.Unlock()
			return errors.New("P3ACC_SNAPSHOT_INVALID")
		}
		if !state.ui.FinalizedSeen {
			state.ui.FinalizedSeen, changed = true, true
		}
	default:
		state.mu.Unlock()
		return errors.New("P3ACC_SNAPSHOT_INVALID")
	}
	if changed {
		state.ui.ObservationCount++
	}
	state.mu.Unlock()
	return a.writeP3ACCAcceptanceSnapshot()
}

// AckP3ACCAcceptanceLiveEventRendered is deliberately argument-free. The
// embedded UI probe acks in FIFO order only after two animation frames; Go
// owns the monotonic emit timestamps and never accepts event data or clocks.
func (a *DesktopApp) AckP3ACCAcceptanceLiveEventRendered() error {
	state := p3ACCAcceptanceStateFor(a)
	if state == nil {
		return errors.New("P3ACC_NOT_READY")
	}
	state.mu.Lock()
	if !state.uiLatencyTracking || state.uiLatencyInvalid || len(state.uiLatencyPending) == 0 {
		state.uiLatencyInvalid = true
		state.updateP3ACCUIEventLatencyLocked()
		state.mu.Unlock()
		return errors.New("P3ACC_SNAPSHOT_INVALID")
	}
	pending := state.uiLatencyPending[0]
	state.uiLatencyPending = state.uiLatencyPending[1:]
	delay := time.Since(pending.emittedAt)
	if pending.eventCount < 1 || delay < 0 || delay > p3ACCAcceptanceMaximumUILatency ||
		len(state.uiLatencySamples)+pending.eventCount > p3ACCAcceptanceMaximumLatencySamples {
		state.uiLatencyInvalid = true
	} else {
		latencyMS := delay.Milliseconds()
		for index := 0; index < pending.eventCount; index++ {
			state.uiLatencySamples = append(state.uiLatencySamples, latencyMS)
		}
	}
	state.updateP3ACCUIEventLatencyLocked()
	invalid := state.uiLatencyInvalid
	state.mu.Unlock()
	if invalid {
		return errors.New("P3ACC_SNAPSHOT_INVALID")
	}
	return nil
}

func (a *DesktopApp) MarkP3ACCAcceptanceUILatencyFailure() error {
	state := p3ACCAcceptanceStateFor(a)
	if state == nil {
		return errors.New("P3ACC_NOT_READY")
	}
	state.mu.Lock()
	state.uiLatencyInvalid = true
	state.updateP3ACCUIEventLatencyLocked()
	state.mu.Unlock()
	return a.writeP3ACCAcceptanceSnapshot()
}

func (s *p3ACCAcceptanceState) updateP3ACCUIEventLatencyLocked() {
	s.ui.LatencyPendingCount = len(s.uiLatencyPending)
	s.ui.LatencySampleCount = len(s.uiLatencySamples)
	s.ui.LatencyP95MS = 0
	s.ui.LatencyMaxMS = 0
	s.ui.LatencyWithinTarget = false
	if s.uiLatencyInvalid || len(s.uiLatencySamples) == 0 || len(s.uiLatencyPending) != 0 {
		return
	}
	ordered := append([]int64(nil), s.uiLatencySamples...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left] < ordered[right] })
	percentileIndex := (95*len(ordered)+99)/100 - 1
	if percentileIndex < 0 || percentileIndex >= len(ordered) {
		s.uiLatencyInvalid = true
		return
	}
	s.ui.LatencyP95MS = ordered[percentileIndex]
	s.ui.LatencyMaxMS = ordered[len(ordered)-1]
	s.ui.LatencyWithinTarget = s.ui.LatencyP95MS < 1000
}

// CrashP3ACCAcceptanceRecorder has no arguments and returns no process or
// correlation identifiers. All four internal fences are resolved atomically
// from the isolated room state and durable attempt journal.
func (a *DesktopApp) CrashP3ACCAcceptanceRecorder() error {
	state := p3ACCAcceptanceStateFor(a)
	if state == nil {
		return errors.New("P3ACC_NOT_READY")
	}
	state.mu.Lock()
	if state.crashInjected || state.crashInFlight {
		state.mu.Unlock()
		return errors.New("P3ACC_RECORDER_FENCE_MISMATCH")
	}
	state.crashInFlight = true
	roomID := state.roomID
	status := state.status
	state.mu.Unlock()
	crashSucceeded := false
	defer func() {
		if crashSucceeded {
			return
		}
		state.mu.Lock()
		state.crashInFlight = false
		state.mu.Unlock()
	}()
	database, err := a.queryP3ACCInternalDatabase(status.SessionID)
	if err != nil || !database.Found || database.CurrentAttemptID == "" ||
		database.SessionID != status.SessionID || database.OperationID != status.OperationID {
		return errors.New("P3ACC_RECORDER_FENCE_MISMATCH")
	}
	state.mu.Lock()
	state.crashBaselineAttemptID = database.CurrentAttemptID
	state.crashBaselineOperationID = database.OperationID
	state.crashBaselineAtMS = time.Now().UTC().UnixMilli()
	state.crashBaselineAttemptCount = len(database.Attempts)
	state.crashBaselineGapCount = database.Gaps.RecordingRestart
	state.crashBaselineRestartCount = state.progress.RestartCount
	state.crashBaselineRevision = status.Revision
	state.crashBaselineProgressAt = state.progressInternalUpdatedAt
	state.postCrashReconnecting = false
	state.postCrashGapOpened = false
	state.postCrashGapClosed = false
	state.postCrashProgress = false
	state.crashRecoveryProgress = p3ACCRecoveryProgressBaseline{}
	state.recoveryProven = false
	state.resetP3ACCSteadyLocked()
	state.mu.Unlock()
	ctx, cancel := context.WithTimeout(a.application.Context(), p3ACCAcceptanceQueryTimeout)
	defer cancel()
	err = capture.CrashP3AcceptanceCurrentRecorder(
		ctx, a.application.CaptureCoordinator(), roomID,
		database.SessionID, database.OperationID, database.CurrentAttemptID,
	)
	if err != nil {
		switch {
		case errors.Is(err, capture.ErrP3AcceptanceRecorderFence):
			return errors.New("P3ACC_RECORDER_FENCE_MISMATCH")
		case errors.Is(err, capture.ErrP3AcceptanceRecorderUnavailable):
			return errors.New("P3ACC_RECORDER_UNAVAILABLE")
		default:
			return errors.New("P3ACC_RECORDER_CRASH_FAILED")
		}
	}
	state.mu.Lock()
	state.crashInjected = true
	state.crashInFlight = false
	state.mu.Unlock()
	crashSucceeded = true
	return a.writeP3ACCAcceptanceSnapshot()
}

// ArmP3ACCAcceptanceNetworkFault captures a one-use, identifier-free baseline
// immediately before the external controller interrupts its dedicated relay.
func (a *DesktopApp) ArmP3ACCAcceptanceNetworkFault() error {
	state := p3ACCAcceptanceStateFor(a)
	if state == nil {
		return errors.New("P3ACC_NOT_READY")
	}
	state.mu.Lock()
	if state.networkFaultArmed || state.networkArmInFlight || !state.recoveryProven ||
		state.status.State != room.RuntimeRecording ||
		state.status.RecordingStatus != capture.RecordingActive {
		state.mu.Unlock()
		return errors.New("P3ACC_SNAPSHOT_INVALID")
	}
	state.networkArmInFlight = true
	status := state.status
	state.mu.Unlock()
	database, err := a.queryP3ACCInternalDatabase(status.SessionID)
	if err != nil || !database.Found || database.CurrentAttemptID == "" ||
		database.SessionID != status.SessionID || database.OperationID != status.OperationID ||
		!database.CurrentAttemptCommitted {
		state.mu.Lock()
		state.networkArmInFlight = false
		state.mu.Unlock()
		return errors.New("P3ACC_SNAPSHOT_INVALID")
	}
	state.mu.Lock()
	if state.status.Revision != status.Revision || state.status.SessionID != status.SessionID ||
		state.status.OperationID != status.OperationID {
		state.networkArmInFlight = false
		state.mu.Unlock()
		return errors.New("P3ACC_SNAPSHOT_INVALID")
	}
	state.networkBaselineAttemptID = database.CurrentAttemptID
	state.networkBaselineOperationID = database.OperationID
	state.networkBaselineAtMS = time.Now().UTC().UnixMilli()
	state.networkBaselineAttemptCount = len(database.Attempts)
	state.networkBaselineMessageGap = database.Gaps.MessageDisconnect
	state.networkBaselineRecordingGap = database.Gaps.RecordingRestart
	state.networkBaselineRestartCount = state.progress.RestartCount
	state.networkBaselineRevision = state.status.Revision
	state.networkBaselineProgressAt = state.progressInternalUpdatedAt
	state.postNetworkReconnecting = false
	state.postNetworkMessageOpened = false
	state.postNetworkMessageClosed = false
	state.postNetworkRecordingGapOpened = false
	state.postNetworkRecordingGapClosed = false
	state.postNetworkProgress = false
	state.networkRecoveryProgress = p3ACCRecoveryProgressBaseline{}
	state.networkRecoveryProven = false
	state.networkFaultArmed = true
	state.networkArmInFlight = false
	state.mu.Unlock()
	return a.writeP3ACCAcceptanceSnapshot()
}

func (a *DesktopApp) buildP3ACCAcceptanceSnapshot() (p3ACCAcceptanceSnapshot, error) {
	state := p3ACCAcceptanceStateFor(a)
	if state == nil {
		return p3ACCAcceptanceSnapshot{}, errors.New("P3ACC_NOT_READY")
	}
	state.mu.Lock()
	roomID := state.roomID
	status := state.status
	ui := state.ui
	crashInjected := state.crashInjected
	seenRecording := state.seenRecording
	dataRoot := state.paths.DataRoot
	state.mu.Unlock()

	database, err := a.queryP3ACCInternalDatabase(status.SessionID)
	if err != nil {
		return p3ACCAcceptanceSnapshot{}, err
	}
	sessionFenceStable := database.Found && status.SessionID != "" && status.OperationID != "" &&
		database.SessionID == status.SessionID && database.OperationID == status.OperationID
	recorderTargetMatched := false
	recorderMediaActivity := capture.P3AcceptanceRecorderMediaActivity{}
	recorderMediaActivityComplete := false
	if sessionFenceStable && database.CurrentAttemptID != "" {
		recorderTargetMatched = capture.P3AcceptanceCurrentRecorderMatches(
			a.application.CaptureCoordinator(), roomID,
			database.SessionID, database.OperationID, database.CurrentAttemptID,
		)
		if recorderTargetMatched {
			recorderMediaActivity, recorderMediaActivityComplete =
				capture.P3AcceptanceCurrentRecorderMediaActivity(
					a.application.CaptureCoordinator(), roomID,
					database.SessionID, database.OperationID, database.CurrentAttemptID,
				)
		}
	}
	now := time.Now().UTC()
	sessionManifest, err := summarizeP3ACCSessionManifestFile(dataRoot, database)
	if err != nil {
		return p3ACCAcceptanceSnapshot{}, err
	}
	mediaManifest, err := summarizeP3ACCMediaManifestFile(state, dataRoot, database)
	if err != nil {
		return p3ACCAcceptanceSnapshot{}, err
	}

	state.mu.Lock()
	attemptAdvanced := crashInjected && state.crashBaselineAttemptCount > 0 &&
		len(database.Attempts) > state.crashBaselineAttemptCount &&
		database.CurrentAttemptID != "" && database.CurrentAttemptID != state.crashBaselineAttemptID
	if crashInjected && database.Gaps.RecordingRestart > state.crashBaselineGapCount {
		if database.Gaps.OpenRecordingRestart > 0 {
			state.postCrashGapOpened = true
		}
		if state.postCrashGapOpened && database.Gaps.OpenRecordingRestart == 0 {
			state.postCrashGapClosed = true
		}
	}
	if attemptAdvanced && !state.postCrashProgress {
		state.postCrashProgress = state.p3ACCRecoveryProgressAdvancedLocked(
			now, &state.crashRecoveryProgress, database,
			state.crashBaselineProgressAt, state.crashBaselineRestartCount,
			recorderTargetMatched, recorderMediaActivity, recorderMediaActivityComplete,
		)
	}
	if !attemptAdvanced {
		state.postCrashProgress = false
		state.crashRecoveryProgress = p3ACCRecoveryProgressBaseline{}
	}
	state.updateP3ACCSteadyLocked(
		now, status, database, sessionFenceStable, recorderTargetMatched,
		recorderMediaActivity, recorderMediaActivityComplete,
	)
	progress := state.progress
	recoveryNow := crashInjected && state.postCrashReconnecting && database.Gaps.CrashRecoveryMatched &&
		state.postCrashProgress && attemptAdvanced &&
		status.Revision > state.crashBaselineRevision &&
		state.progressInternalUpdatedAt > state.crashBaselineProgressAt &&
		progress.RestartCount > state.crashBaselineRestartCount &&
		status.State == room.RuntimeRecording && status.RecordingStatus == capture.RecordingActive &&
		database.CurrentAttemptCommitted && recorderTargetMatched
	if recoveryNow {
		state.recoveryProven = true
	}
	recoveryProven := state.recoveryProven
	if state.networkFaultArmed && database.Gaps.MessageDisconnect > state.networkBaselineMessageGap {
		if database.Gaps.OpenMessageDisconnect > 0 {
			state.postNetworkMessageOpened = true
		}
		if state.postNetworkMessageOpened && database.Gaps.OpenMessageDisconnect == 0 {
			state.postNetworkMessageClosed = true
		}
	}
	if state.networkFaultArmed && database.Gaps.RecordingRestart > state.networkBaselineRecordingGap {
		if database.Gaps.OpenRecordingRestart > 0 {
			state.postNetworkRecordingGapOpened = true
		}
		if state.postNetworkRecordingGapOpened && database.Gaps.OpenRecordingRestart == 0 {
			state.postNetworkRecordingGapClosed = true
		}
	}
	networkAttemptAdvanced := len(database.Attempts) > state.networkBaselineAttemptCount &&
		database.CurrentAttemptID != "" && database.CurrentAttemptID != state.networkBaselineAttemptID
	if networkAttemptAdvanced && !state.postNetworkProgress {
		state.postNetworkProgress = state.p3ACCRecoveryProgressAdvancedLocked(
			now, &state.networkRecoveryProgress, database,
			state.networkBaselineProgressAt, state.networkBaselineRestartCount,
			recorderTargetMatched, recorderMediaActivity, recorderMediaActivityComplete,
		)
	}
	networkNow := state.networkFaultArmed && state.postNetworkReconnecting &&
		state.postNetworkProgress && database.Gaps.NetworkMessageMatched &&
		database.Gaps.NetworkRecorderMatched &&
		networkAttemptAdvanced && progress.RestartCount > state.networkBaselineRestartCount &&
		status.Revision > state.networkBaselineRevision &&
		state.progressInternalUpdatedAt > state.networkBaselineProgressAt &&
		status.State == room.RuntimeRecording && status.RecordingStatus == capture.RecordingActive &&
		database.CurrentAttemptCommitted && recorderTargetMatched
	if networkNow {
		state.networkRecoveryProven = true
	}
	networkFaultArmed := state.networkFaultArmed
	networkRecoveryProven := state.networkRecoveryProven
	offlineSequenceProven := state.offlineSequenceProven && state.offlineConfirmRevision > 0 &&
		state.offlineFinalRevision > state.offlineConfirmRevision
	resourceSummary := state.p3ACCResourceSummaryForSnapshotLocked()
	state.mu.Unlock()

	finalizationProven := crashInjected && recoveryProven && networkFaultArmed &&
		networkRecoveryProven && offlineSequenceProven &&
		status.State == room.RuntimeWaiting && status.ErrorCode == "ROOM_OFFLINE" &&
		status.SessionID == "" && status.RecordingStatus == "" && validP3ACCUUID(status.OperationID) &&
		validP3ACCCleanOfflineTerminalOutcome(database.SessionStatus, database.RecordingStatus) &&
		database.EndedAt.Valid &&
		database.Database.SessionCount == 1 && database.Database.ActiveSessionCount == 0 && database.Gaps.Open == 0 &&
		database.Checkpoint.Exists && database.Checkpoint.State == "closed" &&
		database.Checkpoint.CoversSourceEvents && database.Checkpoint.GiftFoldsClosed &&
		database.Database.PublishedEventCount > 0 && database.Database.PublishedEventsPersisted &&
		sessionManifest.Exists && sessionManifest.MatchesDatabase && sessionManifest.ManifestClean &&
		mediaManifest.Exists && mediaManifest.MatchesDatabase && mediaManifest.ManifestClean &&
		validP3ACCTerminalMediaEvidence(mediaManifest)
	stage := determineP3ACCStage(status, database, seenRecording, recoveryProven, finalizationProven)
	snapshot := p3ACCAcceptanceSnapshot{
		Schema: p3ACCAcceptanceSchema, Stage: stage, CapturedAt: now.UnixMilli(), UI: ui,
		Runtime: p3ACCRuntimeSummary{
			State: status.State, RecordingStatus: status.RecordingStatus,
			Revision: status.Revision, ErrorCode: safeP3ACCErrorCode(status.ErrorCode),
			HasSession: database.Found, SessionFenceStable: sessionFenceStable,
			CurrentAttemptCommitted: database.CurrentAttemptCommitted,
			AttemptAdvanced:         attemptAdvanced, AttemptCount: len(database.Attempts),
			RecorderTargetMatched: recorderTargetMatched, CrashInjected: crashInjected,
			RecoveryProven: recoveryProven, NetworkFaultArmed: networkFaultArmed,
			NetworkRecoveryProven: networkRecoveryProven, FinalizationProven: finalizationProven,
		},
		Progress: progress, Database: database.Database,
		SessionManifest: sessionManifest, MediaManifest: mediaManifest,
		Gaps: database.Gaps, Checkpoint: database.Checkpoint, Resources: resourceSummary,
	}
	if err := validateP3ACCAcceptanceSnapshot(snapshot); err != nil {
		return p3ACCAcceptanceSnapshot{}, err
	}
	return snapshot, nil
}

func (s *p3ACCAcceptanceState) p3ACCRecoveryProgressAdvancedLocked(
	now time.Time,
	baseline *p3ACCRecoveryProgressBaseline,
	database p3ACCInternalDatabaseSnapshot,
	minimumUpdatedAt int64,
	minimumRestartCount int,
	recorderTargetMatched bool,
	media capture.P3AcceptanceRecorderMediaActivity,
	mediaComplete bool,
) bool {
	if baseline == nil || !recorderTargetMatched || !database.CurrentAttemptCommitted ||
		database.CurrentAttemptID == "" || !mediaComplete || media.FileCount < 1 || media.TotalBytes < 1 {
		return false
	}
	progress := s.progress
	fresh := !s.progressReceivedAt.IsZero() && now.Sub(s.progressReceivedAt) >= 0 &&
		now.Sub(s.progressReceivedAt) <= p3ACCAcceptanceProgressFreshness
	if !fresh || progress.SampleCount < 1 || progress.RestartCount <= minimumRestartCount ||
		s.progressInternalUpdatedAt <= minimumUpdatedAt || progress.ElapsedMS < 1 ||
		s.progressFrame < 1 || progress.SegmentCount < 1 {
		return false
	}
	if baseline.attemptID != database.CurrentAttemptID {
		*baseline = p3ACCRecoveryProgressBaseline{
			attemptID: database.CurrentAttemptID, updatedAt: s.progressInternalUpdatedAt,
			elapsedMS: progress.ElapsedMS, frame: s.progressFrame,
			segmentCount: progress.SegmentCount, physicalBytes: media.TotalBytes,
			physicalFileCount: media.FileCount,
		}
		return false
	}
	return s.progressInternalUpdatedAt > baseline.updatedAt &&
		progress.ElapsedMS > baseline.elapsedMS && s.progressFrame > baseline.frame &&
		progress.SegmentCount >= baseline.segmentCount &&
		media.TotalBytes > baseline.physicalBytes && media.FileCount >= baseline.physicalFileCount
}

func (s *p3ACCAcceptanceState) updateP3ACCSteadyLocked(
	now time.Time,
	status p3ACCObservedStatus,
	database p3ACCInternalDatabaseSnapshot,
	sessionFenceStable bool,
	recorderTargetMatched bool,
	media capture.P3AcceptanceRecorderMediaActivity,
	mediaComplete bool,
) {
	progress := s.progress
	fresh := !s.progressReceivedAt.IsZero() && now.Sub(s.progressReceivedAt) >= 0 &&
		now.Sub(s.progressReceivedAt) <= p3ACCAcceptanceProgressFreshness
	valid := fresh && sessionFenceStable && database.CurrentAttemptCommitted &&
		database.CurrentAttemptID != "" && status.State == room.RuntimeRecording &&
		status.RecordingStatus == capture.RecordingActive && progress.SampleCount > 0 &&
		progress.ElapsedMS > 0 && progress.SegmentCount > 0 && s.progressFrame > 0 &&
		s.progressInternalUpdatedAt > 0 && recorderTargetMatched && mediaComplete &&
		media.FileCount > 0 && media.TotalBytes > 0
	if !valid {
		if !s.steadyStartedAt.IsZero() {
			s.resetP3ACCSteadyLocked()
		}
		return
	}
	sameFence := s.steadySessionID == database.SessionID &&
		s.steadyOperationID == database.OperationID &&
		s.steadyAttemptID == database.CurrentAttemptID &&
		s.steadyRestartCount == progress.RestartCount
	newProgress := s.progressInternalUpdatedAt > s.steadyLastProgressAt
	progressGrew := progress.ElapsedMS > s.steadyLastElapsedMS &&
		s.progressFrame > s.steadyLastFrame &&
		progress.SegmentCount >= s.steadyLastSegmentCount
	physicalRegressed := media.TotalBytes < s.steadyLastPhysicalBytes ||
		media.FileCount < s.steadyLastPhysicalFileCount
	if s.steadyStartedAt.IsZero() || !sameFence || newProgress && !progressGrew || physicalRegressed {
		s.resetP3ACCSteadyLocked()
		s.startP3ACCSteadyBaselineLocked(now, database, progress, media)
		return
	}
	if media.TotalBytes > s.steadyLastPhysicalBytes {
		s.steadyLastPhysicalBytes = media.TotalBytes
		s.steadyLastPhysicalFileCount = media.FileCount
		s.steadyLastPhysicalGrowthAt = now
	} else if media.FileCount > s.steadyLastPhysicalFileCount {
		s.steadyLastPhysicalFileCount = media.FileCount
	}
	if s.steadyLastPhysicalGrowthAt.IsZero() ||
		now.Sub(s.steadyLastPhysicalGrowthAt) > p3ACCAcceptanceProgressFreshness {
		s.resetP3ACCSteadyLocked()
		s.startP3ACCSteadyBaselineLocked(now, database, progress, media)
		return
	}
	if newProgress {
		s.steadyLastProgressAt = s.progressInternalUpdatedAt
		s.steadyLastElapsedMS = progress.ElapsedMS
		s.steadyLastFrame = s.progressFrame
		s.steadyLastSegmentCount = progress.SegmentCount
		s.steadySampleCount++
	}
	s.progress.SteadySampleCount = s.steadySampleCount
	s.progress.SteadyRecordingMS = now.Sub(s.steadyStartedAt).Milliseconds()
	if s.progress.SteadyRecordingMS < 0 {
		s.resetP3ACCSteadyLocked()
	}
}

func (s *p3ACCAcceptanceState) startP3ACCSteadyBaselineLocked(
	now time.Time,
	database p3ACCInternalDatabaseSnapshot,
	progress p3ACCProgressSummary,
	media capture.P3AcceptanceRecorderMediaActivity,
) {
	s.steadyStartedAt = now
	s.steadySessionID = database.SessionID
	s.steadyOperationID = database.OperationID
	s.steadyAttemptID = database.CurrentAttemptID
	s.steadyRestartCount = progress.RestartCount
	s.steadyLastProgressAt = s.progressInternalUpdatedAt
	s.steadyLastElapsedMS = progress.ElapsedMS
	s.steadyLastFrame = s.progressFrame
	s.steadyLastSegmentCount = progress.SegmentCount
	s.steadyLastPhysicalBytes = media.TotalBytes
	s.steadyLastPhysicalFileCount = media.FileCount
	s.steadyLastPhysicalGrowthAt = now
	s.steadySampleCount = 1
	s.progress.SteadySampleCount = 1
}

func validP3ACCCleanOfflineTerminalOutcome(
	status capture.SessionStatus,
	recordingStatus capture.RecordingStatus,
) bool {
	return status == capture.SessionCompleted && recordingStatus == capture.RecordingIncomplete
}

func determineP3ACCStage(
	status p3ACCObservedStatus,
	database p3ACCInternalDatabaseSnapshot,
	seenRecording, recoveryProven, finalizationProven bool,
) p3ACCStage {
	if status.State == room.RuntimeError ||
		(status.State == room.RuntimeFinalizing && status.ErrorCode == "CAPTURE_FINALIZE_FAILED" &&
			status.RetryAt == 0) {
		return p3ACCStageError
	}
	if finalizationProven {
		return p3ACCStageFinalized
	}
	if database.Found {
		switch database.SessionStatus {
		case capture.SessionFinalizing:
			return p3ACCStageFinalizing
		}
	}
	if status.State == room.RuntimeWaiting && status.ErrorCode == "ROOM_OFFLINE" && !seenRecording {
		return p3ACCStageOffline
	}
	if recoveryProven {
		return p3ACCStageRecovered
	}
	switch status.State {
	case room.RuntimeStopped:
		return p3ACCStageConfigured
	case room.RuntimeWaiting:
		return p3ACCStageWaiting
	case room.RuntimeStarting:
		return p3ACCStageStarting
	case room.RuntimeLive:
		return p3ACCStageLive
	case room.RuntimeRecording:
		return p3ACCStageRecording
	case room.RuntimeReconnecting:
		return p3ACCStageReconnecting
	case room.RuntimeFinalizing:
		return p3ACCStageFinalizing
	default:
		return p3ACCStageInitializing
	}
}

func (a *DesktopApp) queryP3ACCInternalDatabase(preferredSessionID string) (p3ACCInternalDatabaseSnapshot, error) {
	state := p3ACCAcceptanceStateFor(a)
	store := a.application.Store()
	if state == nil || store == nil {
		return p3ACCInternalDatabaseSnapshot{}, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	ctx, cancel := context.WithTimeout(a.application.Context(), p3ACCAcceptanceQueryTimeout)
	defer cancel()
	reader := store.Reader()
	state.mu.Lock()
	roomID := state.roomID
	crashBaselineAttemptID := state.crashBaselineAttemptID
	crashBaselineOperationID := state.crashBaselineOperationID
	crashBaselineAtMS := state.crashBaselineAtMS
	networkBaselineAttemptID := state.networkBaselineAttemptID
	networkBaselineOperationID := state.networkBaselineOperationID
	networkBaselineAtMS := state.networkBaselineAtMS
	observedEventIDs := make([]string, 0, len(state.observedEventIDs))
	for eventID := range state.observedEventIDs {
		observedEventIDs = append(observedEventIDs, eventID)
	}
	observedEventOverflow := state.observedEventOverflow
	state.mu.Unlock()
	var result p3ACCInternalDatabaseSnapshot
	query := `SELECT id, room_config_id, operation_id, platform_room_id, title,
		status, recording_status, manifest_dirty, started_at, ended_at, media_epoch_at,
		capture_offset_ms, clock_source, integrity_score, data_path, schema_version,
		created_at, updated_at
		FROM live_sessions WHERE room_config_id = ?`
	arguments := []any{roomID}
	if validP3ACCUUID(preferredSessionID) {
		query += ` AND id = ?`
		arguments = append(arguments, preferredSessionID)
	}
	query += ` ORDER BY started_at DESC, id DESC LIMIT 1`
	var platformRoomID sql.NullString
	var endedAt, mediaEpochAt sql.NullInt64
	var manifestDirty int
	err := reader.QueryRowContext(ctx, query, arguments...).Scan(
		&result.Session.ID, &result.Session.RoomConfigID, &result.Session.OperationID,
		&platformRoomID, &result.Session.Title, &result.Session.Status,
		&result.Session.RecordingStatus, &manifestDirty, &result.Session.StartedAt,
		&endedAt, &mediaEpochAt, &result.Session.CaptureOffsetMS, &result.Session.ClockSource,
		&result.Session.IntegrityScore, &result.Session.DataPath, &result.Session.SchemaVersion,
		&result.Session.CreatedAt, &result.Session.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	result.Session.PlatformRoomID = platformRoomID.String
	if endedAt.Valid {
		value := endedAt.Int64
		result.Session.EndedAt = &value
	}
	if mediaEpochAt.Valid {
		value := mediaEpochAt.Int64
		result.Session.MediaEpochAt = &value
	}
	if err != nil || manifestDirty < 0 || manifestDirty > 1 ||
		result.Session.RoomConfigID != roomID || !validP3ACCUUID(result.Session.ID) ||
		!validP3ACCUUID(result.Session.OperationID) || !validP3ACCSessionStatus(result.Session.Status) ||
		!validP3ACCRecordingStatus(result.Session.RecordingStatus, false) {
		return p3ACCInternalDatabaseSnapshot{}, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	result.Session.ManifestDirty = manifestDirty == 1
	result.SessionID = result.Session.ID
	result.OperationID = result.Session.OperationID
	result.DataPath = result.Session.DataPath
	result.SessionStatus = result.Session.Status
	result.RecordingStatus = result.Session.RecordingStatus
	result.EndedAt = endedAt
	result.Found = true
	if err := reader.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN status IN ('starting', 'recording', 'finalizing') THEN 1 ELSE 0 END), 0)
		FROM live_sessions WHERE room_config_id = ?`, roomID).
		Scan(&result.Database.SessionCount, &result.Database.ActiveSessionCount); err != nil {
		return p3ACCInternalDatabaseSnapshot{}, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	if err := reader.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN event_role = 'source' THEN 1 ELSE 0 END), 0),
		COALESCE(MAX(CASE WHEN event_role = 'source' THEN ingest_sequence ELSE 0 END), 0)
		FROM live_events WHERE session_id = ?`, result.SessionID).
		Scan(&result.Database.EventCount, &result.Database.SourceEventCount,
			&result.Checkpoint.MaxSourceSequence); err != nil {
		return p3ACCInternalDatabaseSnapshot{}, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	result.Database.PublishedEventCount = len(observedEventIDs)
	persistedUIEvents, err := countP3ACCPersistedUIEvents(ctx, reader, result.SessionID, observedEventIDs)
	if err != nil {
		return p3ACCInternalDatabaseSnapshot{}, err
	}
	result.Database.PublishedEventsPersisted = !observedEventOverflow && persistedUIEvents == len(observedEventIDs)
	if err := reader.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN status IN ('complete', 'recovered') THEN 1 ELSE 0 END), 0)
		FROM media_segments WHERE session_id = ?`, result.SessionID).
		Scan(&result.Database.SegmentCount, &result.Database.CompleteSegmentCount); err != nil {
		return p3ACCInternalDatabaseSnapshot{}, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	if err := reader.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN status = 'complete' THEN 1 ELSE 0 END), 0)
		FROM media_artifacts WHERE session_id = ?`, result.SessionID).
		Scan(&result.Database.ArtifactCount, &result.Database.CompleteArtifactCount); err != nil {
		return p3ACCInternalDatabaseSnapshot{}, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	var attemptsJSON string
	var mediaRootID sql.NullString
	var mediaEpoch sql.NullInt64
	var mediaDirty int
	err = reader.QueryRowContext(ctx, `SELECT root_id, relative_path, state, manifest_revision,
		manifest_dirty, media_epoch_at, attempts_json, created_at, updated_at
		FROM session_media WHERE session_id = ?`, result.SessionID).Scan(
		&mediaRootID, &result.Media.RelativePath, &result.Media.State,
		&result.Media.ManifestRevision, &mediaDirty, &mediaEpoch, &attemptsJSON,
		&result.Media.CreatedAt, &result.Media.UpdatedAt,
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return p3ACCInternalDatabaseSnapshot{}, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	if err == nil {
		if mediaRootID.Valid {
			value := mediaRootID.String
			result.Media.RootID = &value
		}
		if mediaEpoch.Valid {
			value := mediaEpoch.Int64
			result.Media.MediaEpochAt = &value
		}
		result.Media.SessionID = result.SessionID
		result.Media.ManifestDirty = mediaDirty == 1
		result.MediaRootID = mediaRootID
		result.MediaState = result.Media.State
		result.MediaRevision = result.Media.ManifestRevision
		result.MediaDirty = result.Media.ManifestDirty
		decoder := json.NewDecoder(strings.NewReader(attemptsJSON))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&result.Attempts); err != nil || ensureP3ACCJSONEOF(decoder) != nil ||
			mediaDirty < 0 || mediaDirty > 1 || !validP3ACCMediaState(result.MediaState) ||
			result.MediaRevision < 0 {
			return p3ACCInternalDatabaseSnapshot{}, errors.New("P3ACC_SNAPSHOT_FAILED")
		}
		for index, attempt := range result.Attempts {
			if !validP3ACCUUID(attempt.ID) || attempt.Ordinal != index+1 {
				return p3ACCInternalDatabaseSnapshot{}, errors.New("P3ACC_SNAPSHOT_FAILED")
			}
		}
		result.Media.Attempts = append([]capture.MediaAttempt(nil), result.Attempts...)
		result.Segments, err = queryP3ACCMediaSegments(ctx, reader, result.SessionID)
		if err != nil {
			return p3ACCInternalDatabaseSnapshot{}, err
		}
		result.Artifacts, err = queryP3ACCMediaArtifacts(ctx, reader, result.SessionID)
		if err != nil {
			return p3ACCInternalDatabaseSnapshot{}, err
		}
		if count := len(result.Attempts); count > 0 {
			current := result.Attempts[count-1]
			result.CurrentAttemptID = current.ID
			result.CurrentAttemptCommitted = current.Committed
		}
	}
	if err := queryP3ACCGaps(ctx, reader, result.SessionID, &result.Gaps); err != nil {
		return p3ACCInternalDatabaseSnapshot{}, err
	}
	if err := queryP3ACCFaultProofs(
		ctx, reader, result.SessionID,
		crashBaselineAttemptID, crashBaselineOperationID, crashBaselineAtMS,
		networkBaselineAttemptID, networkBaselineOperationID, networkBaselineAtMS,
		&result.Gaps,
	); err != nil {
		return p3ACCInternalDatabaseSnapshot{}, err
	}
	if err := queryP3ACCCheckpoint(ctx, reader, result.SessionID, &result.Checkpoint); err != nil {
		return p3ACCInternalDatabaseSnapshot{}, err
	}
	return result, nil
}

func countP3ACCPersistedUIEvents(
	ctx context.Context,
	reader *sql.DB,
	sessionID string,
	eventIDs []string,
) (int, error) {
	if len(eventIDs) == 0 {
		return 0, nil
	}
	total := 0
	for start := 0; start < len(eventIDs); start += 400 {
		end := start + 400
		if end > len(eventIDs) {
			end = len(eventIDs)
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", end-start), ",")
		arguments := make([]any, 0, end-start+1)
		arguments = append(arguments, sessionID)
		for _, eventID := range eventIDs[start:end] {
			arguments = append(arguments, eventID)
		}
		var count int
		query := `SELECT COUNT(*) FROM live_events WHERE session_id = ? AND id IN (` + placeholders + `)`
		if err := reader.QueryRowContext(ctx, query, arguments...).Scan(&count); err != nil {
			return 0, errors.New("P3ACC_SNAPSHOT_FAILED")
		}
		total += count
	}
	return total, nil
}

func queryP3ACCMediaSegments(ctx context.Context, reader *sql.DB, sessionID string) ([]capture.MediaSegment, error) {
	rows, err := reader.QueryContext(ctx, `SELECT id, sequence, relative_path, container,
		video_codec, audio_codec, started_at, ended_at, pts_start_ms, pts_end_ms,
		duration_ms, size_bytes, sha256, status, attempt_id, attempt_sequence,
		source_relative_path, probe_version, error_code
		FROM media_segments WHERE session_id = ? ORDER BY sequence ASC, id ASC
		LIMIT ?`, sessionID, p3ACCAcceptanceMaximumMediaSegments+1)
	if err != nil {
		return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	defer rows.Close()
	result := make([]capture.MediaSegment, 0)
	for rows.Next() {
		var segment capture.MediaSegment
		var video, audio, digest, errorCode sql.NullString
		var ptsStart, ptsEnd, attemptSequence sql.NullInt64
		if err := rows.Scan(
			&segment.ID, &segment.Sequence, &segment.RelativePath, &segment.Container,
			&video, &audio, &segment.StartedAt, &segment.EndedAt, &ptsStart, &ptsEnd,
			&segment.DurationMS, &segment.SizeBytes, &digest, &segment.Status, &segment.AttemptID,
			&attemptSequence, &segment.SourceRelativePath, &segment.ProbeVersion, &errorCode,
		); err != nil {
			return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
		}
		segment.VideoCodec = video.String
		segment.AudioCodec = audio.String
		segment.SHA256 = digest.String
		segment.ErrorCode = errorCode.String
		if ptsStart.Valid {
			value := ptsStart.Int64
			segment.PTSStartMS = &value
		}
		if ptsEnd.Valid {
			value := ptsEnd.Int64
			segment.PTSEndMS = &value
		}
		if attemptSequence.Valid {
			segment.AttemptSequence = int(attemptSequence.Int64)
		}
		result = append(result, segment)
		if len(result) > p3ACCAcceptanceMaximumMediaSegments {
			return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
		}
	}
	if rows.Err() != nil {
		return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	return result, nil
}

func queryP3ACCMediaArtifacts(ctx context.Context, reader *sql.DB, sessionID string) ([]capture.MediaArtifact, error) {
	rows, err := reader.QueryContext(ctx, `SELECT id, media_segment_id, kind, relative_path,
		container, codec, duration_ms, size_bytes, sample_rate, channels, sha256,
		source_sha256, status, error_code, created_at, updated_at
		FROM media_artifacts WHERE session_id = ?
		ORDER BY media_segment_id ASC, kind ASC, id ASC LIMIT ?`,
		sessionID, p3ACCAcceptanceMaximumMediaArtifacts+1)
	if err != nil {
		return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	defer rows.Close()
	result := make([]capture.MediaArtifact, 0)
	for rows.Next() {
		var artifact capture.MediaArtifact
		var errorCode sql.NullString
		if err := rows.Scan(
			&artifact.ID, &artifact.MediaSegmentID, &artifact.Kind, &artifact.RelativePath,
			&artifact.Container, &artifact.Codec, &artifact.DurationMS, &artifact.SizeBytes,
			&artifact.SampleRate, &artifact.Channels, &artifact.SHA256, &artifact.SourceSHA256,
			&artifact.Status, &errorCode, &artifact.CreatedAt, &artifact.UpdatedAt,
		); err != nil {
			return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
		}
		artifact.ErrorCode = errorCode.String
		result = append(result, artifact)
		if len(result) > p3ACCAcceptanceMaximumMediaArtifacts {
			return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
		}
	}
	if rows.Err() != nil {
		return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	return result, nil
}

func queryP3ACCGaps(ctx context.Context, reader *sql.DB, sessionID string, result *p3ACCGapSummary) error {
	err := reader.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN ended_at IS NULL THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN recovered = 1 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN kind = 'recording_restart' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN kind = 'recording_restart' AND ended_at IS NULL THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN kind = 'message_disconnect' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN kind = 'message_disconnect' AND ended_at IS NULL THEN 1 ELSE 0 END), 0)
		FROM capture_gaps WHERE session_id = ?`, sessionID).Scan(
		&result.Total, &result.Open, &result.Recovered, &result.RecordingRestart,
		&result.OpenRecordingRestart, &result.MessageDisconnect,
		&result.OpenMessageDisconnect,
	)
	if err != nil {
		return errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	var open, recovered int
	err = reader.QueryRowContext(ctx, `SELECT kind, reason_code,
		CASE WHEN ended_at IS NULL THEN 1 ELSE 0 END, recovered
		FROM capture_gaps WHERE session_id = ? ORDER BY started_at DESC, id DESC LIMIT 1`, sessionID).
		Scan(&result.LatestKind, &result.LatestReasonCode, &open, &recovered)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil || !validP3ACCGapKind(result.LatestKind) {
		return errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	result.LatestReasonCode = safeP3ACCErrorCode(result.LatestReasonCode)
	result.LatestOpen = open == 1
	result.LatestRecovered = recovered == 1
	return nil
}

func queryP3ACCFaultProofs(
	ctx context.Context,
	reader *sql.DB,
	sessionID string,
	crashAttemptID, crashOperationID string,
	crashAtMS int64,
	networkAttemptID, networkOperationID string,
	networkAtMS int64,
	result *p3ACCGapSummary,
) error {
	recorderGaps, err := queryP3ACCRecorderRecoveryGaps(ctx, reader, sessionID)
	if err != nil {
		return err
	}
	if len(recorderGaps) != result.RecordingRestart {
		return errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	crashMatches := 0
	for _, gap := range recorderGaps {
		if gap.Details.SourceErrorCode == capture.RecorderProcessExitedErrorCode {
			result.ProcessCrash++
		}
		if p3ACCRecorderGapMatches(
			gap, crashAttemptID, crashOperationID, capture.RecorderProcessExitedErrorCode, crashAtMS,
		) {
			crashMatches++
		}
	}
	result.CrashRecoveryMatched = crashMatches == 1

	networkMessage, messageMatches, err := queryP3ACCNetworkMessageProof(
		ctx, reader, sessionID, networkOperationID, networkAtMS, result.MessageDisconnect,
	)
	if err != nil {
		return err
	}
	result.NetworkMessageMatched = messageMatches == 1
	if result.NetworkMessageMatched {
		networkRecorderMatches := 0
		for _, gap := range recorderGaps {
			if p3ACCNetworkRecorderOperationMatches(
				gap.StartedAt, networkMessage, networkOperationID, gap.Details.SourceOperationID,
			) && p3ACCRecorderGapMatches(
				gap, networkAttemptID, gap.Details.SourceOperationID,
				capture.RecorderNetworkFailureErrorCode, networkAtMS,
			) {
				networkRecorderMatches++
			}
		}
		result.NetworkRecorderMatched = networkRecorderMatches == 1
	}
	return nil
}

func p3ACCNetworkRecorderOperationMatches(
	recorderStartedAt int64,
	message p3ACCMessageRecoveryGapEvidence,
	baselineOperationID, actualOperationID string,
) bool {
	switch {
	case recorderStartedAt < message.StartedAt:
		return actualOperationID == baselineOperationID
	case recorderStartedAt > message.StartedAt:
		return actualOperationID == message.OpenedOperationID
	default:
		return actualOperationID == baselineOperationID ||
			actualOperationID == message.OpenedOperationID
	}
}

func queryP3ACCRecorderRecoveryGaps(
	ctx context.Context,
	reader *sql.DB,
	sessionID string,
) ([]p3ACCRecorderRecoveryGapEvidence, error) {
	rows, err := reader.QueryContext(ctx, `SELECT started_at, ended_at, recovered,
		reason_code, details_json, dedupe_key
		FROM capture_gaps WHERE session_id = ? AND kind = 'recording_restart'
		AND media_segment_id IS NULL AND severity = 'warning'
		ORDER BY started_at ASC, id ASC LIMIT ?`, sessionID, p3ACCAcceptanceMaximumRecoveryGaps+1)
	if err != nil {
		return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	defer rows.Close()
	result := make([]p3ACCRecorderRecoveryGapEvidence, 0)
	for rows.Next() {
		var gap p3ACCRecorderRecoveryGapEvidence
		var recovered int
		var detailsJSON string
		if err := rows.Scan(
			&gap.StartedAt, &gap.EndedAt, &recovered,
			&gap.ReasonCode, &detailsJSON, &gap.DedupeKey,
		); err != nil || recovered < 0 || recovered > 1 || gap.StartedAt < 0 ||
			gap.EndedAt.Valid && gap.EndedAt.Int64 < gap.StartedAt {
			return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
		}
		gap.Recovered = recovered == 1
		decoder := json.NewDecoder(strings.NewReader(detailsJSON))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&gap.Details); err != nil || ensureP3ACCJSONEOF(decoder) != nil ||
			gap.Details.Version != 1 || !validP3ACCUUID(gap.Details.SourceAttemptID) ||
			!validP3ACCUUID(gap.Details.SourceOperationID) || gap.Details.RestartAttempts < 0 ||
			gap.Details.LastOccurredAtMS < gap.StartedAt ||
			!validP3ACCRecorderRecoveryCode(gap.Details.SourceErrorCode) ||
			!validP3ACCRecorderRecoveryCode(gap.Details.LastErrorCode) {
			return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
		}
		result = append(result, gap)
		if len(result) > p3ACCAcceptanceMaximumRecoveryGaps {
			return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
		}
	}
	if rows.Err() != nil {
		return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	return result, nil
}

func queryP3ACCNetworkMessageProof(
	ctx context.Context,
	reader *sql.DB,
	sessionID, baselineOperationID string,
	baselineAtMS int64,
	expectedCount int,
) (p3ACCMessageRecoveryGapEvidence, int, error) {
	rows, err := reader.QueryContext(ctx, `SELECT started_at, ended_at, recovered,
		reason_code, details_json, dedupe_key
		FROM capture_gaps WHERE session_id = ? AND kind = 'message_disconnect'
		AND media_segment_id IS NULL AND severity = 'warning'
		ORDER BY started_at ASC, id ASC LIMIT ?`, sessionID, p3ACCAcceptanceMaximumRecoveryGaps+1)
	if err != nil {
		return p3ACCMessageRecoveryGapEvidence{}, 0, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	defer rows.Close()
	count := 0
	matches := 0
	matched := p3ACCMessageRecoveryGapEvidence{}
	for rows.Next() {
		count++
		if count > p3ACCAcceptanceMaximumRecoveryGaps {
			return p3ACCMessageRecoveryGapEvidence{}, 0, errors.New("P3ACC_SNAPSHOT_FAILED")
		}
		var startedAt int64
		var endedAt sql.NullInt64
		var recovered int
		var reasonCode, detailsJSON, dedupeKey string
		if err := rows.Scan(&startedAt, &endedAt, &recovered, &reasonCode, &detailsJSON, &dedupeKey); err != nil ||
			recovered < 0 || recovered > 1 || startedAt < 0 || endedAt.Valid && endedAt.Int64 < startedAt {
			return p3ACCMessageRecoveryGapEvidence{}, 0, errors.New("P3ACC_SNAPSHOT_FAILED")
		}
		var details p3ACCMessageRecoveryGapDetails
		decoder := json.NewDecoder(strings.NewReader(detailsJSON))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&details); err != nil || ensureP3ACCJSONEOF(decoder) != nil ||
			details.Version != 1 || !validP3ACCUUID(details.OpenedOperationID) ||
			!validP3ACCUUID(details.LastOperationID) || details.BeginAttempts < 1 ||
			details.LastOccurredAtMS < startedAt {
			return p3ACCMessageRecoveryGapEvidence{}, 0, errors.New("P3ACC_SNAPSHOT_FAILED")
		}
		if validP3ACCUUID(baselineOperationID) && baselineAtMS > 0 && startedAt >= baselineAtMS &&
			endedAt.Valid && recovered == 1 && reasonCode == capture.MessageRecoveryRecoveredErrorCode &&
			details.OpenedOperationID != baselineOperationID &&
			details.FirstErrorCode == capture.MessageDisconnectErrorCode &&
			details.LastErrorCode == capture.MessageRecoveryRecoveredErrorCode &&
			details.LastOccurredAtMS == endedAt.Int64 &&
			dedupeKey == "message-recovery:"+details.OpenedOperationID {
			matches++
			matched = p3ACCMessageRecoveryGapEvidence{
				StartedAt: startedAt, OpenedOperationID: details.OpenedOperationID,
			}
		}
	}
	if rows.Err() != nil || count != expectedCount {
		return p3ACCMessageRecoveryGapEvidence{}, 0, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	return matched, matches, nil
}

func p3ACCRecorderGapMatches(
	gap p3ACCRecorderRecoveryGapEvidence,
	attemptID, operationID, errorCode string,
	baselineAtMS int64,
) bool {
	return validP3ACCUUID(attemptID) && validP3ACCUUID(operationID) && baselineAtMS > 0 &&
		gap.StartedAt >= baselineAtMS && gap.EndedAt.Valid && gap.EndedAt.Int64 >= gap.StartedAt &&
		gap.Recovered && gap.ReasonCode == errorCode &&
		gap.DedupeKey == "recorder-recovery:"+attemptID &&
		gap.Details.SourceAttemptID == attemptID && gap.Details.SourceOperationID == operationID &&
		gap.Details.SourceErrorCode == errorCode && gap.Details.LastErrorCode == errorCode &&
		gap.Details.RestartAttempts > 0 && gap.Details.LastOccurredAtMS == gap.EndedAt.Int64
}

func validP3ACCRecorderRecoveryCode(value string) bool {
	switch value {
	case capture.RecorderProcessExitedErrorCode,
		capture.RecorderStreamExpiredErrorCode,
		capture.RecorderNetworkFailureErrorCode,
		capture.RecorderUnsupportedInputErrorCode,
		capture.RecorderLocalResourceErrorCode,
		capture.RecorderDependencyFailureErrorCode:
		return true
	default:
		return false
	}
}

func queryP3ACCCheckpoint(ctx context.Context, reader *sql.DB, sessionID string, result *p3ACCCheckpointSummary) error {
	err := reader.QueryRowContext(ctx, `SELECT committed_sequence, state
		FROM event_ingest_checkpoints WHERE session_id = ?`, sessionID).
		Scan(&result.CommittedSequence, &result.State)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil || result.CommittedSequence < 0 || !validP3ACCCheckpointState(result.State) {
		return errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	result.Exists = true
	result.CoversSourceEvents = result.MaxSourceSequence <= result.CommittedSequence
	if err := reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM gift_combo_states
		WHERE session_id = ? AND status = 'open'`, sessionID).Scan(&result.OpenGiftFoldCount); err != nil {
		return errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	result.GiftFoldsClosed = result.OpenGiftFoldCount == 0
	return nil
}

func summarizeP3ACCSessionManifestFile(
	dataRoot string,
	database p3ACCInternalDatabaseSnapshot,
) (p3ACCSessionManifestSummary, error) {
	if !database.Found {
		return p3ACCSessionManifestSummary{}, nil
	}
	sessionDirectory, safe := p3ACCSessionDirectory(dataRoot, database.DataPath)
	if !safe {
		return p3ACCSessionManifestSummary{}, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	payload, err := readP3ACCBoundedFile(
		dataRoot, filepath.Join(sessionDirectory, "session.json"),
		p3ACCAcceptanceMaximumSnapshotBytes,
	)
	if errors.Is(err, os.ErrNotExist) {
		return p3ACCSessionManifestSummary{}, nil
	}
	if err != nil {
		return p3ACCSessionManifestSummary{}, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var manifest capture.LiveSession
	if err := decoder.Decode(&manifest); err != nil || ensureP3ACCJSONEOF(decoder) != nil {
		return p3ACCSessionManifestSummary{}, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	expected, err := json.MarshalIndent(database.Session, "", "  ")
	if err != nil {
		return p3ACCSessionManifestSummary{}, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	expected = append(expected, '\n')
	actualHash := sha256.Sum256(payload)
	expectedHash := sha256.Sum256(expected)
	canonicalMatches := actualHash == expectedHash && bytes.Equal(payload, expected)
	return p3ACCSessionManifestSummary{
		Exists: true, MatchesDatabase: canonicalMatches, CanonicalHashMatches: canonicalMatches,
		ManifestClean: !database.Session.ManifestDirty, Ended: database.Session.EndedAt != nil,
		Status: manifest.Status, RecordingStatus: manifest.RecordingStatus,
	}, nil
}

func summarizeP3ACCMediaManifestFile(
	state *p3ACCAcceptanceState,
	dataRoot string,
	database p3ACCInternalDatabaseSnapshot,
) (p3ACCMediaManifestSummary, error) {
	if !database.Found {
		return p3ACCMediaManifestSummary{}, nil
	}
	if database.MediaRootID.Valid {
		return p3ACCMediaManifestSummary{}, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	if len(database.Attempts) == 0 {
		return p3ACCMediaManifestSummary{}, nil
	}
	sessionDirectory, safe := p3ACCSessionDirectory(dataRoot, database.DataPath)
	if !safe {
		return p3ACCMediaManifestSummary{}, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	payload, err := readP3ACCBoundedFile(
		dataRoot,
		filepath.Join(sessionDirectory, "manifests", "media.json"),
		p3ACCAcceptanceMaximumManifestBytes,
	)
	if errors.Is(err, os.ErrNotExist) {
		return p3ACCMediaManifestSummary{}, nil
	}
	if err != nil {
		return p3ACCMediaManifestSummary{}, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	result, err := summarizeP3ACCMediaManifest(payload, database.SessionID)
	if err != nil {
		return p3ACCMediaManifestSummary{}, err
	}
	expected := p3ACCMediaManifestWire{SchemaVersion: 1}
	expected.Session.ID = database.Media.SessionID
	expected.Session.RecordingRootID = database.Media.RootID
	expected.Session.RelativePath = database.Media.RelativePath
	expected.Session.State = database.Media.State
	expected.Session.Revision = database.Media.ManifestRevision
	expected.Session.MediaEpochAt = database.Media.MediaEpochAt
	expected.Session.CreatedAt = database.Media.CreatedAt
	expected.Session.UpdatedAt = database.Media.UpdatedAt
	expected.Attempts = append([]capture.MediaAttempt(nil), database.Attempts...)
	expected.Segments = append([]capture.MediaSegment(nil), database.Segments...)
	expected.Artifacts = append([]capture.MediaArtifact(nil), database.Artifacts...)
	expectedPayload, err := encodeP3ACCMediaManifestWire(expected)
	if err != nil {
		return p3ACCMediaManifestSummary{}, err
	}
	actualHash := sha256.Sum256(payload)
	expectedHash := sha256.Sum256(expectedPayload)
	canonicalMatches := actualHash == expectedHash && bytes.Equal(payload, expectedPayload)
	result.MatchesDatabase = canonicalMatches
	result.CanonicalHashMatches = canonicalMatches
	result.ManifestClean = !database.Media.ManifestDirty
	if canonicalMatches && result.ManifestClean &&
		(result.State == capture.SessionMediaCompleted || result.State == capture.SessionMediaIncomplete) {
		verification := verifyP3ACCMediaFilesCached(state, dataRoot, database)
		result.FileCheckCount = verification.FileCheckCount
		result.FileFailureCount = verification.FileFailureCount
		result.IncompleteEntryCount = verification.IncompleteEntryCount
		result.IncompleteSegmentCount = verification.IncompleteSegmentCount
		result.AllFilesMatch = verification.AllFilesMatch
		result.SequenceContinuous = verification.SequenceContinuous
		result.AttemptReferencesValid = verification.AttemptReferencesValid
		result.FaultPhaseSegmentsProven = verification.FaultPhaseSegmentsProven
	}
	return result, nil
}

func verifyP3ACCMediaFilesCached(
	state *p3ACCAcceptanceState,
	mediaRoot string,
	database p3ACCInternalDatabaseSnapshot,
) p3ACCMediaManifestSummary {
	return verifyP3ACCMediaFilesCachedWithVerifier(
		state, mediaRoot, database, verifyP3ACCMediaFilesWithRootGuard,
	)
}

func verifyP3ACCMediaFilesCachedWithVerifier(
	state *p3ACCAcceptanceState,
	mediaRoot string,
	database p3ACCInternalDatabaseSnapshot,
	verifier func(
		context.Context, string, p3ACCInternalDatabaseSnapshot,
		*p3ACCMediaRootGuard, p3ACCMediaRootIdentity,
	) p3ACCMediaManifestSummary,
) p3ACCMediaManifestSummary {
	if state == nil || verifier == nil {
		return p3ACCMediaManifestSummary{FileFailureCount: 1}
	}
	rootGuard, rootIdentity, rootValid := openP3ACCMediaRootGuard(mediaRoot)
	if !rootValid {
		return p3ACCMediaManifestSummary{FileFailureCount: 1}
	}
	defer closeP3ACCMediaRootGuard(rootGuard)
	state.mediaVerificationMu.Lock()
	defer state.mediaVerificationMu.Unlock()
	if state.mediaVerification.AllFilesMatch &&
		state.mediaVerificationRevision == database.Media.ManifestRevision &&
		state.mediaVerificationUpdatedAt == database.Media.UpdatedAt &&
		state.mediaVerificationSessionID == database.SessionID &&
		sameP3ACCMediaRootIdentity(state.mediaVerificationRoot, rootIdentity) {
		rootStable := validateP3ACCMediaRootGuard(rootGuard, rootIdentity)
		rootClosed := closeP3ACCMediaRootGuard(rootGuard)
		if rootStable && rootClosed {
			return state.mediaVerification
		}
		return failedP3ACCMediaVerification(state.mediaVerification)
	}
	verification := verifier(state.ctx, mediaRoot, database, rootGuard, rootIdentity)
	state.mu.Lock()
	crashAttemptID := state.crashBaselineAttemptID
	networkAttemptID := state.networkBaselineAttemptID
	state.mu.Unlock()
	verification.SequenceContinuous, verification.AttemptReferencesValid,
		verification.FaultPhaseSegmentsProven = verifyP3ACCMediaLineage(
		database, crashAttemptID, networkAttemptID,
	)
	rootStable := validateP3ACCMediaRootGuard(rootGuard, rootIdentity)
	rootClosed := closeP3ACCMediaRootGuard(rootGuard)
	if !rootStable || !rootClosed {
		verification = failedP3ACCMediaVerification(verification)
	}
	if verification.AllFilesMatch && verification.SequenceContinuous &&
		verification.AttemptReferencesValid && verification.FaultPhaseSegmentsProven {
		state.mediaVerificationRevision = database.Media.ManifestRevision
		state.mediaVerificationUpdatedAt = database.Media.UpdatedAt
		state.mediaVerificationSessionID = database.SessionID
		state.mediaVerificationRoot = rootIdentity
		state.mediaVerification = verification
	}
	return verification
}

func failedP3ACCMediaVerification(result p3ACCMediaManifestSummary) p3ACCMediaManifestSummary {
	result.AllFilesMatch = false
	if result.FileFailureCount == 0 {
		result.FileFailureCount = 1
	}
	return result
}

func verifyP3ACCMediaLineage(
	database p3ACCInternalDatabaseSnapshot,
	crashBaselineAttemptID, networkBaselineAttemptID string,
) (bool, bool, bool) {
	attempts := make(map[string]capture.MediaAttempt, len(database.Attempts))
	for index, attempt := range database.Attempts {
		if !validP3ACCUUID(attempt.ID) || attempt.Ordinal != index+1 {
			return false, false, false
		}
		if _, duplicate := attempts[attempt.ID]; duplicate {
			return false, false, false
		}
		attempts[attempt.ID] = attempt
	}
	sequenceContinuous := len(database.Segments) > 0
	referencesValid := len(attempts) > 0
	segmentsByID := make(map[string]capture.MediaSegment, len(database.Segments))
	attemptSequences := make(map[string]map[int]struct{}, len(attempts))
	durableByAttempt := make(map[string]bool, len(attempts))
	for index, segment := range database.Segments {
		if segment.Sequence != index+1 || !validP3ACCUUID(segment.ID) {
			sequenceContinuous = false
		}
		if _, duplicate := segmentsByID[segment.ID]; duplicate {
			referencesValid = false
		}
		segmentsByID[segment.ID] = segment
		attempt, exists := attempts[segment.AttemptID]
		if !exists || !attempt.Committed || segment.AttemptSequence < 1 {
			referencesValid = false
			continue
		}
		sequences := attemptSequences[segment.AttemptID]
		if sequences == nil {
			sequences = make(map[int]struct{})
			attemptSequences[segment.AttemptID] = sequences
		}
		if _, duplicate := sequences[segment.AttemptSequence]; duplicate {
			referencesValid = false
		}
		sequences[segment.AttemptSequence] = struct{}{}
		if segment.Status == capture.MediaSegmentComplete || segment.Status == capture.MediaSegmentRecovered {
			durableByAttempt[segment.AttemptID] = true
		}
	}
	for _, sequences := range attemptSequences {
		for sequence := 1; sequence <= len(sequences); sequence++ {
			if _, exists := sequences[sequence]; !exists {
				referencesValid = false
			}
		}
	}
	artifactKindsBySegment := make(map[string]map[capture.MediaArtifactKind]struct{}, len(segmentsByID))
	for _, artifact := range database.Artifacts {
		if !validP3ACCUUID(artifact.ID) {
			referencesValid = false
		}
		segment, exists := segmentsByID[artifact.MediaSegmentID]
		if !exists || segment.Status != capture.MediaSegmentComplete &&
			segment.Status != capture.MediaSegmentRecovered {
			referencesValid = false
			continue
		}
		if artifact.Kind != capture.MediaArtifactASRWAV &&
			artifact.Kind != capture.MediaArtifactPlaybackMP4 {
			referencesValid = false
			continue
		}
		kinds := artifactKindsBySegment[artifact.MediaSegmentID]
		if kinds == nil {
			kinds = make(map[capture.MediaArtifactKind]struct{}, 2)
			artifactKindsBySegment[artifact.MediaSegmentID] = kinds
		}
		if _, duplicate := kinds[artifact.Kind]; duplicate {
			referencesValid = false
		}
		kinds[artifact.Kind] = struct{}{}
	}
	for segmentID, segment := range segmentsByID {
		kinds := artifactKindsBySegment[segmentID]
		durable := segment.Status == capture.MediaSegmentComplete ||
			segment.Status == capture.MediaSegmentRecovered
		_, hasASR := kinds[capture.MediaArtifactASRWAV]
		_, hasPlayback := kinds[capture.MediaArtifactPlaybackMP4]
		if durable && (len(kinds) != 2 || !hasASR || !hasPlayback) ||
			!durable && len(kinds) != 0 {
			referencesValid = false
		}
	}
	postNetworkAttemptID := database.CurrentAttemptID
	phaseSegmentsProven := validP3ACCUUID(crashBaselineAttemptID) &&
		validP3ACCUUID(networkBaselineAttemptID) && validP3ACCUUID(postNetworkAttemptID) &&
		crashBaselineAttemptID != networkBaselineAttemptID &&
		networkBaselineAttemptID != postNetworkAttemptID &&
		crashBaselineAttemptID != postNetworkAttemptID &&
		durableByAttempt[crashBaselineAttemptID] && durableByAttempt[networkBaselineAttemptID] &&
		durableByAttempt[postNetworkAttemptID]
	return sequenceContinuous, referencesValid, phaseSegmentsProven
}

func verifyP3ACCMediaFiles(
	ctx context.Context,
	mediaRoot string,
	database p3ACCInternalDatabaseSnapshot,
) p3ACCMediaManifestSummary {
	rootGuard, rootIdentity, valid := openP3ACCMediaRootGuard(mediaRoot)
	if !valid {
		return p3ACCMediaManifestSummary{FileFailureCount: 1}
	}
	defer closeP3ACCMediaRootGuard(rootGuard)
	result := verifyP3ACCMediaFilesWithRootGuard(
		ctx, mediaRoot, database, rootGuard, rootIdentity,
	)
	rootStable := validateP3ACCMediaRootGuard(rootGuard, rootIdentity)
	rootClosed := closeP3ACCMediaRootGuard(rootGuard)
	if !rootStable || !rootClosed {
		return failedP3ACCMediaVerification(result)
	}
	return result
}

func verifyP3ACCMediaFilesWithRootGuard(
	ctx context.Context,
	mediaRoot string,
	database p3ACCInternalDatabaseSnapshot,
	rootGuard *p3ACCMediaRootGuard,
	rootIdentity p3ACCMediaRootIdentity,
) p3ACCMediaManifestSummary {
	if !validateP3ACCMediaRootGuard(rootGuard, rootIdentity) {
		return p3ACCMediaManifestSummary{FileFailureCount: 1}
	}
	result := p3ACCMediaManifestSummary{}
	type observedMediaEvidence struct {
		size   int64
		digest string
		valid  bool
	}
	pathStates := make(map[string]p3ACCMediaPathState)
	observedFiles := make(map[string]observedMediaEvidence)
	mediaPathState := func(relativePath string) p3ACCMediaPathState {
		if state, exists := pathStates[relativePath]; exists {
			return state
		}
		filename, safe := p3ACCMediaFilename(mediaRoot, relativePath)
		state := p3ACCMediaPathUnsafe
		if safe && validateP3ACCMediaRootGuard(rootGuard, rootIdentity) {
			state = inspectP3ACCAcceptanceMediaPath(filename)
		}
		pathStates[relativePath] = state
		return state
	}
	validRecordedEvidence := func(size int64, digest string) bool {
		if size <= 0 || len(digest) != sha256.Size*2 || digest != strings.ToLower(digest) {
			return false
		}
		_, err := hex.DecodeString(digest)
		return err == nil
	}
	observeMediaEvidence := func(relativePath string) observedMediaEvidence {
		if evidence, exists := observedFiles[relativePath]; exists {
			return evidence
		}
		if !validateP3ACCMediaRootGuard(rootGuard, rootIdentity) {
			return observedMediaEvidence{}
		}
		size, digest, valid := readP3ACCMediaFileEvidence(ctx, mediaRoot, relativePath)
		if !validateP3ACCMediaRootGuard(rootGuard, rootIdentity) {
			valid = false
		}
		evidence := observedMediaEvidence{size: size, digest: digest, valid: valid}
		observedFiles[relativePath] = evidence
		return evidence
	}
	exactMediaEvidence := func(relativePath string, size int64, digest string) bool {
		evidence := observeMediaEvidence(relativePath)
		return validRecordedEvidence(size, digest) && evidence.valid &&
			evidence.size == size && evidence.digest == digest
	}
	changedMediaEvidence := func(relativePath string, size int64, digest string) bool {
		evidence := observeMediaEvidence(relativePath)
		return validRecordedEvidence(size, digest) && evidence.valid &&
			(evidence.size != size || evidence.digest != digest)
	}
	checkDurable := func(relativePath string, size int64, digest string) {
		result.FileCheckCount++
		if !exactMediaEvidence(relativePath, size, digest) {
			result.FileFailureCount++
		}
	}
	checkIncomplete := func(segment, valid bool) {
		result.IncompleteEntryCount++
		if segment {
			result.IncompleteSegmentCount++
		}
		if !valid {
			result.FileFailureCount++
		}
	}
	segmentsByID := make(map[string]capture.MediaSegment, len(database.Segments))
	for _, segment := range database.Segments {
		segmentsByID[segment.ID] = segment
		switch segment.Status {
		case capture.MediaSegmentComplete, capture.MediaSegmentRecovered:
			checkDurable(segment.RelativePath, segment.SizeBytes, segment.SHA256)
		case capture.MediaSegmentMissing:
			checkIncomplete(true,
				segment.ErrorCode == "MEDIA_FINAL_MISSING" &&
					validRecordedEvidence(segment.SizeBytes, segment.SHA256) &&
					mediaPathState(segment.RelativePath) == p3ACCMediaPathMissing,
			)
		case capture.MediaSegmentCorrupt:
			valid := false
			switch segment.ErrorCode {
			case "MEDIA_FINAL_CHANGED":
				valid = changedMediaEvidence(
					segment.RelativePath, segment.SizeBytes, segment.SHA256,
				)
			case "MEDIA_TARGET_CONFLICT", "MEDIA_PROBE_TIMEOUT",
				"MEDIA_PROBE_DEPENDENCY", "MEDIA_PROBE_UNSUPPORTED",
				"MEDIA_PROBE_INPUT", "MEDIA_PROBE_UNREADABLE", "MEDIA_PROBE_FAILED":
				valid = exactMediaEvidence(
					segment.RelativePath, segment.SizeBytes, segment.SHA256,
				) || exactMediaEvidence(
					segment.SourceRelativePath, segment.SizeBytes, segment.SHA256,
				)
			}
			checkIncomplete(true, valid)
		default:
			checkIncomplete(true, false)
		}
	}
	sourceSegmentEvidence := func(artifact capture.MediaArtifact) bool {
		segment, exists := segmentsByID[artifact.MediaSegmentID]
		return exists &&
			(segment.Status == capture.MediaSegmentComplete || segment.Status == capture.MediaSegmentRecovered) &&
			artifact.SourceSHA256 != "" && artifact.SourceSHA256 == segment.SHA256 &&
			exactMediaEvidence(segment.RelativePath, segment.SizeBytes, segment.SHA256)
	}
	for _, artifact := range database.Artifacts {
		switch artifact.Status {
		case capture.MediaArtifactComplete:
			result.FileCheckCount++
			if !exactMediaEvidence(artifact.RelativePath, artifact.SizeBytes, artifact.SHA256) ||
				!sourceSegmentEvidence(artifact) {
				result.FileFailureCount++
			}
		case capture.MediaArtifactMissing:
			checkIncomplete(false,
				artifact.ErrorCode == "MEDIA_ARTIFACT_MISSING" &&
					validRecordedEvidence(artifact.SizeBytes, artifact.SHA256) &&
					sourceSegmentEvidence(artifact) &&
					mediaPathState(artifact.RelativePath) == p3ACCMediaPathMissing,
			)
		case capture.MediaArtifactNotApplicable:
			segment := segmentsByID[artifact.MediaSegmentID]
			statusMatchesSource := artifact.Kind == capture.MediaArtifactASRWAV &&
				artifact.ErrorCode == "MEDIA_AUDIO_STREAM_MISSING" && segment.AudioCodec == "" ||
				artifact.Kind == capture.MediaArtifactPlaybackMP4 &&
					artifact.ErrorCode == "MEDIA_VIDEO_STREAM_MISSING" && segment.VideoCodec == ""
			checkIncomplete(false,
				artifact.SizeBytes == 0 && artifact.SHA256 == "" && statusMatchesSource &&
					sourceSegmentEvidence(artifact) &&
					mediaPathState(artifact.RelativePath) == p3ACCMediaPathMissing,
			)
		case capture.MediaArtifactFailed:
			valid := false
			if sourceSegmentEvidence(artifact) {
				switch artifact.ErrorCode {
				case "MEDIA_ARTIFACT_CHANGED":
					valid = changedMediaEvidence(
						artifact.RelativePath, artifact.SizeBytes, artifact.SHA256,
					)
				case "MEDIA_ARTIFACT_CONFLICT":
					valid = mediaPathState(artifact.RelativePath) == p3ACCMediaPathRegular
				case "MEDIA_ARTIFACT_HASH_FAILED":
					valid = exactMediaEvidence(
						artifact.RelativePath, artifact.SizeBytes, artifact.SHA256,
					)
				case "MEDIA_ARTIFACT_FAILED":
					valid = mediaPathState(artifact.RelativePath) == p3ACCMediaPathMissing ||
						exactMediaEvidence(
							artifact.RelativePath, artifact.SizeBytes, artifact.SHA256,
						)
				}
			}
			checkIncomplete(false, valid)
		default:
			checkIncomplete(false, false)
		}
	}
	result.AllFilesMatch = result.FileCheckCount > 0 && result.FileFailureCount == 0
	return result
}

func verifyP3ACCMediaFile(
	ctx context.Context,
	mediaRoot, relativePath string,
	expectedSize int64,
	expectedDigest string,
) bool {
	if expectedSize <= 0 || len(expectedDigest) != sha256.Size*2 ||
		expectedDigest != strings.ToLower(expectedDigest) {
		return false
	}
	if _, err := hex.DecodeString(expectedDigest); err != nil {
		return false
	}
	size, digest, valid := readP3ACCMediaFileEvidence(ctx, mediaRoot, relativePath)
	return valid && size == expectedSize && digest == expectedDigest
}

func readP3ACCMediaFileEvidence(
	ctx context.Context,
	mediaRoot, relativePath string,
) (int64, string, bool) {
	return readP3ACCMediaFileEvidenceWithHook(ctx, mediaRoot, relativePath, nil)
}

func readP3ACCMediaFileEvidenceWithHook(
	ctx context.Context,
	mediaRoot, relativePath string,
	afterRead func(),
) (int64, string, bool) {
	return readP3ACCMediaFileEvidenceWithHooks(ctx, mediaRoot, relativePath, nil, afterRead)
}

func readP3ACCMediaFileEvidenceWithHooks(
	ctx context.Context,
	mediaRoot, relativePath string,
	beforeOpen, afterRead func(),
) (int64, string, bool) {
	rootGuard, rootIdentity, rootValid := openP3ACCMediaRootGuard(mediaRoot)
	if !rootValid {
		return 0, "", false
	}
	defer closeP3ACCMediaRootGuard(rootGuard)
	filename, safe := p3ACCMediaFilename(mediaRoot, relativePath)
	if !safe {
		return 0, "", false
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	file, identity, before, err := openP3ACCMediaEvidenceFile(mediaRoot, filename)
	if err != nil {
		return 0, "", false
	}
	fileClosed := false
	defer func() {
		if !fileClosed {
			_ = file.Close()
		}
	}()
	if before == nil || !before.Mode().IsRegular() || before.Size() <= 0 {
		return 0, "", false
	}
	digest := sha256.New()
	buffer := make([]byte, 128<<10)
	var size int64
	for {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return 0, "", false
			default:
			}
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			size += int64(count)
			_, _ = digest.Write(buffer[:count])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return 0, "", false
		}
	}
	afterIdentity, after, valid := snapshotP3ACCMediaEvidenceHandle(file, mediaRoot, filename)
	if !valid || !sameP3ACCMediaFileIdentity(identity, afterIdentity) ||
		!os.SameFile(before, after) || before.Size() != after.Size() ||
		!before.ModTime().Equal(after.ModTime()) || size != before.Size() {
		return 0, "", false
	}
	if afterRead != nil {
		afterRead()
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return 0, "", false
		default:
		}
	}
	reopened, reopenedIdentity, reopenedInfo, err := openP3ACCMediaEvidenceFile(mediaRoot, filename)
	if err != nil {
		return 0, "", false
	}
	reopenedClosed := false
	defer func() {
		if !reopenedClosed {
			_ = reopened.Close()
		}
	}()
	if reopenedInfo == nil || !sameP3ACCMediaFileIdentity(identity, reopenedIdentity) ||
		!os.SameFile(before, reopenedInfo) || !os.SameFile(after, reopenedInfo) ||
		before.Size() != reopenedInfo.Size() ||
		!before.ModTime().Equal(reopenedInfo.ModTime()) {
		return 0, "", false
	}
	if err := reopened.Close(); err != nil {
		return 0, "", false
	}
	reopenedClosed = true
	if err := file.Close(); err != nil {
		return 0, "", false
	}
	fileClosed = true
	rootStable := validateP3ACCMediaRootGuard(rootGuard, rootIdentity)
	rootClosed := closeP3ACCMediaRootGuard(rootGuard)
	if !rootStable || !rootClosed {
		return 0, "", false
	}
	return size, hex.EncodeToString(digest.Sum(nil)), true
}

func p3ACCMediaFilename(mediaRoot, relativePath string) (string, bool) {
	if relativePath == "" || strings.TrimSpace(relativePath) != relativePath ||
		strings.ContainsAny(relativePath, `\\:%?*<>|`) {
		return "", false
	}
	clean := path.Clean(relativePath)
	if clean != relativePath || clean == "." || clean == ".." || path.IsAbs(clean) ||
		strings.HasPrefix(clean, "../") {
		return "", false
	}
	root, err := filepath.Abs(filepath.Clean(mediaRoot))
	if err != nil {
		return "", false
	}
	candidate, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(clean)))
	if err != nil || !p3ACCAcceptancePathWithin(root, candidate, false) {
		return "", false
	}
	return candidate, true
}

func encodeP3ACCMediaManifestWire(manifest p3ACCMediaManifestWire) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(manifest); err != nil || buffer.Len() == 0 ||
		buffer.Len() > p3ACCAcceptanceMaximumManifestBytes {
		return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	return buffer.Bytes(), nil
}

func summarizeP3ACCMediaManifest(payload []byte, expectedSessionID string) (p3ACCMediaManifestSummary, error) {
	if len(payload) == 0 || len(payload) > p3ACCAcceptanceMaximumManifestBytes || !validP3ACCUUID(expectedSessionID) {
		return p3ACCMediaManifestSummary{}, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var manifest p3ACCMediaManifestWire
	if err := decoder.Decode(&manifest); err != nil || ensureP3ACCJSONEOF(decoder) != nil ||
		manifest.SchemaVersion != 1 || manifest.Session.ID != expectedSessionID ||
		!validP3ACCMediaState(manifest.Session.State) || manifest.Session.Revision < 0 {
		return p3ACCMediaManifestSummary{}, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	result := p3ACCMediaManifestSummary{
		Exists: true, State: manifest.Session.State, Revision: manifest.Session.Revision,
		AttemptCount: len(manifest.Attempts), SegmentCount: len(manifest.Segments),
		ArtifactCount: len(manifest.Artifacts),
	}
	for _, attempt := range manifest.Attempts {
		if attempt.Committed {
			result.CommittedAttemptCount++
		}
		if attempt.Clean {
			result.CleanAttemptCount++
		}
	}
	for _, segment := range manifest.Segments {
		if segment.Status == capture.MediaSegmentComplete || segment.Status == capture.MediaSegmentRecovered {
			result.CompleteSegmentCount++
		}
	}
	for _, artifact := range manifest.Artifacts {
		if artifact.Status == capture.MediaArtifactComplete {
			result.CompleteArtifactCount++
		}
	}
	return result, nil
}

func p3ACCSessionDirectory(dataRoot, dataPath string) (string, bool) {
	if dataPath == "" || strings.TrimSpace(dataPath) != dataPath || strings.ContainsAny(dataPath, `\:%`) {
		return "", false
	}
	clean := path.Clean(dataPath)
	if clean != dataPath || clean == "." || clean == ".." || path.IsAbs(clean) || strings.HasPrefix(clean, "../") {
		return "", false
	}
	root, err := filepath.Abs(filepath.Clean(dataRoot))
	if err != nil {
		return "", false
	}
	candidate, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(clean)))
	if err != nil || !p3ACCAcceptancePathWithin(root, candidate, false) ||
		validateP3ACCAcceptanceDataPath(root, candidate) != nil {
		return "", false
	}
	return candidate, true
}

func readP3ACCBoundedFile(mediaRoot, filename string, maximum int64) ([]byte, error) {
	return readP3ACCBoundedFileWithHooks(mediaRoot, filename, maximum, nil, nil)
}

func readP3ACCBoundedFileWithHooks(
	mediaRoot, filename string,
	maximum int64,
	beforeOpen, afterRead func(),
) ([]byte, error) {
	if maximum < 1 {
		return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	rootGuard, rootIdentity, rootValid := openP3ACCMediaRootGuard(mediaRoot)
	if !rootValid {
		return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	defer closeP3ACCMediaRootGuard(rootGuard)
	if beforeOpen != nil {
		beforeOpen()
	}
	file, identity, before, err := openP3ACCMediaEvidenceFile(mediaRoot, filename)
	if err != nil {
		return nil, err
	}
	fileClosed := false
	defer func() {
		if !fileClosed {
			_ = file.Close()
		}
	}()
	if before == nil || !before.Mode().IsRegular() ||
		before.Size() <= 0 || before.Size() > maximum {
		return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	payload, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(payload)) != before.Size() || int64(len(payload)) > maximum {
		return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	afterIdentity, after, valid := snapshotP3ACCMediaEvidenceHandle(file, mediaRoot, filename)
	if !valid || !sameP3ACCMediaFileIdentity(identity, afterIdentity) ||
		!os.SameFile(before, after) || before.Size() != after.Size() ||
		!before.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	if afterRead != nil {
		afterRead()
	}
	reopened, reopenedIdentity, reopenedInfo, err := openP3ACCMediaEvidenceFile(mediaRoot, filename)
	if err != nil {
		return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	reopenedClosed := false
	defer func() {
		if !reopenedClosed {
			_ = reopened.Close()
		}
	}()
	if reopenedInfo == nil || !sameP3ACCMediaFileIdentity(identity, reopenedIdentity) ||
		!os.SameFile(before, reopenedInfo) || !os.SameFile(after, reopenedInfo) ||
		before.Size() != reopenedInfo.Size() ||
		!before.ModTime().Equal(reopenedInfo.ModTime()) {
		return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	if err := reopened.Close(); err != nil {
		return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	reopenedClosed = true
	if err := file.Close(); err != nil {
		return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	fileClosed = true
	rootStable := validateP3ACCMediaRootGuard(rootGuard, rootIdentity)
	rootClosed := closeP3ACCMediaRootGuard(rootGuard)
	if !rootStable || !rootClosed {
		return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	return payload, nil
}

func (a *DesktopApp) writeP3ACCAcceptanceSnapshot() error {
	state := p3ACCAcceptanceStateFor(a)
	if state == nil {
		return errors.New("P3ACC_NOT_READY")
	}
	snapshot, err := a.buildP3ACCAcceptanceSnapshot()
	if err != nil {
		return errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	data, err := json.Marshal(snapshot)
	if err != nil || len(data) == 0 || len(data) > p3ACCAcceptanceMaximumSnapshotBytes {
		return errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	state.writeMu.Lock()
	defer state.writeMu.Unlock()
	return writeP3ACCAtomic(state.paths.ResultPath, data)
}

func writeP3ACCAcceptanceFailure(paths p3ACCAcceptancePaths, code string) {
	snapshot := p3ACCAcceptanceSnapshot{
		Schema: p3ACCAcceptanceSchema, Stage: p3ACCStageError,
		CapturedAt: time.Now().UTC().UnixMilli(),
		Runtime:    p3ACCRuntimeSummary{State: room.RuntimeError, ErrorCode: safeP3ACCErrorCode(code)},
		Resources:  summarizeP3ACCResources(nil),
	}
	if validateP3ACCAcceptanceSnapshot(snapshot) != nil {
		return
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	_ = writeP3ACCAtomic(paths.ResultPath, payload)
}

func writeP3ACCAtomic(filename string, payload []byte) error {
	if len(payload) == 0 || len(payload) > p3ACCAcceptanceMaximumSnapshotBytes {
		return errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	directory := filepath.Dir(filename)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	temporary, err := os.CreateTemp(directory, ".p3-acc-*.tmp")
	if err != nil {
		return errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	if err := replaceP3ACCAcceptanceFile(temporaryName, filename); err != nil {
		return errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	return nil
}

func readP3ACCResourceSample(app *DesktopApp, dataRoot string) p3ACCResourceSample {
	var memory goruntime.MemStats
	goruntime.ReadMemStats(&memory)
	processTree := readP3ACCProcessTreeResourceSample()
	walBytes, walPresent, walComplete := readP3ACCDatabaseWALSample(dataRoot)
	dataRootPhysicalBytes, dataRootComplete := readP3ACCDataRootPhysicalSample(dataRoot)
	queueStats := eventstore.AggregateQueueStats{}
	if app != nil && app.application != nil {
		queueStats = app.application.EventStoreManager().AggregateQueueStats()
	}
	queueComplete := queueStats.Complete && queueStats.QueueCount > 0 &&
		queueStats.ClosedQueueCount == 0
	return p3ACCResourceSample{
		CapturedAt:             time.Now(),
		Complete:               processTree.Complete && walComplete && dataRootComplete && queueComplete,
		ProcessCPU100NS:        processTree.CPU100NS,
		ProcessCount:           processTree.ProcessCount,
		WorkingSetBytes:        processTree.WorkingSetBytes,
		PrivateBytes:           processTree.PrivateBytes,
		ThreadCount:            processTree.ThreadCount,
		HandleCount:            processTree.HandleCount,
		ProcessReadBytes:       processTree.ReadBytes,
		ProcessWriteBytes:      processTree.WriteBytes,
		Goroutines:             int64(goruntime.NumGoroutine()),
		HeapAllocBytes:         boundedP3ACCUnsigned(memory.HeapAlloc),
		HeapInUseBytes:         boundedP3ACCUnsigned(memory.HeapInuse),
		SysBytes:               boundedP3ACCUnsigned(memory.Sys),
		DatabaseWALBytes:       walBytes,
		DatabaseWALPresent:     walPresent,
		DataRootPhysicalBytes:  dataRootPhysicalBytes,
		EventQueueCount:        queueStats.QueueCount,
		EventQueueItems:        queueStats.Items,
		EventQueueBytes:        queueStats.Bytes,
		EventQueueItemCapacity: queueStats.ItemCapacity,
		EventQueueByteCapacity: queueStats.ByteCapacity,
	}
}

func readP3ACCDatabaseWALSample(dataRoot string) (int64, bool, bool) {
	cleanRoot, err := cleanP3ACCAcceptanceAbsolutePath(dataRoot)
	if err != nil {
		return 0, false, false
	}
	databasePath := filepath.Join(cleanRoot, "app.db")
	walPath := filepath.Join(cleanRoot, "app.db-wal")
	if !p3ACCAcceptancePathWithin(cleanRoot, databasePath, false) ||
		!p3ACCAcceptancePathWithin(cleanRoot, walPath, false) {
		return 0, false, false
	}
	_, databasePresent, databaseComplete := readP3ACCRegularFileSize(databasePath)
	if !databaseComplete || !databasePresent {
		return 0, false, false
	}
	walBytes, walPresent, walComplete := readP3ACCRegularFileSize(walPath)
	return walBytes, walPresent, walComplete
}

func readP3ACCRegularFileSize(filename string) (int64, bool, bool) {
	file, err := openP3ACCAcceptanceRegularFile(filename)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, true
	}
	if err != nil {
		return 0, false, false
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil || closeErr != nil || !info.Mode().IsRegular() || info.Size() < 0 {
		return 0, false, false
	}
	return info.Size(), true, true
}

func (s *p3ACCAcceptanceState) appendResourceSampleLocked(sample p3ACCResourceSample) {
	if !sample.Complete {
		s.latestResourceAttemptComplete = false
		return
	}
	if len(s.resourceSamples) > 0 {
		gap := sample.CapturedAt.Sub(s.resourceSamples[len(s.resourceSamples)-1].CapturedAt)
		if gap <= 0 || gap > p3ACCAcceptanceMaximumResourceGap {
			s.resourceSamples = nil
		}
	}
	s.latestResourceAttemptComplete = true
	if len(s.resourceSamples) >= p3ACCAcceptanceMaximumResourceSamples {
		copy(s.resourceSamples, s.resourceSamples[1:])
		s.resourceSamples[len(s.resourceSamples)-1] = sample
		return
	}
	s.resourceSamples = append(s.resourceSamples, sample)
}

func (s *p3ACCAcceptanceState) currentP3ACCResourceSummaryLocked() p3ACCResourceSummary {
	result := summarizeP3ACCResources(s.resourceSamples)
	if !s.latestResourceAttemptComplete {
		result.SampleComplete = false
		result.StableWindowProven = false
		result.CPUWithinTarget = false
	}
	return result
}

func (s *p3ACCAcceptanceState) p3ACCResourceSummaryForSnapshotLocked() p3ACCResourceSummary {
	result := s.currentP3ACCResourceSummaryLocked()
	faultPhaseStarted := s.crashInFlight || s.crashInjected ||
		s.networkArmInFlight || s.networkFaultArmed || s.offlineConfirmRevision > 0
	if !faultPhaseStarted && !s.resourceSamplingFailed &&
		result.StableWindowProven && result.CPUWithinTarget {
		s.bestResourceSummary = result
	}
	if faultPhaseStarted && !result.StableWindowProven && s.bestResourceSummary.StableWindowProven {
		result = s.bestResourceSummary
		result.Frozen = true
	}
	if s.resourceSamplingFailed {
		result.SampleComplete = false
		result.StableWindowProven = false
		result.CPUWithinTarget = false
	}
	return result
}

func summarizeP3ACCResources(samples []p3ACCResourceSample) p3ACCResourceSummary {
	result := p3ACCResourceSummary{SampleCount: len(samples), SampleComplete: len(samples) > 0, CPUTrend: "INSUFFICIENT"}
	for index, sample := range samples {
		result.SampleComplete = result.SampleComplete && sample.Complete
		if index > 0 {
			gap := sample.CapturedAt.Sub(samples[index-1].CapturedAt)
			if gap <= 0 || gap > p3ACCAcceptanceMaximumResourceGap {
				result.SampleComplete = false
			}
		}
		updateP3ACCResourceMetric(&result.ProcessCount, sample.ProcessCount, index == 0)
		updateP3ACCResourceMetric(&result.WorkingSet, sample.WorkingSetBytes, index == 0)
		updateP3ACCResourceMetric(&result.PrivateBytes, sample.PrivateBytes, index == 0)
		updateP3ACCResourceMetric(&result.Threads, sample.ThreadCount, index == 0)
		updateP3ACCResourceMetric(&result.Handles, sample.HandleCount, index == 0)
		updateP3ACCResourceMetric(&result.Goroutines, sample.Goroutines, index == 0)
		updateP3ACCResourceMetric(&result.HeapAlloc, sample.HeapAllocBytes, index == 0)
		updateP3ACCResourceMetric(&result.HeapInUse, sample.HeapInUseBytes, index == 0)
		updateP3ACCResourceMetric(&result.System, sample.SysBytes, index == 0)
		updateP3ACCResourceMetric(&result.DatabaseWALBytes, sample.DatabaseWALBytes, index == 0)
		updateP3ACCResourceMetric(&result.ProcessReadBytes, sample.ProcessReadBytes, index == 0)
		updateP3ACCResourceMetric(&result.ProcessWriteBytes, sample.ProcessWriteBytes, index == 0)
		updateP3ACCResourceMetric(&result.DataRootPhysicalBytes, sample.DataRootPhysicalBytes, index == 0)
		updateP3ACCResourceMetric(&result.EventQueueCount, sample.EventQueueCount, index == 0)
		updateP3ACCResourceMetric(&result.EventQueueItems, sample.EventQueueItems, index == 0)
		updateP3ACCResourceMetric(&result.EventQueueBytes, sample.EventQueueBytes, index == 0)
		updateP3ACCResourceMetric(&result.EventQueueItemCapacity, sample.EventQueueItemCapacity, index == 0)
		updateP3ACCResourceMetric(&result.EventQueueByteCapacity, sample.EventQueueByteCapacity, index == 0)
		if sample.DatabaseWALPresent {
			result.DatabaseWALObserved = true
		}
		result.EventQueueObserved = result.EventQueueObserved || sample.EventQueueCount > 0
	}
	if len(samples) < 2 {
		for _, metric := range p3ACCResourceMetrics(&result) {
			metric.LatterHalfTrend = "INSUFFICIENT"
		}
		return result
	}
	middle := (len(samples) - 1) / 2
	if middle >= len(samples) {
		middle = len(samples) - 1
	}
	first, latter, latest := samples[0], samples[middle], samples[len(samples)-1]
	result.WindowDurationMS = latest.CapturedAt.Sub(first.CapturedAt).Milliseconds()
	if result.WindowDurationMS < 0 {
		result.WindowDurationMS = 0
		result.SampleComplete = false
	}
	setP3ACCLatterMetric(&result.ProcessCount, latter.ProcessCount, latest.ProcessCount, 0)
	setP3ACCLatterMetric(&result.WorkingSet, latter.WorkingSetBytes, latest.WorkingSetBytes, 1<<20)
	setP3ACCLatterMetric(&result.PrivateBytes, latter.PrivateBytes, latest.PrivateBytes, 1<<20)
	setP3ACCLatterMetric(&result.Threads, latter.ThreadCount, latest.ThreadCount, 2)
	setP3ACCLatterMetric(&result.Handles, latter.HandleCount, latest.HandleCount, 2)
	setP3ACCLatterMetric(&result.Goroutines, latter.Goroutines, latest.Goroutines, 2)
	setP3ACCLatterMetric(&result.HeapAlloc, latter.HeapAllocBytes, latest.HeapAllocBytes, 1<<20)
	setP3ACCLatterMetric(&result.HeapInUse, latter.HeapInUseBytes, latest.HeapInUseBytes, 1<<20)
	setP3ACCLatterMetric(&result.System, latter.SysBytes, latest.SysBytes, 1<<20)
	setP3ACCLatterMetric(&result.DatabaseWALBytes, latter.DatabaseWALBytes, latest.DatabaseWALBytes, 1<<20)
	setP3ACCLatterMetric(&result.ProcessReadBytes, latter.ProcessReadBytes, latest.ProcessReadBytes, 0)
	setP3ACCLatterMetric(&result.ProcessWriteBytes, latter.ProcessWriteBytes, latest.ProcessWriteBytes, 0)
	setP3ACCLatterMetric(&result.DataRootPhysicalBytes, latter.DataRootPhysicalBytes, latest.DataRootPhysicalBytes, 0)
	setP3ACCLatterMetric(&result.EventQueueCount, latter.EventQueueCount, latest.EventQueueCount, 0)
	setP3ACCLatterMetric(&result.EventQueueItems, latter.EventQueueItems, latest.EventQueueItems, 8)
	setP3ACCLatterMetric(&result.EventQueueBytes, latter.EventQueueBytes, latest.EventQueueBytes, 64<<10)
	setP3ACCLatterMetric(&result.EventQueueItemCapacity, latter.EventQueueItemCapacity, latest.EventQueueItemCapacity, 0)
	setP3ACCLatterMetric(&result.EventQueueByteCapacity, latter.EventQueueByteCapacity, latest.EventQueueByteCapacity, 0)
	result.AverageCPUPercent = p3ACCCPUPercent(first, latest)
	result.LatterHalfAverageCPUPercent = p3ACCCPUPercent(latter, latest)
	result.AverageProcessReadBytesPerSecond = p3ACCBytesPerSecond(first, latest, func(value p3ACCResourceSample) int64 { return value.ProcessReadBytes })
	result.AverageProcessWriteBytesPerSecond = p3ACCBytesPerSecond(first, latest, func(value p3ACCResourceSample) int64 { return value.ProcessWriteBytes })
	result.LatterHalfProcessReadBytesPerSecond = p3ACCBytesPerSecond(latter, latest, func(value p3ACCResourceSample) int64 { return value.ProcessReadBytes })
	result.LatterHalfProcessWriteBytesPerSecond = p3ACCBytesPerSecond(latter, latest, func(value p3ACCResourceSample) int64 { return value.ProcessWriteBytes })
	result.AverageDiskWriteBytesPerSecond = p3ACCBytesPerSecond(first, latest, func(value p3ACCResourceSample) int64 { return value.DataRootPhysicalBytes })
	result.LatterHalfDiskWriteBytesPerSecond = p3ACCBytesPerSecond(latter, latest, func(value p3ACCResourceSample) int64 { return value.DataRootPhysicalBytes })
	result.DiskIOObserved = result.DataRootPhysicalBytes.Delta > 0 && result.AverageDiskWriteBytesPerSecond > 0
	result.StableWindowProven = result.SampleComplete &&
		result.WindowDurationMS >= p3ACCAcceptanceStableWindow.Milliseconds() &&
		result.SampleCount >= p3ACCAcceptanceMinimumStableSamples &&
		result.DatabaseWALObserved && result.DiskIOObserved && result.EventQueueObserved &&
		validP3ACCEventQueueCapacity(result)
	result.CPUWithinTarget = result.StableWindowProven && result.AverageCPUPercent < 10
	firstHalf := p3ACCCPUPercent(first, latter)
	result.CPUTrend = p3ACCFloatTrend(result.LatterHalfAverageCPUPercent-firstHalf, 0.5)
	return result
}

func p3ACCResourceMetrics(summary *p3ACCResourceSummary) []*p3ACCResourceMetric {
	return []*p3ACCResourceMetric{
		&summary.ProcessCount, &summary.WorkingSet, &summary.PrivateBytes, &summary.Threads,
		&summary.Handles, &summary.Goroutines,
		&summary.HeapAlloc, &summary.HeapInUse, &summary.System,
		&summary.DatabaseWALBytes, &summary.ProcessReadBytes, &summary.ProcessWriteBytes, &summary.DataRootPhysicalBytes,
		&summary.EventQueueCount, &summary.EventQueueItems, &summary.EventQueueBytes,
		&summary.EventQueueItemCapacity, &summary.EventQueueByteCapacity,
	}
}

func updateP3ACCResourceMetric(metric *p3ACCResourceMetric, value int64, first bool) {
	if value < 0 {
		value = 0
	}
	if first {
		metric.Baseline = value
		metric.Peak = value
	}
	if value > metric.Peak {
		metric.Peak = value
	}
	metric.Latest = value
	metric.Delta = value - metric.Baseline
}

func setP3ACCLatterMetric(metric *p3ACCResourceMetric, baseline, latest, threshold int64) {
	metric.LatterHalfDelta = latest - baseline
	metric.LatterHalfTrend = p3ACCIntegerTrend(metric.LatterHalfDelta, threshold)
}

func p3ACCIntegerTrend(delta, threshold int64) string {
	if threshold < 0 {
		threshold = 0
	}
	if delta > threshold {
		return "RISING"
	}
	if delta < -threshold {
		return "FALLING"
	}
	return "STABLE"
}

func p3ACCFloatTrend(delta, threshold float64) string {
	if delta > threshold {
		return "RISING"
	}
	if delta < -threshold {
		return "FALLING"
	}
	return "STABLE"
}

func p3ACCCPUPercent(first, last p3ACCResourceSample) float64 {
	elapsed100NS := last.CapturedAt.Sub(first.CapturedAt).Nanoseconds() / 100
	cpuDelta := last.ProcessCPU100NS - first.ProcessCPU100NS
	processors := goruntime.NumCPU()
	if elapsed100NS <= 0 || cpuDelta < 0 || processors <= 0 {
		return 0
	}
	value := float64(cpuDelta) * 100 / float64(elapsed100NS) / float64(processors)
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0
	}
	return math.Round(value*100) / 100
}

func p3ACCBytesPerSecond(first, last p3ACCResourceSample, selectBytes func(p3ACCResourceSample) int64) float64 {
	if selectBytes == nil {
		return 0
	}
	elapsedSeconds := last.CapturedAt.Sub(first.CapturedAt).Seconds()
	firstBytes := selectBytes(first)
	lastBytes := selectBytes(last)
	if elapsedSeconds <= 0 || firstBytes < 0 || lastBytes < firstBytes {
		return 0
	}
	value := float64(lastBytes-firstBytes) / elapsedSeconds
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0
	}
	return value
}

func boundedP3ACCUnsigned(value uint64) int64 {
	const maximum = uint64(^uint64(0) >> 1)
	if value > maximum {
		return int64(maximum)
	}
	return int64(value)
}

func validateP3ACCAcceptanceSnapshot(snapshot p3ACCAcceptanceSnapshot) error {
	if snapshot.Schema != p3ACCAcceptanceSchema || !validP3ACCStage(snapshot.Stage) || snapshot.CapturedAt < 0 ||
		!validP3ACCRuntimeState(snapshot.Runtime.State) ||
		!validP3ACCRecordingStatus(snapshot.Runtime.RecordingStatus, true) ||
		!validP3ACCErrorCode(snapshot.Runtime.ErrorCode) || snapshot.Runtime.Revision < 0 ||
		snapshot.Runtime.AttemptCount < 0 || snapshot.Progress.SampleCount < 0 ||
		snapshot.Progress.LiveBatchCount < 0 || snapshot.Progress.LiveEventCount < 0 ||
		snapshot.Progress.ElapsedMS < 0 || snapshot.Progress.BytesWritten < 0 ||
		snapshot.Progress.SegmentCount < 0 || snapshot.Progress.RestartCount < 0 ||
		snapshot.Progress.SteadyRecordingMS < 0 || snapshot.Progress.SteadySampleCount < 0 ||
		!validP3ACCUI(snapshot.UI) || !validP3ACCCounts(snapshot) ||
		!validP3ACCResourceSummary(snapshot.Resources) {
		return errors.New("P3ACC_SNAPSHOT_INVALID")
	}
	if snapshot.SessionManifest.Exists &&
		(!validP3ACCSessionStatus(snapshot.SessionManifest.Status) ||
			!validP3ACCRecordingStatus(snapshot.SessionManifest.RecordingStatus, false)) {
		return errors.New("P3ACC_SNAPSHOT_INVALID")
	}
	if snapshot.SessionManifest.MatchesDatabase != snapshot.SessionManifest.CanonicalHashMatches ||
		snapshot.SessionManifest.MatchesDatabase && !snapshot.SessionManifest.Exists {
		return errors.New("P3ACC_SNAPSHOT_INVALID")
	}
	if snapshot.MediaManifest.Exists &&
		(!validP3ACCMediaState(snapshot.MediaManifest.State) || snapshot.MediaManifest.Revision < 0) {
		return errors.New("P3ACC_SNAPSHOT_INVALID")
	}
	if snapshot.MediaManifest.MatchesDatabase != snapshot.MediaManifest.CanonicalHashMatches ||
		snapshot.MediaManifest.MatchesDatabase && !snapshot.MediaManifest.Exists {
		return errors.New("P3ACC_SNAPSHOT_INVALID")
	}
	if snapshot.MediaManifest.IncompleteSegmentCount > snapshot.MediaManifest.IncompleteEntryCount ||
		snapshot.MediaManifest.FileFailureCount > snapshot.MediaManifest.FileCheckCount+
			snapshot.MediaManifest.IncompleteEntryCount ||
		snapshot.MediaManifest.FileCheckCount+snapshot.MediaManifest.IncompleteEntryCount >
			snapshot.MediaManifest.SegmentCount+snapshot.MediaManifest.ArtifactCount ||
		snapshot.MediaManifest.AllFilesMatch != (snapshot.MediaManifest.FileCheckCount > 0 &&
			snapshot.MediaManifest.FileFailureCount == 0) {
		return errors.New("P3ACC_SNAPSHOT_INVALID")
	}
	if snapshot.Gaps.LatestKind != "" && (!validP3ACCGapKind(snapshot.Gaps.LatestKind) ||
		!validP3ACCErrorCode(snapshot.Gaps.LatestReasonCode)) {
		return errors.New("P3ACC_SNAPSHOT_INVALID")
	}
	if snapshot.Checkpoint.Exists &&
		(!validP3ACCCheckpointState(snapshot.Checkpoint.State) || snapshot.Checkpoint.CommittedSequence < 0 ||
			snapshot.Checkpoint.MaxSourceSequence < 0 || snapshot.Checkpoint.OpenGiftFoldCount < 0) {
		return errors.New("P3ACC_SNAPSHOT_INVALID")
	}
	if snapshot.Runtime.RecoveryProven &&
		(!snapshot.Runtime.CrashInjected || !snapshot.Runtime.AttemptAdvanced) {
		return errors.New("P3ACC_SNAPSHOT_INVALID")
	}
	if snapshot.Runtime.NetworkRecoveryProven &&
		(!snapshot.Runtime.NetworkFaultArmed || !snapshot.Runtime.RecoveryProven) {
		return errors.New("P3ACC_SNAPSHOT_INVALID")
	}
	if snapshot.Runtime.FinalizationProven &&
		(!snapshot.Runtime.RecoveryProven || !snapshot.Runtime.NetworkRecoveryProven ||
			snapshot.Stage != p3ACCStageFinalized) {
		return errors.New("P3ACC_SNAPSHOT_INVALID")
	}
	return nil
}

func validP3ACCUI(summary p3ACCUIObservationSummary) bool {
	if summary.LatencySampleCount < 0 || summary.LatencySampleCount > p3ACCAcceptanceMaximumLatencySamples ||
		summary.LatencyPendingCount < 0 || summary.LatencyPendingCount > p3ACCAcceptanceMaximumLatencyPending ||
		summary.LatencyP95MS < 0 || summary.LatencyMaxMS < summary.LatencyP95MS ||
		summary.LatencySampleCount == 0 && (summary.LatencyP95MS != 0 || summary.LatencyMaxMS != 0) ||
		summary.LatencyWithinTarget && (summary.LatencySampleCount == 0 ||
			summary.LatencyPendingCount != 0 || summary.LatencyP95MS >= 1000) {
		return false
	}
	flags := []bool{
		summary.Ready, summary.RecordingSeen, summary.ProgressAdvanced, summary.TimelineSeen,
		summary.ReconnectingSeen, summary.RecoveredSeen, summary.NetworkReconnectingSeen,
		summary.NetworkRecoveredSeen, summary.OfflineSeen, summary.FinalizedSeen,
	}
	count := 0
	for _, enabled := range flags {
		if enabled {
			count++
		}
	}
	if summary.ObservationCount != count {
		return false
	}
	return (!summary.RecordingSeen || summary.Ready) &&
		(!summary.ProgressAdvanced || summary.RecordingSeen) &&
		(!summary.TimelineSeen || summary.RecordingSeen) &&
		(!summary.ReconnectingSeen || summary.RecordingSeen) &&
		(!summary.RecoveredSeen || summary.ReconnectingSeen) &&
		(!summary.NetworkReconnectingSeen || summary.RecoveredSeen) &&
		(!summary.NetworkRecoveredSeen || summary.NetworkReconnectingSeen) &&
		(!summary.OfflineSeen || summary.NetworkRecoveredSeen) &&
		(!summary.FinalizedSeen || summary.OfflineSeen && summary.LatencyWithinTarget &&
			summary.LatencySampleCount > 0 && summary.LatencyPendingCount == 0)
}

func validP3ACCCounts(snapshot p3ACCAcceptanceSnapshot) bool {
	values := []int{
		snapshot.Database.SessionCount, snapshot.Database.ActiveSessionCount,
		snapshot.Database.PublishedEventCount, snapshot.Database.SegmentCount,
		snapshot.Database.CompleteSegmentCount, snapshot.Database.ArtifactCount,
		snapshot.Database.CompleteArtifactCount, snapshot.MediaManifest.AttemptCount,
		snapshot.MediaManifest.CommittedAttemptCount, snapshot.MediaManifest.CleanAttemptCount,
		snapshot.MediaManifest.SegmentCount, snapshot.MediaManifest.CompleteSegmentCount,
		snapshot.MediaManifest.ArtifactCount, snapshot.MediaManifest.CompleteArtifactCount,
		snapshot.MediaManifest.FileCheckCount, snapshot.MediaManifest.FileFailureCount,
		snapshot.MediaManifest.IncompleteEntryCount, snapshot.MediaManifest.IncompleteSegmentCount,
		snapshot.Gaps.Total, snapshot.Gaps.Open, snapshot.Gaps.Recovered,
		snapshot.Gaps.RecordingRestart, snapshot.Gaps.OpenRecordingRestart,
		snapshot.Gaps.ProcessCrash, snapshot.Gaps.MessageDisconnect,
		snapshot.Gaps.OpenMessageDisconnect,
	}
	for _, value := range values {
		if value < 0 {
			return false
		}
	}
	return snapshot.Database.EventCount >= 0 && snapshot.Database.SourceEventCount >= 0
}

func validP3ACCResourceSummary(summary p3ACCResourceSummary) bool {
	if summary.SampleCount < 0 || summary.SampleCount > p3ACCAcceptanceMaximumResourceSamples {
		return false
	}
	for _, rule := range p3ACCResourceMetricRules(summary) {
		metric := rule.metric
		expectedTrend := "INSUFFICIENT"
		if summary.SampleCount >= 2 {
			expectedTrend = p3ACCIntegerTrend(metric.LatterHalfDelta, rule.threshold)
		}
		if metric.Baseline < 0 || metric.Peak < metric.Baseline || metric.Latest < 0 ||
			metric.Peak < metric.Latest || metric.Delta != metric.Latest-metric.Baseline ||
			metric.LatterHalfTrend != expectedTrend ||
			summary.SampleCount < 2 && metric.LatterHalfDelta != 0 ||
			rule.cumulative && (metric.Delta < 0 || metric.LatterHalfDelta < 0 ||
				metric.Peak != metric.Latest) {
			return false
		}
	}
	if math.IsNaN(summary.AverageCPUPercent) || math.IsInf(summary.AverageCPUPercent, 0) ||
		math.IsNaN(summary.LatterHalfAverageCPUPercent) || math.IsInf(summary.LatterHalfAverageCPUPercent, 0) ||
		summary.AverageCPUPercent < 0 || summary.LatterHalfAverageCPUPercent < 0 ||
		!validP3ACCNonNegativeFinite(summary.AverageProcessReadBytesPerSecond) ||
		!validP3ACCNonNegativeFinite(summary.AverageProcessWriteBytesPerSecond) ||
		!validP3ACCNonNegativeFinite(summary.LatterHalfProcessReadBytesPerSecond) ||
		!validP3ACCNonNegativeFinite(summary.LatterHalfProcessWriteBytesPerSecond) ||
		!validP3ACCNonNegativeFinite(summary.AverageDiskWriteBytesPerSecond) ||
		!validP3ACCNonNegativeFinite(summary.LatterHalfDiskWriteBytesPerSecond) ||
		summary.WindowDurationMS < 0 ||
		!validP3ACCTrend(summary.CPUTrend) ||
		!validP3ACCRateEvidence(
			summary.ProcessReadBytes,
			summary.AverageProcessReadBytesPerSecond,
			summary.LatterHalfProcessReadBytesPerSecond,
		) ||
		!validP3ACCRateEvidence(
			summary.ProcessWriteBytes,
			summary.AverageProcessWriteBytesPerSecond,
			summary.LatterHalfProcessWriteBytesPerSecond,
		) ||
		!validP3ACCRateEvidence(
			summary.DataRootPhysicalBytes,
			summary.AverageDiskWriteBytesPerSecond,
			summary.LatterHalfDiskWriteBytesPerSecond,
		) ||
		summary.DiskIOObserved != (summary.DataRootPhysicalBytes.Delta > 0 &&
			summary.AverageDiskWriteBytesPerSecond > 0) ||
		summary.EventQueueObserved != (summary.EventQueueCount.Peak > 0) ||
		summary.EventQueueObserved && !validP3ACCEventQueueCapacity(summary) {
		return false
	}
	if summary.SampleCount < 2 {
		return !summary.CPUWithinTarget && !summary.StableWindowProven && !summary.DiskIOObserved &&
			summary.CPUTrend == "INSUFFICIENT"
	}
	eligible := summary.SampleComplete && summary.SampleCount >= p3ACCAcceptanceMinimumStableSamples &&
		summary.WindowDurationMS >= p3ACCAcceptanceStableWindow.Milliseconds() &&
		summary.DatabaseWALObserved && summary.DiskIOObserved && summary.EventQueueObserved &&
		validP3ACCEventQueueCapacity(summary)
	if summary.StableWindowProven != eligible ||
		summary.CPUWithinTarget != (eligible && summary.AverageCPUPercent < 10) ||
		eligible && (summary.EventQueueCount.Baseline < 1 || summary.EventQueueCount.Latest < 1) {
		return false
	}
	return true
}

type p3ACCResourceMetricRule struct {
	metric     p3ACCResourceMetric
	threshold  int64
	cumulative bool
}

func p3ACCResourceMetricRules(summary p3ACCResourceSummary) []p3ACCResourceMetricRule {
	return []p3ACCResourceMetricRule{
		{metric: summary.ProcessCount},
		{metric: summary.WorkingSet, threshold: 1 << 20},
		{metric: summary.PrivateBytes, threshold: 1 << 20},
		{metric: summary.Threads, threshold: 2},
		{metric: summary.Handles, threshold: 2},
		{metric: summary.Goroutines, threshold: 2},
		{metric: summary.HeapAlloc, threshold: 1 << 20},
		{metric: summary.HeapInUse, threshold: 1 << 20},
		{metric: summary.System, threshold: 1 << 20},
		{metric: summary.DatabaseWALBytes, threshold: 1 << 20},
		{metric: summary.ProcessReadBytes, cumulative: true},
		{metric: summary.ProcessWriteBytes, cumulative: true},
		{metric: summary.DataRootPhysicalBytes, cumulative: true},
		{metric: summary.EventQueueCount},
		{metric: summary.EventQueueItems, threshold: 8},
		{metric: summary.EventQueueBytes, threshold: 64 << 10},
		{metric: summary.EventQueueItemCapacity},
		{metric: summary.EventQueueByteCapacity},
	}
}

func validP3ACCRateEvidence(
	metric p3ACCResourceMetric,
	average float64,
	latterHalfAverage float64,
) bool {
	if !validP3ACCNonNegativeFinite(average) ||
		!validP3ACCNonNegativeFinite(latterHalfAverage) ||
		metric.Delta < 0 || metric.LatterHalfDelta < 0 {
		return false
	}
	return (metric.Delta > 0) == (average > 0) &&
		(metric.LatterHalfDelta > 0) == (latterHalfAverage > 0)
}

func validP3ACCNonNegativeFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func validP3ACCEventQueueCapacity(summary p3ACCResourceSummary) bool {
	itemCapacity := summary.EventQueueItemCapacity
	byteCapacity := summary.EventQueueByteCapacity
	expectedTrend := "STABLE"
	if summary.SampleCount < 2 {
		expectedTrend = "INSUFFICIENT"
	}
	return itemCapacity.Baseline > 0 && byteCapacity.Baseline > 0 &&
		itemCapacity.Peak == itemCapacity.Baseline && itemCapacity.Latest == itemCapacity.Baseline &&
		itemCapacity.Delta == 0 && itemCapacity.LatterHalfDelta == 0 &&
		itemCapacity.LatterHalfTrend == expectedTrend &&
		byteCapacity.Peak == byteCapacity.Baseline && byteCapacity.Latest == byteCapacity.Baseline &&
		byteCapacity.Delta == 0 && byteCapacity.LatterHalfDelta == 0 &&
		byteCapacity.LatterHalfTrend == expectedTrend &&
		summary.EventQueueItems.Baseline <= itemCapacity.Baseline &&
		summary.EventQueueItems.Peak <= itemCapacity.Baseline &&
		summary.EventQueueItems.Latest <= itemCapacity.Baseline &&
		summary.EventQueueBytes.Baseline <= byteCapacity.Baseline &&
		summary.EventQueueBytes.Peak <= byteCapacity.Baseline &&
		summary.EventQueueBytes.Latest <= byteCapacity.Baseline
}

func validP3ACCTrend(value string) bool {
	return value == "INSUFFICIENT" || value == "STABLE" || value == "RISING" || value == "FALLING"
}

func validP3ACCStage(stage p3ACCStage) bool {
	switch stage {
	case p3ACCStageInitializing, p3ACCStageConfigured, p3ACCStageWaiting,
		p3ACCStageStarting, p3ACCStageLive, p3ACCStageRecording,
		p3ACCStageReconnecting, p3ACCStageRecovered, p3ACCStageFinalizing,
		p3ACCStageFinalized, p3ACCStageOffline, p3ACCStageError:
		return true
	default:
		return false
	}
}

func validP3ACCRuntimeState(state room.RuntimeState) bool {
	switch state {
	case room.RuntimeStopped, room.RuntimeWaiting, room.RuntimeStarting, room.RuntimeLive,
		room.RuntimeRecording, room.RuntimeReconnecting, room.RuntimeFinalizing, room.RuntimeError:
		return true
	default:
		return false
	}
}

func validP3ACCSessionStatus(status capture.SessionStatus) bool {
	switch status {
	case capture.SessionStarting, capture.SessionRecording, capture.SessionFinalizing,
		capture.SessionCompleted, capture.SessionInterrupted, capture.SessionFailed:
		return true
	default:
		return false
	}
}

func validP3ACCRecordingStatus(status capture.RecordingStatus, allowEmpty bool) bool {
	if status == "" {
		return allowEmpty
	}
	switch status {
	case capture.RecordingPending, capture.RecordingDisabled, capture.RecordingStarting,
		capture.RecordingActive, capture.RecordingUnavailable, capture.RecordingReconnecting,
		capture.RecordingFinalizing, capture.RecordingCompleted, capture.RecordingIncomplete,
		capture.RecordingFailed:
		return true
	default:
		return false
	}
}

func validP3ACCTerminalMediaEvidence(summary p3ACCMediaManifestSummary) bool {
	if summary.State != capture.SessionMediaCompleted && summary.State != capture.SessionMediaIncomplete {
		return false
	}
	if !summary.AllFilesMatch || summary.FileFailureCount != 0 || summary.FileCheckCount < 1 ||
		summary.SegmentCount < 1 || !summary.SequenceContinuous ||
		!summary.AttemptReferencesValid || !summary.FaultPhaseSegmentsProven ||
		summary.FileCheckCount+summary.IncompleteEntryCount != summary.SegmentCount+summary.ArtifactCount {
		return false
	}
	if summary.State == capture.SessionMediaCompleted {
		return summary.IncompleteSegmentCount == 0
	}
	return summary.IncompleteSegmentCount > 0
}

func validP3ACCMediaState(state capture.SessionMediaState) bool {
	switch state {
	case capture.SessionMediaOpen, capture.SessionMediaFinalizing,
		capture.SessionMediaCompleted, capture.SessionMediaIncomplete:
		return true
	default:
		return false
	}
}

func validP3ACCGapKind(kind string) bool {
	switch kind {
	case "message_disconnect", "recording_restart", "stream_unavailable",
		"disk_full", "process_crash", "clock_uncertain", "event_persistence":
		return true
	default:
		return false
	}
}

func validP3ACCCheckpointState(state string) bool {
	return state == "open" || state == "closing" || state == "closed" || state == "degraded"
}

func validP3ACCUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.Version() == 7 && strings.ToLower(parsed.String()) == value
}

func safeP3ACCErrorCode(code string) string {
	if validP3ACCErrorCode(code) {
		return code
	}
	return "UNKNOWN"
}

func validP3ACCErrorCode(code string) bool {
	_, ok := p3ACCAcceptanceAllowedCodes[code]
	return ok
}

func ensureP3ACCJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	return errors.New("P3ACC_SNAPSHOT_FAILED")
}
