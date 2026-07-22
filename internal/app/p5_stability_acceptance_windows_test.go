//go:build p5stbacceptance && windows

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	douyinLive "github.com/jwwsjlm/douyinLive/v2"
	"github.com/jwwsjlm/douyinLive/v2/internal/capture"
	"github.com/jwwsjlm/douyinLive/v2/internal/eventstore"
	"github.com/jwwsjlm/douyinLive/v2/internal/room"
	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
	"github.com/jwwsjlm/douyinlive-proto/generated/new_douyin"
	"golang.org/x/sys/windows"
	"google.golang.org/protobuf/proto"
)

const (
	p5STBFormalDuration       = 60 * time.Minute
	p5STBFormalSampleInterval = time.Minute
	p5STBOperationTimeout     = 20 * time.Second
	p5STBMaximumWorkingSet    = 300 << 20
	p5STBMaximumMemoryGrowth  = 64 << 20
	p5STBMaximumHandleGrowth  = 64
	p5STBMaximumGoroutineRise = 64
)

var (
	p5STBKernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	p5STBPSAPI                     = windows.NewLazySystemDLL("psapi.dll")
	p5STBGetProcessHandleCountProc = p5STBKernel32.NewProc("GetProcessHandleCount")
	p5STBGetProcessIOCountersProc  = p5STBKernel32.NewProc("GetProcessIoCounters")
	p5STBGetProcessMemoryInfoProc  = p5STBPSAPI.NewProc("GetProcessMemoryInfo")
)

type p5STBProcessMemoryCounters struct {
	Size                       uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
	PrivateUsage               uintptr
}

type p5STBProcessIOCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type p5STBResourceSample struct {
	Sequence          int   `json:"sequence"`
	ElapsedSeconds    int64 `json:"elapsedSeconds"`
	WorkingSetBytes   int64 `json:"workingSetBytes"`
	PrivateBytes      int64 `json:"privateBytes"`
	Handles           int64 `json:"handles"`
	Goroutines        int64 `json:"goroutines"`
	CPU100Nanoseconds int64 `json:"cpu100Nanoseconds"`
	ReadBytes         int64 `json:"readBytes"`
	WriteBytes        int64 `json:"writeBytes"`
	DatabaseBytes     int64 `json:"databaseBytes"`
	WALBytes          int64 `json:"walBytes"`
	QueueCount        int64 `json:"queueCount"`
	QueueItems        int64 `json:"queueItems"`
	QueueBytes        int64 `json:"queueBytes"`
}

type p5STBReport struct {
	Schema                  string                `json:"schema"`
	DurationSeconds         int64                 `json:"durationSeconds"`
	SampleIntervalSeconds   int64                 `json:"sampleIntervalSeconds"`
	RoomCount               int                   `json:"roomCount"`
	Samples                 []p5STBResourceSample `json:"samples"`
	StatusCounts            map[string]int64      `json:"statusCounts"`
	StatusLatencyP95MS      int64                 `json:"statusLatencyP95Ms"`
	EventCount              int64                 `json:"eventCount"`
	SessionCount            int64                 `json:"sessionCount"`
	DatabaseBusyQueuePeak   int64                 `json:"databaseBusyQueuePeak"`
	DatabaseBusyRecovered   bool                  `json:"databaseBusyRecovered"`
	NetworkFaultObserved    bool                  `json:"networkFaultObserved"`
	NetworkRecoveryObserved bool                  `json:"networkRecoveryObserved"`
	ForcedExitRecovered     bool                  `json:"forcedExitRecovered"`
	DiskFullClassified      bool                  `json:"diskFullClassified"`
	AverageCPUPercent       float64               `json:"averageCpuPercent"`
	MemoryGrowthBytes       int64                 `json:"memoryGrowthBytes"`
	HandleGrowth            int64                 `json:"handleGrowth"`
	GoroutineGrowth         int64                 `json:"goroutineGrowth"`
	Passed                  bool                  `json:"passed"`
}

type p5STBSchedule struct {
	started      time.Time
	duration     time.Duration
	forceLive    bool
	networkFired atomic.Bool
}

