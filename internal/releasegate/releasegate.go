// Package releasegate implements the deterministic, offline-auditable desktop
// release gate. It deliberately uses only repository-pinned Go dependencies
// and locally installed locked tools.
package releasegate

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	ManifestSchema = "douyinlive-release-manifest/v1"
	LicenseSchema  = "douyinlive-license-manifest/v1"
	SBOMSchema     = "SPDX-2.3"
	modulePath     = "github.com/jwwsjlm/douyinLive/v2"
)

var semverPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$`)

type FFmpegLock struct {
	SchemaVersion int    `json:"schemaVersion"`
	Distribution  string `json:"distribution"`
	Version       string `json:"version"`
	Archive       struct {
		URL    string `json:"url"`
		SHA256 string `json:"sha256"`
	} `json:"archive"`
	License      string            `json:"license"`
	SourceCommit string            `json:"sourceCommit"`
	Binaries     map[string]string `json:"binaries"`
}

type WebView2BootstrapperLock struct {
	SchemaVersion      int    `json:"schemaVersion"`
	Distribution       string `json:"distribution"`
	Version            string `json:"version"`
	URL                string `json:"url"`
	SHA256             string `json:"sha256"`
	Size               int64  `json:"size"`
	AuthenticodeSigner string `json:"authenticodeSigner"`
	OriginalFilename   string `json:"originalFilename"`
	License            string `json:"license"`
}

type GitMetadata struct {
	Commit    string
	BuildTime string
	Dirty     bool
}

type Component struct {
	Name              string `json:"name"`
	Version           string `json:"version"`
	Ecosystem         string `json:"ecosystem"`
	License           string `json:"license"`
	PURL              string `json:"purl"`
	LicenseFile       string `json:"licenseFile,omitempty"`
	LicenseFileSHA256 string `json:"licenseFileSHA256,omitempty"`
	licenseText       string
}

type ScanFinding struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Rule string `json:"rule"`
}

type BuildOptions struct {
	RepoRoot             string
	Version              string
	OutputRoot           string
	BuildSource          string
	WebView2Bootstrapper string
	AllowDirty           bool
	SkipBuild            bool
}

type Result struct {
	OutputDirectory    string
	ArtifactPath       string
	ArtifactSHA256     string
	ArtifactSize       int64
	InstallerPath      string
	InstallerSHA256    string
	InstallerSize      int64
	RollbackPath       string
	RollbackSHA256     string
	RollbackSize       int64
	UpdateHelperPath   string
	UpdateHelperSHA256 string
	UpdateHelperSize   int64
	Metadata           GitMetadata
	ComponentCount     int
	ScanFileCount      int
}

func FindRepoRoot() (string, error) {
	output, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("resolve repository root: %w", err)
	}
	root := strings.TrimSpace(string(output))
	if root == "" || !filepath.IsAbs(root) {
		return "", errors.New("repository root is not absolute")
	}
	return filepath.Clean(root), nil
}

func ReadGitMetadata(root string) (GitMetadata, error) {
	commit, err := commandOutput(root, "git", "rev-parse", "HEAD")
	if err != nil {
		return GitMetadata{}, err
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(commit) {
		return GitMetadata{}, fmt.Errorf("invalid full git commit %q", commit)
	}
	buildTime, err := commandOutput(root, "git", "show", "-s", "--format=%cI", "HEAD")
	if err != nil {
		return GitMetadata{}, err
	}
	parsed, err := time.Parse(time.RFC3339, buildTime)
	if err != nil {
		return GitMetadata{}, fmt.Errorf("invalid commit timestamp %q: %w", buildTime, err)
	}
	status, err := commandOutput(root, "git", "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return GitMetadata{}, err
	}
	return GitMetadata{Commit: commit, BuildTime: parsed.Format(time.RFC3339), Dirty: status != ""}, nil
}

func ValidateVersion(root, version string) error {
	if !semverPattern.MatchString(version) {
		return fmt.Errorf("version %q is not SemVer without a v prefix", version)
	}
	var wails struct {
		Info struct {
			ProductVersion string `json:"productVersion"`
		} `json:"info"`
	}
	if err := readJSONLenient(filepath.Join(root, "cmd", "desktop", "wails.json"), &wails); err != nil {
		return err
	}
	var frontend struct {
		Version        string `json:"version"`
		PackageManager string `json:"packageManager"`
	}
	if err := readJSONLenient(filepath.Join(root, "frontend", "package.json"), &frontend); err != nil {
		return err
	}
	if wails.Info.ProductVersion != version || frontend.Version != version {
		return fmt.Errorf("version mismatch: requested=%s wails=%s frontend=%s", version, wails.Info.ProductVersion, frontend.Version)
	}
	if !regexp.MustCompile(`^pnpm@[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(frontend.PackageManager) {
		return fmt.Errorf("frontend packageManager is not exactly pinned: %q", frontend.PackageManager)
	}
	return nil
}

