package playback

import (
	"context"
	"database/sql"
)

// Service is the application-facing read-only playback boundary.
type Service struct {
	repository *Repository
}

func NewService(reader *sql.DB) (*Service, error) {
	repository, err := NewRepository(reader)
	if err != nil {
		return nil, err
	}
	return &Service{repository: repository}, nil
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
