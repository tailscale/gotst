// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
)

// progTestCache is a testResultCache that layers the local disk cache over
// the configured GOCACHEPROG, so machines sharing a GOCACHEPROG backend
// (such as CI or -remote VMs using one cache server) share test results.
//
// Entries are stored as ordinary GOCACHEPROG objects: the action ID is
// derived from the test cache key, and the output is the entry's JSON.
// The machine using an entry still validates its recorded dependencies
// against its own environment and files, so sharing affects only where
// an entry is found, not when it is trusted.
type progTestCache struct {
	local   testResultCache
	command string // GOCACHEPROG

	once   sync.Once
	client *cacheProgClient // nil if it failed to start
}

func newProgTestCache(local testResultCache, command string) *progTestCache {
	return &progTestCache{local: local, command: command}
}

// prog returns the GOCACHEPROG client, starting it on first use, or nil
// if it is unavailable, in which case only the local cache is used.
func (c *progTestCache) prog() *cacheProgClient {
	c.once.Do(func() {
		client, err := startCacheProgClient(c.command)
		if err != nil {
			log.Printf("test result cache: not sharing results via GOCACHEPROG: %v", err)
			return
		}
		if !client.can[cacheProgGet] || !client.can[cacheProgPut] {
			log.Printf("test result cache: not sharing results; GOCACHEPROG lacks get or put")
			client.close()
			return
		}
		c.client = client
	})
	return c.client
}

// progTestCacheActionID returns the GOCACHEPROG action ID for key's entry.
func progTestCacheActionID(key testCacheKey) []byte {
	sum := sha256.Sum256([]byte("gotst test result\n" + key.id()))
	return sum[:]
}

func (c *progTestCache) Get(ctx context.Context, key testCacheKey) (*testCacheEntry, error) {
	entry, err := c.local.Get(ctx, key)
	if !errors.Is(err, errTestCacheMiss) {
		return entry, err
	}
	prog := c.prog()
	if prog == nil {
		return nil, errTestCacheMiss
	}
	res, err := prog.send(ctx, cacheProgRequest{Command: cacheProgGet, ActionID: progTestCacheActionID(key)})
	if err != nil {
		return nil, fmt.Errorf("GOCACHEPROG get: %w", err)
	}
	if res.Miss || res.DiskPath == "" {
		return nil, errTestCacheMiss
	}
	data, err := os.ReadFile(res.DiskPath)
	if err != nil {
		return nil, fmt.Errorf("reading GOCACHEPROG test result: %w", err)
	}
	if sum := sha256.Sum256(data); len(res.OutputID) != 0 && !bytes.Equal(sum[:], res.OutputID) {
		return nil, errTestCacheMiss
	}
	entry = new(testCacheEntry)
	if err := json.Unmarshal(data, entry); err != nil || entry.Version != testCacheVersion || entry.Key.id() != key.id() {
		return nil, errTestCacheMiss
	}
	// Keep a local copy for later runs here.
	_ = c.local.Put(ctx, entry)
	return entry, nil
}

func (c *progTestCache) Put(ctx context.Context, entry *testCacheEntry) error {
	if err := c.local.Put(ctx, entry); err != nil {
		return err
	}
	prog := c.prog()
	if prog == nil {
		return nil
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	outputID := sha256.Sum256(data)
	if _, err := prog.send(ctx, cacheProgRequest{
		Command: cacheProgPut, ActionID: progTestCacheActionID(entry.Key), OutputID: outputID[:],
		BodySize: int64(len(data)), body: bytes.NewReader(data),
	}); err != nil {
		return fmt.Errorf("GOCACHEPROG put: %w", err)
	}
	return nil
}

// Close stops the GOCACHEPROG client, if it was started.
func (c *progTestCache) Close() error {
	var client *cacheProgClient
	c.once.Do(func() {}) // don't start it now
	client = c.client
	if client == nil {
		return nil
	}
	return client.close()
}
