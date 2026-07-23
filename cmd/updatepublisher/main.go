package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	osscredentials "github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
	"github.com/jwwsjlm/douyinLive/v2/internal/releasegate"
	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
	"github.com/jwwsjlm/douyinLive/v2/internal/update"
)

const (
	defaultBucket = "douyinlive-updates-cn-hangzhou-1e8d9993065b"
	defaultRegion = "cn-hangzhou"
)

type options struct {
	releaseDirectory string
	version          string
	channel          string
	notesFile        string
	signingKey       string
	credentialFile   string
	bucket           string
	region           string
	publish          bool
}

type releaseIdentity struct {
	Version   string
	GitCommit string
	Installer update.FileDescriptor
	Manifest  update.FileDescriptor
}

type releaseInstaller struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Scope  string `json:"scope"`
}

func main() {
	var value options
	flag.StringVar(&value.releaseDirectory, "release-directory", "", "absolute release/vX.Y.Z directory")
	flag.StringVar(&value.version, "version", "", "stable X.Y.Z version")
	flag.StringVar(&value.channel, "channel", "canary", "canary or stable")
	flag.StringVar(&value.notesFile, "notes-file", "", "absolute UTF-8 plain-text release notes")
	flag.StringVar(&value.signingKey, "signing-key", "", "absolute DPAPI LocalMachine Ed25519 key file")
	flag.StringVar(&value.credentialFile, "credential-file", "", "absolute DPAPI LocalMachine OSS publishing credential file")
	flag.StringVar(&value.bucket, "bucket", defaultBucket, "fixed OSS bucket")
	flag.StringVar(&value.region, "region", defaultRegion, "fixed OSS region")
	flag.BoolVar(&value.publish, "publish", false, "upload immutable release objects and finally promote the channel")
	flag.Parse()

	envelope, identity, err := prepareEnvelope(value, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, update.ErrorCode(err))
		os.Exit(1)
	}
	output := filepath.Join(value.releaseDirectory, "update.json")
	if err := writeEnvelope(output, envelope); err != nil {
		fmt.Fprintln(os.Stderr, update.ErrorCode(err))
		os.Exit(1)
	}
	if value.publish {
		if err := verifyPublishingSource(value.version, identity.GitCommit); err != nil {
			fmt.Fprintln(os.Stderr, update.ErrorCode(err))
			os.Exit(1)
		}
		if err := publishRelease(context.Background(), value, identity, envelope); err != nil {
			fmt.Fprintln(os.Stderr, update.ErrorCode(err))
			os.Exit(1)
		}
	}
	fmt.Println("UPDATE_RELEASE_PREPARED")
	fmt.Printf("version=%s\n", identity.Version)
	fmt.Printf("channel=%s\n", value.channel)
	fmt.Printf("published=%t\n", value.publish)
}

