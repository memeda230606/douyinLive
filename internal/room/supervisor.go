package room

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	douyinLive "github.com/jwwsjlm/douyinLive/v2"
)

const StatusEventName = "room:status"

type RuntimeState string

const (
	RuntimeStopped      RuntimeState = "STOPPED"
	RuntimeWaiting      RuntimeState = "WAITING"
	RuntimeStarting     RuntimeState = "STARTING"
	RuntimeLive         RuntimeState = "LIVE"
	RuntimeReconnecting RuntimeState = "RECONNECTING"
	RuntimeError        RuntimeState = "ERROR"
)

type RoomRuntimeStatus struct {
	RoomID        string       `json:"roomId"`
	LiveID        string       `json:"liveId"`
	Alias         string       `json:"alias"`
	State         RuntimeState `json:"state"`
	OperationID   string       `json:"operationId,omitempty"`
	LiveName      string       `json:"liveName,omitempty"`
	Title         string       `json:"title,omitempty"`
	LastCheckedAt int64        `json:"lastCheckedAt,omitempty"`
	ChangedAt     int64        `json:"changedAt"`
	RetryAt       int64        `json:"retryAt,omitempty"`
	ErrorCode     string       `json:"errorCode,omitempty"`
	Message       string       `json:"message"`
}

// LiveClient is the minimum existing-core surface required by the desktop
// room supervisor. Tests replace it with an offline in-memory implementation.
type LiveClient interface {
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
	PollInterval   time.Duration
	ReconnectDelay time.Duration
	MaxRooms       int
	Now            func() time.Time
	Factory        LiveClientFactory
	Publisher      StatusPublisher
}

type MonitorManager struct {
	root    context.Context
	service *Service
	logger  *slog.Logger
	options MonitorOptions
	mu      sync.RWMutex
	workers map[string]*monitorWorker
}