func LoadFFmpegLock(path string) (FFmpegLock, error) {
	var lock FFmpegLock
	if err := readJSON(path, &lock); err != nil {
		return FFmpegLock{}, err
	}
	if lock.SchemaVersion != 1 || lock.Distribution == "" || lock.Version == "" || lock.License != "GPL-3.0-or-later" {
		return FFmpegLock{}, errors.New("invalid FFmpeg lock identity")
	}
	if !validSHA256(lock.Archive.SHA256) || !strings.HasPrefix(lock.Archive.URL, "https://www.gyan.dev/ffmpeg/builds/packages/") {
		return FFmpegLock{}, errors.New("invalid FFmpeg archive lock")
	}
	if !regexp.MustCompile(`^[0-9a-f]{7,40}$`).MatchString(lock.SourceCommit) {
		return FFmpegLock{}, errors.New("invalid FFmpeg source commit lock")
	}
	for _, name := range []string{"ffmpeg.exe", "ffprobe.exe"} {
		if !validSHA256(lock.Binaries[name]) {
			return FFmpegLock{}, fmt.Errorf("invalid FFmpeg binary lock for %s", name)
		}
	}
	return lock, nil
}

func LoadWebView2BootstrapperLock(path string) (WebView2BootstrapperLock, error) {
	var lock WebView2BootstrapperLock
	if err := readJSON(path, &lock); err != nil {
		return WebView2BootstrapperLock{}, err
	}
	if lock.SchemaVersion != 1 ||
		lock.Distribution != "Microsoft Edge WebView2 Evergreen Bootstrapper" ||
		!regexp.MustCompile(`^[0-9]+(?:\.[0-9]+){3}$`).MatchString(lock.Version) ||
		lock.URL != "https://go.microsoft.com/fwlink/p/?LinkId=2124703" ||
		!validSHA256(lock.SHA256) || lock.Size <= 0 ||
		lock.AuthenticodeSigner != "CN=Microsoft Corporation, O=Microsoft Corporation, L=Redmond, S=Washington, C=US" ||
		lock.OriginalFilename != "MicrosoftEdgeUpdateSetup.exe" ||
		lock.License != "LicenseRef-Microsoft-WebView2" {
		return WebView2BootstrapperLock{}, errors.New("invalid WebView2 bootstrapper lock")
	}
	return lock, nil
}

func VerifyWebView2Bootstrapper(path string, lock WebView2BootstrapperLock) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New("WebView2 bootstrapper path must be absolute")
	}
	absolute := filepath.Clean(path)
	info, err := os.Stat(absolute)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("WebView2 bootstrapper is not a regular file")
	}
	digest, size, err := HashFile(absolute)
	if err != nil {
		return "", err
	}
	if digest != lock.SHA256 || size != lock.Size {
		return "", fmt.Errorf("WebView2 bootstrapper identity mismatch: got %s/%d", digest, size)
	}
	return absolute, nil
}

func VerifyInstalledFFmpeg(lock FFmpegLock) error {
	for _, name := range []string{"ffmpeg.exe", "ffprobe.exe"} {
		path, err := exec.LookPath(strings.TrimSuffix(name, ".exe"))
		if err != nil {
			return fmt.Errorf("locate %s: %w", name, err)
		}
		digest, _, err := HashFile(path)
		if err != nil {
			return err
		}
		if digest != lock.Binaries[name] {
			return fmt.Errorf("%s checksum mismatch: got %s", name, digest)
		}
	}
	line, err := commandOutput("", "ffmpeg", "-version")
	if err != nil {
		return err
	}
	first, _, _ := strings.Cut(line, "\n")
	if !strings.Contains(first, "ffmpeg version "+lock.Version) {
		return fmt.Errorf("FFmpeg version mismatch: %q", first)
	}
	return nil
}

