// Package update implements the signed, privacy-safe desktop update protocol.
package update

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	EnvelopeSchema = "douyinlive-update-envelope/v1"
	PayloadSchema  = "douyinlive-update/v1"
	Product        = "douyin-live-desktop"
	Platform       = "windows/amd64"
	MaxEnvelope    = 96 << 10
	MaxInstaller   = 512 << 20
	MaxNotes       = 8 << 10
)

var (
	versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	shaPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	commitPattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

type Envelope struct {
	Schema    string `json:"schema"`
	KeyID     string `json:"keyId"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

type FileDescriptor struct {
	ObjectKey string `json:"objectKey"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
}

type Payload struct {
	Schema                string         `json:"schema"`
	Product               string         `json:"product"`
	Channel               string         `json:"channel"`
	Version               string         `json:"version"`
	PublishedAt           string         `json:"publishedAt"`
	GitCommit             string         `json:"gitCommit"`
	Platform              string         `json:"platform"`
	DatabaseSchemaVersion int            `json:"databaseSchemaVersion"`
	UpdaterProtocol       int            `json:"updaterProtocol"`
	ReleaseNotes          string         `json:"releaseNotes"`
	Installer             FileDescriptor `json:"installer"`
	ReleaseManifest       FileDescriptor `json:"releaseManifest"`
}

type Verified struct {
	Payload Payload
	Origin  *url.URL
}

func Sign(payload Payload, keyID string, privateKey ed25519.PrivateKey) ([]byte, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("UPDATE_SIGNING_KEY_INVALID")
	}
	if strings.TrimSpace(keyID) == "" || len(keyID) > 64 {
		return nil, errors.New("UPDATE_KEY_ID_INVALID")
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	signature := ed25519.Sign(privateKey, payloadBytes)
	return json.Marshal(Envelope{
		Schema: EnvelopeSchema, KeyID: keyID,
		Payload:   base64.StdEncoding.EncodeToString(payloadBytes),
		Signature: base64.StdEncoding.EncodeToString(signature),
	})
}

func VerifyEnvelope(data []byte, trusted map[string]ed25519.PublicKey, currentVersion, highestSeen, expectedChannel, baseURL string) (Verified, error) {
	if len(data) == 0 || len(data) > MaxEnvelope {
		return Verified{}, errors.New("UPDATE_METADATA_TOO_LARGE")
	}
	var envelope Envelope
	if err := decodeStrict(data, &envelope); err != nil {
		return Verified{}, fmt.Errorf("UPDATE_METADATA_INVALID: %w", err)
	}
	if envelope.Schema != EnvelopeSchema {
		return Verified{}, errors.New("UPDATE_SCHEMA_UNSUPPORTED")
	}
	publicKey, ok := trusted[envelope.KeyID]
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return Verified{}, errors.New("UPDATE_KEY_UNTRUSTED")
	}
	payloadBytes, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil || len(payloadBytes) == 0 || len(payloadBytes) > MaxEnvelope {
		return Verified{}, errors.New("UPDATE_PAYLOAD_INVALID")
	}
	signature, err := base64.StdEncoding.DecodeString(envelope.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(publicKey, payloadBytes, signature) {
		return Verified{}, errors.New("UPDATE_SIGNATURE_INVALID")
	}
	var payload Payload
	if err := decodeStrict(payloadBytes, &payload); err != nil {
		return Verified{}, fmt.Errorf("UPDATE_PAYLOAD_INVALID: %w", err)
	}
	origin, err := validatePayload(payload, currentVersion, highestSeen, expectedChannel, baseURL)
	if err != nil {
		return Verified{}, err
	}
	return Verified{Payload: payload, Origin: origin}, nil
}

func validatePayload(payload Payload, currentVersion, highestSeen, expectedChannel, baseURL string) (*url.URL, error) {
	if payload.Schema != PayloadSchema || payload.Product != Product || payload.Platform != Platform || payload.UpdaterProtocol != 1 {
		return nil, errors.New("UPDATE_PAYLOAD_INCOMPATIBLE")
	}
	if payload.Channel != expectedChannel || (payload.Channel != "stable" && payload.Channel != "canary") {
		return nil, errors.New("UPDATE_CHANNEL_MISMATCH")
	}
	if !ValidVersion(payload.Version) || !ValidVersion(currentVersion) {
		return nil, errors.New("UPDATE_VERSION_INVALID")
	}
	if CompareVersions(payload.Version, currentVersion) <= 0 {
		return nil, errors.New("UPDATE_NOT_NEWER")
	}
	if highestSeen != "" {
		if !ValidVersion(highestSeen) || CompareVersions(payload.Version, highestSeen) < 0 {
			return nil, errors.New("UPDATE_ROLLBACK_REJECTED")
		}
	}
	published, err := time.Parse(time.RFC3339, payload.PublishedAt)
	if err != nil || payload.PublishedAt != published.Format(time.RFC3339) {
		return nil, errors.New("UPDATE_PUBLISHED_AT_INVALID")
	}
	if !commitPattern.MatchString(payload.GitCommit) || payload.DatabaseSchemaVersion <= 0 {
		return nil, errors.New("UPDATE_BUILD_IDENTITY_INVALID")
	}
	if len(payload.ReleaseNotes) > MaxNotes || strings.ContainsRune(payload.ReleaseNotes, 0) {
		return nil, errors.New("UPDATE_RELEASE_NOTES_INVALID")
	}
	versionPrefix := "releases/v" + payload.Version + "/"
	if err := validateFile(payload.Installer, versionPrefix, MaxInstaller, "-windows-amd64-installer.exe"); err != nil {
		return nil, fmt.Errorf("UPDATE_INSTALLER_INVALID: %w", err)
	}
	if err := validateFile(payload.ReleaseManifest, versionPrefix, MaxEnvelope, "release-manifest.json"); err != nil {
		return nil, fmt.Errorf("UPDATE_RELEASE_MANIFEST_INVALID: %w", err)
	}
	origin, err := url.Parse(baseURL)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil ||
		origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
		return nil, errors.New("UPDATE_ORIGIN_INVALID")
	}
	return origin, nil
}

