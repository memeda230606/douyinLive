package room

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	douyinLive "github.com/jwwsjlm/douyinLive/v2"
	"github.com/jwwsjlm/douyinLive/v2/internal/capture"
)

const (
	StatusEventName              = "room:status"
	maxAutomaticFinalizeAttempts = 3
)

var errCaptureFinalizeNonterminal = errors.New("capture session finalization remained nonterminal")

type RuntimeState string

const (
	RuntimeStopped      RuntimeState = "STOPPED"
	RuntimeWaiting      RuntimeState = "WAITING"
	RuntimeStarting     RuntimeState = "STARTING"
	RuntimeLive         RuntimeState = "LIVE"
	RuntimeRecording    RuntimeState = "RECORDING"
	RuntimeReconnecting RuntimeState = "RECONNECTING"
	RuntimeFinalizing   RuntimeState = "FINALIZING"
	RuntimeError        RuntimeState = "ERROR"
)

type RoomRuntimeStatus struct {
	RoomID          string                  `json:"roomId"`
	LiveID          string                  `json:"liveId"`
	Alias           string                  `json:"alias"`
	State           RuntimeState            `json:"state"`
	OperationID     string                  `json:"operationId,omitempty"`
	SessionID       string                  `json:"sessionId,omitempty"`
	RecordingStatus capture.RecordingStatus `json:"recordingStatus,omitempty"`
	LiveName        string                  `json:"liveName,omitempty"`
	Title           string                  `json:"title,omitempty"`
	LastCheckedAt   int64                   `json:"lastCheckedAt,omitempty"`
	ChangedAt       int64                   `json:"changedAt"`
	Revision        int64                   `json:"revision"`
	RetryAt         int64                   `json:"retryAt,omitempty"`
	ErrorCode       string                  `json:"errorCode,omitempty"`
	Message         string                  `json:"message"`
}

// LiveClient is the existing-core surface shared by monitoring and capture.
// Tests replace it with an in-memory implementation.
type LiveClient interface {
	capture.CaptureSource
	PrepareWebSocketContext() error
	IsKnownOfflineStatus() bool
	Start() error
	Close()
	Dispose()
	GetName() string
	GetTitle() string
}

type LiveClientFactory func(context.Context, RoomConfig, string) (LiveClient, error)
type StatusPublisher func(RoomRuntimeStatus)

type MonitorOptions struct {
	PollInterval    time.Duration
	ReconnectDelay  time.Duration
	FinalizeTimeout time.Duration
	MaxRooms        int
	Now             func() time.Time
	Factory         LiveClientFactory
	Coordinator     capture.CaptureCoordinator
	Publisher       StatusPublisher
}

type MonitorManager struct {
	root                   context.Context
	service                *Service
	logger                 *slog.Logger
	options                MonitorOptions
	statusRevision         atomic.Int64
	mu                     sync.RWMutex
	workers                map[string]*monitorWorker
	shuttingDown           bool
	closed                 bool
	shutdownDone           chan struct{}
	shutdownErr            error
	operationMu            sync.Mutex
	roomOperations         map[string]*sync.Mutex
	inflightRoomOperations int
	roomOperationsDrain    chan struct{}
	// beforeSetMonitorEnabled is a package-private test barrier. Production
	// leaves it nil and tests assign it before starting concurrent operations.
	beforeSetMonitorEnabled func(string, bool)
	// afterStopIntent is a package-private test barrier invoked before the
	// worker context is cancelled. Production leaves it nil.
	afterStopIntent func()
}

