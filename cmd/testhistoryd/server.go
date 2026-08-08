// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tailscale/gotst/history"
)

const maxRequestBytes = 16 << 20

type apiServer struct {
	store  history.Store
	token  string
	logger *slog.Logger
}

func (s *apiServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("POST "+history.LookupPath, s.serveLookup)
	mux.HandleFunc("POST "+history.RecordPath, s.serveRecord)
	return s.authenticate(mux)
}

func (s *apiServer) authenticate(next http.Handler) http.Handler {
	if s.token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *apiServer) serveLookup(w http.ResponseWriter, r *http.Request) {
	var req history.LookupRequest
	if err := decodeRequest(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	if req.Version != history.Version {
		writeAPIError(w, http.StatusBadRequest, fmt.Errorf("unsupported protocol version %d", req.Version))
		return
	}
	if len(req.Keys) > 10_000 {
		writeAPIError(w, http.StatusBadRequest, errors.New("too many lookup keys; maximum is 10000"))
		return
	}
	if req.RecentPerKey < 0 || req.RecentPerKey > 256 {
		writeAPIError(w, http.StatusBadRequest, errors.New("limit must be between 0 and 256"))
		return
	}
	histories, err := s.store.Lookup(r.Context(), req.Keys, history.LookupOptions{RecentPerKey: req.RecentPerKey})
	if err != nil {
		s.internalError(w, "lookup", err)
		return
	}
	response := history.LookupResponse{Version: history.Version}
	for _, key := range req.Keys {
		if h := histories[key.ID()]; h != nil {
			response.Histories = append(response.Histories, h)
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *apiServer) serveRecord(w http.ResponseWriter, r *http.Request) {
	var req history.RecordRequest
	if err := decodeRequest(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	if req.Version != history.Version {
		writeAPIError(w, http.StatusBadRequest, fmt.Errorf("unsupported protocol version %d", req.Version))
		return
	}
	if len(req.Observations) > 10_000 {
		writeAPIError(w, http.StatusBadRequest, errors.New("too many observations; maximum is 10000"))
		return
	}
	for i, obs := range req.Observations {
		if err := validateObservation(obs); err != nil {
			writeAPIError(w, http.StatusBadRequest, fmt.Errorf("observation %d: %w", i, err))
			return
		}
	}
	if err := s.store.Record(r.Context(), req.Observations); err != nil {
		s.internalError(w, "record", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeRequest(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON request: %w", err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request contains multiple JSON values")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func writeAPIError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (s *apiServer) internalError(w http.ResponseWriter, operation string, err error) {
	if s.logger != nil {
		s.logger.Error("history request failed", "operation", operation, "error", err)
	}
	writeAPIError(w, http.StatusInternalServerError, errors.New("internal server error"))
}
