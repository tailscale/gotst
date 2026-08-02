// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const profileConfigName = ".gotst.yml"

type profileFile struct {
	Version  int                   `yaml:"version"`
	Profiles map[string]rawProfile `yaml:"profiles"`
}

type rawProfile struct {
	Include         []string `yaml:"include"`
	Packages        []string `yaml:"packages"`
	ExcludePackages []string `yaml:"exclude_packages"`
	Tags            []string `yaml:"tags"`
	TestFlags       []string `yaml:"test_flags"`
	Short           *bool    `yaml:"short"`
	Timeout         *string  `yaml:"timeout"`
}

// runProfile is the fully resolved configuration for one gotst invocation.
type runProfile struct {
	Name            string
	Root            string
	Packages        []string
	ExcludePackages []string
	Tags            []string
	TestFlags       []string
	Short           bool
	Timeout         time.Duration
	shortSet        bool
}

func loadRunProfile(configPath, name string) (runProfile, error) {
	if name == "" {
		name = "default"
	}
	if configPath == "" {
		var err error
		configPath, err = findProfileConfig()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && name == "default" {
				wd, wdErr := os.Getwd()
				if wdErr != nil {
					return runProfile{}, wdErr
				}
				return runProfile{Name: name, Root: wd, Packages: []string{"./..."}}, nil
			}
			return runProfile{}, err
		}
	}

	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		return runProfile{}, fmt.Errorf("resolving config path: %w", err)
	}
	f, err := os.Open(absConfig)
	if err != nil {
		return runProfile{}, fmt.Errorf("opening profile config %q: %w", absConfig, err)
	}
	defer f.Close()

	var pf profileFile
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&pf); err != nil {
		return runProfile{}, fmt.Errorf("parsing profile config %q: %w", absConfig, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return runProfile{}, fmt.Errorf("parsing profile config %q: multiple YAML documents are not supported", absConfig)
		}
		return runProfile{}, fmt.Errorf("parsing profile config %q: %w", absConfig, err)
	}
	if pf.Version != 1 {
		return runProfile{}, fmt.Errorf("profile config %q has version %d; only version 1 is supported", absConfig, pf.Version)
	}
	if len(pf.Profiles) == 0 {
		return runProfile{}, fmt.Errorf("profile config %q defines no profiles", absConfig)
	}

	resolved, err := resolveProfile(pf.Profiles, name)
	if err != nil {
		return runProfile{}, fmt.Errorf("profile config %q: %w", absConfig, err)
	}
	resolved.Name = name
	resolved.Root = filepath.Dir(absConfig)
	if len(resolved.Packages) == 0 {
		resolved.Packages = []string{"./..."}
	}
	return resolved, nil
}

func findProfileConfig() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		path := filepath.Join(dir, profileConfigName)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

func resolveProfile(profiles map[string]rawProfile, name string) (runProfile, error) {
	var stack []string
	resolved := make(map[string]runProfile)
	visiting := make(map[string]bool)

	var visit func(string) (runProfile, error)
	visit = func(cur string) (runProfile, error) {
		if p, ok := resolved[cur]; ok {
			return p, nil
		}
		raw, ok := profiles[cur]
		if !ok {
			names := make([]string, 0, len(profiles))
			for n := range profiles {
				names = append(names, n)
			}
			slices.Sort(names)
			return runProfile{}, fmt.Errorf("unknown profile %q (available: %s)", cur, strings.Join(names, ", "))
		}
		if visiting[cur] {
			at := slices.Index(stack, cur)
			cycle := append(slices.Clone(stack[at:]), cur)
			return runProfile{}, fmt.Errorf("profile include cycle: %s", strings.Join(cycle, " -> "))
		}
		visiting[cur] = true
		stack = append(stack, cur)

		var out runProfile
		for _, inc := range raw.Include {
			p, err := visit(inc)
			if err != nil {
				return runProfile{}, err
			}
			mergeRunProfile(&out, p)
		}
		if err := mergeRawProfile(&out, raw); err != nil {
			return runProfile{}, fmt.Errorf("profile %q: %w", cur, err)
		}

		stack = stack[:len(stack)-1]
		visiting[cur] = false
		resolved[cur] = out
		return out, nil
	}
	return visit(name)
}

func mergeRunProfile(dst *runProfile, src runProfile) {
	dst.Packages = appendUnique(dst.Packages, src.Packages...)
	dst.ExcludePackages = appendUnique(dst.ExcludePackages, src.ExcludePackages...)
	dst.Tags = appendUnique(dst.Tags, src.Tags...)
	dst.TestFlags = appendUnique(dst.TestFlags, src.TestFlags...)
	if src.shortSet {
		dst.Short = src.Short
		dst.shortSet = true
	}
	if src.Timeout != 0 {
		dst.Timeout = src.Timeout
	}
}

func mergeRawProfile(dst *runProfile, src rawProfile) error {
	dst.Packages = appendUnique(dst.Packages, src.Packages...)
	dst.ExcludePackages = appendUnique(dst.ExcludePackages, src.ExcludePackages...)
	dst.Tags = appendUnique(dst.Tags, src.Tags...)
	dst.TestFlags = appendUnique(dst.TestFlags, src.TestFlags...)
	if src.Short != nil {
		dst.Short = *src.Short
		dst.shortSet = true
	}
	if src.Timeout != nil {
		d, err := time.ParseDuration(*src.Timeout)
		if err != nil {
			return fmt.Errorf("invalid timeout %q: %w", *src.Timeout, err)
		}
		if d <= 0 {
			return fmt.Errorf("timeout must be positive, got %v", d)
		}
		dst.Timeout = d
	}
	return nil
}

func appendUnique(dst []string, values ...string) []string {
	for _, v := range values {
		if v != "" && !slices.Contains(dst, v) {
			dst = append(dst, v)
		}
	}
	return dst
}
