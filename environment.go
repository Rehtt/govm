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
	"time"
)

const (
	defaultMetadataURL = "https://go.dev/dl/?mode=json"
	defaultMetadataTTL = 6 * time.Hour
	defaultHTTPTimeout = 5 * time.Minute
)

// Environment contains the locations and transports used by govm. Keeping
// these values injectable makes all network and filesystem behaviour testable.
type Environment struct {
	Root            string
	MetadataURL     string
	DownloadBaseURL string
	HTTPClient      *http.Client
	MetadataTTL     time.Duration
	Output          io.Writer
	ErrorOutput     io.Writer
}

// DefaultEnvironment returns a fresh environment. It intentionally never
// consults ~/.gvm: govm owns ~/.govm exclusively.
func DefaultEnvironment() *Environment {
	root := os.Getenv("GOVM_HOME")
	if root == "" {
		if home, err := os.UserHomeDir(); err == nil {
			root = filepath.Join(home, ".govm")
		}
	}
	metadataURL := os.Getenv("GOVM_RELEASES_URL")
	if metadataURL == "" {
		metadataURL = os.Getenv("GOVM_METADATA_URL")
	}
	if metadataURL == "" {
		metadataURL = defaultMetadataURL
	}
	downloadBaseURL := os.Getenv("GOVM_DOWNLOAD_BASE_URL")
	if downloadBaseURL == "" {
		downloadBaseURL = os.Getenv("GOVM_DOWNLOAD_URL")
	}
	return &Environment{
		Root:            root,
		MetadataURL:     metadataURL,
		DownloadBaseURL: downloadBaseURL,
		HTTPClient:      &http.Client{Timeout: defaultHTTPTimeout},
		MetadataTTL:     defaultMetadataTTL,
		Output:          os.Stdout,
		ErrorOutput:     os.Stderr,
	}
}

func (e *Environment) normalized() (*Environment, error) {
	if e == nil {
		e = DefaultEnvironment()
	}
	copy := *e
	if copy.Root == "" {
		copy.Root = DefaultEnvironment().Root
	}
	if copy.Root == "" {
		return nil, errors.New("cannot determine the govm home directory")
	}
	root, err := filepath.Abs(copy.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve govm home: %w", err)
	}
	copy.Root = filepath.Clean(root)
	if copy.MetadataURL == "" {
		copy.MetadataURL = defaultMetadataURL
	}
	if copy.HTTPClient == nil {
		copy.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if copy.MetadataTTL <= 0 {
		copy.MetadataTTL = defaultMetadataTTL
	}
	if copy.Output == nil {
		copy.Output = io.Discard
	}
	if copy.ErrorOutput == nil {
		copy.ErrorOutput = io.Discard
	}
	return &copy, nil
}

func (e *Environment) versionsDir() string { return filepath.Join(e.Root, "versions") }
func (e *Environment) downloadsDir() string {
	return filepath.Join(e.Root, "cache", "downloads")
}
func (e *Environment) bootstrapDir() string {
	return filepath.Join(e.Root, "cache", "bootstrap")
}
func (e *Environment) releasesDir() string {
	return filepath.Join(e.Root, "cache", "releases")
}
func (e *Environment) currentPath() string { return filepath.Join(e.Root, "current") }
func (e *Environment) defaultPath() string { return filepath.Join(e.Root, "default") }
func (e *Environment) locksDir() string    { return filepath.Join(e.Root, "locks") }
func (e *Environment) logsDir() string     { return filepath.Join(e.Root, "logs") }

func (e *Environment) ensureLayout() error {
	for _, dir := range []string{
		e.Root,
		e.versionsDir(),
		e.downloadsDir(),
		e.bootstrapDir(),
		e.releasesDir(),
		e.locksDir(),
		e.logsDir(),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

func (e *Environment) metadataCachePath(all bool) string {
	name := "stable.json"
	if all {
		name = "all.json"
	}
	return filepath.Join(e.releasesDir(), name)
}

func (e *Environment) downloadPath(filename string) string {
	return filepath.Join(e.downloadsDir(), filename)
}

func (e *Environment) bootstrapVersionPath(version string) string {
	return filepath.Join(e.bootstrapDir(), version)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".govm-write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		// Windows cannot rename over an existing file. The path is a small
		// state file and the temporary file is still used for the write.
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return err
		}
		if retryErr := os.Rename(tmpName, path); retryErr != nil {
			return fmt.Errorf("replace %s: %w", path, retryErr)
		}
	}
	return nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func samePathOrChild(path, parent string) bool {
	pathAbs, err1 := filepath.Abs(path)
	parentAbs, err2 := filepath.Abs(parent)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(parentAbs, pathAbs)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
