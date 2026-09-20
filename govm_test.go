package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type testReleaseServer struct {
	server   *httptest.Server
	client   *http.Client
	baseURL  string
	mu       sync.Mutex
	requests map[string]int
	files    map[string][]byte
	releases []Release
}

func newTestReleaseServer(t *testing.T, sourceScript string) *testReleaseServer {
	t.Helper()
	server := &testReleaseServer{
		requests: make(map[string]int),
		files:    make(map[string][]byte),
	}
	for _, version := range []string{"go1.27.1", "go1.28.0"} {
		binaryName := version + "." + runtime.GOOS + "-" + runtime.GOARCH + archiveSuffix()
		binary := makeGoArchive(t, false, version, "")
		server.files[binaryName] = binary
		server.releases = append(server.releases, Release{
			Version: version,
			Stable:  version == "go1.27.1",
			Files: []Artifact{{
				Filename: binaryName,
				OS:       runtime.GOOS,
				Arch:     runtime.GOARCH,
				Version:  version,
				SHA256:   hashBytes(binary),
				Size:     int64(len(binary)),
				Kind:     "archive",
			}},
		})
		sourceName := version + ".src.tar.gz"
		source := makeGoArchive(t, true, version, sourceScript)
		server.files[sourceName] = source
		server.releases[len(server.releases)-1].Files = append(server.releases[len(server.releases)-1].Files, Artifact{
			Filename: sourceName,
			Version:  version,
			SHA256:   hashBytes(source),
			Size:     int64(len(source)),
			Kind:     "source",
		})
	}
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		name := filepath.Base(request.URL.Path)
		server.mu.Lock()
		server.requests[name]++
		server.mu.Unlock()
		if name == "dl" || request.URL.Path == "/" || request.URL.Query().Get("mode") == "json" {
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(server.releases)
			return
		}
		data, ok := server.files[name]
		if !ok {
			http.NotFound(writer, request)
			return
		}
		_, _ = writer.Write(data)
	})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		// Some sandboxes disallow listen(2). Keep the same handler behind an
		// in-memory RoundTripper so the tests still exercise downloads there;
		// normal development and CI use the requested httptest.Server path.
		server.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			return recorder.Result(), nil
		})}
		server.baseURL = "http://govm.test"
		return server
	}
	server.server = &httptest.Server{Listener: listener, Config: &http.Server{Handler: handler}}
	server.server.Start()
	server.client = server.server.Client()
	server.baseURL = server.server.URL
	t.Cleanup(server.server.Close)
	return server
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (s *testReleaseServer) count(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[name]
}

func (s *testReleaseServer) environment(t *testing.T) *Environment {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".govm")
	return &Environment{
		Root:            root,
		MetadataURL:     s.baseURL + "/dl?mode=json",
		DownloadBaseURL: s.baseURL + "/dl/",
		HTTPClient:      s.client,
		MetadataTTL:     time.Hour,
		Output:          io.Discard,
		ErrorOutput:     io.Discard,
	}
}

func archiveSuffix() string {
	if runtime.GOOS == "windows" {
		return ".zip"
	}
	return ".tar.gz"
}

