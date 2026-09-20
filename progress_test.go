package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
)

func testDownloadProgress() (*downloadProgress, *bytes.Buffer) {
	output := &bytes.Buffer{}
	return &downloadProgress{
		output:  output,
		enabled: true,
		files:   make(map[string]*progressFile),
	}, output
}

func TestDownloadProgressAggregatesConcurrentDownloads(t *testing.T) {
	progress, output := testDownloadProgress()
	const (
		chunk = int64(128 << 10)
		loops = 32
	)
	progress.register("one.tar.gz", chunk*loops)
	progress.register("two.tar.gz", chunk*loops)
	progress.begin("one.tar.gz", 0)
	progress.begin("two.tar.gz", 0)

	var wait sync.WaitGroup
	for _, name := range []string{"one.tar.gz", "two.tar.gz"} {
		name := name
		wait.Add(1)
		go func() {
			defer wait.Done()
			for i := 0; i < loops; i++ {
				progress.addBytes(name, chunk)
			}
		}()
	}
	wait.Wait()
	progress.finish("one.tar.gz", true)
	progress.finish("two.tar.gz", true)
	progress.close()

	text := output.String()
	for _, want := range []string{"Downloading", "2/2", "100%", "MiB/s"} {
		if !strings.Contains(text, want) {
			t.Fatalf("progress output %q does not contain %q", text, want)
		}
	}
	if !strings.HasSuffix(text, "\r\033[2K") {
		t.Fatalf("successful progress output does not end its line: %q", text)
	}
}

func TestDownloadProgressHidesPercentageWhenSizeIsUnknown(t *testing.T) {
	progress, output := testDownloadProgress()
	progress.register("unknown.tar.gz", 0)
	progress.begin("unknown.tar.gz", 0)
	progress.addBytes("unknown.tar.gz", 512)
	progress.finish("unknown.tar.gz", true)
	progress.close()

	text := output.String()
	if strings.Contains(text, "%") {
		t.Fatalf("unknown-size progress unexpectedly contains a percentage: %q", text)
	}
	if !strings.Contains(text, "MiB/s") {
		t.Fatalf("unknown-size progress does not contain speed: %q", text)
	}
}

func TestDownloadProgressDisablesOutputForNonTTY(t *testing.T) {
	output := &bytes.Buffer{}
	progress := newDownloadProgress(output)
	progress.begin("archive.tar.gz", 10)
	progress.addBytes("archive.tar.gz", 10)
	progress.finish("archive.tar.gz", true)
	progress.close()

	if output.Len() != 0 {
		t.Fatalf("non-TTY progress output = %q, want no output", output.String())
	}
}

func TestDownloadArtifactProgressUsesContentLength(t *testing.T) {
	payload := []byte("archive payload")
	env := &Environment{
		Root:            t.TempDir(),
		DownloadBaseURL: "https://govm.test/downloads/",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Status:        "200 OK",
				Body:          io.NopCloser(bytes.NewReader(payload)),
				ContentLength: int64(len(payload)),
				Header:        make(http.Header),
			}, nil
		})},
	}
	artifact := Artifact{Filename: "archive.tar.gz", SHA256: hashBytes(payload)}
	progress, output := testDownloadProgress()

	if _, err := downloadArtifact(context.Background(), env, artifact, progress); err != nil {
		t.Fatal(err)
	}
	progress.close()
	if !strings.Contains(output.String(), "100%") {
		t.Fatalf("content-length progress did not reach 100%%: %q", output.String())
	}
}