func (s *p5STBSchedule) isLive(roomIndex int, now time.Time) bool {
	if s.forceLive {
		return true
	}
	if roomIndex < 2 || s.duration <= 0 {
		return false
	}
	elapsedFromStart := now.Sub(s.started)
	busyWindow := 10 * time.Second
	if s.duration < p5STBFormalDuration {
		busyWindow = 3 * time.Second
	}
	if roomIndex == 2 && elapsedFromStart >= s.duration/3-time.Second && elapsedFromStart <= s.duration/3+busyWindow {
		return true
	}
	cycle := s.duration / 6
	if cycle < 1500*time.Millisecond {
		cycle = 1500 * time.Millisecond
	}
	offset := time.Duration(0)
	if roomIndex%2 == 1 {
		offset = cycle / 2
	}
	elapsed := now.Sub(s.started) + offset
	if elapsed < 0 {
		return false
	}
	return elapsed%cycle < cycle*2/3
}

func (s *p5STBSchedule) shouldDisconnect(roomIndex int, now time.Time) bool {
	if s.forceLive || roomIndex != 2 || now.Sub(s.started) < s.duration/2 {
		return false
	}
	return s.networkFired.CompareAndSwap(false, true)
}

type p5STBLiveClient struct {
	ctx       context.Context
	schedule  *p5STBSchedule
	roomIndex int
	offline   atomic.Bool
	closed    chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
	handler   douyinLive.LiveMessageHandler
	sequence  atomic.Uint64
}

func newP5STBLiveClient(ctx context.Context, schedule *p5STBSchedule, roomIndex int) *p5STBLiveClient {
	return &p5STBLiveClient{ctx: ctx, schedule: schedule, roomIndex: roomIndex, closed: make(chan struct{})}
}

func (c *p5STBLiveClient) PrepareWebSocketContext() error {
	offline := !c.schedule.isLive(c.roomIndex, time.Now())
	c.offline.Store(offline)
	if offline {
		return douyinLive.ErrLiveNotStarted
	}
	return nil
}

func (c *p5STBLiveClient) IsKnownOfflineStatus() bool { return c.offline.Load() }
func (c *p5STBLiveClient) GetName() string            { return "synthetic" }
func (c *p5STBLiveClient) GetTitle() string           { return "P5 stability fixture" }

func (c *p5STBLiveClient) Start() error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case <-c.closed:
			return nil
		case now := <-ticker.C:
			if c.schedule.shouldDisconnect(c.roomIndex, now) {
				return errors.New("synthetic network disconnect")
			}
			if !c.schedule.isLive(c.roomIndex, now) {
				c.offline.Store(true)
				return nil
			}
			c.emit(now)
		}
	}
}

func (c *p5STBLiveClient) Close()   { c.closeOnce.Do(func() { close(c.closed) }) }
func (c *p5STBLiveClient) Dispose() { c.Close() }
func (c *p5STBLiveClient) ResolveStreams() ([]douyinLive.ResolvedStream, error) {
	return nil, nil
}

func (c *p5STBLiveClient) SubscribeMessage(handler douyinLive.LiveMessageHandler) string {
	c.mu.Lock()
	c.handler = handler
	c.mu.Unlock()
	return "p5-stability-subscription"
}

func (c *p5STBLiveClient) Unsubscribe(string) {
	c.mu.Lock()
	c.handler = nil
	c.mu.Unlock()
}

func (c *p5STBLiveClient) emit(now time.Time) {
	sequence := c.sequence.Add(1)
	payload, err := proto.Marshal(&new_douyin.Webcast_Im_ChatMessage{
		Common:  &new_douyin.Webcast_Im_Common{MsgId: sequence},
		Content: "synthetic event",
	})
	if err != nil {
		return
	}
	c.mu.Lock()
	handler := c.handler
	c.mu.Unlock()
	if handler != nil {
		handler(&douyinLive.LiveMessage{
			Raw:        &new_douyin.Webcast_Im_Message{Method: "WebcastChatMessage", Payload: payload},
			ReceivedAt: now.UTC(),
		})
	}
}

type p5STBStatusCollector struct {
	mu               sync.Mutex
	counts           map[string]int64
	latencies        []int64
	networkRoomID    string
	networkFaultSeen bool
	networkRecovery  bool
	lastNetworkFault int64
}

func newP5STBStatusCollector() *p5STBStatusCollector {
	return &p5STBStatusCollector{counts: make(map[string]int64)}
}

