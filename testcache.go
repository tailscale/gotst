// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const testCacheVersion = 2

var errTestCacheMiss = errors.New("test result cache miss")

// testResultCache is deliberately small so a future implementation can proxy
// requests to a child process or network service without coupling the runner
// to the on-disk representation.
type testResultCache interface {
	Get(context.Context, testCacheKey) (*testCacheEntry, error)
	Put(context.Context, *testCacheEntry) error
}

type testCacheKey struct {
	BinarySHA256 string   `json:"binary_sha256"`
	Package      string   `json:"package"`
	Test         string   `json:"test"`
	WorkingDir   string   `json:"working_dir"`
	Args         []string `json:"args,omitempty"`
}

func (k testCacheKey) id() string {
	h := sha256.New()
	fmt.Fprintf(h, "gotst test cache key v%d\n", testCacheVersion)
	fmt.Fprintf(h, "binary %s\npackage %s\ntest %s\nworking dir %s\n", k.BinarySHA256, k.Package, k.Test, k.WorkingDir)
	for _, arg := range k.Args {
		fmt.Fprintf(h, "arg %q\n", arg)
	}
	return hex.EncodeToString(h.Sum(nil))
}

type testCacheEntry struct {
	Version      int               `json:"version"`
	Key          testCacheKey      `json:"key"`
	Created      time.Time         `json:"created"`
	PassedIn     time.Duration     `json:"passed_in_ns"`
	Dependencies []cacheDependency `json:"dependencies"`
}

type cacheDependency struct {
	Operation string `json:"operation"` // getenv, open, stat, or chdir
	Name      string `json:"name"`      // name as reported by the test binary
	Path      string `json:"path,omitempty"`

	Present     *bool           `json:"present,omitempty"`
	ValueSHA256 string          `json:"value_sha256,omitempty"`
	File        *fileDependency `json:"file,omitempty"`
}

type fileDependency struct {
	Exists            bool       `json:"exists"`
	Error             string     `json:"error,omitempty"`
	Stat              *fileState `json:"stat,omitempty"`
	Lstat             *fileState `json:"lstat,omitempty"`
	ContentSHA256     string     `json:"content_sha256,omitempty"`
	DirectorySHA256   string     `json:"directory_sha256,omitempty"`
	FingerprintSHA256 string     `json:"fingerprint_sha256"`
}

type fileState struct {
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	ModTime time.Time `json:"mod_time"`
	IsDir   bool      `json:"is_dir"`
}

type diskTestCache struct {
	dir string
}

func newDiskTestCache(cacheRoot string) (*diskTestCache, error) {
	dir := filepath.Join(cacheRoot, "test-results", fmt.Sprintf("v%d", testCacheVersion))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating test cache directory: %w", err)
	}
	return &diskTestCache{dir: dir}, nil
}

func (c *diskTestCache) Get(ctx context.Context, key testCacheKey) (*testCacheEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(c.entryPath(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errTestCacheMiss
		}
		return nil, err
	}
	var entry testCacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, fmt.Errorf("decoding cache entry: %w", err)
	}
	if entry.Version != testCacheVersion || entry.Key.id() != key.id() {
		return nil, errors.New("cache entry key or version mismatch")
	}
	return &entry, nil
}

func (c *diskTestCache) Put(ctx context.Context, entry *testCacheEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := c.entryPath(entry.Key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".entry-*")
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
	return os.Rename(tmpName, path)
}

func (c *diskTestCache) entryPath(key testCacheKey) string {
	bin := key.BinarySHA256
	if len(bin) < 2 {
		bin = "invalid-" + hashString(bin)
	}
	id := key.id()
	testName := safeCacheName(key.Test)
	return filepath.Join(c.dir, bin[:2], bin, testName+"-"+id+".json")
}

func safeCacheName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
		if b.Len() >= 80 {
			break
		}
	}
	if b.Len() == 0 {
		return "test"
	}
	return b.String()
}

