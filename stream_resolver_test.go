package douyinLive

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestResolveStreamsUsesInstanceRoomEnterData(t *testing.T) {
	dl, err := NewDouyinLive("public-api-fixture", nil, "")
	if err != nil {
		t.Fatalf("new DouyinLive: %v", err)
	}
	defer dl.Dispose()

	const body = `{
		"data": {
			"user": {"id_str": "owner-public-api", "nickname": "fixture-owner"},
			"data": [{
				"id_str": "room-public-api",
				"stream_url": {
					"flv_pull_url": {
						"HD1": "https://public-api.example.invalid/live/current.flv?token=secret"
					}
				}
			}]
		}
	}`
	if ok := dl.ristretto.SetWithTTL(dl.liveID, body, 1, time.Minute); !ok {
		t.Fatal("cache rejected room-enter fixture")
	}
	dl.ristretto.Wait()

	streams, err := dl.ResolveStreams()
	if err != nil {
		t.Fatalf("ResolveStreams returned error: %v", err)
	}
	if len(streams) != 1 {
		t.Fatalf("stream count = %d, want 1", len(streams))
	}
	stream := streams[0]
	if stream.Protocol != "flv" || stream.QualityKey != "hd" || stream.Quality != "超清" {
		t.Fatalf("unexpected public stream metadata: %s", stream)
	}
	if stream.URL != "https://public-api.example.invalid/live/current.flv?token=secret" {
		t.Fatal("ResolveStreams did not preserve the internal stream URL")
	}
	if stream.SourcePath != "data.data.0.stream_url.flv_pull_url.HD1" {
		t.Fatalf("source path = %q", stream.SourcePath)
	}
	if info := dl.roomInfoSnapshot(); info.roomID != "room-public-api" || info.pushID != "owner-public-api" {
		t.Fatalf("ResolveStreams did not reuse room-enter state updates: %#v", info)
	}

	rendered := fmt.Sprint(streams)
	if strings.Contains(rendered, "token=secret") || strings.Contains(rendered, "public-api.example.invalid") ||
		strings.Contains(rendered, stream.SourcePath) {
		t.Fatalf("rendered public stream exposes URL or source path: %s", rendered)
	}
	encoded, err := json.Marshal(streams)
	if err != nil {
		t.Fatalf("marshal public streams: %v", err)
	}
	if strings.Contains(string(encoded), "token=secret") || strings.Contains(string(encoded), "SourcePath") || strings.Contains(string(encoded), "URL") {
		t.Fatalf("JSON public stream exposes internal fields: %s", encoded)
	}
}

func TestResolvedStreamCandidateStringRedactsSourcePath(t *testing.T) {
	const secretPath = "data.data.0.private_source_path"
	candidate := resolvedStreamCandidate{
		ID: "safe-id", Protocol: "flv", QualityKey: "hd", Codec: "h264",
		URL: "https://string.example.invalid/live.flv?token=secret", SourcePath: secretPath,
	}
	rendered := fmt.Sprint(candidate)
	if strings.Contains(rendered, secretPath) || strings.Contains(rendered, "string.example.invalid") || strings.Contains(rendered, "token=secret") {
		t.Fatalf("candidate String exposes internal provenance: %s", rendered)
	}
}