func TestDownloadArtifactCacheHitDoesNotStartProgress(t *testing.T) {
	payload := []byte("cached archive")
	env := &Environment{
		Root:            t.TempDir(),
		DownloadBaseURL: "https://govm.test/downloads/",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return nil, errors.New("cache hit should not make a request")
		})},
	}
	if err := env.ensureLayout(); err != nil {
		t.Fatal(err)
	}
	artifact := Artifact{
		Filename: "cached.tar.gz",
		SHA256:   hashBytes(payload),
		Size:     int64(len(payload)),
	}
	if err := os.WriteFile(env.downloadPath(artifact.Filename), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	progress, output := testDownloadProgress()
	if _, err := downloadArtifact(context.Background(), env, artifact, progress); err != nil {
		t.Fatal(err)
	}
	progress.close()
	if output.Len() != 0 {
		t.Fatalf("cache hit produced progress output: %q", output.String())
	}
}

func TestDownloadArtifactProgressClearsFailedLine(t *testing.T) {
	payload := []byte("partial")
	env := &Environment{
		Root:            t.TempDir(),
		DownloadBaseURL: "https://govm.test/downloads/",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Status:        "200 OK",
				Body:          &progressErrorReader{data: payload},
				ContentLength: int64(len(payload) + 1),
				Header:        make(http.Header),
			}, nil
		})},
	}
	artifact := Artifact{Filename: "failed.tar.gz", SHA256: hashBytes(payload)}
	progress, output := testDownloadProgress()
	if _, err := downloadArtifact(context.Background(), env, artifact, progress); err == nil {
		t.Fatal("download unexpectedly succeeded")
	}
	progress.close()
	if !strings.HasSuffix(output.String(), "\r\033[2K") {
		t.Fatalf("failed progress line was not cleared: %q", output.String())
	}
}

type progressErrorReader struct {
	data []byte
	done bool
}

func (r *progressErrorReader) Read(data []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(data, r.data), nil
	}
	return 0, errors.New("read failed")
}

func (r *progressErrorReader) Close() error { return nil }

type cancelProgressReader struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func (r *cancelProgressReader) Read([]byte) (int, error) {
	r.cancel()
	return 0, r.ctx.Err()
}

func (r *cancelProgressReader) Close() error { return nil }

func TestDownloadProgressCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := &Environment{Root: t.TempDir(), DownloadBaseURL: "https://govm.test/",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, ContentLength: 100,
				Body: &cancelProgressReader{ctx: ctx, cancel: cancel}}, nil
		})}}
	p, output := testDownloadProgress()
	defer p.close()
	_, err := downloadArtifact(ctx, env, Artifact{Filename: "cancel.tar.gz", SHA256: hashBytes(nil)}, p)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if !strings.HasSuffix(output.String(), "\r\033[2K") || p.lineRendered {
		t.Fatalf("cancel left a progress line: %q", output.String())
	}
	p.close()
}

func TestProgressIdleAndCachedBytes(t *testing.T) {
	p, output := testDownloadProgress()
	defer p.close()
	p.register("cached", 10000000)
	p.cached("cached", 10000000)
	p.begin("source", 100)
	p.addBytes("source", 100)
	p.finish("source", true)
	p.mu.Lock()
	if p.lineRendered || len(p.files) != 1 {
		t.Error("idle line or cached bytes retained")
	}
	before := output.Len()
	p.mu.Unlock()
	p.render()
	p.mu.Lock()
	if output.Len() != before {
		t.Error("renderer wrote during extraction/build")
	}
	p.mu.Unlock()
	p.begin("bootstrap", 100)
	p.addBytes("bootstrap", 100)
	p.finish("bootstrap", true)
	p.close()
	if !strings.Contains(output.String(), "2/2 100%") {
		t.Fatalf("dynamic archive not aggregated: %q", output.String())
	}
}

type failingProgressWriter struct{}

func (failingProgressWriter) Write([]byte) (int, error) { return 0, errors.New("output failed") }

func TestProgressOutputFailureDoesNotFailDownload(t *testing.T) {
	server := newTestReleaseServer(t, "")
	p, _ := testDownloadProgress()
	p.output = failingProgressWriter{}
	defer p.close()
	if _, err := downloadArtifact(context.Background(), server.environment(t), server.releases[0].Files[0], p); err != nil {
		t.Fatal(err)
	}
}