func Run(options BuildOptions) (Result, error) {
	root := filepath.Clean(options.RepoRoot)
	if root == "." || !filepath.IsAbs(root) {
		return Result{}, errors.New("release repository root must be absolute")
	}
	if err := ValidateVersion(root, options.Version); err != nil {
		return Result{}, err
	}
	metadata, err := ReadGitMetadata(root)
	if err != nil {
		return Result{}, err
	}
	if metadata.Dirty && !options.AllowDirty {
		return Result{}, errors.New("release worktree is dirty; commit or clean it before release")
	}
	if options.BuildSource == "" {
		options.BuildSource = "local-release"
	}
	if !regexp.MustCompile(`^[A-Za-z0-9._/-]{1,128}$`).MatchString(options.BuildSource) {
		return Result{}, fmt.Errorf("invalid build source %q", options.BuildSource)
	}
	lockPath := filepath.Join(root, "build", "ffmpeg-windows-amd64.lock.json")
	lock, err := LoadFFmpegLock(lockPath)
	if err != nil {
		return Result{}, err
	}
	webView2LockPath := filepath.Join(root, "build", "webview2-bootstrapper-windows.lock.json")
	webView2Lock, err := LoadWebView2BootstrapperLock(webView2LockPath)
	if err != nil {
		return Result{}, err
	}
	if err := VerifyInstalledFFmpeg(lock); err != nil {
		return Result{}, err
	}
	findings, scanned, err := ScanTrackedFiles(root)
	if err != nil {
		return Result{}, err
	}
	if len(findings) != 0 {
		return Result{}, fmt.Errorf("sensitive scan rejected %d finding(s): %s", len(findings), summarizeFindings(findings))
	}
	outputRoot := options.OutputRoot
	if outputRoot == "" {
		outputRoot = filepath.Join(root, "release")
	}
	if !filepath.IsAbs(outputRoot) {
		outputRoot = filepath.Join(root, outputRoot)
	}
	outputRoot = filepath.Clean(outputRoot)
	if !isWithin(root, outputRoot) || outputRoot == root {
		return Result{}, errors.New("release output must be a child of the repository")
	}
	outDir := filepath.Join(outputRoot, "v"+options.Version)
	if !isWithin(outputRoot, outDir) {
		return Result{}, errors.New("invalid version output path")
	}
	if err := os.RemoveAll(outDir); err != nil {
		return Result{}, fmt.Errorf("clean exact release output: %w", err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return Result{}, err
	}
	artifact := filepath.Join(outDir, "douyin-live-desktop-"+options.Version+"-windows-amd64.exe")
	rollback := filepath.Join(outDir, "douyin-live-dbrollback-"+options.Version+"-windows-amd64.exe")
	updateHelper := filepath.Join(outDir, "douyin-live-updater-"+options.Version+"-windows-amd64.exe")
	installer := filepath.Join(outDir, "douyin-live-desktop-"+options.Version+"-windows-amd64-installer.exe")
	var artifactHash string
	var artifactSize int64
	var rollbackHash string
	var rollbackSize int64
	var updateHelperHash string
	var updateHelperSize int64
	var installerHash string
	var installerSize int64
	var webView2Bootstrapper string
	if !options.SkipBuild {
		nodeVersion, err := commandOutput(root, "node", "--version")
		if err != nil {
			return Result{}, err
		}
		if _, err := commandOutput(root, "pnpm", "--dir", "frontend", "install", "--frozen-lockfile"); err != nil {
			return Result{}, err
		}
		artifactHash, artifactSize, err = reproducibleDesktopBuild(root, artifact, options.Version, metadata, options.BuildSource, nodeVersion)
		if err != nil {
			return Result{}, err
		}
		rollbackHash, rollbackSize, err = reproducibleGoToolBuild(root, "./cmd/dbrollback", rollback)
		if err != nil {
			return Result{}, err
		}
		updateHelperHash, updateHelperSize, err = reproducibleGoToolBuild(root, "./cmd/updatehelper", updateHelper)
		if err != nil {
			return Result{}, err
		}
		webView2Bootstrapper, err = VerifyWebView2Bootstrapper(options.WebView2Bootstrapper, webView2Lock)
		if err != nil {
			return Result{}, err
		}
	}
	components, err := CollectComponents(root, lock, webView2Lock, options.Version)
	if err != nil {
		return Result{}, err
	}
	if err := writeReleaseDocuments(root, outDir, options, metadata, lock, webView2Lock, components, findings, scanned, artifact, artifactHash, artifactSize); err != nil {
		return Result{}, err
	}
	if !options.SkipBuild {
		installerHash, installerSize, err = buildWindowsInstaller(
			root, outDir, installer, artifact, rollback, updateHelper,
			webView2Bootstrapper, options.Version, lock,
		)
		if err != nil {
			return Result{}, err
		}
		if err := refreshInstallerManifest(outDir, installer, installerHash, installerSize, rollback, rollbackHash, rollbackSize, updateHelper, updateHelperHash, updateHelperSize); err != nil {
			return Result{}, err
		}
	}
	return Result{OutputDirectory: outDir, ArtifactPath: artifact, ArtifactSHA256: artifactHash,
		ArtifactSize: artifactSize, InstallerPath: installer, InstallerSHA256: installerHash,
		InstallerSize: installerSize, RollbackPath: rollback, RollbackSHA256: rollbackHash,
		RollbackSize: rollbackSize, UpdateHelperPath: updateHelper, UpdateHelperSHA256: updateHelperHash,
		UpdateHelperSize: updateHelperSize, Metadata: metadata, ComponentCount: len(components), ScanFileCount: scanned}, nil
}

func reproducibleDesktopBuild(root, artifact, version string, metadata GitMetadata, source, nodeVersion string) (string, int64, error) {
	buildDir := filepath.Join(root, "cmd", "desktop")
	ldflags := strings.Join([]string{
		"-s", "-w", "-buildid=",
		"-X", modulePath + "/internal/buildinfo.productVersion=" + version,
		"-X", modulePath + "/internal/buildinfo.gitCommit=" + metadata.Commit,
		"-X", modulePath + "/internal/buildinfo.buildTime=" + metadata.BuildTime,
		"-X", modulePath + "/internal/buildinfo.buildSource=" + source,
		"-X", modulePath + "/internal/buildinfo.nodeVersion=" + nodeVersion,
	}, " ")
	args := []string{"build", "-clean", "-platform", "windows/amd64", "-trimpath", "-m", "-nosyncgomod", "-nocolour", "-o", "douyin-live-desktop.exe", "-ldflags", ldflags}
	epoch, err := time.Parse(time.RFC3339, metadata.BuildTime)
	if err != nil {
		return "", 0, err
	}
	environment := append(os.Environ(), "SOURCE_DATE_EPOCH="+fmt.Sprint(epoch.Unix()), "TZ=UTC")
	buildOnce := func() (string, int64, error) {
		command := exec.Command("wails", args...)
		command.Dir = buildDir
		command.Env = environment
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Run(); err != nil {
			return "", 0, fmt.Errorf("Wails release build: %w", err)
		}
		built := filepath.Join(buildDir, "build", "bin", "douyin-live-desktop.exe")
		if err := validateBuildInfo(built, metadata.Commit, version, metadata.BuildTime, source); err != nil {
			return "", 0, err
		}
		return HashFile(built)
	}
	firstHash, firstSize, err := buildOnce()
	if err != nil {
		return "", 0, err
	}
	firstBuilt := filepath.Join(buildDir, "build", "bin", "douyin-live-desktop.exe")
	if err := copyFile(firstBuilt, artifact); err != nil {
		return "", 0, err
	}
	secondHash, secondSize, err := buildOnce()
	if err != nil {
		return "", 0, err
	}
	if firstHash != secondHash || firstSize != secondSize {
		return "", 0, fmt.Errorf("reproducibility mismatch: first=%s/%d second=%s/%d", firstHash, firstSize, secondHash, secondSize)
	}
	if copiedHash, copiedSize, err := HashFile(artifact); err != nil || copiedHash != firstHash || copiedSize != firstSize {
		return "", 0, fmt.Errorf("copied artifact identity mismatch: hash=%s size=%d err=%v", copiedHash, copiedSize, err)
	}
	return firstHash, firstSize, nil
}

func validateBuildInfo(path, commit, version, buildTime, source string) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read Go build info: %w", err)
	}
	if info.Path != modulePath+"/cmd/desktop" {
		return fmt.Errorf("unexpected desktop module path %q", info.Path)
	}
	settings := make(map[string]string)
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["-trimpath"] != "true" {
		return errors.New("desktop build info is missing trimpath")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read desktop artifact: %w", err)
	}
	for name, value := range map[string]string{
		"product version": version,
		"git commit":      commit,
		"build time":      buildTime,
		"build source":    source,
	} {
		if !bytes.Contains(content, []byte(value)) {
			return fmt.Errorf("desktop artifact is missing injected %s", name)
		}
	}
	return nil
}

