// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tailscale/gotst/history"
)

const (
	localHistoryLimit     = 32
	historyRequestLimit   = 32
	historyRequestTimeout = 5 * time.Second
)

var errHistoryNotFound = errors.New("test history not found")

func newObservationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	// crypto/rand failure is extraordinarily unlikely. The ID is only for
	// deduplicating retries at a remote history service, not security.
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
}

func historyKeyForTask(task testTask, profile runProfile) history.Key {
	args := effectiveTestArgs(task, profile)
	tags := slices.Clone(profile.Tags)
	sort.Strings(tags)
	return history.Key{
		Package: task.bin.pkg,
		Test:    task.test,
		GOOS:    runtime.GOOS,
		GOARCH:  runtime.GOARCH,
		Tags:    tags,
		Args:    args,
	}
}

func historyDependencies(deps []cacheDependency, packageDir, moduleRoot string) []history.Dependency {
	if deps == nil {
		return nil
	}
	ret := make([]history.Dependency, 0, len(deps))
	for _, dep := range deps {
		hd := history.Dependency{Operation: dep.Operation}
		if dep.Operation == "getenv" {
			hd.Name = dep.Name
		}
		if dep.Path != "" {
			hd.Path = portableHistoryPath(dep.Path, packageDir, moduleRoot)
		}
		ret = append(ret, hd)
	}
	slices.SortFunc(ret, func(a, b history.Dependency) int {
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

var _ history.Store = (*diskHistoryStore)(nil)

func newDiskHistoryStore(cacheRoot string) (*diskHistoryStore, error) {
	dir := filepath.Join(cacheRoot, "history", fmt.Sprintf("v%d", history.Version))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &diskHistoryStore{dir: dir}, nil
}

func (s *diskHistoryStore) Lookup(ctx context.Context, keys []history.Key, opts history.LookupOptions) (map[string]*history.History, error) {
	limit := opts.RecentPerKey
	if limit <= 0 {
		limit = historyRequestLimit
	}
	ret := make(map[string]*history.History)
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
		ret[key.ID()] = h
	}
	return ret, nil
}

func (s *diskHistoryStore) Record(ctx context.Context, observations []history.Observation) error {
	keyDirs := make(map[string]bool)
	seen := make(map[string]bool)
	for _, obs := range observations {
		if err := ctx.Err(); err != nil {
			return err
		}
		entryID := obs.Key.ID() + "\x00" + obs.ID
		if seen[entryID] {
			continue
		}
		seen[entryID] = true
		if err := s.writeObservation(obs); err != nil {
			return err
		}
		keyDirs[obs.Key.ID()] = true
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

func (s *diskHistoryStore) read(key history.Key) (*history.History, error) {
	dir := s.keyDir(key.ID())
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errHistoryNotFound
	}
	if err != nil {
		return nil, err
	}
	h := &history.History{Version: history.Version, Key: key}
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
		var obs history.Observation
		if err := json.Unmarshal(data, &obs); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", filepath.Join(dir, entry.Name()), err)
		}
		if obs.Key.ID() != key.ID() {
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

func (s *diskHistoryStore) writeObservation(obs history.Observation) error {
	dir := s.keyDir(obs.Key.ID())
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
		obs  history.Observation
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
		var obs history.Observation
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

func compareHistoryObservations(a, b history.Observation) int {
	if c := b.ObservedAt.Compare(a.ObservedAt); c != 0 {
		return c
	}
	return strings.Compare(a.ID, b.ID)
}

func (s *diskHistoryStore) keyDir(id string) string {
	return filepath.Join(s.dir, id[:2], id)
}

func (s *diskHistoryStore) observationPath(obs history.Observation) string {
	// Hash the opaque observation ID rather than treating it as a filename.
	// This also gives a fixed-length path if a future producer uses UUIDs or a
	// server-assigned ID of a different form.
	h := sha256.Sum256([]byte(obs.ID))
	return filepath.Join(s.keyDir(obs.Key.ID()), hex.EncodeToString(h[:])+".json")
}

type httpHistoryStore struct {
	base   *url.URL
	client *http.Client
	token  string
}

var _ history.Store = (*httpHistoryStore)(nil)

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

func (s *httpHistoryStore) Lookup(ctx context.Context, keys []history.Key, opts history.LookupOptions) (map[string]*history.History, error) {
	limit := opts.RecentPerKey
	if limit <= 0 {
		limit = historyRequestLimit
	}
	var res history.LookupResponse
	err := s.doJSON(ctx, history.LookupPath, history.LookupRequest{
		Version: history.Version, Keys: keys, RecentPerKey: limit,
	}, &res)
	if err != nil {
		return nil, err
	}
	if res.Version != history.Version {
		return nil, fmt.Errorf("history service returned version %d, want %d", res.Version, history.Version)
	}
	ret := make(map[string]*history.History)
	wanted := make(map[string]bool, len(keys))
	for _, key := range keys {
		wanted[key.ID()] = true
	}
	for _, h := range res.Histories {
		if h == nil || h.Version != history.Version || !wanted[h.Key.ID()] {
			continue
		}
		for _, obs := range h.Observations {
			if obs.Key.ID() != h.Key.ID() {
				return nil, fmt.Errorf("history service returned observation %q under the wrong key", obs.ID)
			}
		}
		slices.SortFunc(h.Observations, compareHistoryObservations)
		if len(h.Observations) > limit {
			h.Observations = h.Observations[:limit]
		}
		ret[h.Key.ID()] = h
	}
	return ret, nil
}

func (s *httpHistoryStore) Record(ctx context.Context, observations []history.Observation) error {
	return s.doJSON(ctx, history.RecordPath, history.RecordRequest{
		Version: history.Version, Observations: observations,
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
	keys := make([]history.Key, 0, len(tasks))
	seen := make(map[string]bool)
	for _, task := range tasks {
		key := historyKeyForTask(task, s.profile)
		if id := key.ID(); !seen[id] {
			seen[id] = true
			keys = append(keys, key)
		}
	}
	histories, err := s.history.Lookup(s.ctx, keys, history.LookupOptions{RecentPerKey: historyRequestLimit})
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

// orderTestTasksByHistory applies longest-processing-time-first scheduling,
// with tests whose newest observation failed placed ahead of all other tests.
// Starting likely failures early gives useful feedback sooner; starting long
// tests before short ones reduces the chance that one straggler extends the
// end of an otherwise idle run.
func (s *Server) orderTestTasksByHistory(tasks []testTask) {
	s.mu.Lock()
	histories := maps.Clone(s.histories)
	s.mu.Unlock()
	type priority struct {
		failed   bool
		known    bool
		duration time.Duration
	}
	priorities := make(map[string]priority, len(tasks))
	for _, task := range tasks {
		key := historyKeyForTask(task, s.profile)
		h := histories[key.ID()]
		if h == nil || len(h.Observations) == 0 {
			continue
		}
		latest := h.Observations[0]
		for _, obs := range h.Observations[1:] {
			if compareHistoryObservations(obs, latest) < 0 {
				latest = obs
			}
		}
		priorities[task.bin.pkg+"\x00"+task.test] = priority{
			failed:   latest.Outcome == history.OutcomeFail,
			known:    true,
			duration: latest.Duration,
		}
	}
	slices.SortFunc(tasks, func(a, b testTask) int {
		ap := priorities[a.bin.pkg+"\x00"+a.test]
		bp := priorities[b.bin.pkg+"\x00"+b.test]
		if ap.failed != bp.failed {
			if ap.failed {
				return -1
			}
			return 1
		}
		if ap.duration != bp.duration {
			return cmp.Compare(bp.duration, ap.duration)
		}
		if ap.known != bp.known {
			if ap.known {
				return -1
			}
			return 1
		}
		if c := strings.Compare(a.bin.pkg, b.bin.pkg); c != 0 {
			return c
		}
		return strings.Compare(a.test, b.test)
	})
}

func (s *Server) recordHistory(task testTask, outcome history.Outcome, duration time.Duration, attempts int, deps []cacheDependency) {
	if s.history == nil {
		return
	}
	key := historyKeyForTask(task, s.profile)
	obs := history.Observation{
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
