// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Link skipping: like cmd/go's own test cache, which reports a test binary's
// link as up to date when the test's result is cached, gotst avoids
// obtaining test binaries whose tests may all be cached.
//
// After capturing and listing a binary built or restored for link action A,
// gotst stores a small "link record" in GOCACHEPROG under an action ID
// derived from A, giving the binary's SHA-256, size, Go build ID, and test
// names. When cmd/go later asks the cache broker for A, the broker first
// looks up A's link record; on a hit it answers with a small placeholder
// file carrying the real build ID (which cmd/go reads from cached link
// outputs) and the record, instead of fetching the executable. The -exec
// capture wrapper recognizes the placeholder and reports the real hash and
// tests, so the package needs neither a download nor a -test.list run.
// A test that misses the result cache then fetches the real executable
// for A directly from GOCACHEPROG, verifying its hash.
var linkSkip = flag.Bool("link-skip", true, "with an external GOCACHEPROG and result caching, don't fetch or list test binaries whose link records say what they contain until a test needs to run (for profiling, -link-skip=false always obtains every binary)")

const linkRecordVersion = 1

// linkRecord describes the test executable a link action produced.
type linkRecord struct {
	Version  int
	OutputID string // SHA-256 of the executable
	Size     int64
	BuildID  string
	Tests    []string // as listed by the executable
}

// linkRecordActionID returns the GOCACHEPROG action ID under which the
// link record for link action linkAction is stored.
func linkRecordActionID(linkAction []byte) []byte {
	h := sha256.New()
	fmt.Fprintf(h, "gotst link record v%d\n", linkRecordVersion)
	h.Write(linkAction)
	return h.Sum(nil)
}

// linkStubMarker follows the build ID in a placeholder, before its JSON.
const linkStubMarker = "gotst-link-stub:"

// linkStub is the payload of a placeholder for a skipped test executable.
type linkStub struct {
	LinkAction string // hex
	linkRecord
}

// goBuildIDPrefix and goBuildIDEnd delimit a raw Go build ID, as
// cmd/internal/buildid reads from files that are not ELF, Mach-O, or
// archives.
const (
	goBuildIDPrefix = "\xff Go build ID: \""
	goBuildIDEnd    = "\"\n \xff"
)

// linkStubContents returns the placeholder for st.
func linkStubContents(st linkStub) ([]byte, error) {
	j, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString(goBuildIDPrefix + st.BuildID + goBuildIDEnd + "\n" + linkStubMarker)
	b.Write(j)
	b.WriteByte('\n')
	return b.Bytes(), nil
}

// readLinkStub reports whether the file at path is a placeholder and, if
// so, returns its payload.
func readLinkStub(path string) (linkStub, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return linkStub{}, false, err
	}
	defer f.Close()
	head := make([]byte, len(goBuildIDPrefix))
	if _, err := io.ReadFull(f, head); err != nil || string(head) != goBuildIDPrefix {
		return linkStub{}, false, nil
	}
	data, err := io.ReadAll(io.LimitReader(f, 64<<20))
	if err != nil {
		return linkStub{}, false, err
	}
	_, rest, ok := strings.Cut(string(data), goBuildIDEnd+"\n"+linkStubMarker)
	if !ok {
		return linkStub{}, false, nil
	}
	var st linkStub
	if err := json.Unmarshal([]byte(strings.TrimSpace(rest)), &st); err != nil {
		return linkStub{}, false, fmt.Errorf("parsing test binary placeholder: %w", err)
	}
	return st, true, nil
}

// auxCachePrograms are GOCACHEPROG instances that gotst itself uses outside
// cmd/go builds, for result-cache entries, link records, and executables,
// one per command, started on first use and left running until gotst exits.
// Every request waits for its response, so nothing is lost at exit.
var auxCachePrograms struct {
	sync.Mutex
	m map[string]*auxCacheProgram
}

type auxCacheProgram struct {
	once   sync.Once
	client *cacheProgClient
	err    error
}

