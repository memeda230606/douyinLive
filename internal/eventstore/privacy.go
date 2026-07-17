package eventstore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

const (
	defaultDisplayNameLimit = 256
	defaultContentLimit     = 4096
)

var ErrPrivacyKeyMissing = errors.New("EVENT_PRIVACY_KEY_MISSING")

// Identity contains the supported platform identifiers in priority order.
// Values are transient and must never be persisted before HashIdentity returns.
type Identity struct {
	OpenID     string
	WebcastUID string
	SecUID     string
	IDStr      string
	ID         uint64
}

// PrivacyOptions controls optional personal-data retention and text limits.
type PrivacyOptions struct {
	StoreDisplayName    bool
	MaxDisplayNameBytes int
	MaxContentBytes     int
}

// PrivacyFilter converts transient platform identity and text into the only
// representations allowed in normalized storage.
type PrivacyFilter struct {
	key                 []byte
	keyID               string
	storeDisplayName    atomic.Bool
	maxDisplayNameBytes int
	maxContentBytes     int
}

func NewPrivacyFilter(key []byte, options PrivacyOptions) (*PrivacyFilter, error) {
	if len(key) == 0 {
		return nil, ErrPrivacyKeyMissing
	}
	keyCopy := append([]byte(nil), key...)
	digest := sha256.Sum256(keyCopy)
	maxDisplayName := options.MaxDisplayNameBytes
	if maxDisplayName <= 0 {
		maxDisplayName = defaultDisplayNameLimit
	}
	maxContent := options.MaxContentBytes
	if maxContent <= 0 {
		maxContent = defaultContentLimit
	}
	filter := &PrivacyFilter{
		key:                 keyCopy,
		keyID:               hex.EncodeToString(digest[:8]),
		maxDisplayNameBytes: maxDisplayName,
		maxContentBytes:     maxContent,
	}
	filter.storeDisplayName.Store(options.StoreDisplayName)
	return filter, nil
}

// KeyID identifies the installation HMAC key without exposing key material.
// It is safe to persist in checkpoints for fail-closed recovery validation.
func (f *PrivacyFilter) KeyID() string {
	if f == nil {
		return ""
	}
	return f.keyID
}

// SetStoreDisplayName atomically updates whether future normalized events may
// retain display names. Events already persisted are never rewritten.
func (f *PrivacyFilter) SetStoreDisplayName(enabled bool) {
	if f != nil {
		f.storeDisplayName.Store(enabled)
	}
}

// HashIdentity returns an installation-keyed identifier. The source name and
// caller namespace are authenticated too, preventing cross-domain correlation.
func (f *PrivacyFilter) HashIdentity(namespace string, identity Identity) string {
	if f == nil || len(f.key) == 0 {
		return ""
	}
	source, value := preferredIdentity(identity)
	if value == "" {
		return ""
	}
	mac := hmac.New(sha256.New, f.key)
	mac.Write([]byte("douyin-live\x00"))
	mac.Write([]byte(namespace))
	mac.Write([]byte{0})
	mac.Write([]byte(source))
	mac.Write([]byte{0})
	mac.Write([]byte(value))
	return "h1." + f.keyID + "." + hex.EncodeToString(mac.Sum(nil))
}

func preferredIdentity(identity Identity) (string, string) {
	if value := strings.TrimSpace(identity.OpenID); value != "" {
		return "open_id", value
	}
	if value := strings.TrimSpace(identity.WebcastUID); value != "" {
		return "webcast_uid", value
	}
	if value := strings.TrimSpace(identity.SecUID); value != "" {
		return "sec_uid", value
	}
	if value := strings.TrimSpace(identity.IDStr); value != "" {
		return "id_str", value
	}
	if identity.ID != 0 {
		return "id", strconv.FormatUint(identity.ID, 10)
	}
	return "", ""
}

func (f *PrivacyFilter) DisplayName(value string) string {
	if f == nil || !f.storeDisplayName.Load() {
		return ""
	}
	return validLimitedUTF8(value, f.maxDisplayNameBytes)
}

func (f *PrivacyFilter) Content(value string) string {
	if f == nil {
		return validLimitedUTF8(value, defaultContentLimit)
	}
	return validLimitedUTF8(value, f.maxContentBytes)
}

func (f *PrivacyFilter) ShortText(value string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = defaultDisplayNameLimit
		if f != nil {
			maxBytes = f.maxDisplayNameBytes
		}
	}
	return validLimitedUTF8(value, maxBytes)
}

func validLimitedUTF8(value string, maxBytes int) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
