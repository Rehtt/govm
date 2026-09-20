package main

import (
	"context"
	"errors"
	"fmt"
	"os"
)

func useGoWithEnvironment(environment *Environment, requested string, setDefault bool) error {
	env, err := environment.normalized()
	if err != nil {
		return err
	}
	if err := env.ensureLayout(); err != nil {
		return err
	}
	version, err := installedVersion(env, requested)
	if err != nil {
		return err
	}
	if !validInstalledTree(env, version) {
		return fmt.Errorf("Go version %s is not installed or is incomplete", version)
	}
	if err := withStateLock(context.Background(), env, func() error {
		if err := setCurrent(env, version); err != nil {
			return err
		}
		if setDefault {
			return setDefaultVersion(env, version)
		}
		return nil
	}); err != nil {
		return err
	}
	fmt.Fprintf(env.Output, "using %s\n", version)
	return nil
}

func setDefaultVersion(env *Environment, version string) error {
	return setDefault(env, version)
}

func uninstallGo(environment *Environment, requested string, force bool) error {
	env, err := environment.normalized()
	if err != nil {
		return err
	}
	if err := env.ensureLayout(); err != nil {
		return err
	}
	version, err := installedVersion(env, requested)
	if err != nil {
		return err
	}
	if err := withStateLock(context.Background(), env, func() error {
		current, currentErr := currentVersion(env)
		if currentErr != nil {
			return currentErr
		}
		defaultVersion, defaultErr := readDefaultVersion(env)
		if defaultErr != nil {
			return defaultErr
		}
		if !force && (version == current || version == defaultVersion) {
			return fmt.Errorf("cannot uninstall %s while it is current or default; use --force", version)
		}
		if err := removeInstalledVersion(env, version); err != nil {
			return err
		}
		if force && version == current {
			if err := removeCurrentLink(env); err != nil {
				return err
			}
		}
		if force && version == defaultVersion {
			if err := os.Remove(env.defaultPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("clear default version: %w", err)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	fmt.Fprintf(env.Output, "uninstalled %s\n", version)
	return nil
}

func removeCurrentLink(env *Environment) error {
	info, err := os.Lstat(env.currentPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("refusing to remove non-symlink current path %s", env.currentPath())
	}
	return os.Remove(env.currentPath())
}
