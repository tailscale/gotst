// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"os"
	"testing"
)

func Test(t *testing.T) {
	// TODO. For now, just
}

// TestReadFile exists for testing -test.testlogfile.
func TestReadFile(t *testing.T) {
	_, err := os.ReadFile("README.md")
	if err != nil {
		t.Errorf("failed to read README.md: %v", err)
	}
	os.ReadFile("../README.md")
}

// TestCheckEnvVar exists for testing -test.testlogfile.
func TestCheckEnvVar(t *testing.T) {
	if os.Getenv("GOTST_TEST_ENV_VAR") == "fail" {
		t.Fatal("fail")
	}
}
