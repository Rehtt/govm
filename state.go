package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

type InstalledVersion struct {
	Version string `json:"version"`
	Current bool   `json:"current"`
	Default bool   `json:"default"`
	Valid   bool   `json:"valid"`
}

func versionDir(env *Environment, version string) string {
	return filepath.Join(env.versionsDir(), version)
}

func installedVersion(env *Environment, requested string) (string, error) {
	version, err := canonicalVersion(requested)
	if err != nil {
		return "", err
	}
	if version == "latest" {
		versions, err := installedVersions(env)
		if err != nil {
			return "", err
		}
		if len(versions) == 0 {
			return "", errors.New("no Go versions are installed")
		}
		return versions[len(versions)-1], nil
	}
	return version, nil
}

func installedVersions(env *Environment) ([]string, error) {
	entries, err := os.ReadDir(env.versionsDir())
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	versions := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !isSafeVersion(entry.Name()) || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		versions = append(versions, entry.Name())
	}
	sort.Slice(versions, func(i, j int) bool { return compareVersions(versions[i], versions[j]) < 0 })
	return versions, nil
}

func validInstalledTree(env *Environment, version string) bool {
	return validBinaryTree(versionDir(env, version))
}

func readDefaultVersion(env *Environment) (string, error) {
	data, err := os.ReadFile(env.defaultPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(data))
	if version == "" {
		return "", nil
	}
	if !isSafeVersion(version) {
		return "", fmt.Errorf("default points to an invalid version %q", version)
	}
	return version, nil
}

func currentVersion(env *Environment) (string, error) {
	info, err := os.Lstat(env.currentPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("%s exists but is not a symbolic link", env.currentPath())
	}
	resolved, err := filepath.EvalSymlinks(env.currentPath())
	if err != nil {
		return "", fmt.Errorf("resolve current link: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	versionsRoot, err := filepath.Abs(env.versionsDir())
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(versionsRoot, resolved)
	if err != nil || rel == "." || strings.Contains(rel, string(filepath.Separator)) || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("current link points outside %s", versionsRoot)
	}
	if !isSafeVersion(rel) {
		return "", fmt.Errorf("current link points to invalid version %q", rel)
	}
	return rel, nil
}

func setCurrent(env *Environment, version string) error {
	if !isSafeVersion(version) {
		return fmt.Errorf("invalid Go version %q", version)
	}
	if !validInstalledTree(env, version) {
		return fmt.Errorf("Go version %s is not installed or is incomplete", version)
	}
	if err := os.MkdirAll(env.Root, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(env.Root, ".current-*")
	if err != nil {
		return fmt.Errorf("create current link temporary path: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Remove(tmpPath); err != nil {
		return err
	}
	target := versionDir(env, version)
	if err := os.Symlink(target, tmpPath); err != nil {
		if runtime.GOOS == "windows" {
			return fmt.Errorf("cannot create ~/.govm/current symlink: %w; on Windows run as Administrator or enable Developer Mode", err)
		}
		return fmt.Errorf("create current symlink: %w", err)
	}
	defer os.Remove(tmpPath)
	if err := os.Rename(tmpPath, env.currentPath()); err == nil {
		return nil
	} else {
		// Windows does not replace an existing link with Rename. Only remove
		// an existing link; never remove a real directory at this path.
		if info, statErr := os.Lstat(env.currentPath()); statErr == nil {
			if info.Mode()&os.ModeSymlink == 0 {
				return fmt.Errorf("replace current: %s exists and is not a symbolic link", env.currentPath())
			}
			if removeErr := os.Remove(env.currentPath()); removeErr != nil {
				if runtime.GOOS == "windows" {
					return fmt.Errorf("cannot replace current symlink: %w; on Windows run as Administrator or enable Developer Mode", removeErr)
				}
				return fmt.Errorf("remove old current symlink: %w", removeErr)
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		if retryErr := os.Rename(tmpPath, env.currentPath()); retryErr != nil {
			if runtime.GOOS == "windows" {
				return fmt.Errorf("cannot install current symlink: %w; on Windows run as Administrator or enable Developer Mode", retryErr)
			}
			return fmt.Errorf("install current symlink: %w (initial error: %v)", retryErr, err)
		}
	}
	return nil
}

func setDefault(env *Environment, version string) error {
	if !isSafeVersion(version) {
		return fmt.Errorf("invalid Go version %q", version)
	}
	if !validInstalledTree(env, version) {
		return fmt.Errorf("Go version %s is not installed or is incomplete", version)
	}
	return writeFileAtomic(env.defaultPath(), []byte(version+"\n"), 0o644)
}

func withStateLock(ctx context.Context, env *Environment, fn func() error) error {
	lock, err := acquireFileLock(ctx, filepath.Join(env.locksDir(), "state.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	return fn()
}

func removeInstalledVersion(env *Environment, version string) error {
	if !isSafeVersion(version) {
		return fmt.Errorf("invalid Go version %q", version)
	}
	target := versionDir(env, version)
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("Go version %s is not installed", version)
	}
	if err != nil {
		return err
	}
	if !samePathOrChild(target, env.versionsDir()) || filepath.Clean(target) == filepath.Clean(env.versionsDir()) {
		return errors.New("refusing to remove a path outside the versions directory")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return os.Remove(target)
	}
	return os.RemoveAll(target)
}