func (c *p5STBStatusCollector) publish(status room.RoomRuntimeStatus) {
	now := time.Now().UTC().UnixMilli()
	latency := now - status.ChangedAt
	if latency < 0 {
		latency = 0
	}
	c.mu.Lock()
	c.counts[string(status.State)]++
	c.latencies = append(c.latencies, latency)
	if status.RoomID == c.networkRoomID {
		if status.ErrorCode == "ROOM_CONNECTION_INTERRUPTED" || status.State == room.RuntimeReconnecting {
			c.networkFaultSeen = true
			c.lastNetworkFault = status.Revision
		}
		if c.networkFaultSeen && status.Revision > c.lastNetworkFault &&
			(status.State == room.RuntimeLive || status.State == room.RuntimeRecording) {
			c.networkRecovery = true
		}
	}
	c.mu.Unlock()
}

func (c *p5STBStatusCollector) snapshot() (map[string]int64, int64, bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	counts := make(map[string]int64, len(c.counts))
	for key, value := range c.counts {
		counts[key] = value
	}
	latencies := append([]int64(nil), c.latencies...)
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	var p95 int64
	if len(latencies) > 0 {
		index := int(math.Ceil(float64(len(latencies))*0.95)) - 1
		if index < 0 {
			index = 0
		}
		p95 = latencies[index]
	}
	return counts, p95, c.networkFaultSeen, c.networkRecovery
}

type p5STBBusyResult struct {
	queuePeak int64
	recovered bool
	err       error
}

func TestP5STBStabilitySmoke(t *testing.T) {
	runP5STBStability(t, 12*time.Second, 2*time.Second, t.TempDir(), "")
}

func TestP5STB60MinuteStability(t *testing.T) {
	if os.Getenv("P5STB_RUN_60M") != "1" {
		t.Fatal("P5STB_EXPLICIT_60M_AUTHORIZATION_REQUIRED")
	}
	root, resultPath := p5STBFormalPaths(t)
	t.Log("P5STB_60M_RUNNING")
	runP5STBStability(t, p5STBFormalDuration, p5STBFormalSampleInterval, root, resultPath)
	t.Log("P5STB_60M_PASSED")
}

