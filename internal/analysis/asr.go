package analysis

import (
	"context"
	"errors"
	"reflect"
	"regexp"
)

const (
	ASRContractVersion              = 1
	DisabledASRProviderID           = "disabled"
	ASRStateDisabled                = "disabled"
	ASRStateReady                   = "ready"
	ASRStateUnavailable             = "unavailable"
	ASRNotConfiguredErrorCode       = "ASR_NOT_CONFIGURED"
	ASRProviderUnavailableErrorCode = "ASR_PROVIDER_UNAVAILABLE"
)

var (
	ErrASRNotConfigured = errors.New("asr provider is not configured")
	providerIDPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// ASRProvider is the internal adapter boundary for future local or remote
// transcription implementations. Provider-specific configuration and secrets
// never cross the analysis DTO boundary.
type ASRProvider interface {
	ID() string
	Validate(context.Context) error
	Transcribe(context.Context, AudioInput, ProgressFunc) ([]TranscriptSegment, error)
}

type AudioInput struct {
	SessionID         string
	MediaSegmentID    string
	SourceAudioSHA256 string `json:"-"`
	AudioPath         string `json:"-"`
	SessionStartMS    int64
	DurationMS        int64
	Language          string
}

type ASRProgress struct {
	CompletedMS int64
	TotalMS     int64
}

type ProgressFunc func(ASRProgress) error

type TranscriptSegment struct {
	StartMS    int64
	EndMS      int64
	Text       string
	Confidence *float64
	Speaker    string
}

type ASRStatusDTO struct {
	Version    int    `json:"version"`
	ProviderID string `json:"providerId"`
	State      string `json:"state"`
	Configured bool   `json:"configured"`
	Available  bool   `json:"available"`
	ErrorCode  string `json:"errorCode,omitempty"`
}

// DisabledASRProvider is the production default until a provider is selected.
// It performs no I/O, never invokes progress callbacks, and returns a stable
// configuration error without exposing provider details.
type DisabledASRProvider struct{}

func (DisabledASRProvider) ID() string { return DisabledASRProviderID }

func (DisabledASRProvider) Validate(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrASRNotConfigured
}

func (DisabledASRProvider) Transcribe(
	ctx context.Context,
	_ AudioInput,
	_ ProgressFunc,
) ([]TranscriptSegment, error) {
	if ctx == nil {
		return nil, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, ErrASRNotConfigured
}

func normalizeASRProvider(provider ASRProvider) (ASRProvider, error) {
	if provider == nil {
		return DisabledASRProvider{}, nil
	}
	value := reflect.ValueOf(provider)
	if (value.Kind() == reflect.Chan || value.Kind() == reflect.Func ||
		value.Kind() == reflect.Interface || value.Kind() == reflect.Map ||
		value.Kind() == reflect.Pointer || value.Kind() == reflect.Slice) && value.IsNil() {
		return nil, ErrInvalidArgument
	}
	if !providerIDPattern.MatchString(provider.ID()) {
		return nil, ErrInvalidArgument
	}
	return provider, nil
}

// GetASRStatus reports only a stable allowlist. Provider validation errors are
// intentionally collapsed so credentials, endpoints, models, and local paths
// cannot reach Wails or logs through this contract.
func (s *Service) GetASRStatus(ctx context.Context) (ASRStatusDTO, error) {
	if ctx == nil || s == nil || s.asrProvider == nil {
		return ASRStatusDTO{}, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return ASRStatusDTO{}, err
	}
	providerID := s.asrProvider.ID()
	if !providerIDPattern.MatchString(providerID) {
		return ASRStatusDTO{
			Version: ASRContractVersion, ProviderID: DisabledASRProviderID,
			State: ASRStateUnavailable, Configured: true,
			ErrorCode: ASRProviderUnavailableErrorCode,
		}, nil
	}
	err := s.asrProvider.Validate(ctx)
	if err == nil {
		return ASRStatusDTO{
			Version: ASRContractVersion, ProviderID: providerID,
			State: ASRStateReady, Configured: true, Available: true,
		}, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ASRStatusDTO{}, ctxErr
	}
	if errors.Is(err, ErrASRNotConfigured) {
		return ASRStatusDTO{
			Version: ASRContractVersion, ProviderID: providerID,
			State: ASRStateDisabled, ErrorCode: ASRNotConfiguredErrorCode,
		}, nil
	}
	return ASRStatusDTO{
		Version: ASRContractVersion, ProviderID: providerID,
		State: ASRStateUnavailable, Configured: true,
		ErrorCode: ASRProviderUnavailableErrorCode,
	}, nil
}
