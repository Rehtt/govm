package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

type InstallOptions struct {
	Build       bool
	Force       bool
	NoInit      bool
	Jobs        int
	Environment *Environment
	Context     context.Context
}

type installRequest struct {
	release  Release
	artifact Artifact
}

func installVersions(requested []string, options InstallOptions) error {
	env, err := options.Environment.normalized()
	if err != nil {
		return err
	}
	if err := env.ensureLayout(); err != nil {
		return err
	}
	if len(requested) == 0 {
		return errors.New("version is required")
	}
	if options.Jobs < 0 {
		return errors.New("jobs must be at least 1")
	}
	jobs := options.Jobs
	if jobs < 1 {
		jobs = 1
	}
	if options.Build && options.Jobs == 0 {
		jobs = 1
	}
	if !options.Build && options.Jobs == 0 {
		jobs = 2
	}

	before, err := installedVersions(env)
	if err != nil {
		return fmt.Errorf("read installed versions: %w", err)
	}
	firstInstall := true
	for _, version := range before {
		if validInstalledTree(env, version) {
			firstInstall = false
			break
		}
	}
	ctx := contextOrBackground(options.Context)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Include archived and pre-release metadata for explicit version requests;
	// latest still resolves only to stable releases below.
	releases, err := fetchReleases(ctx, env, true, false)
	if err != nil {
		return err
	}
	requests := make([]installRequest, 0, len(requested))
	seen := make(map[string]struct{}, len(requested))
	for _, value := range requested {
		release, artifact, resolveErr := resolveReleaseArtifact(releases, value, options.Build, runtime.GOOS, runtime.GOARCH)
		if resolveErr != nil {
			return resolveErr
		}
		if _, ok := seen[release.Version]; ok {
			continue
		}
		seen[release.Version] = struct{}{}
		requests = append(requests, installRequest{release: release, artifact: artifact})
	}
	if len(requests) == 0 {
		return errors.New("no versions to install")
	}

	if jobs > len(requests) {
		jobs = len(requests)
	}
	tasks := make(chan installRequest)
	var wait sync.WaitGroup
	var firstErr error
	var firstErrOnce sync.Once
	worker := func() {
		defer wait.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case request, ok := <-tasks:
				if !ok {
					return
				}
				if installErr := installOne(ctx, env, request.release, request.artifact, options); installErr != nil {
					firstErrOnce.Do(func() { firstErr = installErr })
					cancel()
					return
				}
			}
		}
	}
	wait.Add(jobs)
	for i := 0; i < jobs; i++ {
		go worker()
	}