func TestParseResolvedStreamsDirectFixture(t *testing.T) {
	body := string(readStreamFixture(t, "direct_flv_hls.json"))
	candidates, err := parseResolvedStreams(body)
	if err != nil {
		t.Fatalf("parse direct fixture: %v", err)
	}
	if len(candidates) != 5 {
		t.Fatalf("candidate count = %d, want 5", len(candidates))
	}

	want := map[string]string{
		"flv/uhd":    "蓝光",
		"flv/hd":     "超清",
		"hls/uhd":    "蓝光",
		"hls/hd":     "超清",
		"hls/origin": "原画",
	}
	for _, candidate := range candidates {
		key := candidate.Protocol + "/" + candidate.QualityKey
		if candidate.Quality != want[key] {
			t.Fatalf("%s quality = %q, want %q", key, candidate.Quality, want[key])
		}
		if candidate.Codec != "unknown" {
			t.Fatalf("%s codec = %q, want unknown", key, candidate.Codec)
		}
		if !strings.HasPrefix(candidate.ID, "stream-") {
			t.Fatalf("%s ID = %q", key, candidate.ID)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Fatalf("missing candidates: %v", want)
	}
}

func TestParseResolvedStreamsSDKFixture(t *testing.T) {
	body := string(readStreamFixture(t, "sdk_stream_data.json"))
	candidates, err := parseResolvedStreams(body)
	if err != nil {
		t.Fatalf("parse SDK fixture: %v", err)
	}
	if len(candidates) != 4 {
		t.Fatalf("candidate count = %d, want 4", len(candidates))
	}

	wantBitrates := map[string]int64{"hd": 6000000, "origin": 8000000}
	for _, candidate := range candidates {
		if candidate.Codec != "h265" {
			t.Fatalf("%s codec = %q, want h265", candidate.SourcePath, candidate.Codec)
		}
		if candidate.Bitrate != wantBitrates[candidate.QualityKey] {
			t.Fatalf("%s bitrate = %d, want %d", candidate.QualityKey, candidate.Bitrate, wantBitrates[candidate.QualityKey])
		}
	}
}

func TestParseResolvedStreamsEmptyFixture(t *testing.T) {
	body := string(readStreamFixture(t, "empty_streams.json"))
	candidates, err := parseResolvedStreams(body)
	if !errors.Is(err, errNoStreamCandidates) {
		t.Fatalf("error = %v, want errNoStreamCandidates", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidate count = %d, want 0", len(candidates))
	}
}

func TestParseResolvedStreamsFallbackSources(t *testing.T) {
	body := `{
		"data": {
			"data": [{
				"stream_url": {
					"pull_datas": {
						"HD1": {"main": {"flv": "https://pull.example.invalid/live/fallback.flv"}}
					}
				},
				"additional_stream_url": {
					"hls_pull_url_map": {
						"SD2": "https://additional.example.invalid/live/fallback.m3u8"
					}
				}
			}],
			"web_stream_url": "https://web.example.invalid/live/last-resort.flv"
		}
	}`

	candidates, err := parseResolvedStreams(body)
	if err != nil {
		t.Fatalf("parse fallback sources: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidate count = %d, want 2", len(candidates))
	}
	for _, candidate := range candidates {
		if candidate.SourcePath == "data.web_stream_url" {
			t.Fatal("web_stream_url was used despite earlier candidates")
		}
	}

	webOnly := `{"data":{"data":[],"web_stream_url":"https://web.example.invalid/live/last-resort.flv"}}`
	candidates, err = parseResolvedStreams(webOnly)
	if err != nil {
		t.Fatalf("parse web fallback: %v", err)
	}
	if len(candidates) != 1 || candidates[0].SourcePath != "data.web_stream_url" {
		t.Fatalf("web fallback candidates = %#v", candidates)
	}
}

func TestParseResolvedStreamsDeduplicatesWithoutQuery(t *testing.T) {
	body := `{
		"data": {
			"data": [{
				"stream_url": {
					"flv_pull_url": {
						"HD1": "https://dedupe.example.invalid/live/same.flv?token=first"
					},
					"pull_datas": {
						"hd": {
							"main": {
								"flv": "https://dedupe.example.invalid/live/same.flv?token=second"
							}
						}
					}
				}
			}]
		}
	}`

	first, err := parseResolvedStreams(body)
	if err != nil {
		t.Fatalf("parse duplicate candidates: %v", err)
	}
	second, err := parseResolvedStreams(body)
	if err != nil {
		t.Fatalf("parse duplicate candidates again: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("candidate count = %d, want 1", len(first))
	}
	if first[0].ID != second[0].ID {
		t.Fatalf("candidate IDs are not deterministic: %q != %q", first[0].ID, second[0].ID)
	}
	if strings.Contains(first[0].ID, "token") {
		t.Fatalf("candidate ID exposes query: %q", first[0].ID)
	}
	if rendered := fmt.Sprint(first); strings.Contains(rendered, "token=") || strings.Contains(rendered, "dedupe.example.invalid") {
		t.Fatalf("rendered candidate exposes URL: %s", rendered)
	}
}

func TestParseResolvedStreamsReportsNestedJSONWithoutContent(t *testing.T) {
	body := `{
		"data": {
			"data": [{
				"stream_url": {
					"flv_pull_url": {
						"HD1": "https://safe.example.invalid/live/valid.flv"
					},
					"live_core_sdk_data": {
						"pull_data": {
							"stream_data": "{not-json-secret}"
						}
					}
				}
			}]
		}
	}`

	candidates, err := parseResolvedStreams(body)
	if err == nil {
		t.Fatal("expected nested JSON error")
	}
	if len(candidates) != 1 {
		t.Fatalf("candidate count = %d, want 1", len(candidates))
	}
	if strings.Contains(err.Error(), "not-json-secret") || strings.Contains(err.Error(), "safe.example.invalid") {
		t.Fatalf("parse error exposes response content: %v", err)
	}
	if !strings.Contains(err.Error(), "stream_data") || !strings.Contains(err.Error(), "length=") {
		t.Fatalf("parse error lacks field path and length: %v", err)
	}
}

func TestParseResolvedStreamsRejectsInvalidTopLevelJSON(t *testing.T) {
	_, err := parseResolvedStreams(`{"data":`)
	if !errors.Is(err, errInvalidStreamResponse) {
		t.Fatalf("error = %v, want errInvalidStreamResponse", err)
	}
	if strings.Contains(err.Error(), `{"data":`) {
		t.Fatalf("parse error exposes response content: %v", err)
	}
}
