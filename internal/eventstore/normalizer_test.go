package eventstore

import (
	"strings"
	"testing"
	"time"

	"github.com/jwwsjlm/douyinlive-proto/generated/new_douyin"
	"google.golang.org/protobuf/proto"
)

func testNormalizer(t *testing.T, storeDisplayName bool) (*Normalizer, *PrivacyFilter) {
	t.Helper()
	privacy, err := NewPrivacyFilter([]byte("0123456789abcdef0123456789abcdef"), PrivacyOptions{
		StoreDisplayName:    storeDisplayName,
		MaxDisplayNameBytes: 64,
		MaxContentBytes:     16,
	})
	if err != nil {
		t.Fatal(err)
	}
	normalizer, err := NewNormalizer(privacy, "test-v1")
	if err != nil {
		t.Fatal(err)
	}
	return normalizer, privacy
}

func protoPayload(t *testing.T, message proto.Message) []byte {
	t.Helper()
	payload, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func baseEnvelope(method string, payload []byte, at time.Time) IngestEnvelope {
	return IngestEnvelope{
		EventID:         "018f0000-0000-7000-8000-000000000001",
		SessionID:       "session",
		Sequence:        1,
		Method:          method,
		PlatformRoomID:  "room",
		ReceivedAt:      at,
		SessionOffsetMS: 1234,
		Payload:         payload,
	}
}

func TestNormalizerCoreMethodsAndPrivacyAllowlist(t *testing.T) {
	normalizer, privacy := testNormalizer(t, true)
	at := time.Unix(1_800_000_000, 0).UTC()
	user := &new_douyin.Webcast_Data_User{
		Id:         42,
		IdStr:      "raw-id-str",
		SecUid:     "raw-sec-uid",
		WebcastUid: "raw-webcast-uid",
		Nickname:   "Viewer",
	}

	chat := &new_douyin.Webcast_Im_ChatMessage{
		Common:  &new_douyin.Webcast_Im_Common{MsgId: 101, CreateTime: uint64(at.Unix())},
		User:    user,
		Content: "0123456789abcdefghijk",
	}
	chatEvent := normalizer.Normalize(baseEnvelope(methodChat, protoPayload(t, chat), at))
	if chatEvent.Kind != EventChat || chatEvent.ParseStatus != ParseParsed || chatEvent.ID == "" {
		t.Fatalf("chat event = %#v", chatEvent)
	}
	if chatEvent.Content != "0123456789abcdef" {
		t.Fatalf("limited chat content = %q", chatEvent.Content)
	}
	if chatEvent.UserHash != privacy.HashIdentity("douyin:user", Identity{WebcastUID: user.WebcastUid}) {
		t.Fatalf("chat user hash = %q", chatEvent.UserHash)
	}
	if chatEvent.DisplayName != "Viewer" || chatEvent.PlatformMessageID != "101" || chatEvent.MessageCreateAt == nil {
		t.Fatalf("chat scalar mapping = %#v", chatEvent)
	}

	gift := &new_douyin.Webcast_Im_GiftMessage{
		Common:      &new_douyin.Webcast_Im_Common{MsgId: 102, CreateTime: uint64(at.UnixMilli())},
		GiftId:      7,
		RepeatCount: 4,
		GroupId:     999,
		TraceId:     "trace-sensitive",
		LogId:       "log-sensitive",
		User:        user,
		Gift: &new_douyin.Webcast_Data_GiftStruct{
			Name:         "Rose",
			Combo:        true,
			DiamondCount: 9,
			SchemeUrl:    "https://url-sensitive.invalid/path",
		},
	}
	giftResult := normalizer.NormalizeDetailed(baseEnvelope(methodGift, protoPayload(t, gift), at))
	if giftResult.Event.Kind != EventGift || giftResult.Gift == nil {
		t.Fatalf("gift result = %#v", giftResult)
	}
	if giftResult.Event.NumericValue == nil || *giftResult.Event.NumericValue != 4 {
		t.Fatalf("gift source numeric value must be count: %#v", giftResult.Event.NumericValue)
	}
	if giftResult.Gift.UnitDiamond != 9 || giftResult.Gift.GroupID != 999 || giftResult.Gift.TraceID != "trace-sensitive" {
		t.Fatalf("gift transient observation = %#v", giftResult.Gift)
	}
	for _, forbidden := range []string{
		"raw-id-str", "raw-sec-uid", "raw-webcast-uid",
		"trace-sensitive", "log-sensitive", "url-sensitive",
		"diamond",
	} {
		if strings.Contains(giftResult.Event.NormalizedJSON, forbidden) {
			t.Fatalf("gift allowlist leaked %q: %s", forbidden, giftResult.Event.NormalizedJSON)
		}
	}

	like := &new_douyin.Webcast_Im_LikeMessage{
		Common: &new_douyin.Webcast_Im_Common{MsgId: 103},
		User:   user,
		Count:  3,
		Total:  30,
	}
	likeEvent := normalizer.Normalize(baseEnvelope(methodLike, protoPayload(t, like), at))
	if likeEvent.Kind != EventLike || likeEvent.NumericValue == nil || *likeEvent.NumericValue != 3 {
		t.Fatalf("like event = %#v", likeEvent)
	}

	member := &new_douyin.Webcast_Im_MemberMessage{
		Common:      &new_douyin.Webcast_Im_Common{MsgId: 104},
		User:        user,
		UserOpenId:  "raw-open-id",
		Action:      1,
		EnterType:   2,
		MemberCount: 12,
	}
	memberEvent := normalizer.Normalize(baseEnvelope(methodMember, protoPayload(t, member), at))
	expectedMemberHash := privacy.HashIdentity("douyin:user", Identity{OpenID: "raw-open-id"})
	if memberEvent.Kind != EventMember || memberEvent.UserHash != expectedMemberHash {
		t.Fatalf("member event = %#v", memberEvent)
	}
	if strings.Contains(memberEvent.NormalizedJSON, "raw-open-id") {
		t.Fatalf("member JSON leaked user identity: %s", memberEvent.NormalizedJSON)
	}

	social := &new_douyin.Webcast_Im_SocialMessage{
		Common:          &new_douyin.Webcast_Im_Common{MsgId: 105},
		User:            user,
		Action:          1,
		ShareType:       2,
		ShareTarget:     "https://share-target.invalid/secret",
		FollowCount:     5,
		ShareTotalCount: 9,
	}
	socialEvent := normalizer.Normalize(baseEnvelope(methodSocial, protoPayload(t, social), at))
	if socialEvent.Kind != EventSystem {
		t.Fatalf("unverified social action classified as %q", socialEvent.Kind)
	}
	if strings.Contains(socialEvent.NormalizedJSON, "share-target") {
		t.Fatalf("social JSON leaked target URL: %s", socialEvent.NormalizedJSON)
	}
}

func TestNormalizerKnownUnknownFailedAndEmpty(t *testing.T) {
	normalizer, _ := testNormalizer(t, false)
	at := time.Unix(1_800_000_000, 0).UTC()
	control := &new_douyin.Webcast_Im_ControlMessage{
		Common: &new_douyin.Webcast_Im_Common{MsgId: 1},
		Action: 3,
		Tips:   "not allowlisted",
	}
	known := normalizer.Normalize(baseEnvelope("WebcastControlMessage", protoPayload(t, control), at))
	if known.Kind != EventSystem || known.ParseStatus != ParseParsed || known.NormalizedJSON != "{\"known\":true}" {
		t.Fatalf("known system event = %#v", known)
	}

	unknown := normalizer.Normalize(baseEnvelope("WebcastDefinitelyUnknownMessage", []byte{1}, at))
	if unknown.ParseStatus != ParseUnknown || unknown.ParseErrorCode != "EVENT_METHOD_UNKNOWN" {
		t.Fatalf("unknown event = %#v", unknown)
	}
	failed := normalizer.Normalize(baseEnvelope(methodChat, []byte{0xff}, at))
	if failed.ParseStatus != ParseFailed || failed.ParseErrorCode != "EVENT_PROTO_DECODE_FAILED" {
		t.Fatalf("failed event = %#v", failed)
	}
	empty := normalizer.Normalize(baseEnvelope(methodChat, nil, at))
	if empty.ParseStatus != ParseFailed || empty.ParseErrorCode != "EVENT_PAYLOAD_EMPTY" {
		t.Fatalf("empty event = %#v", empty)
	}
	missingMethod := normalizer.Normalize(baseEnvelope("", []byte{1}, at))
	if missingMethod.ParseErrorCode != "EVENT_METHOD_MISSING" {
		t.Fatalf("missing-method event = %#v", missingMethod)
	}
}

func TestNormalizerCopiesPooledScalars(t *testing.T) {
	normalizer, _ := testNormalizer(t, true)
	at := time.Unix(1_800_000_000, 0).UTC()
	firstMessage := &new_douyin.Webcast_Im_ChatMessage{
		Common:  &new_douyin.Webcast_Im_Common{MsgId: 1},
		User:    &new_douyin.Webcast_Data_User{WebcastUid: "first-user", Nickname: "First"},
		Content: "first-content",
	}
	first := normalizer.Normalize(baseEnvelope(methodChat, protoPayload(t, firstMessage), at))
	secondMessage := &new_douyin.Webcast_Im_ChatMessage{
		Common:  &new_douyin.Webcast_Im_Common{MsgId: 2},
		User:    &new_douyin.Webcast_Data_User{WebcastUid: "second-user", Nickname: "Second"},
		Content: "second-content",
	}
	_ = normalizer.Normalize(baseEnvelope(methodChat, protoPayload(t, secondMessage), at))
	if first.Content != "first-content" || first.DisplayName != "First" || first.PlatformMessageID != "1" {
		t.Fatalf("pooled message mutated prior event: %#v", first)
	}
}

func TestNormalizerSelectsSecondsMillisecondsAndMicroseconds(t *testing.T) {
	normalizer, _ := testNormalizer(t, false)
	received := time.Unix(1_800_000_000, 123_456_000).UTC()
	tests := []struct {
		name string
		raw  uint64
		want time.Time
	}{
		{name: "seconds", raw: uint64(received.Unix()), want: time.Unix(received.Unix(), 0).UTC()},
		{name: "milliseconds", raw: uint64(received.UnixMilli()), want: time.UnixMilli(received.UnixMilli()).UTC()},
		{name: "microseconds", raw: uint64(received.UnixMicro()), want: time.UnixMicro(received.UnixMicro()).UTC()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message := &new_douyin.Webcast_Im_LikeMessage{
				Common: &new_douyin.Webcast_Im_Common{CreateTime: test.raw},
			}
			event := normalizer.Normalize(baseEnvelope(methodLike, protoPayload(t, message), received))
			if event.MessageCreateAt == nil || !event.MessageCreateAt.Equal(test.want) || event.ClockConfidence < 0.5 {
				t.Fatalf("time = %v confidence=%v, want %v", event.MessageCreateAt, event.ClockConfidence, test.want)
			}
		})
	}

	far := &new_douyin.Webcast_Im_LikeMessage{
		Common: &new_douyin.Webcast_Im_Common{CreateTime: uint64(received.Add(-11 * time.Minute).UnixMilli())},
	}
	event := normalizer.Normalize(baseEnvelope(methodLike, protoPayload(t, far), received))
	if event.MessageCreateAt != nil || event.ClockConfidence != 0 {
		t.Fatalf("untrusted time accepted: %#v", event)
	}
}
