// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tailscale/gotst/history"
)

func TestValidateObservation(t *testing.T) {
	key := history.Key{Package: "example.com/p", Test: "TestOne", GOOS: "linux", GOARCH: "amd64"}
	if err := validateObservation(validObservation(key, "one")); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*history.Observation){
		func(o *history.Observation) { o.ID = "" },
		func(o *history.Observation) { o.Key.Package = "" },
		func(o *history.Observation) { o.ObservedAt = time.Time{} },
		func(o *history.Observation) { o.Outcome = "unknown" },
		func(o *history.Observation) { o.Duration = -1 },
		func(o *history.Observation) { o.Attempts = 0 },
	} {
		obs := validObservation(key, "one")
		mutate(&obs)
		if err := validateObservation(obs); err == nil {
			t.Fatalf("validateObservation(%+v) unexpectedly succeeded", obs)
		}
	}
}

func TestParseUUID(t *testing.T) {
	for _, good := range []string{
		"00000000-0000-0000-0000-000000000000",
		"7D29976E-ABB4-4C2A-A614-A2F01E6D85C1",
	} {
		if _, err := parseUUID(good); err != nil {
			t.Errorf("parseUUID(%q): %v", good, err)
		}
	}
	for _, bad := range []string{
		"", "7d29976eabb44c2aa614a2f01e6d85c1",
		"7d29976e-abb4-4c2a-a614-a2f01e6d85cz",
		"7d29976eabb4-4c2a-a614-a2f01e6d85c1-",
	} {
		if _, err := parseUUID(bad); err == nil {
			t.Errorf("parseUUID(%q) unexpectedly succeeded", bad)
		}
	}
}

func TestPostgresStore(t *testing.T) {
	dsn := os.Getenv("TESTHISTORYD_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TESTHISTORYD_TEST_POSTGRES_DSN to run the PostgreSQL 17 integration test")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "testhistoryd_" + hex.EncodeToString(random[:])
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("dropping test schema: %v", err)
		}
	})
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	const scope = "7d29976e-abb4-4c2a-a614-a2f01e6d85c1"
	store, err := newPostgresStore(ctx, pool, scope)
	if err != nil {
		t.Fatal(err)
	}

	key := history.Key{Package: "example.com/p", Test: "TestOne", GOOS: "linux", GOARCH: "amd64"}
	older := validObservation(key, "older")
	newer := validObservation(key, "newer")
	newer.ObservedAt = older.ObservedAt.Add(time.Hour)
	newer.Outcome = history.OutcomeFlaky
	newer.Attempts = 2
	newer.PeakRSSBytes = 12345
	newer.Dependencies = nil
	if err := store.Record(ctx, []history.Observation{older, newer, newer}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Lookup(ctx, []history.Key{key}, history.LookupOptions{RecentPerKey: 1})
	if err != nil {
		t.Fatal(err)
	}
	h := got[key.ID()]
	if h == nil || len(h.Observations) != 1 {
		t.Fatalf("Lookup = %+v", got)
	}
	if got := h.Observations[0]; got.ID != newer.ID || got.Outcome != newer.Outcome || got.PeakRSSBytes != newer.PeakRSSBytes || got.Dependencies != nil {
		t.Fatalf("newest observation = %+v; want %+v", got, newer)
	}

	other := validObservation(history.Key{Package: "example.com/p", Test: "TestOther", GOOS: "linux", GOARCH: "amd64"}, newer.ID)
	if err := store.Record(ctx, []history.Observation{other}); err == nil {
		t.Fatal("reusing an observation ID for another key unexpectedly succeeded")
	}

	got, err = store.Lookup(ctx, []history.Key{key}, history.LookupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if observations := got[key.ID()].Observations; len(observations) != 2 || !reflect.DeepEqual(observations[1].Dependencies, older.Dependencies) {
		t.Fatalf("all observations = %+v", observations)
	}
}