func NewMonitorManager(root context.Context, service *Service, logger *slog.Logger, options MonitorOptions) (*MonitorManager, error) {
	if root == nil {
		return nil, errors.New("monitor manager root context is nil")
	}
	if service == nil {
		return nil, errors.New("monitor manager room service is nil")
	}
	if options.Coordinator == nil {
		return nil, errors.New("monitor manager capture coordinator is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if options.PollInterval <= 0 {
		options.PollInterval = 15 * time.Second
	}
	if options.ReconnectDelay <= 0 {
		options.ReconnectDelay = 2 * time.Second
	}
	if options.FinalizeTimeout <= 0 {
		options.FinalizeTimeout = 8 * time.Second
	}
	if options.MaxRooms <= 0 {
		options.MaxRooms = 8
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Publisher == nil {
		options.Publisher = func(RoomRuntimeStatus) {}
	}
	if options.Factory == nil {
		options.Factory = defaultLiveClientFactory(logger)
	}
	return &MonitorManager{
		root: root, service: service, logger: logger, options: options,
		workers:        make(map[string]*monitorWorker),
		shutdownDone:   make(chan struct{}),
		roomOperations: make(map[string]*sync.Mutex),
	}, nil
}

func defaultLiveClientFactory(logger *slog.Logger) LiveClientFactory {
	return func(ctx context.Context, config RoomConfig, cookie string) (LiveClient, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return douyinLive.NewDouyinLiveWithSlog(config.LiveID, logger, cookie)
	}
}

func (m *MonitorManager) StartEnabled(ctx context.Context) error {
	endOperation, err := m.beginRoomOperation()
	if err != nil {
		return err
	}
	defer endOperation()
	rooms, err := m.service.ListRooms(ctx)
	if err != nil {
		return err
	}
	if err := m.startLifecycleError(); err != nil {
		return err
	}
	for _, config := range rooms {
		if !config.MonitorEnabled {
			continue
		}
		operation := m.roomOperation(config.ID)
		operation.Lock()
		err := m.startConfigured(config)
		operation.Unlock()
		if err != nil {
			if ErrorCode(err) == "MONITOR_MANAGER_SHUTTING_DOWN" {
				return err
			}
			m.logger.WarnContext(ctx, "自动恢复房间监控失败",
				"component", "room", "error_code", ErrorCode(err), "room_config_id", config.ID)
		}
	}
	return nil
}

func (m *MonitorManager) StartMonitoring(ctx context.Context, id string) error {
	endOperation, err := m.beginRoomOperation()
	if err != nil {
		return err
	}
	defer endOperation()
	operation, err := m.lockStartOperation(id)
	if err != nil {
		return err
	}
	defer operation.Unlock()
	if err := m.startLifecycleError(); err != nil {
		return err
	}
	config, err := m.setMonitorEnabled(ctx, id, true)
	if err != nil {
		return err
	}
	if err := m.startConfigured(config); err != nil {
		_, _ = m.setMonitorEnabled(ctx, id, false)
		return err
	}
	return nil
}

func (m *MonitorManager) StopMonitoring(ctx context.Context, id string) error {
	endOperation, err := m.beginRoomOperation()
	if err != nil {
		return err
	}
	defer endOperation()
	operation := m.roomOperation(id)
	operation.Lock()
	defer operation.Unlock()
	if _, err := m.setMonitorEnabled(ctx, id, false); err != nil {
		return err
	}
	return m.detach(ctx, id, capture.FinalizeStopped)
}

func (m *MonitorManager) ReconcileRoom(ctx context.Context, id string) error {
	endOperation, err := m.beginRoomOperation()
	if err != nil {
		return err
	}
	defer endOperation()
	operation := m.roomOperation(id)
	operation.Lock()
	defer operation.Unlock()
	config, err := m.service.GetRoom(ctx, id)
	if err != nil {
		return err
	}
	if err := m.detach(ctx, id, capture.FinalizeStopped); err != nil {
		return err
	}
	if config.MonitorEnabled {
		return m.startConfigured(config)
	}
	return nil
}

func (m *MonitorManager) DetachRoom(ctx context.Context, id string) error {
	endOperation, err := m.beginRoomOperation()
	if err != nil {
		return err
	}
	defer endOperation()
	operation := m.roomOperation(id)
	operation.Lock()
	defer operation.Unlock()
	return m.detach(ctx, id, capture.FinalizeStopped)
}

func (m *MonitorManager) GetRoomStatus(ctx context.Context, id string) (RoomRuntimeStatus, error) {
	endOperation, err := m.beginRoomOperation()
	if err != nil {
		return RoomRuntimeStatus{}, err
	}
	defer endOperation()
	if ctx == nil {
		return RoomRuntimeStatus{}, errors.New("room status context is nil")
	}
	if err := ctx.Err(); err != nil {
		return RoomRuntimeStatus{}, err
	}
	m.mu.RLock()
	worker := m.workers[id]
	m.mu.RUnlock()
	if worker != nil {
		return worker.snapshot(), nil
	}
	config, err := m.service.GetRoom(ctx, id)
	if err != nil {
		return RoomRuntimeStatus{}, err
	}
	// Recheck under the registry lock after the storage read. If a worker is
	// created concurrently, either its revision is already visible here or its
	// initial revision will be allocated after this stopped snapshot. This keeps
	// response ordering correct even when an older query returns after an event.
	m.mu.RLock()
	worker = m.workers[id]
	if worker == nil {
		revision := m.nextStatusRevision()
		m.mu.RUnlock()
		return RoomRuntimeStatus{
			RoomID: config.ID, LiveID: config.LiveID, Alias: config.Alias,
			State: RuntimeStopped, ChangedAt: m.options.Now().UTC().UnixMilli(),
			Revision: revision, Message: "已停止监控",
		}, nil
	}
	m.mu.RUnlock()
	return worker.snapshot(), nil
}

func (m *MonitorManager) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("monitor shutdown context is nil")
	}
	m.mu.Lock()
	if m.closed {
		result := m.shutdownErr
		m.mu.Unlock()
		return result
	}
	if !m.shuttingDown {
		m.shuttingDown = true
		operationsDrain := m.roomOperationsDrain
		go m.runShutdown(operationsDrain)
	}
	done := m.shutdownDone
	m.mu.Unlock()
	select {
	case <-done:
		return m.shutdownResult()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ShutdownComplete reports whether the shared asynchronous shutdown has
// drained public room operations and completed all worker stop attempts.
func (m *MonitorManager) ShutdownComplete() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.closed
}

func (m *MonitorManager) runShutdown(operationsDrain <-chan struct{}) {
	if operationsDrain != nil {
		<-operationsDrain
	}

	var shutdownErrors []error
	for {
		m.mu.RLock()
		workers := make([]*monitorWorker, 0, len(m.workers))
		for _, worker := range m.workers {
			workers = append(workers, worker)
		}
		m.mu.RUnlock()
		if len(workers) == 0 {
			m.finishShutdown(errors.Join(shutdownErrors...))
			return
		}
		for _, worker := range workers {
			worker.stop(capture.FinalizeShutdown)
		}
		for _, worker := range workers {
			<-worker.done
			err := m.retryDoneFinalization(context.Background(), worker, capture.FinalizeShutdown)
			if !m.workerRegistered(worker) {
				shutdownErrors = append(shutdownErrors, err)
			}
		}
		m.mu.RLock()
		remaining := len(m.workers)
		m.mu.RUnlock()
		if remaining == 0 {
			m.finishShutdown(errors.Join(shutdownErrors...))
			return
		}
		timer := time.NewTimer(m.options.ReconnectDelay)
		<-timer.C
	}
}

func (m *MonitorManager) workerRegistered(worker *monitorWorker) bool {
	roomID := worker.snapshot().RoomID
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.workers[roomID] == worker
}

func (m *MonitorManager) finishShutdown(result error) {
	m.mu.Lock()
	m.shutdownErr = result
	m.closed = true
	close(m.shutdownDone)
	m.mu.Unlock()
}

func (m *MonitorManager) shutdownResult() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.shutdownErr
}