func ScanTrackedFiles(root string) ([]ScanFinding, int, error) {
	command := exec.Command("git", "ls-files", "-z")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		return nil, 0, fmt.Errorf("list tracked files: %w", err)
	}
	var findings []ScanFinding
	count := 0
	for _, raw := range bytes.Split(output, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		relative := filepath.ToSlash(string(raw))
		if !scanExtension(relative) {
			continue
		}
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			return nil, count, err
		}
		if len(content) > 8<<20 || bytes.IndexByte(content, 0) >= 0 {
			continue
		}
		count++
		findings = append(findings, scanContent(relative, content)...)
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Path != findings[j].Path {
			return findings[i].Path < findings[j].Path
		}
		if findings[i].Line != findings[j].Line {
			return findings[i].Line < findings[j].Line
		}
		return findings[i].Rule < findings[j].Rule
	})
	return findings, count, nil
}

func scanContent(path string, content []byte) []ScanFinding {
	privateKey := regexp.MustCompile(`-----BEGIN (RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----`)
	githubToken := regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{30,}`)
	awsKey := regexp.MustCompile(`AKIA[A-Z0-9]{16}`)
	assignment := regexp.MustCompile(`(?i)(cookie|mstoken|a_bogus|signature)["']?\s*[:=]\s*["']([A-Za-z0-9_+%./=-]{16,})["']`)
	query := regexp.MustCompile(`(?i)(?:[?&]|\\u0026)(cookie|mstoken|a_bogus|(?:x-)?signature)=([^&\\\s"']{12,})`)
	rules := []struct {
		name string
		re   *regexp.Regexp
	}{{"private-key", privateKey}, {"github-token", githubToken}, {"aws-access-key", awsKey}, {"sensitive-assignment", assignment}, {"signed-url-query", query}}
	var findings []ScanFinding
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		for _, rule := range rules {
			matches := rule.re.FindAllStringSubmatch(text, -1)
			for _, match := range matches {
				candidate := match[0]
				if len(match) > 2 {
					candidate = match[len(match)-1]
				}
				if isPlaceholder(candidate) {
					continue
				}
				findings = append(findings, ScanFinding{Path: path, Line: line, Rule: rule.name})
			}
		}
	}
	return findings
}

func isPlaceholder(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{"placeholder", "redacted", "example", "invalid", "secret", "fake", "mock", "dummy", "encoded", "base64", "replace", "abc", "xyz", "xxx", "${{"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	trimmed := strings.Trim(value, "\"' ")
	if len(trimmed) >= 12 && strings.Count(trimmed, string(trimmed[0])) == len(trimmed) {
		return true
	}
	return false
}

func summarizeFindings(findings []ScanFinding) string {
	const limit = 50
	parts := make([]string, 0, min(len(findings), limit)+1)
	for index, finding := range findings {
		if index == limit {
			parts = append(parts, fmt.Sprintf("and %d more", len(findings)-limit))
			break
		}
		parts = append(parts, fmt.Sprintf("%s:%d %s", finding.Path, finding.Line, finding.Rule))
	}
	return strings.Join(parts, "; ")
}

func CollectComponents(root string, lock FFmpegLock, webView2Lock WebView2BootstrapperLock, productVersion string) ([]Component, error) {
	components, err := collectGoComponents(root, productVersion)
	if err != nil {
		return nil, err
	}
	npm, err := collectNPMComponents(filepath.Join(root, "frontend", "node_modules"))
	if err != nil {
		return nil, err
	}
	components = append(components, npm...)
	components = append(components, Component{Name: "ffmpeg-gyan-essentials", Version: lock.Version,
		Ecosystem: "binary", License: lock.License, PURL: "pkg:generic/ffmpeg-gyan-essentials@" + lock.Version})
	components = append(components, Component{Name: "microsoft-edge-webview2-evergreen-bootstrapper",
		Version: webView2Lock.Version, Ecosystem: "binary", License: webView2Lock.License,
		PURL: "pkg:generic/microsoft-edge-webview2-evergreen-bootstrapper@" + webView2Lock.Version})
	sort.Slice(components, func(i, j int) bool {
		if components[i].Ecosystem != components[j].Ecosystem {
			return components[i].Ecosystem < components[j].Ecosystem
		}
		if components[i].Name != components[j].Name {
			return components[i].Name < components[j].Name
		}
		return components[i].Version < components[j].Version
	})
	return components, nil
}

type goModule struct {
	Path    string
	Version string
	Dir     string
	GoMod   string
	Main    bool
}

func collectGoComponents(root, productVersion string) ([]Component, error) {
	command := exec.Command("go", "list", "-deps", "-json", "./cmd/desktop")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("Go desktop dependency inventory: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	modules := make(map[string]goModule)
	for {
		var pkg struct {
			Module *goModule
		}
		if err := decoder.Decode(&pkg); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		if pkg.Module == nil || pkg.Module.Path == "" {
			continue
		}
		module := *pkg.Module
		if module.Dir == "" && module.GoMod != "" {
			module.Dir = filepath.Dir(module.GoMod)
		}
		modules[module.Path+"@"+module.Version] = module
	}
	components := make([]Component, 0, len(modules))
	for _, module := range modules {
		version := module.Version
		if module.Main {
			version = productVersion
		}
		if module.Dir == "" {
			return nil, fmt.Errorf("Go module %s has no local source directory", module.Path)
		}
		license, filename, digest, text, err := licenseEvidence(module.Dir, "")
		if err != nil {
			return nil, fmt.Errorf("Go module %s: %w", module.Path, err)
		}
		components = append(components, Component{Name: module.Path, Version: version, Ecosystem: "go",
			License: license, PURL: "pkg:golang/" + module.Path + "@" + version,
			LicenseFile: filename, LicenseFileSHA256: digest, licenseText: text})
	}
	return components, nil
}

func collectNPMComponents(nodeModules string) ([]Component, error) {
	if _, err := os.Stat(nodeModules); err != nil {
		return nil, fmt.Errorf("frontend node_modules unavailable; run frozen install: %w", err)
	}
	seen := make(map[string]Component)
	err := filepath.WalkDir(nodeModules, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != "package.json" || !strings.Contains(filepath.ToSlash(path), "/node_modules/") {
			return nil
		}
		var manifest struct {
			Name    string          `json:"name"`
			Version string          `json:"version"`
			License json.RawMessage `json:"license"`
		}
		if err := readJSONLenient(path, &manifest); err != nil {
			return nil
		}
		if manifest.Name == "" || manifest.Version == "" {
			return nil
		}
		license := parseNPMDeclaredLicense(manifest.License)
		if license == "" {
			return fmt.Errorf("npm package %s@%s has no declared license", manifest.Name, manifest.Version)
		}
		_, filename, digest, text, _ := licenseEvidence(filepath.Dir(path), license)
		key := manifest.Name + "@" + manifest.Version
		seen[key] = Component{Name: manifest.Name, Version: manifest.Version, Ecosystem: "npm",
			License: license, PURL: "pkg:npm/" + strings.ReplaceAll(manifest.Name, "@", "%40") + "@" + manifest.Version,
			LicenseFile: filename, LicenseFileSHA256: digest, licenseText: text}
		return nil
	})
	if err != nil {
		return nil, err
	}
	components := make([]Component, 0, len(seen))
	for _, component := range seen {
		components = append(components, component)
	}
	return components, nil
}

func parseNPMDeclaredLicense(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return strings.TrimSpace(value)
	}
	var object struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &object) == nil {
		return strings.TrimSpace(object.Type)
	}
	return ""
}

