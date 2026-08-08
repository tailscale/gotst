// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	historyVersion        = 1
	localHistoryLimit     = 32
	historyRequestLimit   = 32
	historyRequestTimeout = 5 * time.Second
)

var errHistoryNotFound = errors.New("test history not found")

// testHistoryStore is scheduling and resource history, independent from the
// correctness-sensitive test result cache. Implementations may be local or
// remote; failures must never prevent tests from running.
type testHistoryStore interface {
	Lookup(context.Context, []historyKey, int) (map[string]*testHistory, error)
	Record(context.Context, []historyObservation) error
}

// historyKey is stable across source changes, rebuilds, checkout locations,
// and machines. It intentionally excludes the test binary hash.
type historyKey struct {
	Package string   `json:"package"`
	Test    string   `json:"test"`
	GOOS    string   `json:"goos"`
	GOARCH  string   `json:"goarch"`
	Tags    []string `json:"tags,omitempty"`
	Args    []string `json:"args,omitempty"`
}

func (k historyKey) id() string {
	h := sha256.New()
	fmt.Fprintf(h, "gotst history key v%d\npackage %s\ntest %s\ngoos %s\ngoarch %s\n",
		historyVersion, k.Package, k.Test, k.GOOS, k.GOARCH)
	for _, tag := range k.Tags {
		fmt.Fprintf(h, "tag %q\n", tag)
	}
	for _, arg := range k.Args {
		fmt.Fprintf(h, "arg %q\n", arg)
	}
	return hex.EncodeToString(h.Sum(nil))
}

type historyOutcome string

const (
	historyPass  historyOutcome = "pass"
	historyFail  historyOutcome = "fail"
	historyFlaky historyOutcome = "flaky"
)

type historyDependency struct {
	Operation string `json:"operation"`
	Name      string `json:"name"`
	Path      string `json:"path,omitempty"`
}

type historyObservation struct {
	ID           string         `json:"id"`
	Key          historyKey     `json:"key"`
	ObservedAt   time.Time      `json:"observed_at"`
	Outcome      historyOutcome `json:"outcome"`
	Duration     time.Duration  `json:"duration_ns"`
	Attempts     int            `json:"attempts"`
	PeakRSSBytes int64          `json:"peak_rss_bytes,omitempty"`
	// Dependencies is nil when input capture failed. An empty, non-nil slice
	// means capture succeeded and observed no inputs.
	Dependencies []historyDependency `json:"dependencies"`
}

type testHistory struct {
	Version      int                  `json:"version"`
	Key          historyKey           `json:"key"`
	Observations []historyObservation `json:"observations"` // newest first
}

func newObservationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	// crypto/rand failure is extraordinarily unlikely. The ID is only for
	// deduplicating retries at a remote history service, not security.
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
}

func historyKeyForTask(task testTask, profile runProfile) historyKey {
	args := effectiveTestArgs(task, profile)
	tags := slices.Clone(profile.Tags)
	sort.Strings(tags)
	return historyKey{
		Package: task.bin.pkg,
		Test:    task.test,
		GOOS:    runtime.GOOS,
		GOARCH:  runtime.GOARCH,
		Tags:    tags,
		Args:    args,
	}
}

func historyDependencies(deps []cacheDependency, packageDir, moduleRoot string) []historyDependency {
	if deps == nil {
		return nil
	}
	ret := make([]historyDependency, 0, len(deps))
	for _, dep := range deps {
		hd := historyDependency{Operation: dep.Operation}
		if dep.Operation == "getenv" {
			hd.Name = dep.Name
		}
		if dep.Path != "" {
			hd.Path = portableHistoryPath(dep.Path, packageDir, moduleRoot)
		}
		ret = append(ret, hd)
	}
	slices.SortFunc(ret, func(a, b historyDependency) int {
		if c := strings.Compare(a.Operation, b.Operation); c != 0 {
			return c
		}
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Path, b.Path)
	})
	return slices.Compact(ret)
}

func portableHistoryPath(path, packageDir, moduleRoot string) string {
	path = filepath.Clean(path)
	if rel, ok := relativeWithin(path, packageDir); ok {
		return "$PACKAGE/" + filepath.ToSlash(rel)
	}
	if rel, ok := relativeWithin(path, moduleRoot); ok {
		return "$MODULE/" + filepath.ToSlash(rel)
	}
	// Avoid persisting machine-specific absolute paths, usernames, or checkout
	// locations to a remote history service. A hash still distinguishes two
	// external paths with the same basename; unlike module-relative inputs,
	// these dependencies intentionally do not match across differing machine
	// layouts.
	sum := sha256.Sum256([]byte(filepath.ToSlash(path)))
	return "$EXTERNAL/" + hex.EncodeToString(sum[:8]) + "/" + filepath.Base(path)
}

