package douyinLive

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRankResolvedStreamsPreferences(t *testing.T) {
	candidates := []ResolvedStream{
		{
			ID: "origin-h265-flv", Protocol: "flv", QualityKey: "origin",
			Codec: "h265", Bitrate: 8_000_000,
			URL: "https://select.example.invalid/live/origin-h265.flv",
		},
		{
			ID: "origin-h264-hls", Protocol: "hls", QualityKey: "origin",
			Codec: "h264", Bitrate: 6_000_000,
			URL: "https://select.example.invalid/live/origin-h264.m3u8",
		},
		{
			ID: "origin-h264-flv", Protocol: "flv", QualityKey: "origin",
			Codec: "h264", Bitrate: 5_000_000,
			URL: "https://select.example.invalid/live/origin-h264.flv",
		},
		{
			ID: "hd-h264-flv", Protocol: "flv", QualityKey: "hd",
			Codec: "h264", Bitrate: 7_000_000,
			URL: "https://select.example.invalid/live/hd-h264.flv",
		},
	}

	tests := []struct {
		name       string
		preference streamSelectionPreference
		wantFirst  string
	}{
		{
			name:       "automatic compatibility prefers H264 then FLV",
			preference: streamSelectionPreference{CompatibilityMode: true},
			wantFirst:  "origin-h264-flv",
		},
		{
			name: "explicit HLS is respected after quality and codec",
			preference: streamSelectionPreference{
				Protocol: "hls", CompatibilityMode: true,
			},
			wantFirst: "origin-h264-hls",
		},
		{
			name: "explicit quality outranks higher automatic quality",
			preference: streamSelectionPreference{
				QualityKey: "hd", CompatibilityMode: true,
			},
			wantFirst: "hd-h264-flv",
		},
		{
			name:       "non compatibility mode allows highest bitrate H265",
			preference: streamSelectionPreference{},
			wantFirst:  "origin-h265-flv",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ranked, err := rankResolvedStreams(candidates, test.preference)
			if err != nil {
				t.Fatalf("rank streams: %v", err)
			}
			if len(ranked) != len(candidates) {
				t.Fatalf("ranked count = %d, want %d", len(ranked), len(candidates))
			}
			if ranked[0].ID != test.wantFirst {
				t.Fatalf("first candidate = %q, want %q; order=%v", ranked[0].ID, test.wantFirst, streamIDs(ranked))
			}
		})
	}
}

func TestRankResolvedStreamsCompatibilityAllowsH265Only(t *testing.T) {
	candidates := []ResolvedStream{
		{
			ID: "h265-unknown-bitrate", Protocol: "hls", QualityKey: "hd",
			Codec: "h265",
			URL:   "https://h265.example.invalid/live/unknown.m3u8",
		},
		{
			ID: "h265-known-bitrate", Protocol: "hls", QualityKey: "hd",
			Codec: "h265", Bitrate: 4_000_000,
			URL: "https://h265.example.invalid/live/known.m3u8",
		},
	}

	ranked, err := rankResolvedStreams(candidates, streamSelectionPreference{CompatibilityMode: true})
	if err != nil {
		t.Fatalf("rank H265-only streams: %v", err)
	}
	if len(ranked) != 2 {
		t.Fatalf("ranked count = %d, want 2", len(ranked))
	}
	if ranked[0].ID != "h265-known-bitrate" {
		t.Fatalf("known bitrate was not preferred: %v", streamIDs(ranked))
	}
	for _, stream := range ranked {
		if stream.Codec != "h265" {
			t.Fatalf("codec = %q, want h265", stream.Codec)
		}
	}
}

func TestRankResolvedStreamsBuildsFallbackOrder(t *testing.T) {
	candidates := []ResolvedStream{
		{
			ID: "hls-fallback", Protocol: "hls", QualityKey: "hd",
			Codec: "h264", Bitrate: 6_000_000,
			URL: "https://fallback.example.invalid/live/stream.m3u8",
		},
		{
			ID: "flv-primary", Protocol: "flv", QualityKey: "hd",
			Codec: "h264", Bitrate: 5_000_000,
			URL: "https://fallback.example.invalid/live/stream.flv",
		},
	}

	ranked, err := rankResolvedStreams(candidates, streamSelectionPreference{CompatibilityMode: true})
	if err != nil {
		t.Fatalf("rank streams: %v", err)
	}
	if got := streamIDs(ranked); fmt.Sprint(got) != "[flv-primary hls-fallback]" {
		t.Fatalf("fallback order = %v", got)
	}
	if class := classifyStreamFailure(403, errors.New("response details are redacted")); class != streamFailureURLExpired {
		t.Fatalf("403 class = %q, want %q", class, streamFailureURLExpired)
	}
	if ranked[1].ID != "hls-fallback" {
		t.Fatalf("expired primary did not degrade to HLS: %v", streamIDs(ranked))
	}
}

func TestRankResolvedStreamsRejectsMissingFields(t *testing.T) {
	candidates := []ResolvedStream{
		{ID: "missing-url", Protocol: "flv", QualityKey: "hd"},
		{
			ID: "missing-protocol", QualityKey: "hd",
			URL: "https://missing.example.invalid/live/no-extension",
		},
		{
			ID: "invalid-url", Protocol: "flv", QualityKey: "hd",
			URL: "://invalid-url-secret",
		},
	}

	ranked, err := rankResolvedStreams(candidates, streamSelectionPreference{})
	if !errors.Is(err, errNoSelectableStreamCandidates) {
		t.Fatalf("error = %v, want errNoSelectableStreamCandidates", err)
	}
	if len(ranked) != 0 {
		t.Fatalf("ranked count = %d, want 0", len(ranked))
	}
	if class := classifyStreamFailure(0, err); class != streamFailureNoCandidates {
		t.Fatalf("missing-field class = %q, want %q", class, streamFailureNoCandidates)
	}
	if err != nil && (strings.Contains(err.Error(), "missing.example.invalid") || strings.Contains(err.Error(), "invalid-url-secret")) {
		t.Fatalf("missing-field error exposes candidate content: %v", err)
	}
}

func TestClassifyStreamFailure(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		err        error
		want       streamFailureClass
	}{
		{name: "403", statusCode: 403, want: streamFailureURLExpired},
		{name: "404", statusCode: 404, want: streamFailureURLExpired},
		{name: "410", statusCode: 410, want: streamFailureURLExpired},
		{name: "status text", err: errors.New("input failed with status=403"), want: streamFailureURLExpired},
		{name: "no candidates", err: errNoStreamCandidates, want: streamFailureNoCandidates},
		{name: "deadline", err: context.DeadlineExceeded, want: streamFailureTemporaryNetwork},
		{name: "connection reset", err: errors.New("connection reset by peer"), want: streamFailureTemporaryNetwork},
		{name: "unsupported", err: errors.New("unsupported codec"), want: streamFailureUnsupported},
		{name: "unknown", err: errors.New("process exited"), want: streamFailureUnknown},
		{name: "nil", want: streamFailureUnknown},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyStreamFailure(test.statusCode, test.err); got != test.want {
				t.Fatalf("class = %q, want %q", got, test.want)
			}
		})
	}
}

func streamIDs(streams []ResolvedStream) []string {
	ids := make([]string, len(streams))
	for index, stream := range streams {
		ids[index] = stream.ID
	}
	return ids
}