func licenseEvidence(directory, declared string) (string, string, string, string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", "", "", "", err
	}
	for _, entry := range entries {
		upper := strings.ToUpper(entry.Name())
		if entry.IsDir() || !(strings.HasPrefix(upper, "LICENSE") || strings.HasPrefix(upper, "LICENCE") || strings.HasPrefix(upper, "COPYING")) {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			return "", "", "", "", err
		}
		license := declared
		if license == "" {
			license = detectLicense(string(content))
		}
		if license == "" {
			digest := sha256.Sum256(content)
			license = "LicenseRef-" + hex.EncodeToString(digest[:6])
		}
		digest := sha256.Sum256(content)
		return license, entry.Name(), hex.EncodeToString(digest[:]), string(content), nil
	}
	if declared != "" {
		return declared, "", "", "", nil
	}
	// SPDX permits NOASSERTION when an upstream package does not ship a
	// license declaration. Preserve that uncertainty in the generated
	// inventory instead of inventing a license or silently dropping the
	// dependency from the SBOM.
	return "NOASSERTION", "", "", "", nil
}

func detectLicense(text string) string {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "apache license") && strings.Contains(lower, "version 2.0"):
		return "Apache-2.0"
	case strings.Contains(lower, "mozilla public license") && strings.Contains(lower, "2.0"):
		return "MPL-2.0"
	case strings.Contains(lower, "boost software license"):
		return "BSL-1.0"
	case strings.Contains(lower, "the unlicense"):
		return "Unlicense"
	case strings.Contains(lower, "permission to use, copy, modify, and/or distribute this software for any purpose"):
		return "ISC"
	case strings.Contains(lower, "redistribution and use in source and binary forms") && strings.Contains(lower, "neither the name"):
		return "BSD-3-Clause"
	case strings.Contains(lower, "redistribution and use in source and binary forms"):
		return "BSD-2-Clause"
	case strings.Contains(lower, "permission is hereby granted, free of charge"):
		return "MIT"
	default:
		return ""
	}
}

