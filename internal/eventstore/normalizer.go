package eventstore

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/jwwsjlm/douyinlive-proto/generated"
	"github.com/jwwsjlm/douyinlive-proto/generated/new_douyin"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const (
	DefaultNormalizerVersion = "event-normalizer/v1"
	maxTrustedClockSkew      = 10 * time.Minute

	methodChat   = "WebcastChatMessage"
	methodGift   = "WebcastGiftMessage"
	methodLike   = "WebcastLikeMessage"
	methodMember = "WebcastMemberMessage"
	methodSocial = "WebcastSocialMessage"
)

type Normalizer struct {
	privacy *PrivacyFilter
	version string
}

type NormalizedResult struct {
	Event Event
	Gift  *GiftObservation
}

func NewNormalizer(privacy *PrivacyFilter, version string) (*Normalizer, error) {
	if privacy == nil || len(privacy.key) == 0 {
		return nil, ErrPrivacyKeyMissing
	}
	version = strings.TrimSpace(version)
	if version == "" {
		version = DefaultNormalizerVersion
	}
	return &Normalizer{privacy: privacy, version: version}, nil
}

// Normalize returns the standard source row. Use NormalizeDetailed when gift
// combo folding also needs the transient, non-persistable observation.
func (n *Normalizer) Normalize(envelope IngestEnvelope) Event {
	return n.NormalizeDetailed(envelope).Event
}

