package update

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const StatusEventName = "update:status"

type State string

const (
	StateDisabled    State = "disabled"
	StateIdle        State = "idle"
	StateChecking    State = "checking"
	StateAvailable   State = "available"
	StateDownloading State = "downloading"
	StateReady       State = "ready"
	StateInstalling  State = "installing"
	StateFailed      State = "failed"
)

type StatusDTO struct {
	Version          int    `json:"version"`
	State            State  `json:"state"`
	CurrentVersion   string `json:"currentVersion"`
	AvailableVersion string `json:"availableVersion,omitempty"`
	PublishedAt      string `json:"publishedAt,omitempty"`
	ReleaseNotes     string `json:"releaseNotes,omitempty"`
	DownloadedBytes  int64  `json:"downloadedBytes,omitempty"`
	TotalBytes       int64  `json:"totalBytes,omitempty"`
	CheckedAt        int64  `json:"checkedAt,omitempty"`
	InstallBlocked   bool   `json:"installBlocked"`
	BlockReason      string `json:"blockReason,omitempty"`
	ErrorCode        string `json:"errorCode,omitempty"`
}

type Options struct {
	BaseURL        string
	Channel        string
	CurrentVersion string
	TrustedKeys    map[string]ed25519.PublicKey
	Root           string
	HTTPClient     *http.Client
	Now            func() time.Time
	CanTransfer    func() (bool, string)
	Publisher      func(StatusDTO)
}

type persistedState struct {
	Version            int    `json:"version"`
	HighestSeenVersion string `json:"highestSeenVersion,omitempty"`
}

type Service struct {
	mu             sync.RWMutex
	options        Options
	origin         *url.URL
	status         StatusDTO
	envelope       []byte
	verified       *Verified
	installerPath  string
	highestSeen    string
	downloadCancel context.CancelFunc
}

func NewService(options Options) (*Service, error) {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Channel == "" {
		options.Channel = "stable"
	}
	if options.Channel != "stable" && options.Channel != "canary" {
		return nil, errors.New("UPDATE_CHANNEL_INVALID")
	}
	if strings.TrimSpace(options.Root) == "" {
		return nil, errors.New("UPDATE_ROOT_INVALID")
	}
	origin, err := url.Parse(options.BaseURL)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil ||
		origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
		return nil, errors.New("UPDATE_ORIGIN_INVALID")
	}
	if options.HTTPClient == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = http.ProxyFromEnvironment
		transport.ResponseHeaderTimeout = 15 * time.Second
		transport.TLSHandshakeTimeout = 10 * time.Second
		options.HTTPClient = &http.Client{
			Transport: transport,
			Timeout:   30 * time.Minute,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("UPDATE_REDIRECT_REJECTED")
			},
		}
	}
	service := &Service{
		options: options, origin: origin,
		status: StatusDTO{Version: 1, State: StateIdle, CurrentVersion: options.CurrentVersion},
	}
	if !ValidVersion(options.CurrentVersion) {
		service.status.State = StateDisabled
		service.status.ErrorCode = "UPDATE_BUILD_NOT_RELEASE"
		return service, nil
	}
	if err := os.MkdirAll(options.Root, 0o700); err != nil {
		return nil, fmt.Errorf("UPDATE_ROOT_CREATE_FAILED: %w", err)
	}
	service.loadState()
	return service, nil
}

func (s *Service) Status() StatusDTO {
	s.mu.RLock()
	defer s.mu.RUnlock()
	status := s.status
	s.applyBlock(&status)
	return status
}

