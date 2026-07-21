//go:build p3accacceptance && windows

package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

type p3ACCMediaRootIdentity struct {
	canonicalPath string
	fileIdentity  p3ACCDataRootFileIdentity
}

type p3ACCMediaRootGuard struct {
	file     *os.File
	identity p3ACCMediaRootIdentity
}

type p3ACCMediaFileIdentity = p3ACCDataRootFileIdentity

func cleanP3ACCMediaRoot(mediaRoot string) (string, bool) {
	if mediaRoot == "" || strings.TrimSpace(mediaRoot) != mediaRoot {
		return "", false
	}
	clean, err := filepath.Abs(filepath.Clean(mediaRoot))
	if err != nil || !filepath.IsAbs(clean) || p3ACCValidateNoReparsePath(clean, true) != nil {
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
	file, identity, _, err := p3ACCOpenDataRootPath(clean, clean, true)
	if err != nil {
		return nil, p3ACCMediaRootIdentity{}, false
	}
	rootIdentity := p3ACCMediaRootIdentity{canonicalPath: clean, fileIdentity: identity}
	return &p3ACCMediaRootGuard{file: file, identity: rootIdentity}, rootIdentity, true
}

func validateP3ACCMediaRootGuard(
	guard *p3ACCMediaRootGuard,
	expected p3ACCMediaRootIdentity,
) bool {
	if guard == nil || guard.file == nil ||
		!sameP3ACCMediaRootIdentity(guard.identity, expected) {
		return false
	}
	handleIdentity, _, valid := p3ACCDataRootHandleSnapshot(
		guard.file, expected.canonicalPath, expected.canonicalPath, true,
	)
	if !valid || handleIdentity != expected.fileIdentity {
		return false
	}
	pathFile, pathIdentity, _, err := p3ACCOpenDataRootPath(
		expected.canonicalPath, expected.canonicalPath, true,
	)
	if err != nil {
		return false
	}
	pathRootIdentity := p3ACCMediaRootIdentity{
		canonicalPath: expected.canonicalPath,
		fileIdentity:  pathIdentity,
	}
	return pathFile.Close() == nil &&
		sameP3ACCMediaRootIdentity(expected, pathRootIdentity)
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
	return left.canonicalPath != "" && right.canonicalPath != "" &&
		strings.EqualFold(left.canonicalPath, right.canonicalPath) &&
		p3ACCValidDataRootGeneration(p3ACCDataRootGeneration(left.fileIdentity)) &&
		left.fileIdentity == right.fileIdentity
}

func openP3ACCMediaEvidenceFile(
	mediaRoot, filename string,
) (*os.File, p3ACCMediaFileIdentity, os.FileInfo, error) {
	root, valid := cleanP3ACCMediaRoot(mediaRoot)
	if !valid || filename == "" || filepath.Clean(filename) != filename ||
		!filepath.IsAbs(filename) || !p3ACCAcceptancePathWithin(root, filename, false) {
		return nil, p3ACCMediaFileIdentity{}, nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	pointer, err := windows.UTF16PtrFromString(filename)
	if err != nil {
		return nil, p3ACCMediaFileIdentity{}, nil, err
	}
	handle, err := windows.CreateFile(
		pointer,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, p3ACCMediaFileIdentity{}, nil, err
	}
	file := os.NewFile(uintptr(handle), filepath.Base(filename))
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, p3ACCMediaFileIdentity{}, nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	identity, info, ok := p3ACCDataRootHandleSnapshot(file, root, filename, false)
	if !ok {
		_ = file.Close()
		return nil, p3ACCMediaFileIdentity{}, nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	return file, identity, info, nil
}

func snapshotP3ACCMediaEvidenceHandle(
	file *os.File,
	mediaRoot, filename string,
) (p3ACCMediaFileIdentity, os.FileInfo, bool) {
	root, valid := cleanP3ACCMediaRoot(mediaRoot)
	if !valid || filename == "" || filepath.Clean(filename) != filename ||
		!filepath.IsAbs(filename) {
		return p3ACCMediaFileIdentity{}, nil, false
	}
	return p3ACCDataRootHandleSnapshot(file, root, filename, false)
}

func sameP3ACCMediaFileIdentity(left, right p3ACCMediaFileIdentity) bool {
	return left.volumeSerialNumber != 0 && left.fileIndex != 0 &&
		left.creationTime100NS != 0 && left == right
}

func validateP3ACCAcceptanceRoot(root string) error {
	clean := filepath.Clean(root)
	volumeRoot := filepath.Clean(filepath.VolumeName(clean) + string(filepath.Separator))
	if strings.EqualFold(clean, volumeRoot) {
		return errors.New("P3ACC_CONFIG_INVALID")
	}
	if err := p3ACCValidateNoReparsePath(clean, true); err != nil {
		return err
	}
	sentinel := filepath.Join(clean, p3ACCAcceptanceSentinelName)
	file, err := openP3ACCAcceptanceRegularFile(sentinel)
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
	return p3ACCValidateNoReparsePath(candidate, true)
}

func openP3ACCAcceptanceRegularFile(filename string) (*os.File, error) {
	if err := p3ACCValidateNoReparsePath(filepath.Dir(filename), true); err != nil {
		return nil, err
	}
	pointer, err := windows.UTF16PtrFromString(filename)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pointer,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	file := os.NewFile(uintptr(handle), filepath.Base(filename))
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	return file, nil
}

func inspectP3ACCAcceptanceMediaPath(filename string) p3ACCMediaPathState {
	if err := p3ACCValidateNoReparsePath(filepath.Dir(filename), true); err != nil {
		return p3ACCMediaPathUnsafe
	}
	pointer, err := windows.UTF16PtrFromString(filename)
	if err != nil {
		return p3ACCMediaPathUnsafe
	}
	attributes, err := windows.GetFileAttributes(pointer)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return p3ACCMediaPathMissing
	}
	if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return p3ACCMediaPathUnsafe
	}
	file, err := openP3ACCAcceptanceRegularFile(filename)
	if err != nil {
		return p3ACCMediaPathUnsafe
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil || closeErr != nil || !info.Mode().IsRegular() || info.Size() < 0 {
		return p3ACCMediaPathUnsafe
	}
	return p3ACCMediaPathRegular
}

func p3ACCValidateNoReparsePath(value string, requireDirectory bool) error {
	clean := filepath.Clean(value)
	volume := filepath.VolumeName(clean)
	if volume == "" || !filepath.IsAbs(clean) {
		return errors.New("P3ACC_CONFIG_INVALID")
	}
	volumeRoot := filepath.Clean(volume + string(filepath.Separator))
	relative, err := filepath.Rel(volumeRoot, clean)
	if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("P3ACC_CONFIG_INVALID")
	}
	current := volumeRoot
	if relative != "." {
		for _, component := range strings.Split(relative, string(filepath.Separator)) {
			if component == "" || component == "." || component == ".." {
				return errors.New("P3ACC_CONFIG_INVALID")
			}
			current = filepath.Join(current, component)
			pointer, err := windows.UTF16PtrFromString(current)
			if err != nil {
				return err
			}
			attributes, err := windows.GetFileAttributes(pointer)
			if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
				return errors.New("P3ACC_CONFIG_INVALID")
			}
		}
	}
	if requireDirectory {
		pointer, err := windows.UTF16PtrFromString(clean)
		if err != nil {
			return err
		}
		attributes, err := windows.GetFileAttributes(pointer)
		if err != nil || attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			return errors.New("P3ACC_CONFIG_INVALID")
		}
	}
	return nil
}