func (m *MonitorManager) startLifecycleError() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.shuttingDown || m.closed {
		return &BusinessError{Code: "MONITOR_MANAGER_SHUTTING_DOWN", Message: "监控管理器正在关闭，无法启动新任务"}
	}
	return nil
}

func (m *MonitorManager) roomOperation(id string) *sync.Mutex {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	operation := m.roomOperations[id]
	if operation == nil {
		operation = &sync.Mutex{}
		m.roomOperations[id] = operation
	}
	return operation
}

func (m *MonitorManager) beginRoomOperation() (func(), error) {
	m.mu.Lock()
	if m.shuttingDown || m.closed {
		m.mu.Unlock()
		return nil, &BusinessError{Code: "MONITOR_MANAGER_SHUTTING_DOWN", Message: "监控管理器正在关闭，无法执行房间操作"}
	}
	if m.inflightRoomOperations == 0 {
		m.roomOperationsDrain = make(chan struct{})
	}
	m.inflightRoomOperations++
	m.mu.Unlock()
	return m.endRoomOperation, nil
}

func (m *MonitorManager) endRoomOperation() {
	m.mu.Lock()
	if m.inflightRoomOperations <= 0 {
		m.mu.Unlock()
		panic("monitor manager room operation counter underflow")
	}
	m.inflightRoomOperations--
	if m.inflightRoomOperations == 0 {
		close(m.roomOperationsDrain)
		m.roomOperationsDrain = nil
	}
	m.mu.Unlock()
}

func (m *MonitorManager) setMonitorEnabled(ctx context.Context, id string, enabled bool) (RoomConfig, error) {
	if hook := m.beforeSetMonitorEnabled; hook != nil {
		hook(id, enabled)
	}
	return m.service.SetMonitorEnabled(ctx, id, enabled)
}

func (m *MonitorManager) lockStartOperation(id string) (*sync.Mutex, error) {
	operation := m.roomOperation(id)
	if operation.TryLock() {
		return operation, nil
	}
	m.mu.RLock()
	worker := m.workers[id]
	m.mu.RUnlock()
	if worker != nil {
		status := worker.snapshot()
		if worker.isStopping() || status.State == RuntimeFinalizing {
			return nil, &BusinessError{Code: "CAPTURE_FINALIZING", Message: "直播场次正在收尾，请稍后重试"}
		}
	}
	operation.Lock()
	return operation, nil
}

func (m *MonitorManager) startConfigured(config RoomConfig) error {
	m.mu.Lock()
	if m.shuttingDown || m.closed {
		m.mu.Unlock()
		return &BusinessError{Code: "MONITOR_MANAGER_SHUTTING_DOWN", Message: "监控管理器正在关闭，无法启动新任务"}
	}
	if existing := m.workers[config.ID]; existing != nil {
		status := existing.snapshot()
		stopping := existing.isStopping()
		m.mu.Unlock()
		if stopping || status.State == RuntimeFinalizing {
			return &BusinessError{Code: "CAPTURE_FINALIZING", Message: "直播场次正在收尾，请稍后重试"}
		}
		existing.wakeNow()
		return nil
	}
	if len(m.workers) >= m.options.MaxRooms {
		m.mu.Unlock()
		return &BusinessError{Code: "MONITOR_LIMIT_REACHED", Message: "已达到等待开播房间上限"}
	}
	workerContext, cancel := context.WithCancel(m.root)
	revision := m.nextStatusRevision()
	worker := &monitorWorker{
		manager: m, ctx: workerContext, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1),
		status: RoomRuntimeStatus{
			RoomID: config.ID, LiveID: config.LiveID, Alias: config.Alias,
			State: RuntimeWaiting, ChangedAt: m.options.Now().UTC().UnixMilli(),
			Revision: revision, Message: "等待开播",
		},
	}
	m.workers[config.ID] = worker
	m.mu.Unlock()
	worker.publish()
	go worker.run()
	return nil
}