func prepareEnvelope(value options, now time.Time) ([]byte, releaseIdentity, error) {
	if !update.ValidVersion(value.version) || (value.channel != "canary" && value.channel != "stable") ||
		value.bucket != defaultBucket || value.region != defaultRegion {
		return nil, releaseIdentity{}, errors.New("UPDATE_PUBLISH_ARGUMENT_INVALID")
	}
	for _, candidate := range []*string{
		&value.releaseDirectory, &value.notesFile, &value.signingKey,
	} {
		absolute, err := filepath.Abs(filepath.Clean(*candidate))
		if err != nil || *candidate == "" || absolute != *candidate {
			return nil, releaseIdentity{}, errors.New("UPDATE_PUBLISH_PATH_INVALID")
		}
	}
	manifestPath := filepath.Join(value.releaseDirectory, "release-manifest.json")
	manifestContent, err := os.ReadFile(manifestPath)
	if err != nil || len(manifestContent) == 0 || len(manifestContent) > update.MaxEnvelope {
		return nil, releaseIdentity{}, errors.New("UPDATE_RELEASE_MANIFEST_INVALID")
	}
	var manifest map[string]json.RawMessage
	if err := update.DecodeStrictJSON(manifestContent, &manifest); err != nil {
		return nil, releaseIdentity{}, errors.New("UPDATE_RELEASE_MANIFEST_INVALID")
	}
	requiredString := func(name string) (string, error) {
		var result string
		if err := json.Unmarshal(manifest[name], &result); err != nil || result == "" {
			return "", errors.New("UPDATE_RELEASE_MANIFEST_INVALID")
		}
		return result, nil
	}
	schema, _ := requiredString("schema")
	product, _ := requiredString("product")
	version, _ := requiredString("version")
	commit, _ := requiredString("gitCommit")
	platform, _ := requiredString("platform")
	var dirty bool
	if err := json.Unmarshal(manifest["dirty"], &dirty); err != nil ||
		schema != releasegate.ManifestSchema || product != update.Product ||
		version != value.version || platform != update.Platform || dirty ||
		len(commit) != 40 {
		return nil, releaseIdentity{}, errors.New("UPDATE_RELEASE_MANIFEST_INVALID")
	}
	var installer releaseInstaller
	if err := update.DecodeStrictJSON(manifest["installer"], &installer); err != nil ||
		installer.Scope != "user" {
		return nil, releaseIdentity{}, errors.New("UPDATE_RELEASE_MANIFEST_INVALID")
	}
	wantInstallerName := "douyin-live-desktop-" + value.version + "-windows-amd64-installer.exe"
	if installer.Path != wantInstallerName {
		return nil, releaseIdentity{}, errors.New("UPDATE_INSTALLER_INVALID")
	}
	installerPath := filepath.Join(value.releaseDirectory, installer.Path)
	installerHash, installerSize, err := releasegate.HashFile(installerPath)
	if err != nil || installerHash != installer.SHA256 || installerSize != installer.Size ||
		installerSize <= 0 || installerSize > update.MaxInstaller {
		return nil, releaseIdentity{}, errors.New("UPDATE_HASH_MISMATCH")
	}
	manifestHash := sha256.Sum256(manifestContent)
	notes, err := os.ReadFile(value.notesFile)
	if err != nil || len(notes) > update.MaxNotes || bytes.IndexByte(notes, 0) >= 0 {
		return nil, releaseIdentity{}, errors.New("UPDATE_RELEASE_NOTES_INVALID")
	}
	keyID, privateKey, err := update.LoadProtectedSigningKey(value.signingKey)
	if err != nil {
		return nil, releaseIdentity{}, fmt.Errorf("UPDATE_SIGNING_KEY_INVALID: %w", err)
	}
	prefix := "releases/v" + value.version + "/"
	payload := update.Payload{
		Schema: update.PayloadSchema, Product: update.Product, Channel: value.channel,
		Version: value.version, PublishedAt: now.UTC().Truncate(time.Second).Format(time.RFC3339),
		GitCommit: commit, Platform: update.Platform,
		DatabaseSchemaVersion: storage.LatestSchemaVersion(), UpdaterProtocol: 1,
		ReleaseNotes: string(notes),
		Installer: update.FileDescriptor{
			ObjectKey: prefix + installer.Path, SHA256: installerHash, Size: installerSize,
		},
		ReleaseManifest: update.FileDescriptor{
			ObjectKey: prefix + "release-manifest.json",
			SHA256:    hex.EncodeToString(manifestHash[:]), Size: int64(len(manifestContent)),
		},
	}
	envelope, err := update.Sign(payload, keyID, privateKey)
	if err != nil {
		return nil, releaseIdentity{}, err
	}
	if _, err := update.VerifyEnvelope(
		envelope, map[string]ed25519.PublicKey{keyID: privateKey.Public().(ed25519.PublicKey)},
		"0.0.0", "", value.channel, update.ProductionBaseURL,
	); err != nil {
		return nil, releaseIdentity{}, err
	}
	return envelope, releaseIdentity{
		Version: value.version, GitCommit: commit,
		Installer: payload.Installer, Manifest: payload.ReleaseManifest,
	}, nil
}

