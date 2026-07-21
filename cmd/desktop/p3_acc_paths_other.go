//go:build p3accacceptance && !windows

package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type p3ACCMediaRootIdentity struct {
	canonicalPath string
	info          os.FileInfo
}

type p3ACCMediaRootGuard struct {
	file     *os.File
	identity p3ACCMediaRootIdentity
}

type p3ACCMediaFileIdentity struct {
	info os.FileInfo
}

func cleanP3ACCMediaRoot(mediaRoot string) (string, bool) {
	if mediaRoot == "" || strings.TrimSpace(mediaRoot) != mediaRoot {
		return "", false
	}
	clean, err := filepath.Abs(filepath.Clean(mediaRoot))
	if err != nil || !filepath.IsAbs(clean) || p3ACCValidateNoSymlinkPath(clean, true) != nil {
		return "", false
	}
	return clean, true
}

func openP3ACCMediaRootGuard(
	mediaRoot string,
) (*p3ACCMediaRootGuard, p3ACCMediaRootIdentity, bool) {
	clean, valid := cleanP3ACCMediaRoot(mediaRoot)
	if !valid {
		return nil, p3ACCMediaRootIdentity{}, false
	}
	pathInfo, err := os.Lstat(clean)
	if err != nil || !pathInfo.IsDir() || pathInfo.Mode()&os.ModeSymlink != 0 {
		return nil, p3ACCMediaRootIdentity{}, false
	}
	file, err := os.Open(clean)
	if err != nil {
		return nil, p3ACCMediaRootIdentity{}, false
	}
	handleInfo, statErr := file.Stat()
	finalPathInfo, pathErr := os.Lstat(clean)
	if statErr != nil || pathErr != nil || !handleInfo.IsDir() ||
		!finalPathInfo.IsDir() || finalPathInfo.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(pathInfo, handleInfo) || !os.SameFile(handleInfo, finalPathInfo) ||
		!pathInfo.ModTime().Equal(handleInfo.ModTime()) ||
		!handleInfo.ModTime().Equal(finalPathInfo.ModTime()) {
		_ = file.Close()
		return nil, p3ACCMediaRootIdentity{}, false
	}
	identity := p3ACCMediaRootIdentity{canonicalPath: clean, info: handleInfo}
	return &p3ACCMediaRootGuard{file: file, identity: identity}, identity, true
}

func validateP3ACCMediaRootGuard(
	guard *p3ACCMediaRootGuard,
	expected p3ACCMediaRootIdentity,
) bool {
	if guard == nil || guard.file == nil ||
		!sameP3ACCMediaRootIdentity(guard.identity, expected) {
		return false
	}
	handleInfo, handleErr := guard.file.Stat()
	pathInfo, pathErr := os.Lstat(expected.canonicalPath)
	return handleErr == nil && pathErr == nil && handleInfo.IsDir() && pathInfo.IsDir() &&
		pathInfo.Mode()&os.ModeSymlink == 0 && expected.info != nil &&
		os.SameFile(expected.info, handleInfo) && os.SameFile(handleInfo, pathInfo) &&
		expected.info.ModTime().Equal(handleInfo.ModTime()) &&
		handleInfo.ModTime().Equal(pathInfo.ModTime())
}

func closeP3ACCMediaRootGuard(guard *p3ACCMediaRootGuard) bool {
	if guard == nil || guard.file == nil {
		return false
	}
	file := guard.file
	guard.file = nil
	return file.Close() == nil
}

func captureP3ACCMediaRootIdentity(mediaRoot string) (p3ACCMediaRootIdentity, bool) {
	guard, identity, valid := openP3ACCMediaRootGuard(mediaRoot)
	if !valid {
		return p3ACCMediaRootIdentity{}, false
	}
	defer closeP3ACCMediaRootGuard(guard)
	stable := validateP3ACCMediaRootGuard(guard, identity)
	closed := closeP3ACCMediaRootGuard(guard)
	if !stable || !closed {
		return p3ACCMediaRootIdentity{}, false
	}
	return identity, true
}

func sameP3ACCMediaRootIdentity(left, right p3ACCMediaRootIdentity) bool {
	return left.canonicalPath != "" && left.canonicalPath == right.canonicalPath &&
		left.info != nil && right.info != nil && os.SameFile(left.info, right.info) &&
		left.info.ModTime().Equal(right.info.ModTime())
}

