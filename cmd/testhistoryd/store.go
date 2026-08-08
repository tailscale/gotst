// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tailscale/gotst/history"
)

const schemaVersion = 1

//go:embed schema.sql
var schemaSQL string

type postgresStore struct {
	pool    *pgxpool.Pool
	scopeID string
}

var _ history.Store = (*postgresStore)(nil)

func newPostgresStore(ctx context.Context, pool *pgxpool.Pool, scopeID string) (*postgresStore, error) {
	if pool == nil {
		return nil, errors.New("nil PostgreSQL pool")
	}
	if _, err := parseUUID(scopeID); err != nil {
		return nil, fmt.Errorf("invalid scope ID: %w", err)
	}
	var major int
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::integer / 10000`).Scan(&major); err != nil {
		return nil, fmt.Errorf("checking PostgreSQL version: %w", err)
	}
	if major != 17 {
		return nil, fmt.Errorf("PostgreSQL major version is %d; testhistoryd requires 17", major)
	}
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("applying history schema: %w", err)
	}
	var version int
	if err := pool.QueryRow(ctx, `SELECT version FROM test_history_schema WHERE singleton`).Scan(&version); err != nil {
		return nil, fmt.Errorf("reading history schema version: %w", err)
	}
	if version != schemaVersion {
		return nil, fmt.Errorf("database schema version is %d; testhistoryd supports %d", version, schemaVersion)
	}
	return &postgresStore{pool: pool, scopeID: scopeID}, nil
}

func (s *postgresStore) Lookup(ctx context.Context, keys []history.Key, opts history.LookupOptions) (map[string]*history.History, error) {
	limit := opts.RecentPerKey
	if limit <= 0 {
		limit = 32
	}
	if limit > 256 {
		limit = 256
	}
	ret := make(map[string]*history.History)
	if len(keys) == 0 {
		return ret, nil
	}

	keyByHash := make(map[string]history.Key, len(keys))
	hashes := make([][]byte, 0, len(keys))
	for _, key := range keys {
		hash, err := hex.DecodeString(key.ID())
		if err != nil {
			return nil, err
		}
		hexHash := hex.EncodeToString(hash)
		if _, ok := keyByHash[hexHash]; ok {
			continue
		}
		keyByHash[hexHash] = key
		hashes = append(hashes, hash)
	}

	rows, err := s.pool.Query(ctx, `
SELECT o.key_hash, o.observation_id, o.observed_at, o.outcome,
       o.duration_ns, o.attempts, o.peak_rss_bytes, o.dependencies
FROM unnest($1::bytea[]) WITH ORDINALITY AS requested(key_hash, ordinality)
CROSS JOIN LATERAL (
    SELECT key_hash, observation_id, observed_at, outcome, duration_ns,
           attempts, peak_rss_bytes, dependencies
    FROM test_history_observation
    WHERE scope_id = $2::uuid AND key_hash = requested.key_hash
    ORDER BY observed_at DESC, observation_id
    LIMIT $3
) AS o
ORDER BY requested.ordinality, o.observed_at DESC, o.observation_id`, hashes, s.scopeID, limit)
	if err != nil {
		return nil, fmt.Errorf("looking up history: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			hash, dependenciesJSON []byte
			obs                    history.Observation
			outcome                string
			peakRSS                *int64
		)
		if err := rows.Scan(&hash, &obs.ID, &obs.ObservedAt, &outcome, &obs.Duration,
			&obs.Attempts, &peakRSS, &dependenciesJSON); err != nil {
			return nil, fmt.Errorf("scanning history: %w", err)
		}
		key, ok := keyByHash[hex.EncodeToString(hash)]
		if !ok {
			return nil, errors.New("database returned an unrequested history key")
		}
		obs.Key = key
		obs.Outcome = history.Outcome(outcome)
		if peakRSS != nil {
			obs.PeakRSSBytes = *peakRSS
		}
		if dependenciesJSON != nil {
			if err := json.Unmarshal(dependenciesJSON, &obs.Dependencies); err != nil {
				return nil, fmt.Errorf("decoding dependencies for %q: %w", obs.ID, err)
			}
		}
		id := key.ID()
		h := ret[id]
		if h == nil {
			h = &history.History{Version: history.Version, Key: key}
			ret[id] = h
		}
		h.Observations = append(h.Observations, obs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading history: %w", err)
	}
	return ret, nil
}

func (s *postgresStore) Record(ctx context.Context, observations []history.Observation) error {
	if len(observations) == 0 {
		return nil
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("beginning history transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	for _, obs := range observations {
		if err := validateObservation(obs); err != nil {
			return err
		}
		keyHash, _ := hex.DecodeString(obs.Key.ID())
		tagsJSON, err := json.Marshal(obs.Key.Tags)
		if err != nil {
			return err
		}
		argsJSON, err := json.Marshal(obs.Key.Args)
		if err != nil {
			return err
		}
		dependenciesJSON, err := marshalDependencies(obs.Dependencies)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO test_history_key
    (scope_id, key_hash, package, test_name, goos, goarch, tags, args, last_observed_at)
VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (scope_id, key_hash) DO UPDATE
SET last_observed_at = greatest(test_history_key.last_observed_at, excluded.last_observed_at)`,
			s.scopeID, keyHash, obs.Key.Package, obs.Key.Test, obs.Key.GOOS, obs.Key.GOARCH,
			tagsJSON, argsJSON, obs.ObservedAt); err != nil {
			return fmt.Errorf("upserting history key: %w", err)
		}
		var existingHash []byte
		err = tx.QueryRow(ctx, `
INSERT INTO test_history_observation
    (scope_id, observation_id, key_hash, observed_at, outcome, duration_ns,
     attempts, peak_rss_bytes, dependencies)
VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, nullif($8, 0), $9)
ON CONFLICT (scope_id, observation_id) DO UPDATE
SET observation_id = excluded.observation_id
RETURNING key_hash`, s.scopeID, obs.ID, keyHash, obs.ObservedAt, string(obs.Outcome),
			obs.Duration.Nanoseconds(), obs.Attempts, obs.PeakRSSBytes, dependenciesJSON).Scan(&existingHash)
		if err != nil {
			return fmt.Errorf("inserting observation %q: %w", obs.ID, err)
		}
		if !slices.Equal(existingHash, keyHash) {
			return fmt.Errorf("observation ID %q is already associated with another key", obs.ID)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing history: %w", err)
	}
	return nil
}

