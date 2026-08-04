// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalBuildCachePutGet(t *testing.T) {
	c := &localBuildCache{dir: t.TempDir()}
	actionID := sha256.Sum256([]byte("action"))
	body := []byte("cache object")
	outputID := sha256.Sum256(body)
	path, err := c.put(actionID[:], outputID[:], int64(len(body)), body)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.get(actionID[:])
	if err != nil {
		t.Fatal(err)
	}
	if res.Miss || !bytes.Equal(res.OutputID, outputID[:]) || res.Size != int64(len(body)) || res.DiskPath != path {
		t.Fatalf("get = %+v", res)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("object = %q, %v", got, err)
	}
	missID := sha256.Sum256([]byte("missing"))
	if miss, err := c.get(missID[:]); err != nil || !miss.Miss {
		t.Fatalf("missing get = %+v, %v", miss, err)
	}
}

func TestLocalBuildCacheRejectsWrongHash(t *testing.T) {
	c := &localBuildCache{dir: t.TempDir()}
	actionID := sha256.Sum256([]byte("action"))
	wrong := sha256.Sum256([]byte("other"))
	if _, err := c.put(actionID[:], wrong[:], 4, []byte("body")); err == nil {
		t.Fatal("put with wrong output hash succeeded")
	}
}

func TestReadStockGoCache(t *testing.T) {
	dir := t.TempDir()
	actionID := sha256.Sum256([]byte("action"))
	body := []byte("stock object")
	outputID := sha256.Sum256(body)
	actionHex, outputHex := hex.EncodeToString(actionID[:]), hex.EncodeToString(outputID[:])
	objectPath := filepath.Join(dir, outputHex[:2], outputHex+"-d")
	if err := os.MkdirAll(filepath.Dir(objectPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objectPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	actionPath := filepath.Join(dir, actionHex[:2], actionHex+"-a")
	if err := os.MkdirAll(filepath.Dir(actionPath), 0700); err != nil {
		t.Fatal(err)
	}
	record := fmt.Sprintf("v1 %s %s %20d %20d\n", actionHex, outputHex, len(body), time.Now().UnixNano())
	if err := os.WriteFile(actionPath, []byte(record), 0600); err != nil {
		t.Fatal(err)
	}
	res, ok := readStockGoCache(dir, actionHex)
	if !ok || res.DiskPath != objectPath || res.Size != int64(len(body)) || !bytes.Equal(res.OutputID, outputID[:]) {
		t.Fatalf("readStockGoCache = %+v, %v", res, ok)
	}
}