func NewMonitorManager(root context.Context, service *Service, logger *slog.Logger, options MonitorOptions) (*MonitorManager, error) {
	if root == nil {
		return nil, errors.New("monitor manager root context is nil")
	}
	if service == nil {
		return nil, errors.New("monitor manager room service is nil")
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
		workers: make(map[string]*monitorWorker),
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

// StartEnabled resumes persisted monitoring preferences without rewriting the
// database. A single invalid room must not prevent the desktop from starting.
func (m *MonitorManager) StartEnabled(ctx context.Context) error {
	rooms, err := m.service.ListRooms(ctx)
	if err != nil {
		return err
	}
	for _, config := range rooms {
		if !config.MonitorEnabled {
			continue
		}
		if err := m.startConfigured(config); err != nil {
			m.logger.WarnContext(ctx, "自动恢复房间监控失败",
				"component", "room", "error_code", ErrorCode(err), "room_config_id", config.ID)
		}
	}
	return nil
}

func (m *MonitorManager) StartMonitoring(ctx context.Context, id string) error {
	config, err := m.service.SetMonitorEnabled(ctx, id, true)
	if err != nil {
		return err
	}
	if err := m.startConfigured(config); err != nil {
		_, _ = m.service.SetMonitorEnabled(ctx, id, false)
		return err
	}
	return nil
}

func (m *MonitorManager) StopMonitoring(ctx context.Context, id string) error {
	if _, err := m.service.SetMonitorEnabled(ctx, id, false); err != nil {
		return err
	}
	return m.detach(ctx, id)
}

// ReconcileRoom restarts a worker after the persisted room configuration has
// changed. It preserves the already-saved monitor_enabled preference.
func (m *MonitorManager) ReconcileRoom(ctx context.Context, id string) error {
	config, err := m.service.GetRoom(ctx, id)
	if err != nil {
		return err
	}
	if err := m.detach(ctx, id); err != nil {
		return err
	}
	if config.MonitorEnabled {
		return m.startConfigured(config)
	}
	return nil
}

// DetachRoom stops a worker without changing persistence. It is used around
// update/delete transactions and during application shutdown.
func (m *MonitorManager) DetachRoom(ctx context.Context, id string) error {
	return m.detach(ctx, id)
}

func (m *MonitorManager) GetRoomStatus(ctx context.Context, id string) (RoomRuntimeStatus, error) {
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
	return RoomRuntimeStatus{
		RoomID: config.ID, LiveID: config.LiveID, Alias: config.Alias,
		State: RuntimeStopped, ChangedAt: m.options.Now().UTC().UnixMilli(), Message: "已停止监控",
	}, nil
}

func (m *MonitorManager) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("monitor shutdown context is nil")
	}
	m.mu.Lock()
	workers := make([]*monitorWorker, 0, len(m.workers))
	for _, worker := range m.workers {
		workers = append(workers, worker)
	}
	m.workers = make(map[string]*monitorWorker)
	m.mu.Unlock()
	for _, worker := range workers {
		worker.stop()
	}
	for _, worker := range workers {
		select {
		case <-worker.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (m *MonitorManager) startConfigured(config RoomConfig) error {
	m.mu.Lock()
	if existing := m.workers[config.ID]; existing != nil {
		m.mu.Unlock()
		existing.wakeNow()
		return nil
	}
	if len(m.workers) >= m.options.MaxRooms {
		m.mu.Unlock()
		return &BusinessError{Code: "MONITOR_LIMIT_REACHED", Message: "已达到等待开播房间上限"}
	}
	workerContext, cancel := context.WithCancel(m.root)
	worker := &monitorWorker{
		manager: m, ctx: workerContext, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1),
		status: RoomRuntimeStatus{
			RoomID: config.ID, LiveID: config.LiveID, Alias: config.Alias,
			State: RuntimeWaiting, ChangedAt: m.options.Now().UTC().UnixMilli(), Message: "等待开播",
		},
	}
	m.workers[config.ID] = worker
	m.mu.Unlock()
	worker.publish()
	go worker.run()
	return nil
}

func (m *MonitorManager) detach(ctx context.Context, id string) error {
	if ctx == nil {
		return errors.New("monitor detach context is nil")
	}
	m.mu.Lock()
	worker := m.workers[id]
	delete(m.workers, id)
	m.mu.Unlock()
	if worker == nil {
		return nil
	}
	worker.stop()
	select {
	case <-worker.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type monitorWorker struct {
	manager *MonitorManager
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	wake    chan struct{}
	mu      sync.RWMutex
	status  RoomRuntimeStatus
	active  LiveClient
}

func (w *monitorWorker) run() {
	defer close(w.done)
	defer func() {
		w.closeActive()
		w.transition(RuntimeStopped, "", "", "已停止监控", 0, 0, nil)
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
			w.transition(RuntimeError, "", "ROOM_NOT_FOUND", "直播间配置不存在", 0, 0, nil)
			delay, automatic = 0, false
			continue
		}
		w.refreshConfig(config)

		cookie, err := w.manager.service.LoadRoomCookie(w.ctx, config.ID)
		if err != nil {
			w.transition(RuntimeError, "", "COOKIE_INVALID", "Cookie 无法读取，请重新配置", 0, 0, nil)
			delay, automatic = 0, false
			continue
		}
		operationID := newOperationID()
		w.transition(RuntimeStarting, operationID, "", "正在检查直播状态", 0, 0, nil)

		client, err := w.manager.options.Factory(w.ctx, config, cookie)
		cookie = ""
		if err != nil {
			retryAt := w.manager.options.Now().Add(w.manager.options.PollInterval).UTC().UnixMilli()
			w.transition(RuntimeWaiting, operationID, "ROOM_CHECK_FAILED", "直播状态检查失败，将自动重试", w.nowMillis(), retryAt, nil)
			delay = w.manager.options.PollInterval
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
			switch {
			case errors.Is(prepareErr, douyinLive.ErrRoomNotFound):
				w.transition(RuntimeError, operationID, "ROOM_NOT_FOUND", "直播间不存在或标识无效", checkedAt, 0, client)
				delay, automatic = 0, false
			case offline:
				retryAt := w.manager.options.Now().Add(w.manager.options.PollInterval).UTC().UnixMilli()
				w.transition(RuntimeWaiting, operationID, "ROOM_OFFLINE", "直播间未开播，继续等待", checkedAt, retryAt, client)
				delay = w.manager.options.PollInterval
			default:
				retryAt := w.manager.options.Now().Add(w.manager.options.PollInterval).UTC().UnixMilli()
				w.transition(RuntimeWaiting, operationID, "ROOM_CHECK_FAILED", "直播状态检查失败，将自动重试", checkedAt, retryAt, client)
				delay = w.manager.options.PollInterval
			}
			continue
		}

		w.transition(RuntimeLive, operationID, "", "直播中", checkedAt, 0, client)
		startErr := client.Start()
		w.releaseActive(client)
		if w.ctx.Err() != nil {
			return
		}
		if startErr != nil {
			w.manager.logger.WarnContext(w.ctx, "直播监听结束，准备重新检查",
				"component", "room", "error_code", "ROOM_CONNECTION_INTERRUPTED", "room_config_id", config.ID, "err", startErr)
		}
		retryAt := w.manager.options.Now().Add(w.manager.options.ReconnectDelay).UTC().UnixMilli()
		w.transition(RuntimeReconnecting, operationID, "ROOM_CONNECTION_INTERRUPTED", "连接已结束，正在重新检查", w.nowMillis(), retryAt, client)
		delay = w.manager.options.ReconnectDelay
	}
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

func (w *monitorWorker) transition(state RuntimeState, operationID, errorCode, message string, checkedAt, retryAt int64, client LiveClient) {
	w.mu.Lock()
	w.status.State = state
	w.status.OperationID = operationID
	w.status.ErrorCode = errorCode
	w.status.Message = message
	w.status.RetryAt = retryAt
	w.status.ChangedAt = w.nowMillis()
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
	status := w.status
	w.mu.Unlock()
	w.manager.publish(status)
}

func (w *monitorWorker) refreshConfig(config RoomConfig) {
	w.mu.Lock()
	w.status.LiveID = config.LiveID
	w.status.Alias = config.Alias
	w.mu.Unlock()
}

func (w *monitorWorker) snapshot() RoomRuntimeStatus {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.status
}

func (w *monitorWorker) publish() {
	w.manager.publish(w.snapshot())
}

func (m *MonitorManager) publish(status RoomRuntimeStatus) {
	defer func() {
		if recovered := recover(); recovered != nil {
			m.logger.Error("发布房间状态事件时发生 panic",
				"component", "room",
				"error_code", "ROOM_STATUS_PUBLISH_FAILED",
				"room_config_id", status.RoomID,
			)
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

func (w *monitorWorker) stop() {
	w.cancel()
	w.mu.RLock()
	client := w.active
	w.mu.RUnlock()
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

func (w *monitorWorker) nowMillis() int64 {
	return w.manager.options.Now().UTC().UnixMilli()
}

func newOperationID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}

func (s RoomRuntimeStatus) String() string {
	return fmt.Sprintf("RoomRuntimeStatus{room=%s state=%s code=%s}", s.RoomID, s.State, s.ErrorCode)
}