func (s *Service) Check(ctx context.Context) (StatusDTO, error) {
	if ctx == nil {
		return s.fail("UPDATE_CONTEXT_INVALID", errors.New("nil context"))
	}
	s.mu.Lock()
	if s.status.State == StateDisabled {
		status := s.status
		s.mu.Unlock()
		return status, errors.New(status.ErrorCode)
	}
	if s.status.State == StateChecking || s.status.State == StateDownloading || s.status.State == StateInstalling {
		s.mu.Unlock()
		return s.Status(), errors.New("UPDATE_BUSY")
	}
	s.status.State = StateChecking
	s.status.ErrorCode = ""
	s.status.BlockReason = ""
	s.status.InstallBlocked = false
	checking := s.status
	s.mu.Unlock()
	s.publish(checking)

	channelURL := ObjectURL(s.origin, "channels/"+s.options.Channel+".json")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, channelURL, nil)
	if err != nil {
		return s.fail("UPDATE_REQUEST_INVALID", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "DouyinLiveDesktop/"+s.options.CurrentVersion+" windows-amd64")
	response, err := s.options.HTTPClient.Do(request)
	if err != nil {
		return s.fail("UPDATE_CHECK_FAILED", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Request == nil || !sameOrigin(response.Request.URL, s.origin) {
		return s.fail("UPDATE_CHECK_FAILED", fmt.Errorf("unexpected response status %d", response.StatusCode))
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, MaxEnvelope+1))
	if err != nil {
		return s.fail("UPDATE_CHECK_FAILED", err)
	}
	verified, err := VerifyEnvelope(content, s.options.TrustedKeys, s.options.CurrentVersion, s.highestSeen, s.options.Channel, s.options.BaseURL)
	if err != nil {
		if strings.Contains(err.Error(), "UPDATE_NOT_NEWER") {
			s.mu.Lock()
			s.envelope = nil
			s.verified = nil
			s.installerPath = ""
			s.status = StatusDTO{
				Version: 1, State: StateIdle, CurrentVersion: s.options.CurrentVersion,
				CheckedAt: s.options.Now().UTC().UnixMilli(),
			}
			status := s.status
			s.mu.Unlock()
			s.publish(status)
			return status, nil
		}
		return s.fail(errorCode(err), err)
	}

	s.mu.Lock()
	s.envelope = append(s.envelope[:0], content...)
	s.verified = &verified
	s.installerPath = ""
	s.highestSeen = verified.Payload.Version
	s.status = StatusDTO{
		Version: 1, State: StateAvailable, CurrentVersion: s.options.CurrentVersion,
		AvailableVersion: verified.Payload.Version, PublishedAt: verified.Payload.PublishedAt,
		ReleaseNotes: verified.Payload.ReleaseNotes, TotalBytes: verified.Payload.Installer.Size,
		CheckedAt: s.options.Now().UTC().UnixMilli(),
	}
	status := s.status
	s.mu.Unlock()
	if err := s.persistState(); err != nil {
		return s.fail("UPDATE_STATE_PERSIST_FAILED", err)
	}
	s.applyBlock(&status)
	s.publish(status)
	return status, nil
}

func (s *Service) Prepare(ctx context.Context) (StatusDTO, error) {
	if ctx == nil {
		return s.fail("UPDATE_CONTEXT_INVALID", errors.New("nil context"))
	}
	if ok, reason := s.canTransfer(); !ok {
		s.mu.Lock()
		s.status.InstallBlocked = true
		s.status.BlockReason = reason
		status := s.status
		s.mu.Unlock()
		s.publish(status)
		return status, errors.New("UPDATE_BUSY")
	}
	s.mu.Lock()
	if s.verified == nil || (s.status.State != StateAvailable && s.status.State != StateFailed) {
		s.mu.Unlock()
		return s.Status(), errors.New("UPDATE_NOT_AVAILABLE")
	}
	verified := *s.verified
	downloadCtx, cancel := context.WithCancel(ctx)
	s.downloadCancel = cancel
	s.status.State = StateDownloading
	s.status.ErrorCode = ""
	s.status.DownloadedBytes = 0
	s.status.TotalBytes = verified.Payload.Installer.Size
	status := s.status
	s.mu.Unlock()
	s.publish(status)
	defer func() {
		s.mu.Lock()
		s.downloadCancel = nil
		s.mu.Unlock()
	}()

	installerPath, err := s.download(downloadCtx, verified)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return s.fail("UPDATE_DOWNLOAD_CANCELLED", err)
		}
		return s.fail(errorCode(err), err)
	}
	s.mu.Lock()
	s.installerPath = installerPath
	s.status.State = StateReady
	s.status.DownloadedBytes = verified.Payload.Installer.Size
	s.status.TotalBytes = verified.Payload.Installer.Size
	s.status.ErrorCode = ""
	status = s.status
	s.mu.Unlock()
	s.applyBlock(&status)
	s.publish(status)
	return status, nil
}

func (s *Service) CancelDownload() StatusDTO {
	s.mu.RLock()
	cancel := s.downloadCancel
	s.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
	return s.Status()
}

