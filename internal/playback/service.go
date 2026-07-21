package playback

import (
	"context"
	"database/sql"
	"path/filepath"
)

type ServiceOptions struct {
	DataRoot string
}

// Service is the application-facing read-only playback boundary.
type Service struct {
	repository *Repository
	dataRoot   string
}

func NewService(reader *sql.DB) (*Service, error) {
	return NewServiceWithOptions(reader, ServiceOptions{})
}

func NewServiceWithOptions(reader *sql.DB, options ServiceOptions) (*Service, error) {
	repository, err := NewRepository(reader)
	if err != nil {
		return nil, err
	}
	dataRoot := ""
	if options.DataRoot != "" {
		absolute, absoluteErr := filepath.Abs(options.DataRoot)
		if absoluteErr != nil {
			return nil, absoluteErr
		}
		dataRoot = filepath.Clean(absolute)
	}
	return &Service{repository: repository, dataRoot: dataRoot}, nil
}

func (s *Service) GetSession(ctx context.Context, sessionID string) (SessionResult, error) {
	return s.repository.GetSession(ctx, sessionID)
}

func (s *Service) ListSessions(
	ctx context.Context,
	filter SessionFilter,
	page PageRequest,
) (SessionPage, error) {
	return s.repository.ListSessions(ctx, filter, page)
}

func (s *Service) ListEvents(
	ctx context.Context,
	filter EventFilter,
	page PageRequest,
) (EventPage, error) {
	return s.repository.ListEvents(ctx, filter, page)
}

func (s *Service) ListGaps(
	ctx context.Context,
	filter GapFilter,
	page PageRequest,
) (GapPage, error) {
	return s.repository.ListGaps(ctx, filter, page)
}

func (s *Service) ListMediaSegments(
	ctx context.Context,
	filter MediaFilter,
	page PageRequest,
) (MediaPage, error) {
	return s.repository.ListMediaSegments(ctx, filter, page)
}

func (s *Service) LocateMedia(
	ctx context.Context,
	request MediaLocationRequest,
) (MediaLocationResult, error) {
	return s.repository.LocateMedia(ctx, request)
}