func runP5STBStability(t *testing.T, duration, sampleInterval time.Duration, root, resultPath string) {
	t.Helper()
	if duration <= 0 || sampleInterval <= 0 || duration%sampleInterval != 0 {
		t.Fatal("P5STB_INVALID_DURATION")
	}
	dataRoot := filepath.Join(root, "data")
	application := New(Options{Name: "p5-stability", Version: "test"})
	application.Startup(context.Background())
	cleanupPending := true
	defer func() {
		if cleanupPending {
			ctx, cancel := context.WithTimeout(context.Background(), p5STBOperationTimeout)
			defer cancel()
			_ = application.Shutdown(ctx)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), p5STBOperationTimeout)
	err := application.InitializeInfrastructure(ctx, InfrastructureOptions{DataRoot: dataRoot, DisableDiagnostics: true})
	cancel()
	if err != nil {
		t.Fatalf("P5STB_INIT_FAILED: %v", err)
	}
	store := application.Store()
	service := application.RoomService()
	coordinator := application.CaptureCoordinator()
	events := application.EventStoreManager()
	if store == nil || service == nil || coordinator == nil || events == nil {
		t.Fatal("P5STB_INFRASTRUCTURE_INCOMPLETE")
	}

	started := time.Now()
	schedule := &p5STBSchedule{started: started, duration: duration}
	collector := newP5STBStatusCollector()
	roomIndexes := make(map[string]int)
	manager, err := room.NewMonitorManager(context.Background(), service, application.Logger(), room.MonitorOptions{
		PollInterval: sampleInterval / 10, ReconnectDelay: sampleInterval / 20,
		FinalizeTimeout: 5 * time.Second, Coordinator: coordinator, Publisher: collector.publish,
		Factory: func(ctx context.Context, config room.RoomConfig, _ string) (room.LiveClient, error) {
			return newP5STBLiveClient(ctx, schedule, roomIndexes[config.ID]), nil
		},
	})
	if err != nil {
		t.Fatalf("P5STB_MONITOR_INIT_FAILED: %v", err)
	}
	managerCleanup := true
	defer func() {
		if managerCleanup {
			_ = manager.Shutdown(context.Background())
		}
	}()

	roomIDs := make([]string, 0, 4)
	for index := 0; index < 4; index++ {
		createCtx, createCancel := context.WithTimeout(context.Background(), p5STBOperationTimeout)
		config, createErr := service.CreateRoom(createCtx, room.CreateRoomInput{
			LiveID: fmt.Sprintf("90000000000000000%02d", index+1), Alias: fmt.Sprintf("synthetic-%d", index+1),
			MonitorEnabled: false, RecordEnabled: false,
			RecordingProfile: room.RecordingProfile{Quality: room.QualityAuto, SegmentMinutes: 5},
		})
		createCancel()
		if createErr != nil {
			t.Fatalf("P5STB_ROOM_CREATE_FAILED: %v", createErr)
		}
		roomIndexes[config.ID] = index
		roomIDs = append(roomIDs, config.ID)
	}
	collector.mu.Lock()
	collector.networkRoomID = roomIDs[2]
	collector.mu.Unlock()
	for _, roomID := range roomIDs {
		startCtx, startCancel := context.WithTimeout(context.Background(), p5STBOperationTimeout)
		startErr := manager.StartMonitoring(startCtx, roomID)
		startCancel()
		if startErr != nil {
			t.Fatalf("P5STB_MONITOR_START_FAILED: %v", startErr)
		}
	}

	busyResult := make(chan p5STBBusyResult, 1)
	go p5STBInjectDatabaseBusy(store.Writer(), events, duration/3, p5STBBusyHold(duration), busyResult)
	sampleCount := int(duration/sampleInterval) + 1
	samples := make([]p5STBResourceSample, 0, sampleCount)
	for sequence := 0; sequence < sampleCount; sequence++ {
		if sequence > 0 {
			deadline := started.Add(time.Duration(sequence) * sampleInterval)
			timer := time.NewTimer(time.Until(deadline))
			<-timer.C
		}
		runtime.GC()
		sample, sampleErr := p5STBSampleResources(store, events, sequence, time.Since(started))
		if sampleErr != nil {
			t.Fatalf("P5STB_RESOURCE_SAMPLE_FAILED: %v", sampleErr)
		}
		samples = append(samples, sample)
	}

	var busy p5STBBusyResult
	select {
	case busy = <-busyResult:
	case <-time.After(p5STBOperationTimeout):
		t.Fatal("P5STB_DATABASE_BUSY_RESULT_TIMEOUT")
	}
	if busy.err != nil || !busy.recovered || busy.queuePeak == 0 {
		t.Fatalf("P5STB_DATABASE_BUSY_NOT_RECOVERED peak=%d recovered=%t err=%v", busy.queuePeak, busy.recovered, busy.err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), p5STBOperationTimeout)
	if err := manager.Shutdown(shutdownCtx); err != nil {
		shutdownCancel()
		t.Fatalf("P5STB_MONITOR_SHUTDOWN_FAILED: %v", err)
	}
	shutdownCancel()
	managerCleanup = false

	queryCtx, queryCancel := context.WithTimeout(context.Background(), p5STBOperationTimeout)
	var activeSessions, sessionCount, eventCount int64
	if err := store.Reader().QueryRowContext(queryCtx,
		`SELECT COUNT(*) FROM live_sessions WHERE status IN ('starting','recording','finalizing')`,
	).Scan(&activeSessions); err != nil {
		queryCancel()
		t.Fatalf("P5STB_ACTIVE_SESSION_QUERY_FAILED: %v", err)
	}
	if err := store.Reader().QueryRowContext(queryCtx, `SELECT COUNT(*) FROM live_sessions`).Scan(&sessionCount); err != nil {
		queryCancel()
		t.Fatalf("P5STB_SESSION_QUERY_FAILED: %v", err)
	}
	if err := store.Reader().QueryRowContext(queryCtx, `SELECT COUNT(*) FROM live_events`).Scan(&eventCount); err != nil {
		queryCancel()
		t.Fatalf("P5STB_EVENT_QUERY_FAILED: %v", err)
	}
	queryCancel()
	if activeSessions != 0 || sessionCount == 0 || eventCount == 0 {
		t.Fatalf("P5STB_DURABILITY_MISMATCH active=%d sessions=%d events=%d", activeSessions, sessionCount, eventCount)
	}

	counts, latencyP95, networkFault, networkRecovery := collector.snapshot()
	report := p5STBBuildReport(duration, sampleInterval, samples, counts, latencyP95, eventCount, sessionCount, busy)
	report.NetworkFaultObserved = networkFault && schedule.networkFired.Load()
	report.NetworkRecoveryObserved = networkRecovery
	report.ForcedExitRecovered = p5STBRunForcedExitRecovery(t)
	report.DiskFullClassified = capture.RecorderErrorCodeForAcceptance("There is not enough space on the disk") == capture.RecorderLocalResourceErrorCode
	p5STBValidateReport(t, &report)
	if resultPath != "" {
		p5STBWriteReport(t, resultPath, report)
	}

	appShutdownCtx, appShutdownCancel := context.WithTimeout(context.Background(), p5STBOperationTimeout)
	if err := application.Shutdown(appShutdownCtx); err != nil {
		appShutdownCancel()
		t.Fatalf("P5STB_APP_SHUTDOWN_FAILED: %v", err)
	}
	appShutdownCancel()
	cleanupPending = false
	if err := os.Rename(store.Path(), store.Path()+".closed"); err != nil {
		t.Fatalf("P5STB_DATABASE_HANDLE_LEAK: %v", err)
	}
}

