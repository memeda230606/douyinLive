package capture

import (
	"errors"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
)

var (
	ErrMediaPathInvalid     = errors.New("MEDIA_PATH_INVALID")
	ErrMediaPathUnavailable = errors.New("MEDIA_PATH_UNAVAILABLE")
)

func secureMediaSessionDirectory(root, relative string) (string, error) {
	root, err := canonicalMediaRoot(root)
	if err != nil {
		return "", err
	}
	target, err := createSecureMediaDirectoryTree(root, relative)
	if err != nil {
		return "", err
	}
	for _, name := range []string{"media", "audio", "manifests"} {
		directory := filepath.Join(target, name)
		if err := ensureSecureMediaDirectory(directory); err != nil {
			return "", err
		}
	}
	return target, nil
}

// createSecureMediaDirectoryTree never descends through a link or Windows
// reparse point. In particular, a rejected component cannot make Mkdir create
// children outside root before containment is checked.
func createSecureMediaDirectoryTree(root, relative string) (string, error) {
	target, err := mediaAbsolutePath(root, relative)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(root)
	for _, component := range strings.Split(relative, "/") {
		current = filepath.Join(current, component)
		if err := ensureSecureMediaDirectory(current); err != nil {
			return "", err
		}
	}
	if !recorderPathsEqual(filepath.Clean(current), filepath.Clean(target)) {
		return "", ErrMediaPathInvalid
	}
	return target, nil
}

func ensureSecureMediaDirectory(directory string) error {
	directory = filepath.Clean(directory)
	for attempt := 0; attempt < 2; attempt++ {
		info, err := os.Lstat(directory)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(directory, 0o700); err != nil {
				if errors.Is(err, os.ErrExist) {
					continue
				}
				return ErrMediaPathUnavailable
			}
			continue
		}
		if err != nil {
			return ErrMediaPathUnavailable
		}
		reparse, err := mediaPathIsReparsePoint(directory, info)
		if err != nil {
			return ErrMediaPathUnavailable
		}
		if !info.IsDir() || reparse {
			return ErrMediaPathInvalid
		}
		resolved, err := filepath.EvalSymlinks(directory)
		if err != nil {
			return ErrMediaPathUnavailable
		}
		resolved, err = filepath.Abs(resolved)
		if err != nil || !recorderPathsEqual(filepath.Clean(resolved), directory) {
			return ErrMediaPathInvalid
		}
		return nil
	}
	return ErrMediaPathInvalid
}

func canonicalMediaRoot(root string) (string, error) {
	if !validMediaAbsolutePath(root) {
		return "", ErrMediaPathInvalid
	}
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return "", ErrMediaPathUnavailable
	}
	reparse, err := mediaPathIsReparsePoint(root, info)
	if err != nil || reparse {
		return "", ErrMediaPathUnavailable
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", ErrMediaPathUnavailable
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", ErrMediaPathInvalid
	}
	return filepath.Clean(resolved), nil
}

func mediaAbsolutePath(root, relative string) (string, error) {
	if !validMediaRelativePath(relative) || !validMediaAbsolutePath(root) {
		return "", ErrMediaPathInvalid
	}
	root = filepath.Clean(root)
	target := filepath.Join(root, filepath.FromSlash(relative))
	relativeTarget, err := filepath.Rel(root, target)
	if err != nil || relativeTarget == ".." || strings.HasPrefix(relativeTarget, ".."+string(filepath.Separator)) {
		return "", ErrMediaPathInvalid
	}
	return target, nil
}

func joinMediaRelativePath(base string, elements ...string) (string, error) {
	if !validMediaRelativePath(base) {
		return "", ErrMediaPathInvalid
	}
	joined := base
	for _, element := range elements {
		if element == "" || strings.ContainsAny(element, `/\\`) ||
			ffmpegControlCharacters.MatchString(element) {
			return "", ErrMediaPathInvalid
		}
		joined = pathpkg.Join(joined, element)
	}
	if !validMediaRelativePath(joined) {
		return "", ErrMediaPathInvalid
	}
	return joined, nil
}
