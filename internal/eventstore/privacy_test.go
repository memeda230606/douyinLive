package eventstore

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPrivacyFilterIdentityPriorityAndNamespace(t *testing.T) {
	filter, err := NewPrivacyFilter([]byte("0123456789abcdef0123456789abcdef"), PrivacyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	all := Identity{OpenID: "open", WebcastUID: "webcast", SecUID: "sec", IDStr: "id-str", ID: 42}
	got := filter.HashIdentity("viewer", all)
	parts := strings.Split(got, ".")
	if len(parts) != 3 || parts[0] != "h1" || len(parts[1]) != 16 || len(parts[2]) != 64 {
		t.Fatalf("unexpected hash format %q", got)
	}
	if filter.KeyID() != parts[1] {
		t.Fatalf("KeyID() = %q, hash key id = %q", filter.KeyID(), parts[1])
	}
	if got != filter.HashIdentity("viewer", Identity{OpenID: "open"}) {
		t.Fatal("openId must have highest priority")
	}
	if got == filter.HashIdentity("anchor", Identity{OpenID: "open"}) {
		t.Fatal("namespace must separate identical source identifiers")
	}
	if filter.HashIdentity("viewer", Identity{}) != "" {
		t.Fatal("missing identity must stay empty")
	}
}

func TestPrivacyFilterDisplayNameAndUTF8Limits(t *testing.T) {
	disabled, err := NewPrivacyFilter([]byte("key"), PrivacyOptions{MaxContentBytes: 5})
	if err != nil {
		t.Fatal(err)
	}
	if disabled.DisplayName("private") != "" {
		t.Fatal("display name should be disabled by default")
	}

	enabled, err := NewPrivacyFilter([]byte("key"), PrivacyOptions{
		StoreDisplayName:    true,
		MaxDisplayNameBytes: 5,
		MaxContentBytes:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	name := enabled.DisplayName("你好世界")
	if !utf8.ValidString(name) || len(name) > 5 {
		t.Fatalf("invalid limited display name %q", name)
	}
	enabled.SetStoreDisplayName(false)
	if enabled.DisplayName("private") != "" {
		t.Fatal("runtime-disabled display name was retained")
	}
	enabled.SetStoreDisplayName(true)
	if enabled.DisplayName("abc") != "abc" {
		t.Fatal("runtime-enabled display name was discarded")
	}
	content := enabled.Content(string([]byte{'a', 0xff, 'b', 'c', 'd', 'e'}))
	if !utf8.ValidString(content) || len(content) > 5 {
		t.Fatalf("invalid repaired content %q", content)
	}
}

func TestNewPrivacyFilterRequiresKey(t *testing.T) {
	if _, err := NewPrivacyFilter(nil, PrivacyOptions{}); err != ErrPrivacyKeyMissing {
		t.Fatalf("error = %v, want %v", err, ErrPrivacyKeyMissing)
	}
	var nilFilter *PrivacyFilter
	if nilFilter.KeyID() != "" {
		t.Fatal("nil filter must not expose a key id")
	}
	nilFilter.SetStoreDisplayName(true)
}