func p5STBBusyHold(duration time.Duration) time.Duration {
	if duration >= p5STBFormalDuration {
		return 7 * time.Second
	}
	return time.Second
}

func p5STBInjectDatabaseBusy(db *sql.DB, events *eventstore.Manager, delay, hold time.Duration, result chan<- p5STBBusyResult) {
	timer := time.NewTimer(delay)
	<-timer.C
	ctx, cancel := context.WithTimeout(context.Background(), p5STBOperationTimeout)
	defer cancel()
	connection, err := db.Conn(ctx)
	if err != nil {
		result <- p5STBBusyResult{err: err}
		return
	}
	defer connection.Close()
	if _, err = connection.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		result <- p5STBBusyResult{err: err}
		return
	}
	peak := int64(0)
	deadline := time.Now().Add(hold)
	for time.Now().Before(deadline) {
		stats := events.AggregateQueueStats()
		if stats.Complete && stats.Items > peak {
			peak = stats.Items
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err = connection.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		result <- p5STBBusyResult{queuePeak: peak, err: err}
		return
	}
	if err = connection.Close(); err != nil {
		result <- p5STBBusyResult{queuePeak: peak, err: err}
		return
	}
	recoveryDeadline := time.Now().Add(p5STBOperationTimeout)
	for time.Now().Before(recoveryDeadline) {
		stats := events.AggregateQueueStats()
		probeCtx, probeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		probeErr := db.PingContext(probeCtx)
		probeCancel()
		if stats.Complete && probeErr == nil {
			result <- p5STBBusyResult{
				queuePeak: peak, recovered: true,
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	result <- p5STBBusyResult{queuePeak: peak, err: errors.New("event queue did not drain")}
}

func p5STBSampleResources(store *storage.Store, events *eventstore.Manager, sequence int, elapsed time.Duration) (p5STBResourceSample, error) {
	process := windows.CurrentProcess()
	memory := p5STBProcessMemoryCounters{Size: uint32(unsafe.Sizeof(p5STBProcessMemoryCounters{}))}
	if result, _, _ := p5STBGetProcessMemoryInfoProc.Call(uintptr(process), uintptr(unsafe.Pointer(&memory)), uintptr(memory.Size)); result == 0 {
		return p5STBResourceSample{}, errors.New("GetProcessMemoryInfo failed")
	}
	var handles uint32
	if result, _, _ := p5STBGetProcessHandleCountProc.Call(uintptr(process), uintptr(unsafe.Pointer(&handles))); result == 0 {
		return p5STBResourceSample{}, errors.New("GetProcessHandleCount failed")
	}
	var ioCounters p5STBProcessIOCounters
	if result, _, _ := p5STBGetProcessIOCountersProc.Call(uintptr(process), uintptr(unsafe.Pointer(&ioCounters))); result == 0 {
		return p5STBResourceSample{}, errors.New("GetProcessIoCounters failed")
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(process, &creation, &exit, &kernel, &user); err != nil {
		return p5STBResourceSample{}, err
	}
	queue := events.AggregateQueueStats()
	if !queue.Complete {
		return p5STBResourceSample{}, errors.New("event queue snapshot incomplete")
	}
	databaseBytes, err := p5STBRegularFileSize(store.Path())
	if err != nil {
		return p5STBResourceSample{}, err
	}
	walBytes, err := p5STBOptionalRegularFileSize(store.Path() + "-wal")
	if err != nil {
		return p5STBResourceSample{}, err
	}
	return p5STBResourceSample{
		Sequence: sequence, ElapsedSeconds: int64(elapsed / time.Second),
		WorkingSetBytes: int64(memory.WorkingSetSize), PrivateBytes: int64(memory.PrivateUsage),
		Handles: int64(handles), Goroutines: int64(runtime.NumGoroutine()),
		CPU100Nanoseconds: p5STBFiletimeValue(kernel) + p5STBFiletimeValue(user),
		ReadBytes:         p5STBUint64ToInt64(ioCounters.ReadTransferCount), WriteBytes: p5STBUint64ToInt64(ioCounters.WriteTransferCount),
		DatabaseBytes: databaseBytes, WALBytes: walBytes,
		QueueCount: queue.QueueCount, QueueItems: queue.Items, QueueBytes: queue.Bytes,
	}, nil
}

func p5STBRegularFileSize(path string) (int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return 0, errors.New("resource path is not a regular file")
	}
	return info.Size(), nil
}

func p5STBOptionalRegularFileSize(path string) (int64, error) {
	size, err := p5STBRegularFileSize(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	return size, err
}

func p5STBFiletimeValue(value windows.Filetime) int64 {
	combined := uint64(value.HighDateTime)<<32 | uint64(value.LowDateTime)
	return p5STBUint64ToInt64(combined)
}

func p5STBUint64ToInt64(value uint64) int64 {
	if value > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(value)
}

func p5STBBuildReport(duration, sampleInterval time.Duration, samples []p5STBResourceSample, counts map[string]int64, latencyP95, events, sessions int64, busy p5STBBusyResult) p5STBReport {
	first, last := samples[0], samples[len(samples)-1]
	wall100NS := float64(duration) / float64(100*time.Nanosecond)
	cpuPercent := float64(last.CPU100Nanoseconds-first.CPU100Nanoseconds) / wall100NS * 100
	return p5STBReport{
		Schema: "P5-STB-001/v1", DurationSeconds: int64(duration / time.Second),
		SampleIntervalSeconds: int64(sampleInterval / time.Second), RoomCount: 4,
		Samples: samples, StatusCounts: counts, StatusLatencyP95MS: latencyP95,
		EventCount: events, SessionCount: sessions,
		DatabaseBusyQueuePeak: busy.queuePeak, DatabaseBusyRecovered: busy.recovered,
		AverageCPUPercent: math.Round(cpuPercent*1000) / 1000,
		MemoryGrowthBytes: last.WorkingSetBytes - first.WorkingSetBytes,
		HandleGrowth:      last.Handles - first.Handles,
		GoroutineGrowth:   last.Goroutines - first.Goroutines,
	}
}

func p5STBValidateReport(t *testing.T, report *p5STBReport) {
	t.Helper()
	if len(report.Samples) < 2 || report.Samples[len(report.Samples)-1].WorkingSetBytes > p5STBMaximumWorkingSet ||
		report.MemoryGrowthBytes > p5STBMaximumMemoryGrowth || report.HandleGrowth > p5STBMaximumHandleGrowth ||
		report.GoroutineGrowth > p5STBMaximumGoroutineRise || report.AverageCPUPercent >= 10 ||
		report.StatusLatencyP95MS >= 1000 || !report.DatabaseBusyRecovered ||
		!report.NetworkFaultObserved || !report.NetworkRecoveryObserved || !report.ForcedExitRecovered || !report.DiskFullClassified {
		t.Fatalf("P5STB_RELEASE_GATE_FAILED: %#v", *report)
	}
	for _, sample := range report.Samples {
		if sample.WALBytes > 64<<20 || sample.WorkingSetBytes <= 0 || sample.Handles <= 0 {
			t.Fatalf("P5STB_SAMPLE_INVALID: %#v", sample)
		}
	}
	report.Passed = true
}

func p5STBFormalPaths(t *testing.T) (string, string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Clean(strings.TrimSpace(os.Getenv("P5STB_ROOT"))))
	if err != nil || root == "" || !filepath.IsAbs(root) {
		t.Fatal("P5STB_ROOT_INVALID")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("P5STB_ROOT_INVALID")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("P5STB_ROOT_NOT_EMPTY")
	}
	result, err := filepath.Abs(filepath.Clean(strings.TrimSpace(os.Getenv("P5STB_RESULT_PATH"))))
	if err != nil || filepath.Dir(result) != root || filepath.Base(result) != "p5-stability-result.json" {
		t.Fatal("P5STB_RESULT_PATH_INVALID")
	}
	return root, result
}

func p5STBWriteReport(t *testing.T, path string, report p5STBReport) {
	t.Helper()
	payload, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("P5STB_REPORT_ENCODE_FAILED: %v", err)
	}
	payload = append(payload, '\n')
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("P5STB_REPORT_CREATE_FAILED: %v", err)
	}
	if _, err = file.Write(payload); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(temporary)
		t.Fatalf("P5STB_REPORT_WRITE_FAILED: %v", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		t.Fatalf("P5STB_REPORT_PUBLISH_FAILED: %v", err)
	}
}

func p5STBRunForcedExitRecovery(t *testing.T) bool {
	t.Helper()
	root := filepath.Join(t.TempDir(), "forced-exit")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("P5STB_FORCED_EXIT_ROOT_FAILED: %v", err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestP5STBForcedExitChild$", "-test.v")
	command.Env = append(os.Environ(), "P5STB_FORCED_EXIT_CHILD=1", "P5STB_FORCED_EXIT_ROOT="+root)
	err := command.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 93 {
		t.Fatalf("P5STB_FORCED_EXIT_CHILD_FAILED: %v", err)
	}
	application := New(Options{Name: "p5-stability-recovery", Version: "test"})
	application.Startup(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), p5STBOperationTimeout)
	err = application.InitializeInfrastructure(ctx, InfrastructureOptions{DataRoot: root, DisableDiagnostics: true})
	cancel()
	if err != nil {
		t.Fatalf("P5STB_FORCED_EXIT_RESTART_FAILED: %v", err)
	}
	store := application.Store()
	var active int64
	if err := store.Reader().QueryRow(`SELECT COUNT(*) FROM live_sessions WHERE status IN ('starting','recording','finalizing')`).Scan(&active); err != nil || active != 0 {
		t.Fatalf("P5STB_FORCED_EXIT_RECOVERY_FAILED active=%d err=%v", active, err)
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), p5STBOperationTimeout)
	err = application.Shutdown(shutdownCtx)
	shutdownCancel()
	if err != nil {
		t.Fatalf("P5STB_FORCED_EXIT_SHUTDOWN_FAILED: %v", err)
	}
	return true
}

