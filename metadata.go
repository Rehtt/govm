package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Release struct {
	Version string     `json:"version"`
	Stable  bool       `json:"stable"`
	Files   []Artifact `json:"files"`
}

type Artifact struct {
	Filename string `json:"filename"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Version  string `json:"version"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Kind     string `json:"kind"`
	URL      string `json:"url,omitempty"`
}

func (a Artifact) artifactKind() string {
	if a.Kind != "" {
		return strings.ToLower(a.Kind)
	}
	name := strings.ToLower(a.Filename)
	if strings.Contains(name, ".src.") || strings.HasSuffix(name, ".src.tar.gz") || strings.HasSuffix(name, ".src.zip") {
		return "source"
	}
	return "archive"
}

type metadataCache struct {
	FetchedAt time.Time `json:"fetched_at"`
	Endpoint  string    `json:"endpoint"`
	All       bool      `json:"all"`
	Releases  []Release `json:"releases"`
}

var safeVersionPattern = regexp.MustCompile(`^go[0-9][0-9A-Za-z.+-]*$`)
var sha256Pattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

func fetchReleases(ctx context.Context, env *Environment, all, refresh bool) ([]Release, error) {
	ctx = contextOrBackground(ctx)
	normalized, err := env.normalized()
	if err != nil {
		return nil, err
	}
	env = normalized
	if err := env.ensureLayout(); err != nil {
		return nil, err
	}
	cachePath := env.metadataCachePath(all)
	if !refresh {
		if cached, ok := readMetadataCache(cachePath, env.MetadataTTL, env.MetadataURL, all); ok {
			return cached, nil
		}
	}

	endpoint, err := url.Parse(env.MetadataURL)
	if err != nil {
		return nil, fmt.Errorf("parse release metadata URL: %w", err)
	}
	query := endpoint.Query()
	if query.Get("mode") == "" {
		query.Set("mode", "json")
	}
	if all {
		query.Set("include", "all")
	}
	endpoint.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create metadata request: %w", err)
	}
	response, err := env.HTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download release metadata: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("download release metadata: HTTP %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("read release metadata: %w", err)
	}
	releases, err := parseReleases(body)
	if err != nil {
		return nil, err
	}
	for i := range releases {
		if releases[i].Version == "" || !isSafeVersion(releases[i].Version) {
			return nil, fmt.Errorf("release metadata contains invalid version %q", releases[i].Version)
		}
		for j := range releases[i].Files {
			if releases[i].Files[j].Filename == "" {
				return nil, fmt.Errorf("release %s contains an artifact without a filename", releases[i].Version)
			}
		}
	}
	cache := metadataCache{FetchedAt: time.Now().UTC(), Endpoint: env.MetadataURL, All: all, Releases: releases}
	encoded, err := json.Marshal(cache)
	if err == nil {
		_ = writeFileAtomic(cachePath, encoded, 0o644)
	}
	return releases, nil
}

func readMetadataCache(path string, ttl time.Duration, endpoint string, all bool) ([]Release, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var cache metadataCache
	if json.Unmarshal(data, &cache) != nil || cache.FetchedAt.IsZero() || cache.Releases == nil || cache.Endpoint != endpoint || cache.All != all {
		return nil, false
	}
	if time.Since(cache.FetchedAt) > ttl {
		return nil, false
	}
	return cache.Releases, true
}

func parseReleases(data []byte) ([]Release, error) {
	var releases []Release
	if err := json.Unmarshal(data, &releases); err != nil {
		return nil, fmt.Errorf("parse release metadata: %w", err)
	}
	if releases == nil {
		return nil, errors.New("release metadata is empty")
	}
	return releases, nil
}

func isSafeVersion(version string) bool {
	// The all-releases feed contains the historical go1 release marker.
	// It is a valid metadata/path name even though current platforms may not
	// have an installable archive for it.
	if len(version) < 3 || len(version) > 128 || !safeVersionPattern.MatchString(version) {
		return false
	}
	return !strings.Contains(version, "..") && !strings.ContainsAny(version, `/\\`) && version != "go."
}