func relativeWithin(path, root string) (string, bool) {
	if root == "" {
		return "", false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if rel == "." {
		return "", true
	}
	return rel, true
}

type diskHistoryStore struct {
	dir string
}

func newDiskHistoryStore(cacheRoot string) (*diskHistoryStore, error) {
	dir := filepath.Join(cacheRoot, "history", fmt.Sprintf("v%d", historyVersion))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &diskHistoryStore{dir: dir}, nil
}

func (s *diskHistoryStore) Lookup(ctx context.Context, keys []historyKey, limit int) (map[string]*testHistory, error) {
	if limit <= 0 {
		limit = historyRequestLimit
	}
	ret := make(map[string]*testHistory)
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		h, err := s.read(key)
		if errors.Is(err, errHistoryNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(h.Observations) > limit {
			h.Observations = h.Observations[:limit]
		}
		ret[key.id()] = h
	}
	return ret, nil
}

func (s *diskHistoryStore) Record(ctx context.Context, observations []historyObservation) error {
	keyDirs := make(map[string]bool)
	seen := make(map[string]bool)
	for _, obs := range observations {
		if err := ctx.Err(); err != nil {
			return err
		}
		entryID := obs.Key.id() + "\x00" + obs.ID
		if seen[entryID] {
			continue
		}
		seen[entryID] = true
		if err := s.writeObservation(obs); err != nil {
			return err
		}
		keyDirs[obs.Key.id()] = true
	}
	for id := range keyDirs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.prune(id, localHistoryLimit); err != nil {
			return err
		}
	}
	return nil
}

func (s *diskHistoryStore) read(key historyKey) (*testHistory, error) {
	dir := s.keyDir(key.id())
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errHistoryNotFound
	}
	if err != nil {
		return nil, err
	}
	h := &testHistory{Version: historyVersion, Key: key}
	seen := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			// Another process can prune between ReadDir and ReadFile.
			continue
		}
		if err != nil {
			return nil, err
		}
		var obs historyObservation
		if err := json.Unmarshal(data, &obs); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", filepath.Join(dir, entry.Name()), err)
		}
		if obs.Key.id() != key.id() {
			return nil, fmt.Errorf("history observation %s has wrong key", obs.ID)
		}
		if !seen[obs.ID] {
			h.Observations = append(h.Observations, obs)
			seen[obs.ID] = true
		}
	}
	if len(h.Observations) == 0 {
		return nil, errHistoryNotFound
	}
	slices.SortFunc(h.Observations, compareHistoryObservations)
	return h, nil
}