func (m *MonitorManager) detach(ctx context.Context, id string, reason capture.FinalizeReason) error {
	if ctx == nil {
		return errors.New("monitor detach context is nil")
	}
	m.mu.RLock()
	worker := m.workers[id]
	m.mu.RUnlock()
	if worker == nil {
		return nil
	}
	worker.stop(reason)
	select {
	case <-worker.done:
		return m.retryDoneFinalization(ctx, worker, reason)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *MonitorManager) removeWorker(worker *monitorWorker) {
	roomID := worker.snapshot().RoomID
	m.mu.Lock()
	if m.workers[roomID] == worker {
		delete(m.workers, roomID)
	}
	m.mu.Unlock()
}

func (m *MonitorManager) retryDoneFinalization(ctx context.Context, worker *monitorWorker, reason capture.FinalizeReason) error {
	if !worker.doneClosed() || worker.sessionValue() == nil {
		return worker.result()
	}
	terminal, err := worker.finalizeActiveWithContext(ctx, reason)
	if !terminal {
		if err == nil {
			err = errCaptureFinalizeNonterminal
		}
		worker.addError(err)
		worker.markFinalizeFailed()
		return worker.result()
	}
	worker.setError(err)
	worker.transition(RuntimeStopped, worker.currentOperation(), "", "已停止监控", 0, 0, nil, nil, true)
	m.removeWorker(worker)
	return err
}

type monitorWorker struct {
	manager                 *MonitorManager
	ctx                     context.Context
	cancel                  context.CancelFunc
	done                    chan struct{}
	wake                    chan struct{}
	mu                      sync.RWMutex
	finalizeMu              sync.Mutex
	finalizeInProgress      bool
	status                  RoomRuntimeStatus
	active                  LiveClient
	session                 capture.Session
	recoveryWatchCancel     context.CancelFunc
	recoveryWatchGeneration uint64
	stopRequested           bool
	stopReason              capture.FinalizeReason
	stopOperation           string
	offlineConfirmations    int
	runErr                  error
}

func (w *monitorWorker) run() {
	defer func() {
		defer close(w.done)
		w.closeActive()
		terminal := true
		if w.sessionValue() != nil {
			reason := capture.FinalizeShutdown
			if requestedReason := w.requestedStopReason(); requestedReason != "" {
				reason = requestedReason
			}
			var finalizeErr error
			terminal, finalizeErr = w.finalizeActive(reason)
			if terminal {
				w.setError(finalizeErr)
			} else {
				w.addError(finalizeErr)
			}
		}
		if !terminal {
			if w.result() == nil {
				w.addError(errCaptureFinalizeNonterminal)
			}
			w.markFinalizeFailed()
			return
		}
		w.transition(RuntimeStopped, w.currentOperation(), "", "已停止监控", 0, 0, nil, nil, true)
		w.manager.removeWorker(w)
	}()

	delay := time.Duration(0)
	automatic := true
	for {
		if !w.wait(delay, automatic) {
			return
		}
		automatic = true
		config, err := w.manager.service.GetRoom(w.ctx, w.snapshot().RoomID)
		if err != nil {
			w.transition(RuntimeError, "", "ROOM_NOT_FOUND", "直播间配置不存在", 0, 0, nil, nil, false)
			delay, automatic = 0, false
			continue
		}
		w.refreshConfig(config)

		cookie, err := w.manager.service.LoadRoomCookie(w.ctx, config.ID)
		if err != nil {
			w.transition(RuntimeError, "", "COOKIE_INVALID", "Cookie 无法读取，请重新配置", 0, 0, nil, nil, false)
			delay, automatic = 0, false
			continue
		}
		operationState, operationMessage := RuntimeStarting, "正在检查直播状态"
		if w.sessionValue() != nil {
			operationState, operationMessage = RuntimeReconnecting, "正在重连直播场次"
		}
		operationID := w.beginOperation(operationState, operationMessage)
		client, err := w.manager.options.Factory(w.ctx, config, cookie)
		cookie = ""
		if err != nil {
			if w.ctx.Err() != nil {
				return
			}
			state := RuntimeWaiting
			delay = w.manager.options.PollInterval
			if w.sessionValue() != nil {
				state = RuntimeReconnecting
				delay = w.manager.options.ReconnectDelay
			}
			w.transitionForOperation(operationID, state, "ROOM_CHECK_FAILED", "直播状态检查失败，将自动重试", w.nowMillis(), w.retryAt(delay), nil, nil)
			continue
		}
		w.setActive(client)
		prepareErr := client.PrepareWebSocketContext()
		checkedAt := w.nowMillis()
		if w.ctx.Err() != nil {
			return
		}
		offline := client.IsKnownOfflineStatus() || errors.Is(prepareErr, douyinLive.ErrLiveNotStarted)
		if prepareErr != nil || offline {
			w.releaseActive(client)
			if errors.Is(prepareErr, douyinLive.ErrRoomNotFound) && w.sessionValue() != nil {
				terminal, finalizeErr := w.finalizeActiveWithRetry(capture.FinalizeFailure)
				if w.ctx.Err() != nil {
					return
				}
				if terminal {
					w.transition(RuntimeError, w.currentOperation(), "ROOM_NOT_FOUND", "直播间不存在，场次已结束", checkedAt, 0, client, nil, true)
				} else {
					w.addError(finalizeErr)
				}
				delay, automatic = 0, false
				continue
			}
			if offline && w.sessionValue() != nil {
				if w.recordOfflineConfirmation() >= 2 {
					terminal, finalizeErr := w.finalizeActiveWithRetry(capture.FinalizeOffline)
					if w.ctx.Err() != nil {
						return
					}
					if terminal {
						w.transition(RuntimeWaiting, w.currentOperation(), "ROOM_OFFLINE", "直播间已下播，继续等待", checkedAt, w.retryAt(w.manager.options.PollInterval), client, nil, true)
					} else {
						w.addError(finalizeErr)
						delay, automatic = 0, false
						continue
					}
				} else {
					w.transitionForOperation(operationID, RuntimeReconnecting, "ROOM_OFFLINE_CONFIRMING", "检测到离线，等待再次确认", checkedAt, w.retryAt(w.manager.options.PollInterval), client, nil)
				}
				delay = w.manager.options.PollInterval
				continue
			}
			switch {
			case errors.Is(prepareErr, douyinLive.ErrRoomNotFound):
				w.transitionForOperation(operationID, RuntimeError, "ROOM_NOT_FOUND", "直播间不存在或标识无效", checkedAt, 0, client, nil)
				delay, automatic = 0, false
			case offline:
				w.transitionForOperation(operationID, RuntimeWaiting, "ROOM_OFFLINE", "直播间未开播，继续等待", checkedAt, w.retryAt(w.manager.options.PollInterval), client, nil)
				delay = w.manager.options.PollInterval
			default:
				state := RuntimeWaiting
				delay = w.manager.options.PollInterval
				if w.sessionValue() != nil {
					state = RuntimeReconnecting
					delay = w.manager.options.ReconnectDelay
				}
				w.transitionForOperation(operationID, state, "ROOM_CHECK_FAILED", "直播状态检查失败，将自动重试", checkedAt, w.retryAt(delay), client, nil)
			}
			continue
		}

		w.resetOfflineConfirmations()
		session := w.sessionValue()
		var sessionSnapshot capture.LiveSession
		if session == nil {
			opened, openErr := w.manager.options.Coordinator.Open(w.ctx, capture.OpenRequest{
				RoomConfigID: config.ID, OperationID: operationID, Title: client.GetTitle(),
				PlatformRoomID: config.RoomID,
				RecordEnabled:  config.RecordEnabled, StartedAt: w.manager.options.Now(),
				Profile: capture.RecordingProfile{
					Quality: string(config.RecordingProfile.Quality), SegmentMinutes: config.RecordingProfile.SegmentMinutes,
					SaveDirectory: config.RecordingProfile.SaveDirectory,
				},
			}, client)
			if openErr != nil {
				w.releaseActive(client)
				if w.ctx.Err() != nil {
					return
				}
				w.transitionForOperation(operationID, RuntimeError, "CAPTURE_OPEN_FAILED", "无法创建直播场次，请重试", checkedAt, 0, client, nil)
				delay, automatic = 0, false
				continue
			}
			if !w.installSession(operationID, opened) {
				w.releaseActive(client)
				w.finalizeDetached(opened, capture.FinalizeShutdown)
				return
			}
			session = opened
			sessionSnapshot = opened.Snapshot()
		} else {
			rebound, rebindErr := session.Rebind(w.ctx, operationID, client)
			if rebindErr != nil {
				w.releaseActive(client)
				if w.ctx.Err() != nil {
					return
				}
				w.transitionForOperation(operationID, RuntimeReconnecting, "CAPTURE_REBIND_FAILED", "场次重连失败，将继续重试", checkedAt, w.retryAt(w.manager.options.ReconnectDelay), client, nil)
				delay = w.manager.options.ReconnectDelay
				continue
			}
			sessionSnapshot = rebound
		}

		state, message := runtimeForRecording(sessionSnapshot.RecordingStatus)
		if !w.transitionForOperation(operationID, state, "", message, checkedAt, 0, client, &sessionSnapshot) {
			w.releaseActive(client)
			return
		}
		startErr := client.Start()
		w.releaseActive(client)
		if w.ctx.Err() != nil {
			return
		}
		if startErr != nil {
			w.manager.logger.WarnContext(w.ctx, "直播监听结束，准备重新检查",
				"component", "room", "error_code", "ROOM_CONNECTION_INTERRUPTED", "room_config_id", config.ID, "err", startErr)
		}
		if client.IsKnownOfflineStatus() {
			if w.recordOfflineConfirmation() >= 2 {
				terminal, finalizeErr := w.finalizeActiveWithRetry(capture.FinalizeOffline)
				if w.ctx.Err() != nil {
					return
				}
				if terminal {
					w.transition(RuntimeWaiting, w.currentOperation(), "ROOM_OFFLINE", "直播间已下播，继续等待", w.nowMillis(), w.retryAt(w.manager.options.PollInterval), client, nil, true)
				} else {
					w.addError(finalizeErr)
					delay, automatic = 0, false
					continue
				}
			} else {
				w.transitionForOperation(operationID, RuntimeReconnecting, "ROOM_OFFLINE_CONFIRMING", "检测到离线，等待再次确认", w.nowMillis(), w.retryAt(w.manager.options.PollInterval), client, nil)
			}
			delay = w.manager.options.PollInterval
			continue
		}
		w.transitionForOperation(operationID, RuntimeReconnecting, "ROOM_CONNECTION_INTERRUPTED", "连接已结束，正在重新检查", w.nowMillis(), w.retryAt(w.manager.options.ReconnectDelay), client, nil)
		delay = w.manager.options.ReconnectDelay
	}
}

func runtimeForRecording(status capture.RecordingStatus) (RuntimeState, string) {
	if status == capture.RecordingActive {
		return RuntimeRecording, "直播录制中"
	}
	return RuntimeLive, "直播中"
}

func (w *monitorWorker) finalizeActive(reason capture.FinalizeReason) (bool, error) {
	return w.finalizeActiveWithContext(context.Background(), reason)
}

func (w *monitorWorker) finalizeActiveWithRetry(reason capture.FinalizeReason) (bool, error) {
	var finalizeErrors, pendingErr error
	ordinaryAttempts := 0
	for {
		terminal, err := w.finalizeActiveWithContext(w.ctx, reason)
		if terminal {
			return true, err
		}
		if err == nil {
			err = errCaptureFinalizeNonterminal
		}
		pending := errors.Is(err, capture.ErrCaptureCleanupPending)
		if pending {
			// A recorder proxy/DB cleanup already has an owner. Keep polling its
			// shared completion without consuming the ordinary retry budget.
			pendingErr = err
		} else {
			finalizeErrors = errors.Join(finalizeErrors, err)
			ordinaryAttempts++
			if ordinaryAttempts == maxAutomaticFinalizeAttempts {
				w.markFinalizeFailed()
				return false, finalizeErrors
			}
		}
		delayMultiplier := ordinaryAttempts
		if pending || delayMultiplier == 0 {
			delayMultiplier = 1
		}
		delay := time.Duration(delayMultiplier) * w.manager.options.ReconnectDelay
		w.markFinalizeRetry(delay)
		timer := time.NewTimer(delay)
		select {
		case <-w.ctx.Done():
			timer.Stop()
			return false, errors.Join(finalizeErrors, pendingErr, w.ctx.Err())
		case <-timer.C:
		}
	}
}

func (w *monitorWorker) finalizeActiveWithContext(parent context.Context, reason capture.FinalizeReason) (bool, error) {
	w.finalizeMu.Lock()
	defer w.finalizeMu.Unlock()
	w.mu.Lock()
	session := w.session
	if session == nil {
		w.mu.Unlock()
		return true, nil
	}
	operationID := newOperationID()
	effectiveReason := reason
	if w.stopRequested {
		effectiveReason = w.stopReason
	}
	if w.stopOperation != "" {
		operationID = w.stopOperation
		w.stopOperation = ""
	}
	w.finalizeInProgress = true
	w.mu.Unlock()
	snapshot := session.Snapshot()
	snapshot.RecordingStatus = capture.RecordingFinalizing
	w.transition(RuntimeFinalizing, operationID, "", "正在收尾直播场次", w.nowMillis(), 0, nil, &snapshot, false)
	ctx, cancel := context.WithTimeout(parent, w.manager.options.FinalizeTimeout)
	finalized, err := session.Finalize(ctx, operationID, effectiveReason)
	cancel()
	terminal := terminalSession(finalized.Status)
	w.mu.Lock()
	w.finalizeInProgress = false
	if terminal {
		if w.session == session {
			w.session = nil
			w.cancelRecoveryWatchLocked()
		}
		// A stop intent that linearized after this terminal finalization no
		// longer needs a capture retry; the terminal operation already won.
		w.stopOperation = ""
		w.status.SessionID = finalized.ID
		w.status.RecordingStatus = finalized.RecordingStatus
		w.bumpStatusRevisionLocked()
	}
	w.mu.Unlock()
	return terminal, err
}

func (w *monitorWorker) markFinalizeFailed() {
	session := w.sessionValue()
	if session == nil {
		return
	}
	snapshot := session.Snapshot()
	snapshot.RecordingStatus = capture.RecordingFinalizing
	w.transition(RuntimeFinalizing, w.currentOperation(), "CAPTURE_FINALIZE_FAILED", "场次收尾失败，需要恢复", w.nowMillis(), 0, nil, &snapshot, false)
}

func (w *monitorWorker) markFinalizeRetry(delay time.Duration) {
	session := w.sessionValue()
	if session == nil {
		return
	}
	snapshot := session.Snapshot()
	snapshot.RecordingStatus = capture.RecordingFinalizing
	w.transition(RuntimeFinalizing, w.currentOperation(), "CAPTURE_FINALIZE_FAILED", "场次收尾失败，将自动重试", w.nowMillis(), w.retryAt(delay), nil, &snapshot, false)
}

func (w *monitorWorker) finalizeDetached(session capture.Session, reason capture.FinalizeReason) {
	ordinaryAttempts := 0
	for session != nil {
		ctx, cancel := context.WithTimeout(context.Background(), w.manager.options.FinalizeTimeout)
		finalized, err := session.Finalize(ctx, newOperationID(), reason)
		cancel()
		if terminalSession(finalized.Status) {
			if err != nil {
				w.addError(err)
			}
			return
		}
		pending := errors.Is(err, capture.ErrCaptureCleanupPending)
		if !pending {
			ordinaryAttempts++
			if ordinaryAttempts >= maxAutomaticFinalizeAttempts {
				if err == nil {
					err = errCaptureFinalizeNonterminal
				}
				w.addError(err)
				return
			}
		}
		// The detached session itself remains the owner. Do not observe w.ctx:
		// installSession loss commonly means that context is already cancelled.
		timer := time.NewTimer(w.manager.options.ReconnectDelay)
		<-timer.C
	}
}

func terminalSession(status capture.SessionStatus) bool {
	return status == capture.SessionCompleted || status == capture.SessionInterrupted || status == capture.SessionFailed
}

func (w *monitorWorker) wait(delay time.Duration, automatic bool) bool {
	if !automatic {
		select {
		case <-w.ctx.Done():
			return false
		case <-w.wake:
			return true
		}
	}
	if delay <= 0 {
		select {
		case <-w.ctx.Done():
			return false
		case <-w.wake:
		default:
		}
		return true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-w.ctx.Done():
		return false
	case <-w.wake:
		return true
	case <-timer.C:
		return true
	}
}

func (w *monitorWorker) beginOperation(state RuntimeState, message string) string {
	operationID := newOperationID()
	w.transition(state, operationID, "", message, 0, 0, nil, nil, false)
	return operationID
}

func (w *monitorWorker) transitionForOperation(operationID string, state RuntimeState, errorCode, message string, checkedAt, retryAt int64, client LiveClient, session *capture.LiveSession) bool {
	w.mu.Lock()
	if w.status.OperationID != operationID || w.stopRequested {
		w.mu.Unlock()
		return false
	}
	status := w.updateStatusLocked(state, operationID, errorCode, message, checkedAt, retryAt, client, session, false)
	w.mu.Unlock()
	w.manager.publish(status)
	return true
}

func (w *monitorWorker) transition(state RuntimeState, operationID, errorCode, message string, checkedAt, retryAt int64, client LiveClient, session *capture.LiveSession, clearSession bool) {
	w.mu.Lock()
	status := w.updateStatusLocked(state, operationID, errorCode, message, checkedAt, retryAt, client, session, clearSession)
	w.mu.Unlock()
	w.manager.publish(status)
}

func (w *monitorWorker) updateStatusLocked(state RuntimeState, operationID, errorCode, message string, checkedAt, retryAt int64, client LiveClient, session *capture.LiveSession, clearSession bool) RoomRuntimeStatus {
	w.status.State = state
	if operationID != "" {
		w.status.OperationID = operationID
	}
	w.status.ErrorCode = errorCode
	w.status.Message = message
	w.status.RetryAt = retryAt
	w.status.ChangedAt = w.nowMillis()
	w.bumpStatusRevisionLocked()
	if checkedAt != 0 {
		w.status.LastCheckedAt = checkedAt
	}
	if client != nil {
		if value := client.GetName(); value != "" {
			w.status.LiveName = value
		}
		if value := client.GetTitle(); value != "" {
			w.status.Title = value
		}
	}
	if clearSession {
		w.status.SessionID = ""
		w.status.RecordingStatus = ""
	} else if session != nil {
		w.status.SessionID = session.ID
		w.status.RecordingStatus = session.RecordingStatus
	}
	return w.status
}

func (w *monitorWorker) installSession(operationID string, session capture.Session) bool {
	snapshot := session.Snapshot()
	w.mu.Lock()
	if w.stopRequested || w.status.OperationID != operationID {
		w.mu.Unlock()
		return false
	}
	w.cancelRecoveryWatchLocked()
	w.recoveryWatchGeneration++
	generation := w.recoveryWatchGeneration
	w.session = session
	w.status.SessionID = snapshot.ID
	w.status.RecordingStatus = snapshot.RecordingStatus
	w.bumpStatusRevisionLocked()
	eventSource, watchesRecovery := session.(capture.SessionRecoveryEventSource)
	var watchCtx context.Context
	if watchesRecovery {
		watchCtx, w.recoveryWatchCancel = context.WithCancel(w.ctx)
	}
	w.mu.Unlock()
	if watchesRecovery {
		go w.watchSessionRecovery(watchCtx, generation, session, snapshot.ID, eventSource)
	}
	return true
}

func (w *monitorWorker) cancelRecoveryWatchLocked() {
	if w.recoveryWatchCancel != nil {
		w.recoveryWatchCancel()
		w.recoveryWatchCancel = nil
	}
}

func (w *monitorWorker) watchSessionRecovery(
	ctx context.Context,
	generation uint64,
	session capture.Session,
	sessionID string,
	source capture.SessionRecoveryEventSource,
) {
	events := source.RecoveryEvents()
	if events == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case event, open := <-events:
			if !open {
				return
			}
			w.applySessionRecoveryEvent(generation, session, sessionID, event)
		}
	}
}