func parseTestLog(path, workDir, packageRoot string) ([]cacheDependency, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	bs := bufio.NewScanner(f)
	if !bs.Scan() || bs.Text() != "# test log" {
		return nil, errors.New("malformed test log header")
	}
	pwd := workDir
	var deps []cacheDependency
	// The runtime always reads GODEBUG without reporting it through testlog.
	deps = append(deps, environmentDependency("GODEBUG"))
	for bs.Scan() {
		op, name, ok := strings.Cut(bs.Text(), " ")
		if !ok || name == "" {
			return nil, fmt.Errorf("malformed test log line %q", bs.Text())
		}
		var dep cacheDependency
		switch op {
		case "getenv":
			dep = environmentDependency(name)
		case "open", "stat", "chdir":
			path := name
			if !filepath.IsAbs(path) {
				path = filepath.Join(pwd, path)
			}
			path = filepath.Clean(path)
			// Match cmd/go's test cache: open and stat outside the package's
			// module, GOPATH, or GOROOT root are not rechecked. In particular,
			// creating a temporary file records an internal open of the shared
			// temp root, which is bookkeeping rather than a test input.
			if op != "chdir" && !pathWithinRoot(path, packageRoot) {
				continue
			}
			fd, err := fingerprintPath(path, op == "open")
			if err != nil {
				return nil, fmt.Errorf("fingerprinting %s %q: %w", op, path, err)
			}
			dep = cacheDependency{Operation: op, Name: name, Path: path, File: fd}
			if op == "chdir" {
				pwd = path
			}
		default:
			return nil, fmt.Errorf("unknown test log operation %q", op)
		}
		deps = append(deps, dep)
	}
	if err := bs.Err(); err != nil {
		return nil, err
	}
	return deduplicateDependencies(deps), nil
}

