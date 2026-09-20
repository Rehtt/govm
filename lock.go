package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type fileLock struct {
	path string
}

func acquireFileLock(ctx context.Context, path string) (*fileLock, error) {
	ctx = contextOrBackground(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepathDir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	for {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, _ = file.WriteString(strconv.Itoa(os.Getpid()) + "\n" + time.Now().UTC().Format(time.RFC3339Nano) + "\n")
			_ = file.Close()
			return &fileLock{path: path}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create lock %s: %w", path, err)
		}
		if staleLock(path) {
			_ = os.Remove(path)
			continue
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *fileLock) Close() error {
	if l == nil {
		return nil
	}
	return os.Remove(l.path)
}

func staleLock(path string) bool {
	info, err := os.Stat(path)
	return err == nil && time.Since(info.ModTime()) > 24*time.Hour
}

// Kept as a tiny helper so lock code remains platform-neutral and never needs
// to import syscall-specific path helpers.
func filepathDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			if i == 0 {
				return path[:1]
			}
			return path[:i]
		}
	}
	return "."
}

func lockName(prefix, value string) string {
	value = strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(value)
	return prefix + value + ".lock"
}