func (w *monitorWorker) applySessionRecoveryEvent(
	generation uint64,
	session capture.Session,
	sessionID string,
	event capture.SessionRecoveryEvent,
) {
	if event.SessionID != sessionID || event.OperationID == "" {
		return
	}
	sessionSnapshot := session.Snapshot()
	if sessionSnapshot.ID != sessionID ||
		sessionSnapshot.OperationID != event.OperationID {
		return
	}
	w.mu.Lock()
	if generation != w.recoveryWatchGeneration || w.session != session ||
		w.finalizeInProgress || w.status.State == RuntimeFinalizing ||
		w.status.SessionID != sessionID ||
		w.status.OperationID != event.OperationID {
		w.mu.Unlock()
		return
	}
	errorCode := safeSessionRecoveryErrorCode(event.ErrorCode)
	switch event.State {
	case capture.SessionRecoveryRetryScheduled:
		w.status.State = RuntimeReconnecting
		w.status.RecordingStatus = capture.RecordingReconnecting
		w.status.ErrorCode = errorCode
		w.status.Message = "录制中断，正在自动重试"
		w.status.RetryAt = event.RetryAt
	case capture.SessionRecoveryRecovered:
		w.status.State = RuntimeRecording
		w.status.RecordingStatus = capture.RecordingActive
		w.status.ErrorCode = ""
		w.status.Message = "录制已恢复"
		w.status.RetryAt = 0
	case capture.SessionRecoveryExhausted:
		w.status.State = RuntimeLive
		w.status.RecordingStatus = capture.RecordingUnavailable
		w.status.ErrorCode = errorCode
		w.status.Message = "录制重试已耗尽，直播消息仍在采集"
		w.status.RetryAt = 0
	default:
		w.mu.Unlock()
		return
	}
	w.status.ChangedAt = w.nowMillis()
	w.bumpStatusRevisionLocked()
	status := w.status
	w.mu.Unlock()
	if event.State == capture.SessionRecoveryRecovered {
		w.manager.logger.Info("录制异常恢复完成",
			"component", "room", "error_code", "",
			"room_config_id", status.RoomID, "session_id", status.SessionID)
	} else {
		w.manager.logger.Warn("录制异常恢复状态变化",
			"component", "room", "error_code", status.ErrorCode,
			"room_config_id", status.RoomID, "session_id", status.SessionID)
	}
	w.manager.publish(status)
}