func writeReleaseDocuments(root, outDir string, options BuildOptions, metadata GitMetadata, lock FFmpegLock, webView2Lock WebView2BootstrapperLock, components []Component, findings []ScanFinding, scanned int, artifact, artifactHash string, artifactSize int64) error {
	if err := copyFile(filepath.Join(root, "LICENSE"), filepath.Join(outDir, "LICENSE.txt")); err != nil {
		return err
	}
	if err := copyFile(filepath.Join(root, "build", "ffmpeg-windows-amd64.lock.json"), filepath.Join(outDir, "ffmpeg-windows-amd64.lock.json")); err != nil {
		return err
	}
	if err := copyFile(filepath.Join(root, "build", "webview2-bootstrapper-windows.lock.json"), filepath.Join(outDir, "webview2-bootstrapper-windows.lock.json")); err != nil {
		return err
	}
	if err := copyFile(filepath.Join(root, "docs", "windows-installation-and-rollback.md"), filepath.Join(outDir, "INSTALLATION.md")); err != nil {
		return err
	}
	for source, target := range map[string]string{
		"user-guide.md":        "USER-GUIDE.md",
		"privacy.md":           "PRIVACY.md",
		"known-limitations.md": "KNOWN-LIMITATIONS.md",
		"release-checklist.md": "RELEASE-CHECKLIST.md",
	} {
		if err := copyFile(filepath.Join(root, "docs", source), filepath.Join(outDir, target)); err != nil {
			return err
		}
	}
	publicComponents := make([]Component, len(components))
	copy(publicComponents, components)
	for index := range publicComponents {
		publicComponents[index].licenseText = ""
	}
	licenseManifest := map[string]any{"schema": LicenseSchema, "productVersion": options.Version,
		"gitCommit": metadata.Commit, "generatedAt": metadata.BuildTime, "components": publicComponents}
	if err := writeJSON(filepath.Join(outDir, "licenses.json"), licenseManifest); err != nil {
		return err
	}
	if err := writeNotices(filepath.Join(outDir, "THIRD-PARTY-NOTICES.txt"), components); err != nil {
		return err
	}
	if err := writeSPDX(filepath.Join(outDir, "sbom.spdx.json"), options.Version, metadata, components); err != nil {
		return err
	}
	scanReport := map[string]any{"schema": "douyinlive-sensitive-scan/v1", "gitCommit": metadata.Commit,
		"scannedFiles": scanned, "findingCount": len(findings), "findings": findings}
	if err := writeJSON(filepath.Join(outDir, "sensitive-scan.json"), scanReport); err != nil {
		return err
	}
	files, err := manifestFiles(outDir)
	if err != nil {
		return err
	}
	manifest := map[string]any{
		"schema": ManifestSchema, "product": "douyin-live-desktop", "version": options.Version,
		"gitCommit": metadata.Commit, "buildTime": metadata.BuildTime, "buildSource": options.BuildSource,
		"dirty": metadata.Dirty, "platform": "windows/amd64", "reproducible": !options.SkipBuild,
		"artifact":       map[string]any{"path": filepath.Base(artifact), "sha256": artifactHash, "size": artifactSize},
		"ffmpeg":         map[string]any{"version": lock.Version, "archiveURL": lock.Archive.URL, "archiveSHA256": lock.Archive.SHA256, "license": lock.License},
		"componentCount": len(components), "sensitiveScan": map[string]any{"scannedFiles": scanned, "findingCount": len(findings)},
		"files": files,
	}
	return writeJSON(filepath.Join(outDir, "release-manifest.json"), manifest)
}