// NormalizeDetailed reparses the owned payload by method and never consults a
// LiveMessage.Parsed pointer. All protobuf-derived scalars are copied before the
// pooled message is returned.
func (n *Normalizer) NormalizeDetailed(envelope IngestEnvelope) NormalizedResult {
	payload := append([]byte(nil), envelope.Payload...)
	event := Event{
		ID:                strings.Clone(envelope.EventID),
		SessionID:         strings.Clone(envelope.SessionID),
		IngestSequence:    envelope.Sequence,
		Role:              EventRoleSource,
		Method:            strings.Clone(envelope.Method),
		Kind:              EventUnknown,
		ReceivedAt:        envelope.ReceivedAt,
		SessionOffsetMS:   envelope.SessionOffsetMS,
		NormalizedJSON:    "{}",
		ParseStatus:       ParseFailed,
		NormalizerVersion: n.version,
	}
	if event.ID == "" {
		event.ID = newEventID()
	}
	if strings.TrimSpace(envelope.Method) == "" {
		event.ParseErrorCode = "EVENT_METHOD_MISSING"
		event.DedupeKey = BuildDedupeKey(envelopeWithPayload(envelope, payload), "", nil)
		return NormalizedResult{Event: event}
	}

	message, err := generated.GetMessageInstance(envelope.Method)
	if err != nil {
		event.ParseStatus = ParseUnknown
		event.ParseErrorCode = "EVENT_METHOD_UNKNOWN"
		event.DedupeKey = BuildDedupeKey(envelopeWithPayload(envelope, payload), "", nil)
		return NormalizedResult{Event: event}
	}
	defer generated.PutMessageInstance(envelope.Method, message)

	if len(payload) == 0 {
		event.ParseErrorCode = "EVENT_PAYLOAD_EMPTY"
		event.DedupeKey = BuildDedupeKey(envelopeWithPayload(envelope, payload), "", nil)
		return NormalizedResult{Event: event}
	}
	if err := proto.Unmarshal(payload, message); err != nil {
		event.ParseErrorCode = "EVENT_PROTO_DECODE_FAILED"
		event.DedupeKey = BuildDedupeKey(envelopeWithPayload(envelope, payload), "", nil)
		return NormalizedResult{Event: event}
	}

	var (
		common         *new_douyin.Webcast_Im_Common
		user           *new_douyin.Webcast_Data_User
		openIDOverride string
		timeValues     []uint64
		gift           *GiftObservation
	)
	if carrier, ok := message.(interface {
		GetCommon() *new_douyin.Webcast_Im_Common
	}); ok {
		common = carrier.GetCommon()
		if common != nil {
			if common.GetMsgId() != 0 {
				event.PlatformMessageID = strconv.FormatUint(common.GetMsgId(), 10)
			}
			timeValues = append(timeValues, common.GetCreateTime())
			user = common.GetUser()
		}
	}

	switch decoded := message.(type) {
	case *new_douyin.Webcast_Im_ChatMessage:
		event.Kind = EventChat
		user = firstUser(decoded.GetUser(), user)
		event.Content = n.privacy.Content(strings.Clone(decoded.GetContent()))
		timeValues = append(timeValues, decoded.GetEventTime())
		event.NormalizedJSON = mustAllowlistJSON(struct {
			TextBytes int `json:"text_bytes"`
		}{TextBytes: len(event.Content)})
	case *new_douyin.Webcast_Im_GiftMessage:
		event.Kind = EventGift
		user = firstUser(decoded.GetUser(), user)
		count := maxPositive(
			decoded.GetRepeatCount(),
			decoded.GetComboCount(),
			decoded.GetGroupCount(),
			decoded.GetTotalCount(),
			decoded.GetCount(),
		)
		giftDefinition := decoded.GetGift()
		giftID := decoded.GetGiftId()
		giftName := ""
		combo := false
		unitDiamond := int64(0)
		if giftDefinition != nil {
			if giftID == 0 {
				giftID = giftDefinition.GetId()
			}
			giftName = n.privacy.ShortText(strings.Clone(giftDefinition.GetName()), 256)
			combo = giftDefinition.GetCombo()
			if giftDefinition.GetDiamondCount() > 0 {
				unitDiamond = int64(giftDefinition.GetDiamondCount())
			}
		}
		giftIDText := ""
		if giftID != 0 {
			giftIDText = strconv.FormatUint(giftID, 10)
		}
		event.Content = giftName
		event.NumericValue = floatPointer(float64(count))
		timeValues = append(timeValues, decoded.GetSendTime(), decoded.GetEffectDisplayTs())
		gift = &GiftObservation{
			SessionID:         strings.Clone(envelope.SessionID),
			SourceEventID:     event.ID,
			Sequence:          envelope.Sequence,
			SessionOffsetMS:   envelope.SessionOffsetMS,
			ReceivedAt:        envelope.ReceivedAt,
			GiftID:            giftIDText,
			GiftName:          giftName,
			GroupID:           decoded.GetGroupId(),
			TraceID:           strings.Clone(decoded.GetTraceId()),
			Count:             count,
			UnitDiamond:       unitDiamond,
			Combo:             combo,
			RepeatEnd:         decoded.GetRepeatEnd() != 0,
			NormalizerVersion: n.version,
		}
	case *new_douyin.Webcast_Im_LikeMessage:
		event.Kind = EventLike
		user = firstUser(decoded.GetUser(), user)
		event.NumericValue = floatPointer(float64(decoded.GetCount()))
		event.NormalizedJSON = mustAllowlistJSON(struct {
			Count uint64 `json:"count"`
			Total uint64 `json:"total"`
		}{Count: decoded.GetCount(), Total: decoded.GetTotal()})
	case *new_douyin.Webcast_Im_MemberMessage:
		event.Kind = EventMember
		user = firstUser(decoded.GetUser(), user)
		openIDOverride = strings.Clone(decoded.GetUserOpenId())
		event.NumericValue = floatPointer(float64(decoded.GetMemberCount()))
		event.NormalizedJSON = mustAllowlistJSON(struct {
			Action      uint64 `json:"action"`
			EnterType   uint64 `json:"enter_type"`
			MemberCount uint64 `json:"member_count"`
		}{
			Action:      decoded.GetAction(),
			EnterType:   decoded.GetEnterType(),
			MemberCount: decoded.GetMemberCount(),
		})
	case *new_douyin.Webcast_Im_SocialMessage:
		// No repository fixture proves that action=1 means follow. Until such
		// evidence exists, social actions stay system events.
		event.Kind = EventSystem
		user = firstUser(decoded.GetUser(), user)
		event.NormalizedJSON = mustAllowlistJSON(struct {
			Action          uint64 `json:"action"`
			ShareType       uint64 `json:"share_type"`
			FollowCount     uint64 `json:"follow_count"`
			ShareTotalCount uint64 `json:"share_total_count"`
		}{
			Action:          decoded.GetAction(),
			ShareType:       decoded.GetShareType(),
			FollowCount:     decoded.GetFollowCount(),
			ShareTotalCount: decoded.GetShareTotalCount(),
		})
	default:
		// The generated registry recognized and decoded the method. Its full
		// protobuf representation is deliberately not serialized.
		event.Kind = EventSystem
		event.NormalizedJSON = "{\"known\":true}"
	}

	identity, nickname := copyUserScalars(user, openIDOverride)
	event.UserHash = n.privacy.HashIdentity("douyin:user", identity)
	event.DisplayName = n.privacy.DisplayName(nickname)
	if gift != nil {
		gift.UserHash = event.UserHash
		gift.DisplayName = event.DisplayName
		comboKey, err := GiftComboKey(*gift)
		event.NormalizedJSON = mustAllowlistJSON(struct {
			GiftID    string `json:"gift_id,omitempty"`
			GiftName  string `json:"gift_name,omitempty"`
			Count     int64  `json:"count"`
			Combo     bool   `json:"combo"`
			RepeatEnd bool   `json:"repeat_end"`
			ComboKey  string `json:"combo_key,omitempty"`
		}{
			GiftID:    gift.GiftID,
			GiftName:  gift.GiftName,
			Count:     gift.Count,
			Combo:     gift.Combo,
			RepeatEnd: gift.RepeatEnd,
			ComboKey:  comboKeyIfValid(comboKey, err),
		})
	}

	event.MessageCreateAt, event.ClockConfidence = selectMessageTime(envelope.ReceivedAt, timeValues...)
	event.ParseStatus = ParseParsed
	event.ParseErrorCode = ""
	event.DedupeKey = BuildDedupeKey(envelopeWithPayload(envelope, payload), event.PlatformMessageID, event.MessageCreateAt)
	return NormalizedResult{Event: event, Gift: gift}
}