func validateFile(file FileDescriptor, prefix string, maximum int64, suffix string) error {
	if file.Size <= 0 || file.Size > maximum || !shaPattern.MatchString(file.SHA256) {
		return errors.New("invalid size or digest")
	}
	if file.ObjectKey != path.Clean(file.ObjectKey) || strings.HasPrefix(file.ObjectKey, "/") ||
		!strings.HasPrefix(file.ObjectKey, prefix) || !strings.HasSuffix(file.ObjectKey, suffix) ||
		strings.ContainsAny(file.ObjectKey, `\?#`) {
		return errors.New("invalid object key")
	}
	return nil
}

func ObjectURL(origin *url.URL, objectKey string) string {
	copyURL := *origin
	copyURL.Path = "/" + objectKey
	copyURL.RawPath = ""
	return copyURL.String()
}

func ValidVersion(value string) bool {
	return versionPattern.MatchString(value)
}

func CompareVersions(left, right string) int {
	leftParts := versionParts(left)
	rightParts := versionParts(right)
	for index := range leftParts {
		if leftParts[index] < rightParts[index] {
			return -1
		}
		if leftParts[index] > rightParts[index] {
			return 1
		}
	}
	return 0
}

func versionParts(value string) [3]uint64 {
	var result [3]uint64
	parts := strings.Split(value, ".")
	if len(parts) != len(result) {
		return result
	}
	for index := range result {
		result[index], _ = strconv.ParseUint(parts[index], 10, 64)
	}
	return result
}

func DecodePublicKey(value string) (ed25519.PublicKey, error) {
	decoded, err := hex.DecodeString(strings.TrimSpace(value))
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, errors.New("UPDATE_PUBLIC_KEY_INVALID")
	}
	return ed25519.PublicKey(decoded), nil
}

// DecodeStrictJSON decodes one JSON value while rejecting unknown fields,
// duplicate object keys and trailing data.
func DecodeStrictJSON(data []byte, target any) error {
	return decodeStrict(data, target)
}

func decodeStrict(data []byte, target any) error {
	if err := rejectDuplicateKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("trailing token %v: %w", token, err)
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = struct{}{}
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closeToken, err := decoder.Token()
		if err != nil || closeToken != json.Delim('}') {
			return errors.New("invalid object close")
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closeToken, err := decoder.Token()
		if err != nil || closeToken != json.Delim(']') {
			return errors.New("invalid array close")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}