func (s *diskHistoryStore) writeObservation(obs historyObservation) error {
	dir := s.keyDir(obs.Key.id())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(obs, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".history-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Observation IDs make retries idempotent. Rename is atomic, and writing
	// one file per observation means concurrent gotst processes never perform
	// a lossy read-modify-write of a shared history document.
	dst := s.observationPath(obs)
	if err := os.Rename(tmpName, dst); err != nil {
		// Rename does not replace an existing file on all supported platforms.
		// An existing destination is the same observation ID and therefore an
		// idempotent success.
		if _, statErr := os.Stat(dst); statErr == nil {
			return nil
		}
		return err
	}
	return nil
}

func (s *diskHistoryStore) prune(keyID string, limit int) error {
	dir := s.keyDir(keyID)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	type fileObservation struct {
		path string
		obs  historyObservation
	}
	var observations []fileObservation
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		var obs historyObservation
		if err := json.Unmarshal(data, &obs); err != nil {
			return err
		}
		observations = append(observations, fileObservation{path, obs})
	}
	slices.SortFunc(observations, func(a, b fileObservation) int {
		return compareHistoryObservations(a.obs, b.obs)
	})
	for _, old := range observations[min(limit, len(observations)):] {
		if err := os.Remove(old.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func compareHistoryObservations(a, b historyObservation) int {
	if c := b.ObservedAt.Compare(a.ObservedAt); c != 0 {
		return c
	}
	return strings.Compare(a.ID, b.ID)
}

func (s *diskHistoryStore) keyDir(id string) string {
	return filepath.Join(s.dir, id[:2], id)
}

func (s *diskHistoryStore) observationPath(obs historyObservation) string {
	// Hash the opaque observation ID rather than treating it as a filename.
	// This also gives a fixed-length path if a future producer uses UUIDs or a
	// server-assigned ID of a different form.
	h := sha256.Sum256([]byte(obs.ID))
	return filepath.Join(s.keyDir(obs.Key.id()), hex.EncodeToString(h[:])+".json")
}

type httpHistoryStore struct {
	base   *url.URL
	client *http.Client
	token  string
}

type historyLookupRequest struct {
	Version int          `json:"version"`
	Keys    []historyKey `json:"keys"`
	Limit   int          `json:"limit"`
}

type historyLookupResponse struct {
	Version   int            `json:"version"`
	Histories []*testHistory `json:"histories"`
}

type historyRecordRequest struct {
	Version      int                  `json:"version"`
	Observations []historyObservation `json:"observations"`
}

func newHTTPHistoryStore(rawURL string) (*httpHistoryStore, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("history URL scheme must be http or https")
	}
	if u.Host == "" {
		return nil, errors.New("history URL has no host")
	}
	return &httpHistoryStore{
		base: u, client: &http.Client{Timeout: historyRequestTimeout}, token: os.Getenv("GOTST_HISTORY_TOKEN"),
	}, nil
}

func (s *httpHistoryStore) Lookup(ctx context.Context, keys []historyKey, limit int) (map[string]*testHistory, error) {
	if limit <= 0 {
		limit = historyRequestLimit
	}
	var res historyLookupResponse
	err := s.doJSON(ctx, "/v1/history/lookup", historyLookupRequest{
		Version: historyVersion, Keys: keys, Limit: limit,
	}, &res)
	if err != nil {
		return nil, err
	}
	if res.Version != historyVersion {
		return nil, fmt.Errorf("history service returned version %d, want %d", res.Version, historyVersion)
	}
	ret := make(map[string]*testHistory)
	wanted := make(map[string]bool, len(keys))
	for _, key := range keys {
		wanted[key.id()] = true
	}
	for _, h := range res.Histories {
		if h == nil || h.Version != historyVersion || !wanted[h.Key.id()] {
			continue
		}
		for _, obs := range h.Observations {
			if obs.Key.id() != h.Key.id() {
				return nil, fmt.Errorf("history service returned observation %q under the wrong key", obs.ID)
			}
		}
		slices.SortFunc(h.Observations, compareHistoryObservations)
		if len(h.Observations) > limit {
			h.Observations = h.Observations[:limit]
		}
		ret[h.Key.id()] = h
	}
	return ret, nil
}

func (s *httpHistoryStore) Record(ctx context.Context, observations []historyObservation) error {
	return s.doJSON(ctx, "/v1/history/record", historyRecordRequest{
		Version: historyVersion, Observations: observations,
	}, nil)
}

func (s *httpHistoryStore) doJSON(ctx context.Context, endpoint string, body, dst any) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		return err
	}
	u := *s.base
	u.Path = strings.TrimRight(u.Path, "/") + endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	res, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
		return fmt.Errorf("history service %s: %s: %s", endpoint, res.Status, strings.TrimSpace(string(msg)))
	}
	if dst == nil || res.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(dst)
}

func (s *Server) loadHistory(tasks []testTask) {
	if s.history == nil || len(tasks) == 0 {
		return
	}
	s.mu.Lock()
	if s.historyLoaded {
		s.mu.Unlock()
		return
	}
	// Mark the one lookup attempted even on a soft backend error. History is
	// advisory, so the run proceeds rather than repeatedly delaying execution.
	s.historyLoaded = true
	s.mu.Unlock()
	keys := make([]historyKey, 0, len(tasks))
	seen := make(map[string]bool)
	for _, task := range tasks {
		key := historyKeyForTask(task, s.profile)
		if id := key.id(); !seen[id] {
			seen[id] = true
			keys = append(keys, key)
		}
	}
	histories, err := s.history.Lookup(s.ctx, keys, historyRequestLimit)
	if err != nil {
		if *verbose {
			log.Printf("loading test history: %v", err)
		}
		return
	}
	s.mu.Lock()
	for id, history := range histories {
		s.histories[id] = history
	}
	s.mu.Unlock()
	if *verbose {
		log.Printf("loaded history for %d/%d selected tests", len(histories), len(keys))
	}
}

func (s *Server) recordHistory(task testTask, outcome historyOutcome, duration time.Duration, attempts int, deps []cacheDependency) {
	if s.history == nil {
		return
	}
	key := historyKeyForTask(task, s.profile)
	obs := historyObservation{
		ID: newObservationID(), Key: key, ObservedAt: time.Now().UTC(),
		Outcome: outcome, Duration: duration, Attempts: attempts,
		Dependencies: historyDependencies(deps, task.bin.workDir, s.packageRoot(task.bin.pkg)),
	}
	s.mu.Lock()
	s.historyPending = append(s.historyPending, obs)
	s.mu.Unlock()
}

func (s *Server) flushHistory() {
	if s.history == nil {
		return
	}
	s.mu.Lock()
	pending := slices.Clone(s.historyPending)
	s.historyPending = nil
	s.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), historyRequestTimeout)
	defer cancel()
	if err := s.history.Record(ctx, pending); err != nil && *verbose {
		log.Printf("recording test history: %v", err)
	}
}