func envelopeWithPayload(envelope IngestEnvelope, payload []byte) IngestEnvelope {
	envelope.Payload = payload
	return envelope
}

func firstUser(primary, fallback *new_douyin.Webcast_Data_User) *new_douyin.Webcast_Data_User {
	if primary != nil {
		return primary
	}
	return fallback
}

func copyUserScalars(user *new_douyin.Webcast_Data_User, openIDOverride string) (Identity, string) {
	if user == nil {
		return Identity{OpenID: strings.Clone(openIDOverride)}, ""
	}
	openID := strings.Clone(openIDOverride)
	if openID == "" {
		openID = copyProtoStringField(user.ProtoReflect(), "open_id", "user_open_id")
	}
	return Identity{
		OpenID:     openID,
		WebcastUID: strings.Clone(user.GetWebcastUid()),
		SecUID:     strings.Clone(user.GetSecUid()),
		IDStr:      strings.Clone(user.GetIdStr()),
		ID:         user.GetId(),
	}, strings.Clone(user.GetNickname())
}

func copyProtoStringField(message protoreflect.Message, names ...protoreflect.Name) string {
	fields := message.Descriptor().Fields()
	for _, name := range names {
		field := fields.ByName(name)
		if field == nil || field.Kind() != protoreflect.StringKind || !message.Has(field) {
			continue
		}
		if value := message.Get(field).String(); value != "" {
			return strings.Clone(value)
		}
	}
	return ""
}

func maxPositive(values ...uint64) int64 {
	var max uint64
	for _, value := range values {
		if value > max {
			max = value
		}
	}
	if max == 0 {
		return 1
	}
	const maxInt64 = uint64(1<<63 - 1)
	if max > maxInt64 {
		return int64(maxInt64)
	}
	return int64(max)
}

func selectMessageTime(receivedAt time.Time, values ...uint64) (*time.Time, float64) {
	if receivedAt.IsZero() {
		return nil, 0
	}
	var (
		best      time.Time
		bestDelta = time.Duration(1<<63 - 1)
	)
	const maxInt64 = uint64(1<<63 - 1)
	for _, raw := range values {
		if raw == 0 || raw > maxInt64 {
			continue
		}
		value := int64(raw)
		candidates := [...]time.Time{
			time.Unix(value, 0).UTC(),
			time.UnixMilli(value).UTC(),
			time.UnixMicro(value).UTC(),
		}
		for _, candidate := range candidates {
			delta := candidate.Sub(receivedAt)
			if candidate.Before(receivedAt) {
				// Sub saturates to MaxInt64 here rather than overflowing while
				// negating MinInt64 for adversarial timestamps.
				delta = receivedAt.Sub(candidate)
			}
			if delta < bestDelta {
				best = candidate
				bestDelta = delta
			}
		}
	}
	if best.IsZero() || bestDelta > maxTrustedClockSkew {
		return nil, 0
	}
	confidence := 1 - 0.5*(float64(bestDelta)/float64(maxTrustedClockSkew))
	if confidence < 0.5 {
		confidence = 0.5
	}
	return timePointer(best), confidence
}

func mustAllowlistJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func comboKeyIfValid(key string, err error) string {
	if err != nil {
		return ""
	}
	return key
}