send:
	for _, request := range requests {
		select {
		case <-ctx.Done():
			break send
		case tasks <- request:
		}
	}
	close(tasks)
	wait.Wait()
	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	// Only after every requested version has committed do we alter current or
	// default. A failed source build therefore cannot switch either pointer.
	installed := make([]string, 0, len(requests))
	for _, request := range requests {
		installed = append(installed, request.release.Version)
	}
	if err := withStateLock(ctx, env, func() error {
		current, currentErr := currentVersion(env)
		if currentErr != nil {
			return currentErr
		}
		defaultVersion, defaultErr := readDefaultVersion(env)
		if defaultErr != nil {
			return defaultErr
		}
		if current == "" || !validInstalledTree(env, current) {
			choice := installed[0]
			if validInstalledTree(env, defaultVersion) {
				choice = defaultVersion
			}
			if err := setCurrent(env, choice); err != nil {
				return err
			}
			current = choice
		}
		if defaultVersion == "" || !validInstalledTree(env, defaultVersion) {
			if err := setDefault(env, current); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}

	if firstInstall && !options.NoInit {
		// Unsupported shells are intentionally a non-fatal no-op.
		if err := initShell(env, ""); err != nil {
			return err
		}
	}
	for _, version := range installed {
		fmt.Fprintf(env.Output, "installed %s\n", version)
	}
	return nil
}

func installOne(ctx context.Context, env *Environment, release Release, artifact Artifact, options InstallOptions) error {
	version := release.Version
	lock, err := acquireFileLock(ctx, filepath.Join(env.locksDir(), lockName("install-", version)))
	if err != nil {
		return fmt.Errorf("lock %s: %w", version, err)
	}
	defer lock.Close()
	target := versionDir(env, version)
	if info, statErr := os.Lstat(target); statErr == nil {
		if !options.Force {
			if info.IsDir() {
				return fmt.Errorf("Go version %s is already installed; use --force to replace it", version)
			}
			return fmt.Errorf("installation path for %s already exists; use --force to replace it", version)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}

	temporary, err := os.MkdirTemp(env.versionsDir(), "."+version+".tmp-")
	if err != nil {
		return fmt.Errorf("create installation staging directory: %w", err)
	}
	defer os.RemoveAll(temporary)

	archivePath, err := downloadArtifact(ctx, env, artifact)
	if err != nil {
		return err
	}
	extracted, err := extractArchive(archivePath, filepath.Join(temporary, "extract"))
	if err != nil {
		return fmt.Errorf("extract %s: %w", version, err)
	}
	if options.Build {
		bootstrap, bootstrapErr := ensureBootstrap(ctx, env, nil)
		if bootstrapErr != nil {
			return bootstrapErr
		}
		if err := buildFromSource(ctx, env, extracted, target, version, bootstrap); err != nil {
			return err
		}
	}
	if err := validateGoRoot(extracted, version); err != nil {
		return fmt.Errorf("validate %s: %w", version, err)
	}
	if err := commitVersionDir(env, version, extracted, options.Force); err != nil {
		return err
	}
	return nil
}

func validateGoRoot(root, expectedVersion string) error {
	if !validBinaryTree(root) {
		return errors.New("missing bin/go (or bin/go.exe) and VERSION")
	}
	versionData, err := os.ReadFile(filepath.Join(root, "VERSION"))
	if err != nil {
		return fmt.Errorf("read VERSION: %w", err)
	}
	actual := strings.TrimSpace(string(versionData))
	if actual != expectedVersion {
		return fmt.Errorf("VERSION is %q, expected %q", actual, expectedVersion)
	}
	return nil
}

func commitVersionDir(env *Environment, version, staged string, force bool) error {
	target := versionDir(env, version)
	if !samePathOrChild(staged, env.versionsDir()) || filepath.Clean(staged) == filepath.Clean(env.versionsDir()) {
		return errors.New("refusing to commit a path outside the versions directory")
	}
	if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(staged, target); err != nil {
			return fmt.Errorf("commit %s: %w", version, err)
		}
		return nil
	} else if err != nil {
		return err
	}
	if !force {
		return fmt.Errorf("Go version %s is already installed", version)
	}
	backup, err := os.MkdirTemp(env.versionsDir(), "."+version+".old-")
	if err != nil {
		return err
	}
	backupPath := backup
	if err := os.Remove(backupPath); err != nil {
		return err
	}
	if err := os.Rename(target, backupPath); err != nil {
		return fmt.Errorf("move old %s aside: %w", version, err)
	}
	if err := os.Rename(staged, target); err != nil {
		_ = os.Rename(backupPath, target)
		return fmt.Errorf("commit replacement %s: %w", version, err)
	}
	if err := os.RemoveAll(backupPath); err != nil {
		return fmt.Errorf("remove old %s: %w", version, err)
	}
	return nil
}

func ensureBootstrap(ctx context.Context, env *Environment, releases []Release) (string, error) {
	if root := os.Getenv("GOROOT_BOOTSTRAP"); root != "" && validBootstrapRoot(root) {
		absolute, _ := filepath.Abs(root)
		return absolute, nil
	}
	if goPath, err := exec.LookPath("go"); err == nil {
		if root, err := gorootFromPath(ctx, goPath); err == nil && validBootstrapRoot(root) {
			return root, nil
		}
	}
	if releases == nil {
		var err error
		releases, err = fetchReleases(ctx, env, true, false)
		if err != nil {
			return "", err
		}
	}
	release, artifact, err := resolveReleaseArtifact(releases, "latest", false, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", fmt.Errorf("find bootstrap Go toolchain: %w", err)
	}
	target := env.bootstrapVersionPath(release.Version)
	if validBootstrapRoot(target) {
		return target, nil
	}
	lock, err := acquireFileLock(ctx, filepath.Join(env.locksDir(), lockName("bootstrap-", release.Version)))
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if validBootstrapRoot(target) {
		return target, nil
	}
	temporary, err := os.MkdirTemp(env.bootstrapDir(), "."+release.Version+".tmp-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temporary)
	archivePath, err := downloadArtifact(ctx, env, artifact)
	if err != nil {
		return "", fmt.Errorf("download bootstrap %s: %w", release.Version, err)
	}
	extracted, err := extractArchive(archivePath, filepath.Join(temporary, "extract"))
	if err != nil {
		return "", fmt.Errorf("extract bootstrap %s: %w", release.Version, err)
	}
	if err := validateGoRoot(extracted, release.Version); err != nil {
		return "", fmt.Errorf("validate bootstrap %s: %w", release.Version, err)
	}
	if _, statErr := os.Lstat(target); statErr == nil {
		backup := target + ".old"
		_ = os.RemoveAll(backup)
		if err := os.Rename(target, backup); err != nil {
			return "", err
		}
		if err := os.Rename(extracted, target); err != nil {
			_ = os.Rename(backup, target)
			return "", err
		}
		_ = os.RemoveAll(backup)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	} else if err := os.Rename(extracted, target); err != nil {
		return "", err
	}
	return target, nil
}

func validBootstrapRoot(root string) bool {
	if root == "" || !hasGoExecutable(root) {
		return false
	}
	info, err := os.Stat(root)
	return err == nil && info.IsDir()
}

func hasGoExecutable(root string) bool {
	name := "go"
	if runtime.GOOS == "windows" {
		name = "go.exe"
	}
	info, err := os.Lstat(filepath.Join(root, "bin", name))
	return err == nil && info.Mode().IsRegular()
}

func gorootFromPath(ctx context.Context, goPath string) (string, error) {
	command := exec.CommandContext(contextOrBackground(ctx), goPath, "env", "GOROOT")
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	root := strings.TrimSpace(string(output))
	if root == "" {
		return "", errors.New("go env GOROOT returned an empty path")
	}
	return root, nil
}

func buildFromSource(ctx context.Context, env *Environment, sourceRoot, finalRoot, version, bootstrap string) error {
	if err := os.MkdirAll(env.logsDir(), 0o755); err != nil {
		return err
	}
	logPath := filepath.Join(env.logsDir(), "build-"+version+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create build log: %w", err)
	}
	defer logFile.Close()
	command, args := buildCommand(sourceRoot)
	cmd := exec.CommandContext(contextOrBackground(ctx), command, args...)
	cmd.Dir = filepath.Join(sourceRoot, "src")
	cmd.Env = buildEnvironment(bootstrap, finalRoot)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	fmt.Fprintf(logFile, "command: %s %s\nsource: %s\nGOROOT_BOOTSTRAP: %s\nGOROOT_FINAL: %s\n\n", command, strings.Join(args, " "), sourceRoot, bootstrap, finalRoot)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build Go %s failed (see %s): %w", version, logPath, err)
	}
	return nil
}

func buildCommand(sourceRoot string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd.exe", []string{"/c", filepath.Join(sourceRoot, "src", "make.bat")}
	}
	return filepath.Join(sourceRoot, "src", "make.bash"), nil
}

func buildEnvironment(bootstrap, finalRoot string) []string {
	env := make([]string, 0, len(os.Environ())+4)
	for _, item := range os.Environ() {
		if strings.HasPrefix(item, "GOROOT=") || strings.HasPrefix(item, "GOROOT_BOOTSTRAP=") || strings.HasPrefix(item, "GOROOT_FINAL=") {
			continue
		}
		env = append(env, item)
	}
	pathValue := os.Getenv("PATH")
	bootstrapBin := filepath.Join(bootstrap, "bin")
	if runtime.GOOS == "windows" {
		pathValue = bootstrapBin + ";" + pathValue
	} else {
		pathValue = bootstrapBin + ":" + pathValue
	}
	env = append(env, "PATH="+pathValue, "GOROOT_BOOTSTRAP="+bootstrap, "GOROOT_FINAL="+finalRoot)
	return env
}