// auxCacheProg returns the auxiliary GOCACHEPROG client for command.
func auxCacheProg(command string) (*cacheProgClient, error) {
	auxCachePrograms.Lock()
	if auxCachePrograms.m == nil {
		auxCachePrograms.m = make(map[string]*auxCacheProgram)
	}
	p := auxCachePrograms.m[command]
	if p == nil {
		p = new(auxCacheProgram)
		auxCachePrograms.m[command] = p
	}
	auxCachePrograms.Unlock()
	p.once.Do(func() {
		p.client, p.err = startCacheProgClient(command)
		if p.err == nil && (!p.client.can[cacheProgGet] || !p.client.can[cacheProgPut]) {
			p.client.close()
			p.client, p.err = nil, errors.New("GOCACHEPROG lacks get or put")
		}
	})
	return p.client, p.err
}

// linkSkipEnabled reports whether this process should skip obtaining test
// binaries, given whether result caching is on.
func linkSkipEnabled(resultCache bool) bool {
	return *linkSkip && resultCache && os.Getenv("GOCACHEPROG") != ""
}

// lookupLinkRecord returns the link record for linkAction, if any.
func lookupLinkRecord(ctx context.Context, c *cacheProgClient, linkAction []byte) (*linkRecord, error) {
	res, err := c.send(ctx, cacheProgRequest{Command: cacheProgGet, ActionID: linkRecordActionID(linkAction)})
	if err != nil {
		return nil, err
	}
	if res.Miss || res.DiskPath == "" {
		return nil, nil
	}
	data, err := os.ReadFile(res.DiskPath)
	if err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(data); len(res.OutputID) != 0 && !bytes.Equal(sum[:], res.OutputID) {
		return nil, nil
	}
	var rec linkRecord
	if err := json.Unmarshal(data, &rec); err != nil || rec.Version != linkRecordVersion || !validCacheHex(rec.OutputID) {
		return nil, nil
	}
	// The executable's build ID starts with its link action ID prefix.
	if first, _, _ := strings.Cut(rec.BuildID, "/"); first != actionIDPrefix(linkAction) {
		return nil, nil
	}
	return &rec, nil
}

// putLinkRecord stores the link record for the test executable at absBin,
// produced by link action linkAction (hex), with the given tests.
func putLinkRecord(ctx context.Context, linkAction, absBin string, tests []string) error {
	action, err := hex.DecodeString(linkAction)
	if err != nil || len(action) != sha256.Size {
		return fmt.Errorf("invalid link action ID %q", linkAction)
	}
	c, err := auxCacheProg(os.Getenv("GOCACHEPROG"))
	if err != nil {
		return err
	}
	buildID, err := readGoBuildID(absBin)
	if err != nil {
		return err
	}
	hash, size, err := hashFile(absBin)
	if err != nil {
		return err
	}
	data, err := json.Marshal(linkRecord{
		Version: linkRecordVersion, OutputID: hex.EncodeToString(hash), Size: size, BuildID: buildID,
		Tests: append([]string{}, tests...),
	})
	if err != nil {
		return err
	}
	outputID := sha256.Sum256(data)
	_, err = c.send(ctx, cacheProgRequest{
		Command: cacheProgPut, ActionID: linkRecordActionID(action), OutputID: outputID[:],
		BodySize: int64(len(data)), body: bytes.NewReader(data),
	})
	return err
}

// fetchExecutable obtains the test executable that link action linkAction
// (hex) produced, whose SHA-256 is outputHash, from GOCACHEPROG and places
// it at dest.
func fetchExecutable(ctx context.Context, linkAction, outputHash, dest string) error {
	action, err := hex.DecodeString(linkAction)
	if err != nil || len(action) != sha256.Size {
		return fmt.Errorf("invalid link action ID %q", linkAction)
	}
	c, err := auxCacheProg(os.Getenv("GOCACHEPROG"))
	if err != nil {
		return err
	}
	res, err := c.send(ctx, cacheProgRequest{Command: cacheProgGet, ActionID: action})
	if err != nil {
		return err
	}
	if res.Miss || res.DiskPath == "" {
		return fmt.Errorf("test executable %.12s is no longer in the build cache", outputHash)
	}
	got, _, err := hashFile(res.DiskPath)
	if err != nil {
		return err
	}
	if hex.EncodeToString(got) != outputHash {
		return fmt.Errorf("build cache returned a different test executable for %.12s", outputHash)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	return copyExecutable(res.DiskPath, dest)
}