func (s *Service) PreparedInstaller() (string, Verified, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.status.State != StateReady || s.verified == nil || s.installerPath == "" {
		return "", Verified{}, errors.New("UPDATE_NOT_PREPARED")
	}
	return s.installerPath, *s.verified, nil
}

func (s *Service) PreparedPackage() (string, []byte, Verified, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.status.State != StateReady || s.verified == nil || s.installerPath == "" ||
		len(s.envelope) == 0 {
		return "", nil, Verified{}, errors.New("UPDATE_NOT_PREPARED")
	}
	return s.installerPath, append([]byte(nil), s.envelope...), *s.verified, nil
}

func (s *Service) MarkInstalling() (StatusDTO, error) {
	if ok, reason := s.canTransfer(); !ok {
		s.mu.Lock()
		s.status.InstallBlocked = true
		s.status.BlockReason = reason
		status := s.status
		s.mu.Unlock()
		s.publish(status)
		return status, errors.New("UPDATE_BUSY")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.State != StateReady {
		return s.status, errors.New("UPDATE_NOT_PREPARED")
	}
	s.status.State = StateInstalling
	s.status.InstallBlocked = false
	s.status.BlockReason = ""
	s.publish(s.status)
	return s.status, nil
}

func (s *Service) ReportFailure(code string, err error) (StatusDTO, error) {
	if err == nil {
		err = errors.New(code)
	}
	return s.fail(code, err)
}

func (s *Service) download(ctx context.Context, verified Verified) (string, error) {
	targetDirectory := filepath.Join(s.options.Root, "downloads", verified.Payload.Version)
	if err := os.MkdirAll(targetDirectory, 0o700); err != nil {
		return "", fmt.Errorf("UPDATE_DOWNLOAD_FAILED: %w", err)
	}
	filename := filepath.Base(verified.Payload.Installer.ObjectKey)
	finalPath := filepath.Join(targetDirectory, filename)
	partialPath := finalPath + ".part"
	etagPath := partialPath + ".etag"
	if digest, size, err := hashFile(finalPath); err == nil && digest == verified.Payload.Installer.SHA256 && size == verified.Payload.Installer.Size {
		return finalPath, nil
	}
	_ = os.Remove(finalPath)

	for attempt := 0; attempt < 2; attempt++ {
		existing := int64(0)
		etagBytes, _ := os.ReadFile(etagPath)
		etag := strings.TrimSpace(string(etagBytes))
		if info, err := os.Stat(partialPath); err == nil && info.Mode().IsRegular() && etag != "" {
			existing = info.Size()
			if existing < 0 || existing >= verified.Payload.Installer.Size {
				existing = 0
			}
		}
		if existing == 0 {
			_ = os.Remove(partialPath)
			_ = os.Remove(etagPath)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, ObjectURL(verified.Origin, verified.Payload.Installer.ObjectKey), nil)
		if err != nil {
			return "", fmt.Errorf("UPDATE_DOWNLOAD_FAILED: %w", err)
		}
		request.Header.Set("User-Agent", "DouyinLiveDesktop/"+s.options.CurrentVersion+" windows-amd64")
		if existing > 0 {
			request.Header.Set("Range", "bytes="+strconv.FormatInt(existing, 10)+"-")
			request.Header.Set("If-Range", etag)
		}
		response, err := s.options.HTTPClient.Do(request)
		if err != nil {
			return "", fmt.Errorf("UPDATE_DOWNLOAD_FAILED: %w", err)
		}
		if response.Request == nil || !sameOrigin(response.Request.URL, verified.Origin) {
			response.Body.Close()
			return "", errors.New("UPDATE_ORIGIN_MISMATCH")
		}
		resume := existing > 0 && response.StatusCode == http.StatusPartialContent && response.Header.Get("ETag") == etag
		if response.StatusCode != http.StatusOK && !resume {
			response.Body.Close()
			if existing > 0 {
				_ = os.Remove(partialPath)
				_ = os.Remove(etagPath)
				continue
			}
			return "", fmt.Errorf("UPDATE_DOWNLOAD_FAILED: status %d", response.StatusCode)
		}
		if existing > 0 && !resume {
			response.Body.Close()
			_ = os.Remove(partialPath)
			_ = os.Remove(etagPath)
			continue
		}
		if !resume {
			existing = 0
			etag = response.Header.Get("ETag")
			if etag == "" {
				response.Body.Close()
				return "", errors.New("UPDATE_ETAG_MISSING")
			}
			if err := os.WriteFile(etagPath, []byte(etag), 0o600); err != nil {
				response.Body.Close()
				return "", fmt.Errorf("UPDATE_DOWNLOAD_FAILED: %w", err)
			}
		}
		flags := os.O_CREATE | os.O_WRONLY
		if resume {
			flags |= os.O_APPEND
		} else {
			flags |= os.O_TRUNC
		}
		output, err := os.OpenFile(partialPath, flags, 0o600)
		if err != nil {
			response.Body.Close()
			return "", fmt.Errorf("UPDATE_DOWNLOAD_FAILED: %w", err)
		}
		buffer := make([]byte, 256<<10)
		total := existing
		for {
			count, readErr := response.Body.Read(buffer)
			if count > 0 {
				if total+int64(count) > verified.Payload.Installer.Size {
					readErr = errors.New("download exceeds declared size")
				} else if _, writeErr := output.Write(buffer[:count]); writeErr != nil {
					readErr = writeErr
				} else {
					total += int64(count)
					s.updateProgress(total)
				}
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					readErr = nil
				}
				response.Body.Close()
				closeErr := output.Close()
				if readErr != nil {
					return "", fmt.Errorf("UPDATE_DOWNLOAD_FAILED: %w", readErr)
				}
				if closeErr != nil {
					return "", fmt.Errorf("UPDATE_DOWNLOAD_FAILED: %w", closeErr)
				}
				break
			}
		}
		digest, size, err := hashFile(partialPath)
		if err != nil || size != verified.Payload.Installer.Size || digest != verified.Payload.Installer.SHA256 {
			_ = os.Remove(partialPath)
			_ = os.Remove(etagPath)
			return "", errors.New("UPDATE_HASH_MISMATCH")
		}
		if err := os.Rename(partialPath, finalPath); err != nil {
			return "", fmt.Errorf("UPDATE_DOWNLOAD_FAILED: %w", err)
		}
		_ = os.Remove(etagPath)
		return finalPath, nil
	}
	return "", errors.New("UPDATE_ETAG_CHANGED")
}