func canonicalVersion(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "latest" {
		return value, nil
	}
	if !strings.HasPrefix(value, "go") {
		value = "go" + value
	}
	if !isSafeVersion(value) {
		return "", fmt.Errorf("invalid Go version %q", value)
	}
	return value, nil
}

type versionParts struct {
	major int
	minor int
	patch int
	suf   string
}

func parseVersionParts(version string) versionParts {
	version = strings.TrimPrefix(version, "go")
	parts := strings.SplitN(version, ".", 3)
	result := versionParts{}
	if len(parts) > 0 {
		result.major, _ = strconv.Atoi(parts[0])
	}
	if len(parts) > 1 {
		minorText := parts[1]
		for i, r := range minorText {
			if r < '0' || r > '9' {
				result.suf = minorText[i:]
				minorText = minorText[:i]
				break
			}
		}
		result.minor, _ = strconv.Atoi(minorText)
	}
	if len(parts) > 2 {
		patchText := parts[2]
		for i, r := range patchText {
			if r < '0' || r > '9' {
				if result.suf == "" {
					result.suf = patchText[i:]
				}
				patchText = patchText[:i]
				break
			}
		}
		result.patch, _ = strconv.Atoi(patchText)
	}
	return result
}

func compareVersions(left, right string) int {
	a, b := parseVersionParts(left), parseVersionParts(right)
	if a.major != b.major {
		if a.major < b.major {
			return -1
		}
		return 1
	}
	if a.minor != b.minor {
		if a.minor < b.minor {
			return -1
		}
		return 1
	}
	if a.patch != b.patch {
		if a.patch < b.patch {
			return -1
		}
		return 1
	}
	// A final release sorts after beta/rc suffixes. This is mainly relevant
	// when an endpoint labels a pre-release with a numeric-looking version.
	if a.suf == b.suf {
		return 0
	}
	if a.suf == "" {
		return 1
	}
	if b.suf == "" {
		return -1
	}
	return strings.Compare(a.suf, b.suf)
}

func artifactFor(release Release, build bool, goos, goarch string) (Artifact, bool) {
	wantKind := "archive"
	if build {
		wantKind = "source"
	}
	for _, artifact := range release.Files {
		if artifact.artifactKind() != wantKind {
			continue
		}
		if !build && (artifact.OS != goos || artifact.Arch != goarch) {
			continue
		}
		return artifact, true
	}
	return Artifact{}, false
}

func resolveReleaseArtifact(releases []Release, requested string, build bool, goos, goarch string) (Release, Artifact, error) {
	requested, err := canonicalVersion(requested)
	if err != nil {
		return Release{}, Artifact{}, err
	}
	candidates := make([]Release, 0, len(releases))
	if requested == "latest" {
		for _, release := range releases {
			if release.Stable {
				candidates = append(candidates, release)
			}
		}
	} else {
		for _, release := range releases {
			if release.Version == requested {
				candidates = append(candidates, release)
			}
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return compareVersions(candidates[i].Version, candidates[j].Version) > 0
	})
	for _, release := range candidates {
		if artifact, ok := artifactFor(release, build, goos, goarch); ok {
			return release, artifact, nil
		}
	}
	kind := "binary archive"
	if build {
		kind = "source archive"
	}
	return Release{}, Artifact{}, fmt.Errorf("no %s for Go version %q on %s/%s", kind, requested, goos, goarch)
}

func artifactURL(env *Environment, artifact Artifact) (string, error) {
	if artifact.URL != "" {
		return artifact.URL, nil
	}
	base := env.DownloadBaseURL
	if base == "" {
		metadata, err := url.Parse(env.MetadataURL)
		if err != nil {
			return "", err
		}
		metadata.Path = filepath.ToSlash(filepath.Dir(metadata.Path) + "/")
		metadata.RawQuery = ""
		metadata.Fragment = ""
		base = metadata.String()
		if !strings.HasSuffix(base, "/") {
			base += "/"
		}
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parse download base URL: %w", err)
	}
	if baseURL.Scheme == "" || baseURL.Host == "" {
		return "", fmt.Errorf("download base URL must be absolute: %q", base)
	}
	if !strings.HasSuffix(baseURL.Path, "/") {
		baseURL.Path += "/"
	}
	return baseURL.ResolveReference(&url.URL{Path: artifact.Filename}).String(), nil
}
