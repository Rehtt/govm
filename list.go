package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"sort"
)

func listGoWithOptions(environment *Environment, jsonOutput bool) error {
	env, err := environment.normalized()
	if err != nil {
		return err
	}
	versions, err := installedVersions(env)
	if err != nil {
		return fmt.Errorf("list installed versions: %w", err)
	}
	current, currentErr := currentVersion(env)
	if currentErr != nil {
		return currentErr
	}
	defaultVersion, defaultErr := readDefaultVersion(env)
	if defaultErr != nil {
		return defaultErr
	}
	items := make([]InstalledVersion, 0, len(versions))
	for _, version := range versions {
		items = append(items, InstalledVersion{
			Version: version,
			Current: version == current,
			Default: version == defaultVersion,
			Valid:   validInstalledTree(env, version),
		})
	}
	if jsonOutput {
		return encodeJSON(env.Output, items)
	}
	if len(items) == 0 {
		fmt.Fprintln(env.Output, "No Go versions installed.")
		return nil
	}
	for _, item := range items {
		markers := ""
		if item.Current {
			markers += " current"
		}
		if item.Default {
			markers += " default"
		}
		if !item.Valid {
			markers += " incomplete"
		}
		fmt.Fprintf(env.Output, "%s%s\n", item.Version, markers)
	}
	return nil
}

type RemoteListOptions struct {
	All     bool
	Refresh bool
	JSON    bool
}

type RemoteVersion struct {
	Version  string `json:"version"`
	Stable   bool   `json:"stable"`
	Filename string `json:"filename"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Kind     string `json:"kind"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
}

func listRemoteGoWithOptions(environment *Environment, options RemoteListOptions) error {
	env, err := environment.normalized()
	if err != nil {
		return err
	}
	releases, err := fetchReleases(context.Background(), env, options.All, options.Refresh)
	if err != nil {
		return err
	}
	items := make([]RemoteVersion, 0, len(releases))
	seen := make(map[string]struct{}, len(releases))
	for _, release := range releases {
		if !options.All && !release.Stable {
			continue
		}
		artifact, ok := artifactFor(release, false, runtime.GOOS, runtime.GOARCH)
		if !ok {
			continue
		}
		if _, ok := seen[release.Version]; ok {
			continue
		}
		seen[release.Version] = struct{}{}
		items = append(items, RemoteVersion{
			Version:  release.Version,
			Stable:   release.Stable,
			Filename: artifact.Filename,
			OS:       artifact.OS,
			Arch:     artifact.Arch,
			Kind:     artifact.artifactKind(),
			SHA256:   artifact.SHA256,
			Size:     artifact.Size,
		})
	}
	sort.Slice(items, func(i, j int) bool { return compareVersions(items[i].Version, items[j].Version) > 0 })
	if options.JSON {
		return encodeJSON(env.Output, items)
	}
	for _, item := range items {
		label := item.Version
		if !item.Stable {
			label += " (pre-release)"
		}
		fmt.Fprintln(env.Output, label)
	}
	return nil
}

func encodeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
