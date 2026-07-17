package eventstore

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	douyinLive "github.com/jwwsjlm/douyinLive/v2"
	"github.com/jwwsjlm/douyinLive/v2/internal/credentials"
)

const (
	EventPrivacyCredentialRef = "eventstore.hmac.v1"
	DefaultEventBatchSize     = 250
	MaxEventBatchSize         = 250
	DefaultEventBatchInterval = 500 * time.Millisecond
	DefaultBusyRetryWindow    = 5 * time.Second
	DefaultBusyRetryInitial   = 50 * time.Millisecond
)

var (
	ErrManagerOptions       = errors.New("EVENT_MANAGER_OPTIONS_INVALID")
	ErrManagerClosed        = errors.New("EVENT_MANAGER_CLOSED")
	ErrSessionAlreadyOpen   = errors.New("EVENT_SESSION_ALREADY_OPEN")
	ErrSessionClosed        = errors.New("EVENT_SESSION_CLOSED")
	ErrSessionPathInvalid   = errors.New("EVENT_SESSION_PATH_INVALID")
	ErrPrivacyKeyInvalid    = errors.New("EVENT_PRIVACY_KEY_INVALID")
	ErrPrivacyKeyMismatch   = errors.New("EVENT_PRIVACY_KEY_MISMATCH")
	ErrPersistenceDegraded  = errors.New("EVENT_PERSISTENCE_DEGRADED")
	ErrEventSpoolFatal      = errors.New("EVENT_SPOOL_FATAL")
	ErrEventManagerNotReady = errors.New("EVENT_MANAGER_NOT_READY")
)

type ManagerOptions struct {
	DataRoot         string
	Writer           *Writer
	Credentials      credentials.Store
	Logger           *slog.Logger
	Now              func() time.Time
	QueueLimits      QueueLimits
	BatchSize        int
	BatchInterval    time.Duration
	BusyRetryWindow  time.Duration
	BusyRetryInitial time.Duration
	SpoolOptions     func(root string) SpoolOptions
	PrivacyOptions   PrivacyOptions
}

type SessionDescriptor struct {
	SessionID      string
	DataPath       string
	PlatformRoomID string
	StartedAt      time.Time
}

type Manager struct {
	options ManagerOptions
	privacy *PrivacyFilter

	mu           sync.Mutex
	accepting    bool
	sessions     map[string]*SessionSink
	shutdownOnce sync.Once
	shutdownDone chan struct{}
	shutdownErr  error
}

type SessionSink struct {
	manager    *Manager
	descriptor SessionDescriptor
	eventsRoot string
	runtime    *sessionRuntime
}

var privacyProvisionMu sync.Mutex

func NewManager(ctx context.Context, options ManagerOptions) (*Manager, error) {
	if ctx == nil || options.Writer == nil || options.Credentials == nil {
		return nil, ErrManagerOptions
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := filepath.Abs(filepath.Clean(options.DataRoot))
	if err != nil || options.DataRoot == "" {
		return nil, ErrManagerOptions
	}
	options.DataRoot = root
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.QueueLimits == (QueueLimits{}) {
		options.QueueLimits = DefaultQueueLimits()
	}
	if options.BatchSize <= 0 || options.BatchSize > MaxEventBatchSize {
		options.BatchSize = DefaultEventBatchSize
	}
	if options.BatchInterval <= 0 {
		options.BatchInterval = DefaultEventBatchInterval
	}
	if options.BusyRetryWindow <= 0 {
		options.BusyRetryWindow = DefaultBusyRetryWindow
	}
	if options.BusyRetryInitial <= 0 {
		options.BusyRetryInitial = DefaultBusyRetryInitial
	}
	if options.SpoolOptions == nil {
		options.SpoolOptions = DefaultSpoolOptions
	}
	key, err := loadOrCreatePrivacyKey(ctx, options.Credentials)
	if err != nil {
		return nil, err
	}
	privacy, err := NewPrivacyFilter(key, options.PrivacyOptions)
	clear(key)
	if err != nil {
		return nil, ErrPrivacyKeyInvalid
	}
	manager := &Manager{
		options:      options,
		privacy:      privacy,
		accepting:    true,
		sessions:     make(map[string]*SessionSink),
		shutdownDone: make(chan struct{}),
	}
	return manager, nil
}

func loadOrCreatePrivacyKey(ctx context.Context, store credentials.Store) ([]byte, error) {
	privacyProvisionMu.Lock()
	defer privacyProvisionMu.Unlock()
	key, err := store.Get(ctx, EventPrivacyCredentialRef)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, ErrPrivacyKeyInvalid
		}
		if _, err := store.Put(ctx, EventPrivacyCredentialRef, key); err != nil {
			clear(key)
			return nil, ErrPrivacyKeyInvalid
		}
	} else if err != nil {
		return nil, ErrPrivacyKeyInvalid
	}
	if len(key) != 32 {
		clear(key)
		return nil, ErrPrivacyKeyInvalid
	}
	return append([]byte(nil), key...), nil
}

func (m *Manager) OpenSession(ctx context.Context, descriptor SessionDescriptor) (*SessionSink, error) {
	return m.openSession(ctx, descriptor)
}

func (m *Manager) RecoverSession(ctx context.Context, descriptor SessionDescriptor) error {
	return m.recoverSession(ctx, descriptor)
}

// SetStoreDisplayName atomically updates the privacy policy used by all open
// and future sessions. Existing persisted rows are intentionally unchanged.
func (m *Manager) SetStoreDisplayName(enabled bool) {
	if m != nil && m.privacy != nil {
		m.privacy.SetStoreDisplayName(enabled)
	}
}

func (m *Manager) Shutdown(ctx context.Context) error {
	return m.shutdown(ctx)
}

func (s *SessionSink) Accept(message *douyinLive.LiveMessage) {
	if s != nil && s.runtime != nil {
		s.runtime.accept(message)
	}
}

func (s *SessionSink) FlushAndClose(ctx context.Context) error {
	if s == nil || s.runtime == nil {
		return nil
	}
	return s.runtime.close(ctx)
}
