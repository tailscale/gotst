// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"go/token"
	"slices"
	"strings"
)

// invocation is the resolved command line: a profile, optionally with an
// explicit package set and/or a subset of tests.
type invocation struct {
	profile runProfile
	tests   testSelection
}

type qualifiedTest struct {
	packageSpec string
	packagePath string // filled in after resolving packageSpec with go list
	name        string
}

type testSelection struct {
	unqualified []string
	qualified   []qualifiedTest
}

func parseInvocation(defs *profileDefinitions, args []string) (invocation, error) {
	profileName := "default"
	if len(args) > 0 {
		if _, ok := defs.profiles[args[0]]; ok {
			profileName = args[0]
			args = args[1:]
		}
	}
	profile, err := defs.resolve(profileName)
	if err != nil {
		return invocation{}, err
	}

	var packages []string
	var tests testSelection
	for _, arg := range args {
		if isTestName(arg) {
			tests.unqualified = appendUnique(tests.unqualified, arg)
			continue
		}
		if pkg, name, ok := splitQualifiedTest(arg); ok {
			q := qualifiedTest{packageSpec: pkg, name: name}
			if !slices.Contains(tests.qualified, q) {
				tests.qualified = append(tests.qualified, q)
			}
			continue
		}
		packages = appendUnique(packages, arg)
	}
	if len(packages) > 0 {
		profile.Packages = packages
	}
	return invocation{profile: profile, tests: tests}, nil
}

func isTestName(s string) bool {
	if !token.IsIdentifier(s) {
		return false
	}
	return strings.HasPrefix(s, "Test") || strings.HasPrefix(s, "Fuzz") || strings.HasPrefix(s, "Example")
}

func splitQualifiedTest(arg string) (pkg, test string, ok bool) {
	i := strings.LastIndexByte(arg, '.')
	if i <= 0 || i == len(arg)-1 || !isTestName(arg[i+1:]) {
		return "", "", false
	}
	return arg[:i], arg[i+1:], true
}

func (s testSelection) active() bool {
	return len(s.unqualified) > 0 || len(s.qualified) > 0
}

func (s testSelection) requestedNames() []string {
	ret := slices.Clone(s.unqualified)
	for _, q := range s.qualified {
		ret = append(ret, q.packageSpec+"."+q.name)
	}
	return ret
}

func (s testSelection) wants(pkg, test string) bool {
	if !s.active() {
		return true
	}
	if slices.Contains(s.unqualified, test) {
		return true
	}
	for _, q := range s.qualified {
		if q.packagePath == pkg && q.name == test {
			return true
		}
	}
	return false
}

func (s testSelection) packageTests(pkg string) []string {
	var ret []string
	for _, q := range s.qualified {
		if q.packagePath == pkg {
			ret = appendUnique(ret, q.name)
		}
	}
	return ret
}

func (s testSelection) describeMissing(found map[string]bool) error {
	var missing []string
	for _, name := range s.requestedNames() {
		if !found[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("requested test(s) not found: %s", strings.Join(missing, ", "))
}