func writeSPDX(path, version string, metadata GitMetadata, components []Component) error {
	packages := make([]map[string]any, 0, len(components)+1)
	packages = append(packages, map[string]any{"name": "douyin-live-desktop", "SPDXID": "SPDXRef-Product", "versionInfo": version,
		"downloadLocation": "NOASSERTION", "filesAnalyzed": false, "licenseConcluded": "NOASSERTION", "licenseDeclared": "MIT",
		"copyrightText": "NOASSERTION"})
	relationships := []map[string]string{{"spdxElementId": "SPDXRef-DOCUMENT", "relationshipType": "DESCRIBES", "relatedSpdxElement": "SPDXRef-Product"}}
	for _, component := range components {
		digest := sha256.Sum256([]byte(component.PURL))
		id := "SPDXRef-Package-" + hex.EncodeToString(digest[:8])
		packages = append(packages, map[string]any{"name": component.Name, "SPDXID": id, "versionInfo": component.Version,
			"downloadLocation": "NOASSERTION", "filesAnalyzed": false, "licenseConcluded": "NOASSERTION",
			"licenseDeclared": component.License, "copyrightText": "NOASSERTION",
			"externalRefs": []map[string]string{{"referenceCategory": "PACKAGE-MANAGER", "referenceType": "purl", "referenceLocator": component.PURL}}})
		relationships = append(relationships, map[string]string{"spdxElementId": "SPDXRef-Product", "relationshipType": "DEPENDS_ON", "relatedSpdxElement": id})
	}
	document := map[string]any{"spdxVersion": SBOMSchema, "dataLicense": "CC0-1.0", "SPDXID": "SPDXRef-DOCUMENT",
		"name": "douyin-live-desktop-" + version, "documentNamespace": "https://github.com/memeda230606/douyinLive/sbom/" + version + "/" + metadata.Commit,
		"creationInfo": map[string]any{"created": metadata.BuildTime, "creators": []string{"Tool: douyinLive-releasebuilder"}},
		"packages":     packages, "relationships": relationships}
	return writeJSON(path, document)
}

