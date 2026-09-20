package main

import (
	"fmt"
	"os"
)

func cleanCache(environment *Environment, all bool) error {
	env, err := environment.normalized()
	if err != nil {
		return err
	}
	if err := env.ensureLayout(); err != nil {
		return err
	}
	dirs := []string{env.downloadsDir(), env.releasesDir()}
	if all {
		dirs = append(dirs, env.bootstrapDir())
	}
	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("clean cache %s: %w", dir, err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("recreate cache %s: %w", dir, err)
		}
	}
	fmt.Fprintln(env.Output, "cache cleaned")
	return nil
}