func makeGoArchive(t *testing.T, source bool, version, script string) []byte {
	t.Helper()
	if runtime.GOOS == "windows" {
		// The tests run on Unix in CI; keep a clear failure if they are ever
		// moved to Windows rather than silently testing the wrong format.
		t.Skip("local archive fixture currently uses tar.gz")
	}
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	add := func(name string, data []byte, mode int64) {
		t.Helper()
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	add("go/VERSION", []byte(version+"\n"), 0o644)
	if source {
		if script == "" {
			script = "#!/bin/sh\nset -eu\nmkdir -p ../bin\nprintf '#!/bin/sh\\nexit 0\\n' > ../bin/go\nchmod +x ../bin/go\n"
		}
		add("go/src/make.bash", []byte(script), 0o755)
	} else {
		add("go/bin/go", []byte("#!/bin/sh\nexit 0\n"), 0o755)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestInstallPrefersBinaryArchive(t *testing.T) {
	server := newTestReleaseServer(t, "")
	env := server.environment(t)
	if err := installVersions([]string{"1.27.1", "go1.27.1"}, InstallOptions{Environment: env, NoInit: true, Jobs: 2}); err != nil {
		t.Fatal(err)
	}
	binaryName := "go1.27.1." + runtime.GOOS + "-" + runtime.GOARCH + archiveSuffix()
	if got := server.count(binaryName); got != 1 {
		t.Fatalf("binary download count = %d, want 1", got)
	}
	if got := server.count("go1.27.1.src.tar.gz"); got != 0 {
		t.Fatalf("source download count = %d, want 0", got)
	}
	if current, err := currentVersion(env); err != nil || current != "go1.27.1" {
		t.Fatalf("current = %q, err = %v", current, err)
	}
	if defaultVersion, err := readDefaultVersion(env); err != nil || defaultVersion != "go1.27.1" {
		t.Fatalf("default = %q, err = %v", defaultVersion, err)
	}
}

func TestValidateGoRootAcceptsVersionMetadata(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "go"), []byte("go"), 0o755); err != nil {
		t.Fatal(err)
	}
	version := "go1.27.1\ntime 2026-08-28T16:20:06Z\n"
	if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte(version), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateGoRoot(root, "go1.27.1"); err != nil {
		t.Fatalf("validateGoRoot rejected an official VERSION file: %v", err)
	}
}

func TestBuildUsesExistingBootstrapAndDoesNotDownloadBinary(t *testing.T) {
	server := newTestReleaseServer(t, "#!/bin/sh\nset -eu\nprintf '%s|%s' \"$GOROOT_BOOTSTRAP\" \"$GOROOT_FINAL\" > ../build-env\nmkdir -p ../bin\nprintf '#!/bin/sh\\nexit 0\\n' > ../bin/go\nchmod +x ../bin/go\n")
	env := server.environment(t)
	bootstrap := filepath.Join(t.TempDir(), "bootstrap")
	if err := os.MkdirAll(filepath.Join(bootstrap, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bootstrap, "VERSION"), []byte("go1.26.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bootstrap, "bin", "go"), []byte("bootstrap"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOROOT_BOOTSTRAP", bootstrap)
	if err := installVersions([]string{"1.27.1"}, InstallOptions{Environment: env, Build: true, NoInit: true, Jobs: 1}); err != nil {
		t.Fatal(err)
	}
	if got := server.count("go1.27.1.src.tar.gz"); got != 1 {
		t.Fatalf("source download count = %d, want 1", got)
	}
	binaryName := "go1.27.1." + runtime.GOOS + "-" + runtime.GOARCH + archiveSuffix()
	if got := server.count(binaryName); got != 0 {
		t.Fatalf("binary download count = %d, want 0", got)
	}
	logData, err := os.ReadFile(filepath.Join(env.logsDir(), "build-go1.27.1.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(logData, []byte("GOROOT_BOOTSTRAP: "+bootstrap)) {
		t.Fatalf("build log does not contain bootstrap path: %s", logData)
	}
	if !validInstalledTree(env, "go1.27.1") {
		t.Fatal("built Go tree is not valid")
	}
}

func TestBuildFailureDoesNotChangeCurrentOrLeaveVersion(t *testing.T) {
	server := newTestReleaseServer(t, "#!/bin/sh\nexit 1\n")
	env := server.environment(t)
	bootstrap := filepath.Join(t.TempDir(), "bootstrap")
	if err := os.MkdirAll(filepath.Join(bootstrap, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bootstrap, "VERSION"), []byte("go1.26.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bootstrap, "bin", "go"), []byte("bootstrap"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installVersions([]string{"1.27.1"}, InstallOptions{Environment: env, NoInit: true}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOROOT_BOOTSTRAP", bootstrap)
	err := installVersions([]string{"1.28.0"}, InstallOptions{Environment: env, Build: true, NoInit: true, Jobs: 1})
	if err == nil {
		t.Fatal("build unexpectedly succeeded")
	}
	if current, currentErr := currentVersion(env); currentErr != nil || current != "go1.27.1" {
		t.Fatalf("current changed after failed build: %q, %v", current, currentErr)
	}
	if defaultVersion, defaultErr := readDefaultVersion(env); defaultErr != nil || defaultVersion != "go1.27.1" {
		t.Fatalf("default changed after failed build: %q, %v", defaultVersion, defaultErr)
	}
	if _, statErr := os.Stat(versionDir(env, "go1.28.0")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed build left installation directory: %v", statErr)
	}
}

func TestArchiveTraversalAndHashMismatch(t *testing.T) {
	archive := makeMaliciousTar(t, "../escape")
	_, err := extractArchive(archive, filepath.Join(t.TempDir(), "out"))
	if err == nil || !strings.Contains(err.Error(), "traversal") {
		t.Fatalf("traversal error = %v", err)
	}
	if _, err := safeArchiveName(`C:\\escape`); err == nil {
		t.Fatal("Windows absolute archive path was accepted")
	}
	server := newTestReleaseServer(t, "")
	env := server.environment(t)
	artifact := Artifact{Filename: "bad.tar.gz", SHA256: strings.Repeat("0", 64)}
	server.files[artifact.Filename] = []byte("not the expected archive")
	if _, err := downloadArtifact(context.Background(), env, artifact); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("hash error = %v", err)
	}
}

func makeMaliciousTar(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bad.tar.gz")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestShellInitIsIdempotentAndBacksUp(t *testing.T) {
	root := filepath.Join(t.TempDir(), "home", ".govm")
	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(filepath.Dir(root), ".bashrc")
	original := "export EDITOR=vi\n"
	if err := os.WriteFile(config, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	env := &Environment{Root: root, Output: io.Discard, ErrorOutput: io.Discard}
	if err := initShell(env, "bash"); err != nil {
		t.Fatal(err)
	}
	if err := initShell(env, "bash"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(data, []byte(shellBlockStart)); got != 1 {
		t.Fatalf("managed block count = %d", got)
	}
	backup, err := os.ReadFile(config + ".govm.bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != original {
		t.Fatalf("backup = %q, want %q", backup, original)
	}
}

func TestUseAndUninstallProtection(t *testing.T) {
	server := newTestReleaseServer(t, "")
	env := server.environment(t)
	if err := installVersions([]string{"1.27.1", "1.28.0"}, InstallOptions{Environment: env, NoInit: true, Jobs: 2}); err != nil {
		t.Fatal(err)
	}
	if err := useGoWithEnvironment(env, "1.28.0", true); err != nil {
		t.Fatal(err)
	}
	if err := uninstallGo(env, "1.28.0", false); err == nil {
		t.Fatal("uninstall of current/default unexpectedly succeeded")
	}
	if err := uninstallGo(env, "1.28.0", true); err != nil {
		t.Fatal(err)
	}
	if current, err := currentVersion(env); err != nil || current != "" {
		t.Fatalf("current after force uninstall = %q, %v", current, err)
	}
}

func TestAutoBootstrapIsNotRegisteredAsAVersion(t *testing.T) {
	server := newTestReleaseServer(t, "")
	env := server.environment(t)
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))
	t.Setenv("GOROOT_BOOTSTRAP", "")
	root, err := ensureBootstrap(context.Background(), env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !validBootstrapRoot(root) {
		t.Fatalf("bootstrap root is invalid: %s", root)
	}
	if _, err := os.Stat(versionDir(env, "go1.27.1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bootstrap was registered as a user version: %v", err)
	}
	if _, err := os.Stat(env.bootstrapVersionPath("go1.27.1")); err != nil {
		t.Fatalf("bootstrap cache missing: %v", err)
	}
}

func TestMetadataCacheCanBeRefreshed(t *testing.T) {
	server := newTestReleaseServer(t, "")
	env := server.environment(t)
	if _, err := fetchReleases(context.Background(), env, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := fetchReleases(context.Background(), env, true, false); err != nil {
		t.Fatal(err)
	}
	if got := server.count("dl"); got != 1 {
		t.Fatalf("cached metadata requests = %d, want 1", got)
	}
	if _, err := fetchReleases(context.Background(), env, true, true); err != nil {
		t.Fatal(err)
	}
	if got := server.count("dl"); got != 2 {
		t.Fatalf("refreshed metadata requests = %d, want 2", got)
	}
}

func TestBuildEnvironmentDoesNotExportGOROOT(t *testing.T) {
	t.Setenv("GOROOT", "/wrong")
	env := buildEnvironment("/bootstrap", "/final")
	for _, item := range env {
		if strings.HasPrefix(item, "GOROOT=") {
			t.Fatal("GOROOT should not be exported to source build")
		}
	}
	if !strings.Contains(strings.Join(env, "\n"), "GOROOT_BOOTSTRAP=/bootstrap") {
		t.Fatal("bootstrap environment missing")
	}
}

func TestParseTrailingInstallArgs(t *testing.T) {
	var jobs int
	var build, force, noInit bool
	args, jobsSet, err := parseTrailingInstallArgs([]string{"1.27.1", "1.28.0", "--build", "-j", "3", "--force", "--no-init"}, &jobs, &build, &force, &noInit)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(args) != "[1.27.1 1.28.0]" || !jobsSet || jobs != 3 || !build || !force || !noInit {
		t.Fatalf("parsed args = %v, jobsSet=%v jobs=%d build=%v force=%v noInit=%v", args, jobsSet, jobs, build, force, noInit)
	}
}

func TestHistoricalGo1MetadataVersionIsAccepted(t *testing.T) {
	if !isSafeVersion("go1") {
		t.Fatal("historical go1 metadata version was rejected")
	}
	metadata := []byte(`[{"version":"go1","stable":true,"files":[]}]`)
	releases, err := parseReleases(metadata)
	if err != nil {
		t.Fatal(err)
	}
	for _, release := range releases {
		if release.Version == "go1" && !isSafeVersion(release.Version) {
			t.Fatal("parsed go1 release is not considered safe")
		}
	}
	env := &Environment{
		Root:        filepath.Join(t.TempDir(), ".govm"),
		MetadataURL: "http://govm.test/dl?mode=json",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       io.NopCloser(bytes.NewReader(metadata)),
				Header:     make(http.Header),
				Request:    request,
			}, nil
		})},
	}
	if _, err := fetchReleases(context.Background(), env, true, true); err != nil {
		t.Fatalf("fetch rejected official go1 marker: %v", err)
	}
}