func openP3ACCMediaEvidenceFile(
	mediaRoot, filename string,
) (*os.File, p3ACCMediaFileIdentity, os.FileInfo, error) {
	root, valid := cleanP3ACCMediaRoot(mediaRoot)
	if !valid || filename == "" || filepath.Clean(filename) != filename ||
		!filepath.IsAbs(filename) || !p3ACCAcceptancePathWithin(root, filename, false) {
		return nil, p3ACCMediaFileIdentity{}, nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	pathInfo, err := os.Lstat(filename)
	if err != nil {
		return nil, p3ACCMediaFileIdentity{}, nil, err
	}
	if !pathInfo.Mode().IsRegular() || pathInfo.Mode()&os.ModeSymlink != 0 {
		return nil, p3ACCMediaFileIdentity{}, nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	file, err := openP3ACCAcceptanceRegularFile(filename)
	if err != nil {
		return nil, p3ACCMediaFileIdentity{}, nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(pathInfo, info) {
		_ = file.Close()
		return nil, p3ACCMediaFileIdentity{}, nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	return file, p3ACCMediaFileIdentity{info: info}, info, nil
}

func snapshotP3ACCMediaEvidenceHandle(
	file *os.File,
	mediaRoot, filename string,
) (p3ACCMediaFileIdentity, os.FileInfo, bool) {
	root, valid := cleanP3ACCMediaRoot(mediaRoot)
	if file == nil || !valid || filename == "" || filepath.Clean(filename) != filename ||
		!filepath.IsAbs(filename) || !p3ACCAcceptancePathWithin(root, filename, false) {
		return p3ACCMediaFileIdentity{}, nil, false
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return p3ACCMediaFileIdentity{}, nil, false
	}
	return p3ACCMediaFileIdentity{info: info}, info, true
}

func sameP3ACCMediaFileIdentity(left, right p3ACCMediaFileIdentity) bool {
	return left.info != nil && right.info != nil && os.SameFile(left.info, right.info) &&
		left.info.ModTime().Equal(right.info.ModTime())
}

func validateP3ACCAcceptanceRoot(root string) error {
	clean := filepath.Clean(root)
	if clean == string(filepath.Separator) {
		return errors.New("P3ACC_CONFIG_INVALID")
	}
	if err := p3ACCValidateNoSymlinkPath(clean, true); err != nil {
		return err
	}
	file, err := openP3ACCAcceptanceRegularFile(filepath.Join(clean, p3ACCAcceptanceSentinelName))
	if err != nil {
		return err
	}
	payload, readErr := io.ReadAll(io.LimitReader(file, int64(len(p3ACCAcceptanceSentinelContent)+1)))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || string(payload) != p3ACCAcceptanceSentinelContent {
		return errors.New("P3ACC_CONFIG_INVALID")
	}
	return nil
}

func validateP3ACCAcceptanceDataPath(root, candidate string) error {
	if !p3ACCAcceptancePathWithin(root, candidate, false) {
		return errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	return p3ACCValidateNoSymlinkPath(candidate, true)
}

func openP3ACCAcceptanceRegularFile(filename string) (*os.File, error) {
	if err := p3ACCValidateNoSymlinkPath(filepath.Dir(filename), true); err != nil {
		return nil, err
	}
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	return os.Open(filename)
}

func inspectP3ACCAcceptanceMediaPath(filename string) p3ACCMediaPathState {
	if err := p3ACCValidateNoSymlinkPath(filepath.Dir(filename), true); err != nil {
		return p3ACCMediaPathUnsafe
	}
	pathInfo, err := os.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return p3ACCMediaPathMissing
	}
	if err != nil || !pathInfo.Mode().IsRegular() || pathInfo.Mode()&os.ModeSymlink != 0 {
		return p3ACCMediaPathUnsafe
	}
	file, err := openP3ACCAcceptanceRegularFile(filename)
	if err != nil {
		return p3ACCMediaPathUnsafe
	}
	handleInfo, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil || closeErr != nil || !handleInfo.Mode().IsRegular() ||
		!os.SameFile(pathInfo, handleInfo) || pathInfo.Size() != handleInfo.Size() ||
		!pathInfo.ModTime().Equal(handleInfo.ModTime()) {
		return p3ACCMediaPathUnsafe
	}
	return p3ACCMediaPathRegular
}

func p3ACCValidateNoSymlinkPath(value string, requireDirectory bool) error {
	clean := filepath.Clean(value)
	if !filepath.IsAbs(clean) {
		return errors.New("P3ACC_CONFIG_INVALID")
	}
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("P3ACC_CONFIG_INVALID")
		}
	}
	if requireDirectory {
		info, err := os.Lstat(clean)
		if err != nil || !info.IsDir() {
			return errors.New("P3ACC_CONFIG_INVALID")
		}
	}
	return nil
}
