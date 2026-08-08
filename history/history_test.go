// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package history_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tailscale/gotst/history"
)

func TestKeyID(t *testing.T) {
	k := history.Key{Package: "example.com/p", Test: "TestOne", GOOS: "linux", GOARCH: "amd64"}
	if got, want := k.ID(), "d98a641377c6fb5711b66ae1ddd99952af2cda0ed8f3cfc8dc08c8cc4d7e1920"; got != want {
		t.Fatalf("Key.ID() = %q; want %q", got, want)
	}
	k.Test = "TestTwo"
	if k.ID() == "d98a641377c6fb5711b66ae1ddd99952af2cda0ed8f3cfc8dc08c8cc4d7e1920" {
		t.Fatal("different keys have the same ID")
	}
}

func TestKeyIDTreatsTagsAsSet(t *testing.T) {
	a := history.Key{Package: "example.com/p", Test: "TestOne", Tags: []string{"one", "two"}}
	b := history.Key{Package: "example.com/p", Test: "TestOne", Tags: []string{"two", "one"}}
	if a.ID() != b.ID() {
		t.Fatalf("IDs differ for reordered tags: %q != %q", a.ID(), b.ID())
	}
}

func TestLookupRequestWireFormat(t *testing.T) {
	req := history.LookupRequest{
		Version:      history.Version,
		Keys:         []history.Key{{Package: "example.com/p", Test: "TestOne"}},
		RecentPerKey: 7,
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); !strings.Contains(got, `"limit":7`) || strings.Contains(got, "RecentPerKey") {
		t.Fatalf("LookupRequest JSON = %s; want version 1 limit field", got)
	}
}