func pathWithinRoot(path, root string) bool {
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func environmentDependency(name string) cacheDependency {
	value, present := os.LookupEnv(name)
	return cacheDependency{
		Operation:   "getenv",
		Name:        name,
		Present:     boolPointer(present),
		ValueSHA256: hashPresenceAndValue(present, value),
	}
}

func fingerprintPath(path string, readContent bool) (*fileDependency, error) {
	fd := new(fileDependency)
	info, statErr := os.Stat(path)
	linfo, lstatErr := os.Lstat(path)
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		fd.Error = statErr.Error()
	}
	if lstatErr != nil && !errors.Is(lstatErr, fs.ErrNotExist) && fd.Error == "" {
		fd.Error = lstatErr.Error()
	}
	fd.Exists = statErr == nil || lstatErr == nil
	if statErr == nil {
		fd.Stat = fileInfoState(info)
	}
	if lstatErr == nil {
		fd.Lstat = fileInfoState(linfo)
	}
	if readContent && statErr == nil {
		switch {
		case info.Mode().IsRegular():
			f, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			h := sha256.New()
			_, copyErr := io.Copy(h, f)
			closeErr := f.Close()
			if copyErr != nil {
				return nil, copyErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
			fd.ContentSHA256 = hex.EncodeToString(h.Sum(nil))
		case info.IsDir():
			entries, err := os.ReadDir(path)
			if err != nil {
				return nil, err
			}
			h := sha256.New()
			for _, entry := range entries {
				fmt.Fprintf(h, "%q\n", entry.Name())
				if childInfo, err := entry.Info(); err != nil {
					fmt.Fprintf(h, "error %v\n", err)
				} else {
					writeFileState(h, childInfo)
				}
			}
			fd.DirectorySHA256 = hex.EncodeToString(h.Sum(nil))
		}
	}
	canonical, err := json.Marshal(struct {
		Exists             bool
		Error              string
		Stat, Lstat        *fileState
		Content, Directory string
	}{fd.Exists, fd.Error, fd.Stat, fd.Lstat, fd.ContentSHA256, fd.DirectorySHA256})
	if err != nil {
		return nil, err
	}
	fd.FingerprintSHA256 = hashBytes(canonical)
	return fd, nil
}

func fileInfoState(info fs.FileInfo) *fileState {
	return &fileState{
		Size: info.Size(), Mode: info.Mode().String(), ModTime: info.ModTime(), IsDir: info.IsDir(),
	}
}

func writeFileState(w io.Writer, info fs.FileInfo) {
	fmt.Fprintf(w, "%d %s %s %t\n", info.Size(), info.Mode(), info.ModTime().UTC().Format(time.RFC3339Nano), info.IsDir())
}

func validateDependencies(deps []cacheDependency) (bool, error) {
	changes, err := changedDependencies(deps)
	return len(changes) == 0, err
}

func changedDependencies(deps []cacheDependency) ([]string, error) {
	var changes []string
	for _, old := range deps {
		current, err := currentDependency(old)
		if err != nil {
			return nil, err
		}
		if !dependenciesEqual(old, current) {
			changes = append(changes, describeDependencyChange(old, current))
		}
	}
	return changes, nil
}

func currentDependency(old cacheDependency) (cacheDependency, error) {
	switch old.Operation {
	case "getenv":
		return environmentDependency(old.Name), nil
	case "open", "stat", "chdir":
		fd, err := fingerprintPath(old.Path, old.Operation == "open")
		if err != nil {
			return cacheDependency{}, err
		}
		return cacheDependency{Operation: old.Operation, Name: old.Name, Path: old.Path, File: fd}, nil
	default:
		return cacheDependency{}, fmt.Errorf("unknown cached dependency operation %q", old.Operation)
	}
}

func describeDependencyChange(old, current cacheDependency) string {
	if old.Operation == "getenv" {
		return fmt.Sprintf("getenv %q changed (%s -> %s)", old.Name, environmentSummary(old), environmentSummary(current))
	}
	return fmt.Sprintf("%s %q changed (%s -> %s)", old.Operation, old.Path, fileDependencySummary(old.File), fileDependencySummary(current.File))
}

func environmentSummary(dep cacheDependency) string {
	if dep.Present == nil || !*dep.Present {
		return "unset"
	}
	return "set, sha256=" + shortHash(dep.ValueSHA256)
}

func fileDependencySummary(dep *fileDependency) string {
	if dep == nil {
		return "no fingerprint"
	}
	if !dep.Exists {
		if dep.Error != "" {
			return "missing/error=" + dep.Error
		}
		return "missing"
	}
	var parts []string
	if dep.Stat != nil {
		parts = append(parts, fmt.Sprintf("size=%d mode=%s mtime=%s", dep.Stat.Size, dep.Stat.Mode, dep.Stat.ModTime.UTC().Format(time.RFC3339Nano)))
	}
	if dep.ContentSHA256 != "" {
		parts = append(parts, "content="+shortHash(dep.ContentSHA256))
	}
	if dep.DirectorySHA256 != "" {
		parts = append(parts, "directory="+shortHash(dep.DirectorySHA256))
	}
	if dep.Error != "" {
		parts = append(parts, "error="+dep.Error)
	}
	return strings.Join(parts, " ")
}

func shortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

func dependenciesEqual(a, b cacheDependency) bool {
	if a.Operation != b.Operation || a.Name != b.Name || a.Path != b.Path || a.ValueSHA256 != b.ValueSHA256 {
		return false
	}
	if (a.Present == nil) != (b.Present == nil) || a.Present != nil && *a.Present != *b.Present {
		return false
	}
	if (a.File == nil) != (b.File == nil) {
		return false
	}
	return a.File == nil || a.File.FingerprintSHA256 == b.File.FingerprintSHA256
}

func deduplicateDependencies(deps []cacheDependency) []cacheDependency {
	ret := make([]cacheDependency, 0, len(deps))
	seen := make(map[string]bool)
	for _, dep := range slices.Backward(deps) {
		key := dep.Operation + "\x00" + dep.Name + "\x00" + dep.Path
		if !seen[key] {
			seen[key] = true
			ret = append(ret, dep)
		}
	}
	slices.Reverse(ret)
	return ret
}

func hashPresenceAndValue(present bool, value string) string {
	h := sha256.New()
	if present {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	h.Write([]byte(value))
	return hex.EncodeToString(h.Sum(nil))
}

func hashString(s string) string { return hashBytes([]byte(s)) }
func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func boolPointer(v bool) *bool { return &v }
