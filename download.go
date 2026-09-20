package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func downloadArtifact(ctx context.Context, env *Environment, artifact Artifact) (string, error) {
	ctx = contextOrBackground(ctx)
	env, err := env.normalized()
	if err != nil {
		return "", err
	}
	if err := validateArtifact(artifact); err != nil {
		return "", err
	}
	if err := env.ensureLayout(); err != nil {
		return "", err
	}
	if filepath.Base(artifact.Filename) != artifact.Filename || strings.ContainsAny(artifact.Filename, `/\\`) {
		return "", fmt.Errorf("unsafe artifact filename %q", artifact.Filename)
	}
	finalPath := env.downloadPath(artifact.Filename)
	if validDownloadedFile(finalPath, artifact) {
		return finalPath, nil
	}
	if _, err := os.Stat(finalPath); err == nil {
		if err := os.Remove(finalPath); err != nil {
			return "", fmt.Errorf("remove invalid cached download: %w", err)
		}
	}

	lock, err := acquireFileLock(ctx, filepath.Join(env.locksDir(), lockName("download-", artifact.Filename)))
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if validDownloadedFile(finalPath, artifact) {
		return finalPath, nil
	}

	remoteURL, err := artifactURL(env, artifact)
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, remoteURL, nil)
	if err != nil {
		return "", fmt.Errorf("create download request: %w", err)
	}
	response, err := env.HTTPClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", artifact.Filename, err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("download %s: HTTP %s", artifact.Filename, response.Status)
	}

	tmp, err := os.CreateTemp(env.downloadsDir(), ".download-*")
	if err != nil {
		return "", fmt.Errorf("create temporary download: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	hash := sha256.New()
	count, copyErr := io.Copy(io.MultiWriter(tmp, hash), response.Body)
	if copyErr != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("download %s: %w", artifact.Filename, copyErr)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("flush download %s: %w", artifact.Filename, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close download %s: %w", artifact.Filename, err)
	}
	if artifact.Size > 0 && count != artifact.Size {
		return "", fmt.Errorf("size mismatch for %s: got %d bytes, expected %d", artifact.Filename, count, artifact.Size)
	}
	gotHash := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(gotHash, artifact.SHA256) {
		return "", fmt.Errorf("SHA-256 mismatch for %s: got %s, expected %s", artifact.Filename, gotHash, artifact.SHA256)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		if validDownloadedFile(finalPath, artifact) {
			return finalPath, nil
		}
		return "", fmt.Errorf("cache download %s: %w", artifact.Filename, err)
	}
	return finalPath, nil
}

func validateArtifact(artifact Artifact) error {
	if artifact.Filename == "" {
		return errors.New("artifact filename is empty")
	}
	if !sha256Pattern.MatchString(artifact.SHA256) {
		return fmt.Errorf("artifact %s has no valid SHA-256", artifact.Filename)
	}
	return nil
}

func validDownloadedFile(path string, artifact Artifact) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if artifact.Size > 0 && info.Size() != artifact.Size {
		return false
	}
	got, err := sha256File(path)
	return err == nil && strings.EqualFold(got, artifact.SHA256)
}