func TestP5STBForcedExitChild(t *testing.T) {
	if os.Getenv("P5STB_FORCED_EXIT_CHILD") != "1" {
		return
	}
	root := strings.TrimSpace(os.Getenv("P5STB_FORCED_EXIT_ROOT"))
	if root == "" || !filepath.IsAbs(root) {
		t.Fatal("P5STB_FORCED_EXIT_ROOT_INVALID")
	}
	application := New(Options{Name: "p5-stability-crash", Version: "test"})
	application.Startup(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), p5STBOperationTimeout)
	if err := application.InitializeInfrastructure(ctx, InfrastructureOptions{DataRoot: root, DisableDiagnostics: true}); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	service := application.RoomService()
	createCtx, createCancel := context.WithTimeout(context.Background(), p5STBOperationTimeout)
	config, err := service.CreateRoom(createCtx, room.CreateRoomInput{
		LiveID: "9000000000000000099", Alias: "forced-exit", MonitorEnabled: false, RecordEnabled: false,
		RecordingProfile: room.RecordingProfile{Quality: room.QualityAuto, SegmentMinutes: 5},
	})
	createCancel()
	if err != nil {
		t.Fatal(err)
	}
	source := newP5STBLiveClient(context.Background(), &p5STBSchedule{started: time.Now(), forceLive: true}, 2)
	openCtx, openCancel := context.WithTimeout(context.Background(), p5STBOperationTimeout)
	_, err = application.CaptureCoordinator().Open(openCtx, capture.OpenRequest{
		RoomConfigID: config.ID, OperationID: "018f0000-0000-7000-8000-000000000099",
		RecordEnabled: false, StartedAt: time.Now().UTC(),
	}, source)
	openCancel()
	if err != nil {
		t.Fatal(err)
	}
	source.emit(time.Now())
	time.Sleep(250 * time.Millisecond)
	os.Exit(93)
}