func writeEnvelope(path string, envelope []byte) error {
	if len(envelope) == 0 || len(envelope) > update.MaxEnvelope {
		return errors.New("UPDATE_METADATA_TOO_LARGE")
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".update-envelope-*.tmp")
	if err != nil {
		return fmt.Errorf("UPDATE_ENVELOPE_WRITE_FAILED: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(envelope); err != nil {
		temporary.Close()
		return fmt.Errorf("UPDATE_ENVELOPE_WRITE_FAILED: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("UPDATE_ENVELOPE_WRITE_FAILED: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("UPDATE_ENVELOPE_WRITE_FAILED: %w", err)
	}
	_ = os.Remove(path)
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("UPDATE_ENVELOPE_WRITE_FAILED: %w", err)
	}
	return nil
}

func verifyPublishingSource(version, commit string) error {
	head, err := gitOutput("rev-parse", "HEAD")
	if err != nil || head != commit {
		return errors.New("UPDATE_PUBLISH_COMMIT_MISMATCH")
	}
	status, err := gitOutput("status", "--porcelain=v1", "--untracked-files=no")
	if err != nil || status != "" {
		return errors.New("UPDATE_PUBLISH_WORKTREE_DIRTY")
	}
	tag, err := gitOutput("describe", "--exact-match", "--tags", "HEAD")
	if err != nil || tag != "v"+version {
		return errors.New("UPDATE_PUBLISH_TAG_INVALID")
	}
	return nil
}

func gitOutput(arguments ...string) (string, error) {
	command := exec.Command("git", arguments...)
	output, err := command.Output()
	return strings.TrimSpace(string(output)), err
}

func publishRelease(ctx context.Context, value options, identity releaseIdentity, envelope []byte) error {
	if value.credentialFile == "" || !filepath.IsAbs(value.credentialFile) {
		return errors.New("UPDATE_PUBLISH_CREDENTIAL_PATH_INVALID")
	}
	credentials, err := update.LoadProtectedPublishingCredentials(value.credentialFile)
	if err != nil {
		return fmt.Errorf("UPDATE_PUBLISH_CREDENTIAL_INVALID: %w", err)
	}
	provider := osscredentials.NewStaticCredentialsProvider(
		credentials.AccessKeyID, credentials.AccessKeySecret, credentials.SecurityToken,
	)
	config := oss.LoadDefaultConfig().
		WithRegion(value.region).
		WithCredentialsProvider(provider).
		WithProxyFromEnvironment(true).
		WithEnabledRedirect(false).
		WithUserAgent("DouyinLiveUpdatePublisher/1")
	client := oss.NewClient(config)
	baseURL, err := url.Parse(update.ProductionBaseURL)
	if err != nil {
		return err
	}
	immutableCache := "public, max-age=31536000, immutable"
	releaseFiles := []struct {
		key, path, contentType string
		content                []byte
	}{
		{key: identity.Installer.ObjectKey, path: filepath.Join(value.releaseDirectory, filepath.Base(identity.Installer.ObjectKey)), contentType: "application/vnd.microsoft.portable-executable"},
		{key: identity.Manifest.ObjectKey, path: filepath.Join(value.releaseDirectory, "release-manifest.json"), contentType: "application/json"},
		{key: "releases/v" + value.version + "/update.json", content: envelope, contentType: "application/json"},
	}
	for _, file := range releaseFiles {
		if err := ensureObjectAbsent(ctx, client, value.bucket, file.key); err != nil {
			return err
		}
		var body io.Reader
		var size int64
		if file.content != nil {
			body = bytes.NewReader(file.content)
			size = int64(len(file.content))
		} else {
			handle, err := os.Open(file.path)
			if err != nil {
				return err
			}
			defer handle.Close()
			info, err := handle.Stat()
			if err != nil {
				return err
			}
			body, size = handle, info.Size()
		}
		forbid := "true"
		encryption := "AES256"
		_, err := client.PutObject(ctx, &oss.PutObjectRequest{
			Bucket: oss.Ptr(value.bucket), Key: oss.Ptr(file.key), Body: body,
			ContentLength: oss.Ptr(size), ContentType: oss.Ptr(file.contentType),
			CacheControl: oss.Ptr(immutableCache), ForbidOverwrite: &forbid,
			ServerSideEncryption: &encryption,
		})
		if err != nil {
			return fmt.Errorf("UPDATE_OSS_UPLOAD_FAILED: %w", err)
		}
		expectedHash := ""
		if file.content != nil {
			digest := sha256.Sum256(file.content)
			expectedHash = hex.EncodeToString(digest[:])
		} else {
			expectedHash, _, err = releasegate.HashFile(file.path)
			if err != nil {
				return err
			}
		}
		if err := verifyAnonymousObject(ctx, baseURL, file.key, size, expectedHash); err != nil {
			return err
		}
	}
	channelKey := "channels/" + value.channel + ".json"
	encryption := "AES256"
	noCache := "no-cache, max-age=0"
	_, err = client.PutObject(ctx, &oss.PutObjectRequest{
		Bucket: oss.Ptr(value.bucket), Key: oss.Ptr(channelKey),
		Body: bytes.NewReader(envelope), ContentLength: oss.Ptr(int64(len(envelope))),
		ContentType: oss.Ptr("application/json"), CacheControl: &noCache,
		ServerSideEncryption: &encryption,
	})
	if err != nil {
		return fmt.Errorf("UPDATE_OSS_CHANNEL_PROMOTE_FAILED: %w", err)
	}
	digest := sha256.Sum256(envelope)
	return verifyAnonymousObject(
		ctx, baseURL, channelKey, int64(len(envelope)), hex.EncodeToString(digest[:]),
	)
}

func ensureObjectAbsent(ctx context.Context, client *oss.Client, bucket, key string) error {
	_, err := client.HeadObject(ctx, &oss.HeadObjectRequest{
		Bucket: oss.Ptr(bucket), Key: oss.Ptr(key),
	})
	if err == nil {
		return errors.New("UPDATE_OSS_RELEASE_OBJECT_EXISTS")
	}
	var serviceError *oss.ServiceError
	if errors.As(err, &serviceError) && serviceError.StatusCode == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("UPDATE_OSS_OBJECT_INSPECT_FAILED: %w", err)
}

func verifyAnonymousObject(ctx context.Context, origin *url.URL, key string, size int64, digest string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, update.ObjectURL(origin, key), nil)
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout: 30 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirect rejected")
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("UPDATE_OSS_ANONYMOUS_VERIFY_FAILED: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Request == nil ||
		response.Request.URL.Scheme != origin.Scheme || response.Request.URL.Host != origin.Host {
		return errors.New("UPDATE_OSS_ANONYMOUS_VERIFY_FAILED")
	}
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(response.Body, size+1))
	if err != nil || count != size || hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("UPDATE_OSS_ANONYMOUS_HASH_MISMATCH")
	}
	return nil
}
