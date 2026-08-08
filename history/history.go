// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package history defines gotst's advisory test-history data model, store
// interface, and versioned HTTP wire protocol.
//
// This package deliberately has no dependency on the gotst command. History
// backends and HTTP servers can import it without importing package main.
package history

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"time"
)

const (
	// Version is the current history identity, storage, and HTTP protocol
	// version.
	Version = 1

	// LookupPath and RecordPath are the HTTP endpoints relative to a configured
	// history service base URL.
	LookupPath = "/v1/history/lookup"
	RecordPath = "/v1/history/record"
)

// Store holds advisory scheduling and resource history. It is independent of
// gotst's correctness-sensitive test-result cache. Store failures must not
// prevent tests from running.
type Store interface {
	// Lookup returns history for keys that the store knows. Each returned
	// History contains at most opts.RecentPerKey observations, ordered newest
	// first. The result map is keyed by Key.ID(). A non-positive RecentPerKey
	// asks the implementation to use its default limit.
	Lookup(ctx context.Context, keys []Key, opts LookupOptions) (map[string]*History, error)

	// Record adds observations. Implementations must make repeated writes of an
	// observation with the same Key and ID idempotent.
	Record(ctx context.Context, observations []Observation) error
}

// LookupOptions controls a Store lookup.
type LookupOptions struct {
	// RecentPerKey is the maximum number of recent observations returned for
	// each requested key. A non-positive value selects the store's default.
	RecentPerKey int
}

// Key identifies history that is useful across source changes, rebuilds,
// checkout locations, and machines. It intentionally excludes the test binary
// hash; history is advisory rather than proof that a result can be reused.
type Key struct {
	Package string `json:"package"`
	Test    string `json:"test"`
	GOOS    string `json:"goos"`
	GOARCH  string `json:"goarch"`
	// Tags is treated as an unordered set by ID.
	Tags []string `json:"tags,omitempty"`
	// Args remains order-sensitive.
	Args []string `json:"args,omitempty"`
}

// ID returns the lowercase hexadecimal SHA-256 of Key's versioned canonical
// representation. Store implementations and HTTP services use it as the
// stable database and result-map key.
func (k Key) ID() string {
	h := sha256.New()
	fmt.Fprintf(h, "gotst history key v%d\npackage %s\ntest %s\ngoos %s\ngoarch %s\n",
		Version, k.Package, k.Test, k.GOOS, k.GOARCH)
	tags := slices.Clone(k.Tags)
	sort.Strings(tags)
	for _, tag := range tags {
		fmt.Fprintf(h, "tag %q\n", tag)
	}
	for _, arg := range k.Args {
		fmt.Fprintf(h, "arg %q\n", arg)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Outcome is the aggregate result of one executed top-level test, including
// any retries made by gotst.
type Outcome string

const (
	OutcomePass  Outcome = "pass"
	OutcomeFail  Outcome = "fail"
	OutcomeFlaky Outcome = "flaky"
)

// Dependency describes the portable shape of a test-log dependency. It does
// not contain environment values, file contents, or absolute checkout paths.
type Dependency struct {
	Operation string `json:"operation"`
	Name      string `json:"name"`
	Path      string `json:"path,omitempty"`
}

// Observation describes one completed, actually executed top-level test.
type Observation struct {
	// ID is an opaque idempotency key unique within the history service's
	// repository or tenant scope.
	ID           string        `json:"id"`
	Key          Key           `json:"key"`
	ObservedAt   time.Time     `json:"observed_at"`
	Outcome      Outcome       `json:"outcome"`
	Duration     time.Duration `json:"duration_ns"`
	Attempts     int           `json:"attempts"`
	PeakRSSBytes int64         `json:"peak_rss_bytes,omitempty"`

	// Dependencies is nil when input capture failed. An empty, non-nil slice
	// means capture succeeded and observed no inputs.
	Dependencies []Dependency `json:"dependencies"`
}

// History contains the recent observations for one Key, newest first.
type History struct {
	Version      int           `json:"version"`
	Key          Key           `json:"key"`
	Observations []Observation `json:"observations"`
}

// LookupRequest is the JSON body accepted at LookupPath.
type LookupRequest struct {
	// Version must equal [Version].
	Version int   `json:"version"`
	Keys    []Key `json:"keys"`

	// RecentPerKey is the maximum number of observations requested for each
	// key. It is encoded as "limit" in the version 1 protocol.
	RecentPerKey int `json:"limit"`
}

// LookupResponse is the JSON body returned from LookupPath.
type LookupResponse struct {
	// Version must equal [Version].
	Version   int        `json:"version"`
	Histories []*History `json:"histories"`
}

// RecordRequest is the JSON body accepted at RecordPath.
type RecordRequest struct {
	// Version must equal [Version].
	Version      int           `json:"version"`
	Observations []Observation `json:"observations"`
}