func writeNotices(path string, components []Component) error {
	var buffer strings.Builder
	buffer.WriteString("DouyinLive third-party notices\nGenerated from locked local dependencies.\n")
	seen := make(map[string]struct{})
	for _, component := range components {
		buffer.WriteString("\n- " + component.Ecosystem + ":" + component.Name + "@" + component.Version + " — " + component.License + "\n")
		if component.licenseText == "" || component.LicenseFileSHA256 == "" {
			continue
		}
		if _, exists := seen[component.LicenseFileSHA256]; exists {
			continue
		}
		seen[component.LicenseFileSHA256] = struct{}{}
		buffer.WriteString("\n----- " + component.License + " / " + component.LicenseFileSHA256 + " -----\n")
		buffer.WriteString(strings.ReplaceAll(component.licenseText, "\r\n", "\n"))
		if !strings.HasSuffix(buffer.String(), "\n") {
			buffer.WriteByte('\n')
		}
	}
	return os.WriteFile(path, []byte(buffer.String()), 0o644)
}

func manifestFiles(directory string) ([]map[string]any, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	var files []map[string]any
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "release-manifest.json" {
			continue
		}
		digest, size, err := HashFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		files = append(files, map[string]any{"path": entry.Name(), "sha256": digest, "size": size})
	}
	sort.Slice(files, func(i, j int) bool { return files[i]["path"].(string) < files[j]["path"].(string) })
	return files, nil
}

func HashFile(path string) (string, int64, error) {
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

func copyFile(source, target string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	return errors.Join(copyErr, closeErr)
}

func readJSON(path string, target any) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return nil
}

func readJSONLenient(path string, target any) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(content, target); err != nil {
		return fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return nil
}

func writeJSON(path string, value any) error {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	return os.WriteFile(path, buffer.Bytes(), 0o644)
}

func commandOutput(directory, name string, args ...string) (string, error) {
	command := exec.Command(name, args...)
	if directory != "" {
		command.Dir = directory
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(strings.ReplaceAll(string(output), "\r\n", "\n")), nil
}

func validSHA256(value string) bool { return regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(value) }

func isWithin(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func scanExtension(path string) bool {
	extension := strings.ToLower(filepath.Ext(path))
	switch extension {
	case ".go", ".ts", ".tsx", ".js", ".mjs", ".json", ".yaml", ".yml", ".md", ".txt", ".ps1", ".nsi", ".nsh", ".html", ".css", ".mod", ".sum":
		return true
	default:
		return filepath.Base(path) == "Dockerfile" || filepath.Base(path) == "Makefile" || filepath.Base(path) == "LICENSE"
	}
}

func RuntimePlatform() string { return runtime.GOOS + "/" + runtime.GOARCH }
