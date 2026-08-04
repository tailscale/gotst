// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

// This file implements the persistent local GOCACHEPROG used when the user did
// not configure one. It stores all new artifacts in a gotst-owned cache and
// falls back read-only to the ordinary Go disk cache, preserving the value of a
// developer's already-warm GOCACHE while adding reusable linked executables.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	localCacheDirEnv   = "GOTST_LOCAL_BUILD_CACHE"
	stockGoCacheDirEnv = "GOTST_STOCK_GO_CACHE"
)

type localCacheIndex struct {
	Version   int    `json:"version"`
	OutputID  string `json:"output_id"`
	Size      int64  `json:"size"`
	TimeNanos int64  `json:"time_nanos"`
}

type localBuildCache struct {
	dir      string
	stockDir string
	putMu    sync.Mutex
}

func runLocalCacheProg() error {
	dir := os.Getenv(localCacheDirEnv)
	if dir == "" {
		return errors.New("missing local build cache directory")
	}
	c := &localBuildCache{dir: dir, stockDir: os.Getenv(stockGoCacheDirEnv)}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return c.serve(os.Stdin, os.Stdout)
}

func (c *localBuildCache) serve(r io.Reader, w io.Writer) error {
	dec := json.NewDecoder(bufio.NewReader(r))
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	if err := enc.Encode(&cacheProgResponse{KnownCommands: []cacheProgCommand{
		cacheProgGet, cacheProgPut, cacheProgClose,
	}}); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	var writeMu sync.Mutex
	var requests sync.WaitGroup
	writeResponse := func(res *cacheProgResponse) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = enc.Encode(res)
		_ = bw.Flush()
	}
	for {
		var req cacheProgRequest
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				requests.Wait()
				return nil
			}
			return err
		}
		if len(req.OutputID) == 0 && len(req.ObjectID) != 0 {
			req.OutputID = req.ObjectID
		}
		var body []byte
		if req.Command == cacheProgPut && req.BodySize > 0 {
			if err := dec.Decode(&body); err != nil {
				return err
			}
			if int64(len(body)) != req.BodySize {
				return fmt.Errorf("cache put body is %d bytes; want %d", len(body), req.BodySize)
			}
		}
		if req.Command == cacheProgClose {
			requests.Wait()
			writeResponse(&cacheProgResponse{ID: req.ID})
			return nil
		}
		requests.Add(1)
		go func(req cacheProgRequest, body []byte) {
			defer requests.Done()
			res := &cacheProgResponse{ID: req.ID}
			var err error
			switch req.Command {
			case cacheProgGet:
				res, err = c.get(req.ActionID)
				res.ID = req.ID
			case cacheProgPut:
				res.DiskPath, err = c.put(req.ActionID, req.OutputID, req.BodySize, body)
			default:
				err = fmt.Errorf("unsupported cache command %q", req.Command)
			}
			if err != nil {
				res.Err = err.Error()
			}
			writeResponse(res)
		}(req, body)
	}
}

func (c *localBuildCache) get(actionID []byte) (*cacheProgResponse, error) {
	actionHex := hex.EncodeToString(actionID)
	if !validCacheHex(actionHex) {
		return &cacheProgResponse{Miss: true}, nil
	}
	data, err := os.ReadFile(c.actionPath(actionHex))
	if err == nil {
		var idx localCacheIndex
		if json.Unmarshal(data, &idx) == nil && idx.Version == 1 && validCacheHex(idx.OutputID) {
			path := c.outputPath(idx.OutputID)
			if fi, statErr := os.Stat(path); statErr == nil && fi.Mode().IsRegular() && fi.Size() == idx.Size {
				outputID, decodeErr := hex.DecodeString(idx.OutputID)
				if decodeErr == nil {
					tm := time.Unix(0, idx.TimeNanos)
					return &cacheProgResponse{OutputID: outputID, Size: idx.Size, Time: &tm, DiskPath: path}, nil
				}
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if c.stockDir != "" {
		if res, ok := readStockGoCache(c.stockDir, actionHex); ok {
			return res, nil
		}
	}
	return &cacheProgResponse{Miss: true}, nil
}

func (c *localBuildCache) put(actionID, outputID []byte, size int64, body []byte) (string, error) {
	actionHex, outputHex := hex.EncodeToString(actionID), hex.EncodeToString(outputID)
	if !validCacheHex(actionHex) || !validCacheHex(outputHex) || len(outputID) != sha256.Size {
		return "", errors.New("invalid cache action or output ID")
	}
	if int64(len(body)) != size {
		return "", fmt.Errorf("cache body is %d bytes; want %d", len(body), size)
	}
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:], outputID) {
		return "", errors.New("cache body hash does not match output ID")
	}

	// Serialize local writes. Cross-process races are still safe because files
	// are content-addressed and both index and object installation use rename.
	c.putMu.Lock()
	defer c.putMu.Unlock()
	outputPath := c.outputPath(outputHex)
	if err := writeCacheFile(outputPath, body, 0600); err != nil {
		return "", err
	}
	outputInfo, err := os.Stat(outputPath)
	if err != nil {
		return "", err
	}
	idx, err := json.Marshal(localCacheIndex{
		Version: 1, OutputID: outputHex, Size: size, TimeNanos: outputInfo.ModTime().UnixNano(),
	})
	if err != nil {
		return "", err
	}
	if err := writeCacheFile(c.actionPath(actionHex), append(idx, '\n'), 0600); err != nil {
		return "", err
	}
	return outputPath, nil
}

func writeCacheFile(path string, data []byte, mode os.FileMode) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".gotst-cache-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		// Windows does not replace an existing destination. A concurrent
		// writer installing the same content is success.
		if old, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(old, data) {
			ok = true
			return nil
		}
		return err
	}
	ok = true
	return nil
}

func (c *localBuildCache) actionPath(id string) string {
	return filepath.Join(c.dir, "action", id[:2], id+".json")
}

func (c *localBuildCache) outputPath(id string) string {
	return filepath.Join(c.dir, "object", id[:2], id)
}

func validCacheHex(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// readStockGoCache understands the stable v1 action records used by Go's
// ordinary disk cache. It is deliberately read-only; all new entries go into
// gotst's own cache so we do not need cmd/go's internal locking machinery.
func readStockGoCache(dir, actionHex string) (*cacheProgResponse, bool) {
	if !validCacheHex(actionHex) || !filepath.IsAbs(dir) {
		return nil, false
	}
	actionPath := filepath.Join(dir, actionHex[:2], actionHex+"-a")
	data, err := os.ReadFile(actionPath)
	if err != nil {
		return nil, false
	}
	fields := strings.Fields(string(data))
	if len(fields) != 5 || fields[0] != "v1" || fields[1] != actionHex || !validCacheHex(fields[2]) {
		return nil, false
	}
	size, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil || size < 0 {
		return nil, false
	}
	nanos, err := strconv.ParseInt(fields[4], 10, 64)
	if err != nil || nanos < 0 {
		return nil, false
	}
	path := filepath.Join(dir, fields[2][:2], fields[2]+"-d")
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != size {
		return nil, false
	}
	outputID, err := hex.DecodeString(fields[2])
	if err != nil {
		return nil, false
	}
	tm := time.Unix(0, nanos)
	return &cacheProgResponse{OutputID: outputID, Size: size, Time: &tm, DiskPath: path}, true
}

func goBuildCacheDir(workDir string) (string, error) {
	cmd := exec.Command(goCmd(), "env", "GOCACHE")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(string(out))
	if dir == "off" {
		return "", nil
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("go env GOCACHE returned non-absolute path %q", dir)
	}
	return dir, nil
}