func safeSessionRecoveryErrorCode(code string) string {
	switch code {
	case capture.RecorderProcessExitedErrorCode,
		capture.RecorderStreamExpiredErrorCode,
		capture.RecorderNetworkFailureErrorCode,
		capture.RecorderUnsupportedInputErrorCode,
		capture.RecorderLocalResourceErrorCode,
		capture.RecorderDependencyFailureErrorCode,
		capture.RecorderRecoveryRetryExhaustedErrorCode,
		capture.RecorderRecoveryPersistenceErrorCode:
		return code
	default:
		return capture.RecorderProcessExitedErrorCode
	}
}

func (w *monitorWorker) refreshConfig(config RoomConfig) {
	w.mu.Lock()
	w.status.LiveID = config.LiveID
	w.status.Alias = config.Alias
	w.bumpStatusRevisionLocked()
	w.mu.Unlock()
}

func (w *monitorWorker) snapshot() RoomRuntimeStatus {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.status
}

func (w *monitorWorker) publish() { w.manager.publish(w.snapshot()) }

func (m *MonitorManager) nextStatusRevision() int64 {
	if m == nil {
		return 0
	}
	return m.statusRevision.Add(1)
}

func (w *monitorWorker) bumpStatusRevisionLocked() {
	w.status.Revision = w.manager.nextStatusRevision()
}

