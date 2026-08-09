// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type cacheProgTestRecord struct {
	Command  cacheProgCommand
	ActionID []byte `json:",omitempty"`
	OutputID []byte `json:",omitempty"`
	BodyHash []byte `json:",omitempty"`
	BodySize int64  `json:",omitempty"`
}

// TestCacheProgHelper is run in a subprocess by TestCacheShimLifecycle. Its
// stdout is the cache-helper protocol, so failures are reported by exiting
// without writing non-protocol diagnostics there.
func TestCacheProgHelper(t *testing.T) {
	logPath := os.Getenv("GOTST_TEST_CACHE_HELPER_LOG")
	if logPath == "" {
		return
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(2)
	}
	defer logFile.Close()
	logEnc := json.NewEncoder(logFile)
	dec := json.NewDecoder(bufio.NewReader(os.Stdin))
	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(&cacheProgResponse{KnownCommands: []cacheProgCommand{cacheProgGet, cacheProgPut, cacheProgClose}}); err != nil {
		os.Exit(2)
	}
	for {
		var req cacheProgRequest
		if err := dec.Decode(&req); err != nil {
			if err == io.EOF {
				return
			}
			os.Exit(2)
		}
		record := cacheProgTestRecord{
			Command: req.Command, ActionID: req.ActionID,
			OutputID: req.OutputID, BodySize: req.BodySize,
		}
		if req.BodySize > 0 {
			var body []byte
			if err := dec.Decode(&body); err != nil || int64(len(body)) != req.BodySize {
				os.Exit(2)
			}
			h := sha256.Sum256(body)
			record.BodyHash = h[:]
		}
		if err := logEnc.Encode(&record); err != nil {
			os.Exit(2)
		}
		res := &cacheProgResponse{ID: req.ID}
		switch req.Command {
		case cacheProgGet:
			if len(req.ActionID) > 0 && req.ActionID[len(req.ActionID)-1] == 0x7f {
				hash, size, err := hashFile(os.Args[0])
				if err != nil {
					os.Exit(2)
				}
				res.OutputID, res.Size, res.DiskPath = hash, size, os.Args[0]
			} else {
				res.Miss = true
			}
		case cacheProgPut:
			res.DiskPath = os.Args[0]
		case cacheProgClose:
			if err := enc.Encode(res); err != nil {
				os.Exit(2)
			}
			return
		default:
			os.Exit(2)
		}
		if err := enc.Encode(res); err != nil {
			os.Exit(2)
		}
	}
}

