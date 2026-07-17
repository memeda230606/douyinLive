package douyinLive

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const streamResolverFixtureDir = "testdata/stream_resolver"

func TestStreamResolverFixturesAreSanitizedAndWellFormed(t *testing.T) {
	wantURLCounts := map[string]int{
		"direct_flv_hls.json":  5,
		"empty_streams.json":   0,
		"sdk_stream_data.json": 4,
	}

	entries, err := os.ReadDir(streamResolverFixtureDir)
	if err != nil {
		t.Fatalf("read stream resolver fixture directory: %v", err)
	}

	var fixtureNames []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		fixtureNames = append(fixtureNames, entry.Name())
	}
	sort.Strings(fixtureNames)

	wantNames := make([]string, 0, len(wantURLCounts))
	for name := range wantURLCounts {
		wantNames = append(wantNames, name)
	}
	sort.Strings(wantNames)
	if strings.Join(fixtureNames, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("fixture files = %v, want %v", fixtureNames, wantNames)
	}

	for _, name := range fixtureNames {
		t.Run(name, func(t *testing.T) {
			raw := readStreamFixture(t, name)
			if !json.Valid(raw) {
				t.Fatal("fixture is not valid JSON")
			}

			lower := strings.ToLower(string(raw))
			for _, forbidden := range []string{
				"douyincdn.com",
				"douyinpic.com",
				"sign=",
				"signature=",
				"mstoken",
				"a_bogus",
				"cookie",
			} {
				if strings.Contains(lower, forbidden) {
					t.Fatalf("fixture contains forbidden sensitive marker %q", forbidden)
				}
			}

			var document any
			if err := json.Unmarshal(raw, &document); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			urls := collectFixtureURLs(t, document)
			if len(urls) != wantURLCounts[name] {
				t.Fatalf("URL count = %d, want %d", len(urls), wantURLCounts[name])
			}
			for _, rawURL := range urls {
				assertSanitizedFixtureURL(t, rawURL)
			}
		})
	}
}

func TestStreamResolverFixtureShapes(t *testing.T) {
	direct := readStreamFixture(t, "direct_flv_hls.json")
	if got := len(gjson.GetBytes(direct, "data.data.0.stream_url.flv_pull_url").Map()); got != 2 {
		t.Fatalf("direct FLV variants = %d, want 2", got)
	}
	if got := len(gjson.GetBytes(direct, "data.data.0.stream_url.hls_pull_url_map").Map()); got != 2 {
		t.Fatalf("direct HLS variants = %d, want 2", got)
	}
	if got := gjson.GetBytes(direct, "data.data.0.stream_url.hls_pull_url").String(); got == "" {
		t.Fatal("direct HLS fallback is empty")
	}

	sdk := readStreamFixture(t, "sdk_stream_data.json")
	streamData := gjson.GetBytes(sdk, "data.data.0.stream_url.live_core_sdk_data.pull_data.stream_data").String()
	if !json.Valid([]byte(streamData)) {
		t.Fatal("SDK stream_data is not valid nested JSON")
	}
	for _, quality := range []string{"hd", "origin"} {
		base := "data." + quality + ".main."
		if gjson.Get(streamData, base+"flv").String() == "" {
			t.Fatalf("SDK %s FLV URL is empty", quality)
		}
		if gjson.Get(streamData, base+"hls").String() == "" {
			t.Fatalf("SDK %s HLS URL is empty", quality)
		}
		sdkParams := gjson.Get(streamData, base+"sdk_params").String()
		if !json.Valid([]byte(sdkParams)) {
			t.Fatalf("SDK %s sdk_params is not valid nested JSON", quality)
		}
		if got := gjson.Get(sdkParams, "VCodec").String(); got != "h265" {
			t.Fatalf("SDK %s codec = %q, want h265", quality, got)
		}
	}

	empty := readStreamFixture(t, "empty_streams.json")
	var emptyDocument any
	if err := json.Unmarshal(empty, &emptyDocument); err != nil {
		t.Fatalf("decode empty fixture: %v", err)
	}
	if urls := collectFixtureURLs(t, emptyDocument); len(urls) != 0 {
		t.Fatalf("empty fixture contains %d URLs, want 0", len(urls))
	}
}

func readStreamFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(streamResolverFixtureDir, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

func collectFixtureURLs(t *testing.T, value any) []string {
	t.Helper()
	var found []string
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		case string:
			trimmed := strings.TrimSpace(typed)
			if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
				found = append(found, trimmed)
				return
			}
			if len(trimmed) > 1 && (trimmed[0] == '{' || trimmed[0] == '[') && json.Valid([]byte(trimmed)) {
				var nested any
				if err := json.Unmarshal([]byte(trimmed), &nested); err != nil {
					t.Fatalf("decode nested JSON: %v", err)
				}
				walk(nested)
			}
		}
	}
	walk(value)
	return found
}

func assertSanitizedFixtureURL(t *testing.T, rawURL string) {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse fixture URL: %v", err)
	}
	if parsed.Scheme != "https" {
		t.Fatalf("fixture URL scheme = %q, want https", parsed.Scheme)
	}
	if !strings.HasSuffix(parsed.Hostname(), ".invalid") {
		t.Fatalf("fixture URL host %q does not use .invalid", parsed.Hostname())
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		t.Fatal("fixture URL contains query, fragment, or user info")
	}
}
