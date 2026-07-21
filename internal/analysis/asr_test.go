package analysis

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type statusASRProvider struct {
	id          string
	validateErr error
}

func (p statusASRProvider) ID() string                     { return p.id }
func (p statusASRProvider) Validate(context.Context) error { return p.validateErr }
func (p statusASRProvider) Transcribe(context.Context, AudioInput, ProgressFunc) ([]TranscriptSegment, error) {
	return []TranscriptSegment{{StartMS: 0, EndMS: 1_000, Text: "test"}}, nil
}

func TestDisabledASRProviderIsStableAndDoesNotInvokeProgress(t *testing.T) {
	provider := DisabledASRProvider{}
	if provider.ID() != DisabledASRProviderID {
		t.Fatalf("ID() = %q", provider.ID())
	}
	if err := provider.Validate(context.Background()); !errors.Is(err, ErrASRNotConfigured) {
		t.Fatalf("Validate() error = %v", err)
	}
	progressCalled := false
	segments, err := provider.Transcribe(context.Background(), AudioInput{}, func(ASRProgress) error {
		progressCalled = true
		return nil
	})
	if !errors.Is(err, ErrASRNotConfigured) || segments != nil || progressCalled {
		t.Fatalf("Transcribe() = (%v, %v), progressCalled=%t", segments, err, progressCalled)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := provider.Validate(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Validate() error = %v", err)
	}
}

func TestASRStatusDefaultReadyUnavailableAndPrivacyAllowlist(t *testing.T) {
	tests := []struct {
		name     string
		provider ASRProvider
		want     ASRStatusDTO
	}{
		{
			name: "default disabled",
			want: ASRStatusDTO{Version: ASRContractVersion, ProviderID: DisabledASRProviderID,
				State: ASRStateDisabled, ErrorCode: ASRNotConfiguredErrorCode},
		},
		{
			name:     "ready adapter",
			provider: statusASRProvider{id: "local-test"},
			want: ASRStatusDTO{Version: ASRContractVersion, ProviderID: "local-test",
				State: ASRStateReady, Configured: true, Available: true},
		},
		{
			name:     "selected adapter not configured",
			provider: statusASRProvider{id: "local-test", validateErr: ErrASRNotConfigured},
			want: ASRStatusDTO{Version: ASRContractVersion, ProviderID: "local-test",
				State: ASRStateDisabled, ErrorCode: ASRNotConfiguredErrorCode},
		},
		{
			name:     "unavailable adapter",
			provider: statusASRProvider{id: "remote-test", validateErr: errors.New("secret endpoint token path")},
			want: ASRStatusDTO{Version: ASRContractVersion, ProviderID: "remote-test",
				State: ASRStateUnavailable, Configured: true, ErrorCode: ASRProviderUnavailableErrorCode},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider, err := normalizeASRProvider(test.provider)
			if err != nil {
				t.Fatal(err)
			}
			service := &Service{asrProvider: provider}
			got, err := service.GetASRStatus(context.Background())
			if err != nil || got != test.want {
				t.Fatalf("GetASRStatus() = (%+v, %v), want %+v", got, err, test.want)
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "token") || strings.Contains(string(encoded), "path") {
				t.Fatalf("status leaked provider error: %s", encoded)
			}
		})
	}
}

type changingIDASRProvider struct {
	ids   []string
	calls int
}

func (p *changingIDASRProvider) ID() string {
	value := p.ids[p.calls]
	p.calls++
	return value
}

func (*changingIDASRProvider) Validate(context.Context) error { return nil }
func (*changingIDASRProvider) Transcribe(context.Context, AudioInput, ProgressFunc) ([]TranscriptSegment, error) {
	return nil, nil
}

func TestASRStatusSanitizesProviderIDDrift(t *testing.T) {
	provider := &changingIDASRProvider{ids: []string{"initial-safe", "secret/provider/path"}}
	normalized, err := normalizeASRProvider(provider)
	if err != nil {
		t.Fatal(err)
	}
	status, err := (&Service{asrProvider: normalized}).GetASRStatus(context.Background())
	if err != nil {
		t.Fatalf("GetASRStatus() error = %v", err)
	}
	if status.ProviderID != DisabledASRProviderID || status.State != ASRStateUnavailable ||
		status.ErrorCode != ASRProviderUnavailableErrorCode {
		t.Fatalf("sanitized status = %+v", status)
	}
}

func TestNormalizeASRProviderRejectsUnsafeAndTypedNilAdapters(t *testing.T) {
	if _, err := normalizeASRProvider(statusASRProvider{id: "unsafe/provider"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unsafe provider error = %v", err)
	}
	var typedNil *pointerASRProvider
	if _, err := normalizeASRProvider(typedNil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("typed nil provider error = %v", err)
	}
}

type pointerASRProvider struct{}

func (*pointerASRProvider) ID() string                     { return "pointer" }
func (*pointerASRProvider) Validate(context.Context) error { return nil }
func (*pointerASRProvider) Transcribe(context.Context, AudioInput, ProgressFunc) ([]TranscriptSegment, error) {
	return nil, nil
}