func TestCacheShimLifecycle(t *testing.T) {
	testExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	buildID, err := readGoBuildID(testExe)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(buildID, "/")
	if len(parts) != 4 {
		t.Fatalf("test executable build ID %q has %d parts; want 4", buildID, len(parts))
	}
	prefix, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(prefix) != 15 {
		t.Fatalf("decoding action ID prefix %q: %v (length %d)", parts[0], err, len(prefix))
	}
	actionID := make([]byte, sha256.Size)
	copy(actionID, prefix)
	actionID[len(actionID)-1] = 1
	hitActionID := bytes.Clone(actionID)
	hitActionID[len(hitActionID)-1] = 0x7f

	temp := t.TempDir()
	logPath := filepath.Join(temp, "helper.log")
	if err := os.WriteFile(logPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOTST_TEST_CACHE_HELPER_LOG", logPath)
	socket := filepath.Join(temp, "cache.sock")
	command := fmt.Sprintf("%s -test.run=^TestCacheProgHelper$", quoteCacheProgArg(testExe))
	shim, err := startCacheShim(command, socket)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = shim.close()
		}
	}()

	conn, err := netDialUnix(socket, cacheConnCmdGo)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	var hello cacheProgResponse
	if err := dec.Decode(&hello); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(&cacheProgRequest{ID: 1, Command: cacheProgGet, ActionID: actionID}); err != nil {
		t.Fatal(err)
	}
	var miss cacheProgResponse
	if err := dec.Decode(&miss); err != nil || !miss.Miss {
		t.Fatalf("miss response = %+v, %v", miss, err)
	}
	if err := enc.Encode(&cacheProgRequest{ID: 2, Command: cacheProgGet, ActionID: hitActionID}); err != nil {
		t.Fatal(err)
	}
	var hit cacheProgResponse
	if err := dec.Decode(&hit); err != nil {
		t.Fatal(err)
	}
	if hit.Miss || hit.DiskPath == "" || hit.DiskPath == testExe {
		t.Fatalf("cache hit was not materialized at a private path: %+v", hit)
	}
	if fi, err := os.Stat(hit.DiskPath); err != nil || fi.Mode()&0100 == 0 {
		t.Fatalf("materialized executable mode: %v, %v", fi, err)
	}
	wantHash, wantSize, err := hashFile(testExe)
	if err != nil {
		t.Fatal(err)
	}
	gotHash, gotSize, ok := lookupVerifiedTestExecutable(socket, hit.DiskPath)
	if !ok || gotHash != hex.EncodeToString(wantHash) || gotSize != wantSize {
		t.Fatalf("verified executable = %q, %d, %v; want %x, %d, true", gotHash, gotSize, ok, wantHash, wantSize)
	}
	if _, _, ok := lookupVerifiedTestExecutable(socket, filepath.Join(temp, "unknown")); ok {
		t.Fatal("unknown executable reported as verified")
	}
	if err := enc.Encode(&cacheProgRequest{ID: 3, Command: cacheProgClose}); err != nil {
		t.Fatal(err)
	}
	var closeRes cacheProgResponse
	if err := dec.Decode(&closeRes); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()

	// cmd/go has closed its frontend connection, but a late -exec wrapper can
	// still upload the linked executable through the parent-owned broker.
	outputID, size := wantHash, wantSize
	if err := registerTestExecutable(socket, testExe, hex.EncodeToString(outputID), size); err != nil {
		t.Fatalf("late executable registration: %v", err)
	}
	if err := shim.close(); err != nil {
		t.Fatal(err)
	}
	closed = true

	records, err := readCacheProgTestRecords(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(records), 4; got != want {
		t.Fatalf("helper saw %d requests, want %d: %+v", got, want, records)
	}
	put := records[2]
	if put.Command != cacheProgPut || !bytes.Equal(put.ActionID, actionID) ||
		!bytes.Equal(put.OutputID, outputID) || !bytes.Equal(put.BodyHash, outputID) || put.BodySize != size {
		t.Fatalf("synthetic put = %+v", put)
	}
	if records[3].Command != cacheProgClose {
		t.Fatalf("last helper request = %q, want close", records[3].Command)
	}
}

func netDialUnix(socket string, kind byte) (io.ReadWriteCloser, error) {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write([]byte{kind}); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func readCacheProgTestRecords(path string) ([]cacheProgTestRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var records []cacheProgTestRecord
	dec := json.NewDecoder(f)
	for {
		var record cacheProgTestRecord
		if err := dec.Decode(&record); err != nil {
			if err == io.EOF {
				return records, nil
			}
			return nil, err
		}
		records = append(records, record)
	}
}

func TestCacheProgClientRejectsUnsupportedCommand(t *testing.T) {
	c := &cacheProgClient{can: map[cacheProgCommand]bool{cacheProgGet: true}}
	if _, err := c.send(context.Background(), cacheProgRequest{Command: cacheProgPut}); err == nil {
		t.Fatal("put unexpectedly succeeded")
	}
}

func TestCacheShimWriteOnlyDownstreamRecordsGetMiss(t *testing.T) {
	s := &cacheShim{
		downstream: &cacheProgClient{can: map[cacheProgCommand]bool{cacheProgPut: true}},
		misses:     make(map[string][]byte),
	}
	actionID := bytes.Repeat([]byte{1}, sha256.Size)
	res, err := s.forward(context.Background(), cacheProgRequest{
		Command: cacheProgGet, ActionID: actionID,
	})
	if err != nil || !res.Miss {
		t.Fatalf("forward get = %+v, %v; want miss", res, err)
	}
	if got := s.misses[actionIDPrefix(actionID)]; !bytes.Equal(got, actionID) {
		t.Fatalf("recorded action ID = %x; want %x", got, actionID)
	}
}

func TestFindExecSnarf(t *testing.T) {
	want := ExecSnarf{WorkingDir: "/tmp/work", ExeHash: "abcd", Args: []string{"-test.v"}}
	j, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	output := "diagnostic mentioning ExecSnarf: but not as a record\nExecSnarf:" + string(j) + "\nok\n"
	got, found, err := findExecSnarf(output)
	if err != nil || !found {
		t.Fatalf("findExecSnarf: found=%v, err=%v", found, err)
	}
	if got.WorkingDir != want.WorkingDir || got.ExeHash != want.ExeHash || !slices.Equal(got.Args, want.Args) {
		t.Fatalf("findExecSnarf = %+v; want %+v", got, want)
	}
	if _, found, err := findExecSnarf("no record\n"); found || err != nil {
		t.Fatalf("missing record: found=%v, err=%v", found, err)
	}
}