func validateObservation(obs history.Observation) error {
	if obs.ID == "" || len(obs.ID) > 256 {
		return errors.New("observation ID must contain 1 to 256 bytes")
	}
	if obs.Key.Package == "" || obs.Key.Test == "" || obs.Key.GOOS == "" || obs.Key.GOARCH == "" {
		return errors.New("observation key fields must not be empty")
	}
	if obs.ObservedAt.IsZero() {
		return errors.New("observed_at must not be zero")
	}
	if obs.Duration < 0 || obs.Attempts < 1 || obs.PeakRSSBytes < 0 {
		return errors.New("duration and peak RSS must be non-negative and attempts must be positive")
	}
	switch obs.Outcome {
	case history.OutcomePass, history.OutcomeFail, history.OutcomeFlaky:
	default:
		return fmt.Errorf("invalid outcome %q", obs.Outcome)
	}
	return nil
}

func marshalDependencies(dependencies []history.Dependency) ([]byte, error) {
	if dependencies == nil {
		return nil, nil
	}
	return json.Marshal(dependencies)
}

func parseUUID(v string) ([16]byte, error) {
	var ret [16]byte
	if len(v) != 36 || v[8] != '-' || v[13] != '-' || v[18] != '-' || v[23] != '-' {
		return ret, errors.New("want UUID")
	}
	hexUUID := strings.ReplaceAll(v, "-", "")
	b, err := hex.DecodeString(hexUUID)
	if err != nil {
		return ret, errors.New("want UUID")
	}
	copy(ret[:], b)
	return ret, nil
}