func (m *MonitorManager) publish(status RoomRuntimeStatus) {
	defer func() {
		if recovered := recover(); recovered != nil {
			m.logger.Error("发布房间状态事件时发生 panic",
				"component", "room", "error_code", "ROOM_STATUS_PUBLISH_FAILED", "room_config_id", status.RoomID)
		}
	}()
	m.options.Publisher(status)
}

func (w *monitorWorker) setActive(client LiveClient) {
	w.mu.Lock()
	w.active = client
	w.mu.Unlock()
}

func (w *monitorWorker) releaseActive(client LiveClient) {
	w.mu.Lock()
	if w.active == client {
		w.active = nil
	}
	w.mu.Unlock()
	client.Dispose()
}

func (w *monitorWorker) closeActive() {
	w.mu.Lock()
	client := w.active
	w.active = nil
	w.mu.Unlock()
	if client != nil {
		client.Close()
		client.Dispose()
	}
}

func (w *monitorWorker) stop(reason capture.FinalizeReason) {
	w.mu.Lock()
	statusChanged := false
	intentCreated := false
	if !w.stopRequested {
		intentCreated = true
		w.stopRequested = true
		w.stopReason = reason
		w.stopOperation = newOperationID()
		if !w.finalizeInProgress {
			w.status.OperationID = w.stopOperation
			w.bumpStatusRevisionLocked()
			if w.session != nil {
				w.status.State = RuntimeFinalizing
				w.status.RecordingStatus = capture.RecordingFinalizing
				w.status.Message = "正在收尾直播场次"
				w.status.ErrorCode = ""
				w.status.RetryAt = 0
				w.status.ChangedAt = w.nowMillis()
				statusChanged = true
			}
		}
	}
	client := w.active
	status := w.status
	w.mu.Unlock()
	if statusChanged {
		w.manager.publish(status)
	}
	if intentCreated {
		if hook := w.manager.afterStopIntent; hook != nil {
			hook()
		}
	}
	w.cancel()
	if client != nil {
		client.Close()
	}
	w.wakeNow()
}