func (s *Service) updateProgress(total int64) {
	s.mu.Lock()
	s.status.DownloadedBytes = total
	status := s.status
	s.mu.Unlock()
	s.publish(status)
}

func (s *Service) fail(code string, err error) (StatusDTO, error) {
	if code == "" || code == "UPDATE_ERROR" {
		code = "UPDATE_FAILED"
	}
	s.mu.Lock()
	s.status.State = StateFailed
	s.status.ErrorCode = code
	status := s.status
	s.mu.Unlock()
	s.publish(status)
	return status, fmt.Errorf("%s: %w", code, err)
}

func (s *Service) applyBlock(status *StatusDTO) {
	ok, reason := s.canTransfer()
	status.InstallBlocked = !ok
	if !ok {
		status.BlockReason = reason
	}
}

func (s *Service) canTransfer() (bool, string) {
	if s.options.CanTransfer == nil {
		return true, ""
	}
	return s.options.CanTransfer()
}

func (s *Service) publish(status StatusDTO) {
	if s.options.Publisher != nil {
		s.options.Publisher(status)
	}
}

func (s *Service) statePath() string {
	return filepath.Join(s.options.Root, "state.json")
}

func (s *Service) loadState() {
	content, err := os.ReadFile(s.statePath())
	if err != nil {
		return
	}
	var state persistedState
	if decodeStrict(content, &state) == nil && state.Version == 1 && (state.HighestSeenVersion == "" || ValidVersion(state.HighestSeenVersion)) {
		s.highestSeen = state.HighestSeenVersion
	}
}

func (s *Service) persistState() error {
	content, err := json.Marshal(persistedState{Version: 1, HighestSeenVersion: s.highestSeen})
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(s.options.Root, ".state-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	_ = os.Remove(s.statePath())
	return os.Rename(temporaryPath, s.statePath())
}

func sameOrigin(left, right *url.URL) bool {
	return left != nil && right != nil && left.Scheme == right.Scheme && left.Host == right.Host
}

func errorCode(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	index := strings.Index(message, "UPDATE_")
	if index < 0 {
		return "UPDATE_ERROR"
	}
	end := index
	for end < len(message) {
		value := message[end]
		if (value < 'A' || value > 'Z') && value != '_' {
			break
		}
		end++
	}
	return message[index:end]
}

func hashFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}
