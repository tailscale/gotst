// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/tailscale/gotst/history"
)

type memoryStore struct {
	lookupKeys []history.Key
	lookupOpts history.LookupOptions
	histories  map[string]*history.History
	recorded   []history.Observation
	err        error
}

func (s *memoryStore) Lookup(_ context.Context, keys []history.Key, opts history.LookupOptions) (map[string]*history.History, error) {
	s.lookupKeys, s.lookupOpts = keys, opts
	return s.histories, s.err
}

func (s *memoryStore) Record(_ context.Context, observations []history.Observation) error {
	s.recorded = observations
	return s.err
}

func TestAPIServer(t *testing.T) {
	key := history.Key{Package: "example.com/p", Test: "TestOne", GOOS: "linux", GOARCH: "amd64"}
	obs := validObservation(key, "one")
	store := &memoryStore{histories: map[string]*history.History{
		key.ID(): {Version: history.Version, Key: key, Observations: []history.Observation{obs}},
	}}
	server := httptest.NewServer((&apiServer{store: store, token: "secret"}).handler())
	defer server.Close()

	t.Run("health", func(t *testing.T) {
		res, err := http.Get(server.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %s", res.Status)
		}
	})

	t.Run("authentication", func(t *testing.T) {
		res := postJSON(t, server.URL+history.LookupPath, history.LookupRequest{Version: history.Version}, "")
		defer res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %s; want 401", res.Status)
		}
	})

	t.Run("lookup", func(t *testing.T) {
		res := postJSON(t, server.URL+history.LookupPath, history.LookupRequest{
			Version: history.Version, Keys: []history.Key{key}, RecentPerKey: 7,
		}, "secret")
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %s", res.Status)
		}
		var got history.LookupResponse
		if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.Version != history.Version || len(got.Histories) != 1 || !reflect.DeepEqual(store.lookupKeys, []history.Key{key}) || store.lookupOpts.RecentPerKey != 7 {
			t.Fatalf("response=%+v keys=%+v options=%+v", got, store.lookupKeys, store.lookupOpts)
		}
	})

	t.Run("record", func(t *testing.T) {
		res := postJSON(t, server.URL+history.RecordPath, history.RecordRequest{
			Version: history.Version, Observations: []history.Observation{obs},
		}, "secret")
		defer res.Body.Close()
		if res.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %s", res.Status)
		}
		if !reflect.DeepEqual(store.recorded, []history.Observation{obs}) {
			t.Fatalf("recorded = %#v", store.recorded)
		}
	})
}

func TestAPIServerRejectsInvalidRequests(t *testing.T) {
	store := new(memoryStore)
	handler := (&apiServer{store: store}).handler()
	for _, tt := range []struct {
		name string
		path string
		body string
	}{
		{"malformed", history.LookupPath, `{"version":`},
		{"unknown field", history.LookupPath, `{"version":1,"mystery":true}`},
		{"wrong version", history.LookupPath, `{"version":99}`},
		{"negative limit", history.LookupPath, `{"version":1,"limit":-1}`},
		{"bad observation", history.RecordPath, `{"version":1,"observations":[{}]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.path, bytes.NewBufferString(tt.body))
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body %s", res.Code, res.Body.String())
			}
		})
	}
}

func TestAPIServerStoreError(t *testing.T) {
	handler := (&apiServer{store: &memoryStore{err: errors.New("database details")}}).handler()
	req := httptest.NewRequest(http.MethodPost, history.LookupPath, bytes.NewBufferString(`{"version":1}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusInternalServerError || bytes.Contains(res.Body.Bytes(), []byte("database details")) {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
}

func postJSON(t *testing.T, url string, body any, token string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func validObservation(key history.Key, id string) history.Observation {
	return history.Observation{
		ID: id, Key: key, ObservedAt: time.Unix(123, 0).UTC(), Outcome: history.OutcomePass,
		Duration: 15 * time.Millisecond, Attempts: 1,
		Dependencies: []history.Dependency{{Operation: "getenv", Name: "GODEBUG"}},
	}
}