func (w *monitorWorker) wakeNow() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *monitorWorker) sessionValue() capture.Session {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.session
}

func (w *monitorWorker) currentOperation() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.status.OperationID
}

func (w *monitorWorker) doneClosed() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

func (w *monitorWorker) requestedStopReason() capture.FinalizeReason {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.stopReason
}

func (w *monitorWorker) isStopping() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.stopRequested
}

func (w *monitorWorker) recordOfflineConfirmation() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.offlineConfirmations++
	return w.offlineConfirmations
}

func (w *monitorWorker) resetOfflineConfirmations() {
	w.mu.Lock()
	w.offlineConfirmations = 0
	w.mu.Unlock()
}

func (w *monitorWorker) addError(err error) {
	if err == nil {
		return
	}
	w.mu.Lock()
	w.runErr = errors.Join(w.runErr, err)
	w.mu.Unlock()
}

func (w *monitorWorker) setError(err error) {
	w.mu.Lock()
	w.runErr = err
	w.mu.Unlock()
}

func (w *monitorWorker) result() error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.runErr
}

func (w *monitorWorker) retryAt(delay time.Duration) int64 {
	return w.manager.options.Now().Add(delay).UTC().UnixMilli()
}

func (w *monitorWorker) nowMillis() int64 { return w.manager.options.Now().UTC().UnixMilli() }

func newOperationID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return ""
	}
	return id.String()
}

func (s RoomRuntimeStatus) String() string {
	return fmt.Sprintf("RoomRuntimeStatus{room=%s state=%s code=%s}", s.RoomID, s.State, s.ErrorCode)
}
