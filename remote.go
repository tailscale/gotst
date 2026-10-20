// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/logger"
)

// The -remote mode runs the whole gotst invocation on other machines: one
// leader (-dist) and helpers. The local machine only snapshots its git
// state, ships it to each machine through a user-supplied remote shell
// command, and relays the leader's output. It never runs cmd/go, so no
// build cache traffic passes through it; remote machines share their own
// GOCACHEPROG, if configured.
//
// The remote shell command (-remote-cmd, $GOTST_REMOTE_CMD, or remote.command
// in .gotst.yml) is run once per machine with one more argument, a POSIX
// shell command, which it must run on a fresh or reusable Linux machine with
// standard input, output, and error connected, exiting with its status, as
// "ssh HOST" does. It may prefix that shell command with environment
// settings documented in remote_run.sh. Its environment also has:
//
//	GOTST_REMOTE_PROTOCOL  1
//	GOTST_REMOTE_INDEX     machine index, 0 (the leader) to COUNT-1
//	GOTST_REMOTE_COUNT     number of machines
//	GOTST_REMOTE_REPO_URL  the local checkout's origin URL
//	GOTST_REMOTE_REPO      "owner/name" if origin is on GitHub, else empty
var (
	remoteRun   remoteFlag
	remoteCmd   = flag.String("remote-cmd", "", "with -remote, the remote shell command, split on spaces (default $GOTST_REMOTE_CMD, then remote.command in .gotst.yml)")
	remoteGotst = flag.String("remote-gotst", "", "with -remote, a branch, tag, or commit of github.com/tailscale/gotst for each machine to shallow-fetch and install, instead of installing this binary's version")
)

func init() {
	flag.Var(&remoteRun, "remote", "run this invocation on remote machines via a remote shell command (see README); -remote=N sets the machine count (default remote.machines in .gotst.yml, else 4)")
}

// defaultRemoteMachines is the -remote machine count when neither the
// flag nor .gotst.yml gives one.
const defaultRemoteMachines = 4

// remoteFlag is the -remote flag: a boolean that optionally takes a
// machine count.
type remoteFlag struct {
	on bool
	n  int // 0 means the configured default
}

func (f *remoteFlag) IsBoolFlag() bool { return true }

func (f *remoteFlag) String() string {
	switch {
	case f == nil || !f.on:
		return "false"
	case f.n == 0:
		return "true"
	}
	return strconv.Itoa(f.n)
}

func (f *remoteFlag) Set(s string) error {
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 || n > 64 {
			return errors.New("want a machine count from 0 to 64")
		}
		*f = remoteFlag{on: n > 0, n: n}
		return nil
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return errors.New("want a boolean or a machine count from 0 to 64")
	}
	*f = remoteFlag{on: b}
	return nil
}

//go:embed remote_run.sh
var remoteRunScript string

// remoteLocalFlags are flags that configure -remote itself and are not
// forwarded to the leader.
var remoteLocalFlags = map[string]bool{"remote": true, "remote-cmd": true, "remote-gotst": true}

// remoteBundleRef is the ref name for the snapshot inside src.bundle.
const remoteBundleRef = "refs/gotst-remote/snapshot"

// helperAddrLine matches the leader's announcement of its helper address.
var helperAddrLine = regexp.MustCompile(`^# gotst helpers: gotst -helper=(\S+)`)

// statusAddrLine matches the leader's announcement of its status page.
var statusAddrLine = regexp.MustCompile(`^# gotst status: tailcat browse (\S+)`)

// remoteForwardArgs returns the leader's gotst arguments for this
// invocation: every explicitly set flag except -remote's own and -j, then
// the positional arguments. It returns the -j value separately (or "" if
// unset), since it applies to every machine. The leader serves its status
// page over tailcat; unless -listen is "" or "tailcat", localListen is the
// local address at which to proxy it.
func remoteForwardArgs(fs *flag.FlagSet) (args []string, jobs, localListen string, err error) {
	if f := fs.Lookup("listen"); f != nil {
		localListen = f.DefValue
	}
	fs.Visit(func(f *flag.Flag) {
		switch {
		case remoteLocalFlags[f.Name]:
			return
		case f.Name == "j":
			jobs = f.Value.String()
			return
		case f.Name == "dist" || f.Name == "dist-key" || f.Name == "helper" || f.Name == "helper-name":
			err = fmt.Errorf("-remote cannot be combined with -%s", f.Name)
		case f.Name == "cache-dir" || f.Name == "config":
			if filepath.IsAbs(f.Value.String()) {
				err = fmt.Errorf("-remote cannot use an absolute -%s path", f.Name)
			}
		case f.Name == "listen":
			localListen = f.Value.String()
			return
		}
		args = append(args, "-"+f.Name+"="+f.Value.String())
	})
	if err != nil {
		return nil, "", "", err
	}
	switch localListen {
	case "":
		args = append(args, "-listen=")
	default:
		// Any other address is local; a loopback status server on
		// the leader would be unreachable from here.
		args = append(args, "-listen="+tailcatListenValue)
		if localListen == tailcatListenValue {
			localListen = ""
		}
	}
	if fs.NArg() > 0 && strings.HasPrefix(fs.Arg(0), "-") {
		args = append(args, "--")
	}
	return append(args, fs.Args()...), jobs, localListen, nil
}

// remoteSnapshot is the local checkout state to reproduce on each machine.
type remoteSnapshot struct {
	url    string   // origin remote URL
	repo   string   // GitHub "owner/name", or empty
	rel    string   // working directory relative to the checkout root, or "."
	snap   string   // commit whose tree is the working tree, including uncommitted files
	bases  []string // commits remote machines fetch from the origin
	bundle []byte   // git bundle of snap from bases, or nil if snap is pushed

	commits int           // commits in bundle, including any working tree snapshot
	skipped []skippedFile // large untracked files left out
}

// skippedFile is an untracked file left out of a remote snapshot.
type skippedFile struct {
	path string // relative to the checkout root
	size int64
}

// maxUntrackedSize is the largest untracked file a remote snapshot
// includes. Staging a file with "git add" includes it regardless.
const maxUntrackedSize = 1 << 20

// largeUntracked returns the untracked, unignored files in the checkout
// at root larger than maxUntrackedSize, largest first.
func largeUntracked(root string) ([]skippedFile, error) {
	out, err := gitOutput(root, nil, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	var big []skippedFile
	for _, p := range strings.Split(out, "\x00") {
		if p == "" {
			continue
		}
		if fi, err := os.Lstat(filepath.Join(root, p)); err == nil && fi.Mode().IsRegular() && fi.Size() > maxUntrackedSize {
			big = append(big, skippedFile{p, fi.Size()})
		}
	}
	slices.SortFunc(big, func(a, b skippedFile) int { return cmp.Compare(b.size, a.size) })
	return big, nil
}

func gitOutput(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, bytes.TrimSpace(stderr.Bytes()))
	}
	return strings.TrimSpace(string(out)), nil
}

var githubRepoRx = regexp.MustCompile(`github\.com[:/]+([\w.-]+/[\w.-]+?)(?:\.git)?/*$`)

// githubRepo returns the "owner/name" of a GitHub remote URL.
func githubRepo(url string) (string, bool) {
	m := githubRepoRx.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// takeRemoteSnapshot records the checkout containing dir, including
// uncommitted and untracked (but not ignored) files, as a commit that the
// remote machines can reconstruct from commits on the origin remote plus a bundle.
// It writes git objects and a temporary ref but does not change the
// index, working tree, or HEAD.
func takeRemoteSnapshot(dir string) (*remoteSnapshot, error) {
	root, err := gitOutput(dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	prefix, err := gitOutput(dir, nil, "rev-parse", "--show-prefix")
	if err != nil {
		return nil, err
	}
	url, err := gitOutput(root, nil, "remote", "get-url", "origin")
	if err != nil {
		return nil, err
	}
	sn := &remoteSnapshot{url: url, rel: cmpOr(strings.TrimSuffix(prefix, "/"), ".")}
	sn.repo, _ = githubRepo(url)
	head, err := gitOutput(root, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, err
	}

	// Stage the whole working tree into a temporary copy of the index.
	index, err := gitOutput(root, nil, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "gotst-remote-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	tmpIndex := filepath.Join(tmp, "index")
	if b, err := os.ReadFile(index); err == nil {
		if err := os.WriteFile(tmpIndex, b, 0600); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	env := []string{"GIT_INDEX_FILE=" + tmpIndex}
	if _, err := gitOutput(root, env, "add", "-A", "--", "."); err != nil {
		return nil, err
	}
	// Leave out large untracked files, which are more likely build
	// outputs or downloads than test inputs, but would be uploaded to
	// every machine.
	if sn.skipped, err = largeUntracked(root); err != nil {
		return nil, err
	}
	if len(sn.skipped) > 0 {
		var paths []string
		for _, f := range sn.skipped {
			paths = append(paths, f.path)
		}
		if _, err := gitOutput(root, env, append([]string{"rm", "-q", "--cached", "--"}, paths...)...); err != nil {
			return nil, err
		}
	}
	tree, err := gitOutput(root, env, "write-tree")
	if err != nil {
		return nil, err
	}
	headTree, err := gitOutput(root, nil, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return nil, err
	}
	sn.snap = head
	if tree != headTree {
		sn.snap, err = gitOutput(root, []string{
			"GIT_AUTHOR_NAME=gotst", "GIT_AUTHOR_EMAIL=gotst@localhost",
			"GIT_COMMITTER_NAME=gotst", "GIT_COMMITTER_EMAIL=gotst@localhost",
			// A fixed date makes the snapshot of an unchanged tree reproducible.
			"GIT_AUTHOR_DATE=@0 +0000", "GIT_COMMITTER_DATE=@0 +0000",
		}, "commit-tree", "--no-gpg-sign", tree, "-p", head, "-m", "gotst -remote working tree snapshot")
		if err != nil {
			return nil, err
		}
	}

	// Find which commits the remote already has, per our remote-tracking
	// refs. If many seem unpushed, the refs are probably just stale (as
	// when HEAD came from elsewhere), so fetch and look again rather than
	// bundling commits the remote machines could fetch themselves.
	list, err := gitOutput(root, nil, "rev-list", "--boundary", sn.snap, "--not", "--remotes=origin")
	if err != nil {
		return nil, err
	}
	if n := strings.Count(list, "\n") + 1; list != "" && n > remoteFetchThreshold {
		branches := originBranchesToFetch(root)
		fmt.Fprintf(os.Stderr, "# gotst -remote: %d commits not on origin; fetching origin %s\n", n, strings.Join(branches, " "))
		if _, err := gitOutput(root, nil, append([]string{"fetch", "-q", "origin"}, branches...)...); err != nil {
			return nil, err
		}
		if list, err = gitOutput(root, nil, "rev-list", "--boundary", sn.snap, "--not", "--remotes=origin"); err != nil {
			return nil, err
		}
	}
	if list == "" {
		sn.bases = []string{sn.snap}
		return sn, nil
	}
	for _, line := range strings.Split(list, "\n") {
		if c, ok := strings.CutPrefix(line, "-"); ok {
			sn.bases = append(sn.bases, c)
		} else {
			sn.commits++
		}
	}
	if len(sn.bases) == 0 {
		return nil, errors.New("no commit of HEAD's history is on the origin remote; push or fetch first")
	}

	ref := fmt.Sprintf("refs/gotst-remote/pid%d", os.Getpid())
	if _, err := gitOutput(root, nil, "update-ref", ref, sn.snap); err != nil {
		return nil, err
	}
	defer gitOutput(root, nil, "update-ref", "-d", ref)
	bundle := filepath.Join(tmp, "src.bundle")
	args := []string{"bundle", "create", "-q", bundle, ref}
	for _, b := range sn.bases {
		args = append(args, "^"+b)
	}
	if _, err := gitOutput(root, nil, args...); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(bundle)
	if err != nil {
		return nil, err
	}
	// Rename the ref inside the bundle header to the fixed name the
	// guest script fetches. The header is text, terminated by a blank line.
	end := bytes.Index(b, []byte("\n\n"))
	if end < 0 {
		return nil, errors.New("malformed git bundle")
	}
	hdr := bytes.ReplaceAll(b[:end+1], []byte(" "+ref+"\n"), []byte(" "+remoteBundleRef+"\n"))
	if !bytes.Contains(hdr, []byte(" "+remoteBundleRef+"\n")) {
		return nil, errors.New("unexpected git bundle header")
	}
	sn.bundle = append(hdr, b[end+1:]...)
	return sn, nil
}

// remoteFetchThreshold is how many apparently unpushed commits (plus
// boundary commits) make takeRemoteSnapshot fetch origin first.
const remoteFetchThreshold = 20

// originBranchesToFetch returns origin's default branch and HEAD's
// upstream branch on origin, if any.
func originBranchesToFetch(root string) []string {
	def := "main"
	if ref, err := gitOutput(root, nil, "symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if b, ok := strings.CutPrefix(ref, "origin/"); ok {
			def = b
		}
	}
	branches := []string{def}
	if up, err := gitOutput(root, nil, "rev-parse", "-q", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); err == nil {
		if b, ok := strings.CutPrefix(up, "origin/"); ok && b != def {
			branches = append(branches, b)
		}
	}
	return branches
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// gotstVersion returns the github.com/tailscale/gotst version this binary
// was built from, for remote machines to "go install".
func gotstVersion(bi *debug.BuildInfo) (string, error) {
	if bi == nil {
		return "", errors.New("no build information")
	}
	if bi.Main.Path != "github.com/tailscale/gotst" {
		return "", fmt.Errorf("main module is %q", bi.Main.Path)
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		if strings.HasSuffix(v, "+dirty") {
			return "", errors.New("built from a modified checkout")
		}
		return v, nil
	}
	var rev string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				return "", errors.New("built from a modified checkout")
			}
		}
	}
	if rev == "" {
		return "", errors.New("no VCS revision recorded")
	}
	return rev, nil
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_=+:,./@%") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// remoteMachine describes one machine's role in the run.
type remoteMachine struct {
	index  int
	name   string // display name, also the helper name
	leader bool
}

// sourceURL returns the URL remote machines fetch commits from by default:
// the origin URL, rewritten to anonymous https for GitHub.
func (sn *remoteSnapshot) sourceURL() string {
	if sn.repo != "" {
		return "https://github.com/" + sn.repo
	}
	return sn.url
}

// checkoutName returns a directory name for the remote checkout.
func (sn *remoteSnapshot) checkoutName() string {
	name := cmpOr(sn.repo, strings.TrimSuffix(sn.url, "/"))
	return regexp.MustCompile(`[^\w.-]+`).ReplaceAllString(name, "_")
}

// remoteMeta returns the guest script's meta.env for m.
func remoteMeta(m remoteMachine, sn *remoteSnapshot, gotstVersion, gotstRef, jobs string, args []string) string {
	var b strings.Builder
	role := "helper"
	if m.leader {
		role = "leader"
	}
	for _, kv := range [][2]string{
		{"NAME", m.name},
		{"ROLE", role},
		{"SRC_URL", sn.sourceURL()},
		{"CHECKOUT", sn.checkoutName()},
		{"REL", sn.rel},
		{"SNAP", sn.snap},
		{"BASES", strings.Join(sn.bases, " ")},
		{"BUNDLE_REF", remoteBundleRef},
		{"GOTST_VERSION", gotstVersion},
		{"GOTST_REF", gotstRef},
		{"JOBS", jobs},
	} {
		fmt.Fprintf(&b, "%s=%s\n", kv[0], shellQuote(kv[1]))
	}
	b.WriteString("set --")
	for _, a := range args {
		b.WriteString(" " + shellQuote(a))
	}
	b.WriteString("\n")
	return b.String()
}

// remotePayload returns the gzipped tar archive sent to a machine's standard input.
func remotePayload(meta string, sn *remoteSnapshot) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	add := func(name string, mode int64, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := add("run.sh", 0644, []byte(remoteRunScript)); err != nil {
		return nil, err
	}
	if err := add("meta.env", 0644, []byte(meta)); err != nil {
		return nil, err
	}
	if sn.bundle != nil {
		if err := add("src.bundle", 0644, sn.bundle); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// remoteShellCommand returns the shell command a machine runs for payload:
// it reads exactly that many bytes from standard input, leaving any later
// input (a helper's leader address) for run.sh, and checks them.
func remoteShellCommand(payload []byte) string {
	n := len(payload)
	sum := sha256.Sum256(payload)
	return fmt.Sprintf(`d=$(mktemp -d /tmp/gotst-remote.XXXXXX) && cd "$d" && head -c %d > payload.tgz && `+
		`{ n=$(wc -c < payload.tgz); [ "$n" -eq %d ] || { echo "gotst-remote: received $n of %d payload bytes" >&2; exit 1; }; } && `+
		`{ echo "%x  payload.tgz" | sha256sum -c --status || { echo "gotst-remote: corrupt payload; does the remote shell's startup read standard input?" >&2; exit 1; }; } && `+
		`tar -xzf payload.tgz && exec sh ./run.sh`, n, n, n, sum)
}

// remoteSettings is the resolved -remote configuration.
type remoteSettings struct {
	command  []string
	machines int
}

// resolveRemoteSettings combines the -remote flags, $GOTST_REMOTE_CMD, and
// the remote section of .gotst.yml, in that order of precedence.
func resolveRemoteSettings(defs *profileDefinitions, flagCmd, envCmd string, n int) (remoteSettings, error) {
	var rs remoteSettings
	cfg := defs.remote
	switch {
	case flagCmd != "":
		rs.command = strings.Fields(flagCmd)
	case envCmd != "":
		rs.command = strings.Fields(envCmd)
	case cfg != nil && len(cfg.Command) > 0:
		rs.command = slices.Clone(cfg.Command)
		// A relative program path is relative to the config file.
		if p := rs.command[0]; strings.ContainsRune(p, '/') && !filepath.IsAbs(p) {
			rs.command[0] = filepath.Join(filepath.Dir(defs.path), p)
		}
	}
	if len(rs.command) == 0 {
		return rs, errors.New("no remote shell command; use -remote-cmd, $GOTST_REMOTE_CMD, or remote.command in .gotst.yml")
	}
	rs.machines = n
	if rs.machines == 0 && cfg != nil {
		rs.machines = cfg.Machines
	}
	if rs.machines == 0 {
		rs.machines = defaultRemoteMachines
	}
	return rs, nil
}

// machineOutput relays one machine's output lines to the local stderr.
// Until the guest script starts (its first line has guestLinePrefix), the
// lines are the remote shell command's own, such as VM provisioning
// progress: they are held, and shown only if the machine fails before the
// script starts, or with -vlog.
type machineOutput struct {
	outMu  *sync.Mutex // serializes all machines' output
	prefix string

	mu      sync.Mutex
	started bool
	held    []string
}

// guestLinePrefix begins remote_run.sh's own messages.
const guestLinePrefix = "gotst-remote["

// maxHeldLines bounds machineOutput's held lines; older ones are dropped.
const maxHeldLines = 200

func (o *machineOutput) print(line string) {
	o.outMu.Lock()
	defer o.outMu.Unlock()
	fmt.Fprintf(os.Stderr, "%s%s\n", o.prefix, line)
}

func (o *machineOutput) line(line string) {
	o.mu.Lock()
	if !o.started && strings.HasPrefix(line, guestLinePrefix) {
		o.started = true
		o.held = nil
	}
	if !o.started && !*verbose {
		if len(o.held) == maxHeldLines {
			o.held = o.held[1:]
		}
		o.held = append(o.held, line)
		o.mu.Unlock()
		return
	}
	o.mu.Unlock()
	o.print(line)
}

// exited shows the held lines, if any: the machine ended before its guest
// script started, so they likely say why.
func (o *machineOutput) exited() {
	o.mu.Lock()
	held := o.held
	o.held = nil
	o.mu.Unlock()
	for _, l := range held {
		o.print(l)
	}
}

// copyLines calls onLine for each line read from r.
func copyLines(r io.Reader, onLine func(string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		onLine(sc.Text())
	}
	io.Copy(io.Discard, r)
}

// runRemote runs this invocation on remote machines and returns the
// leader's exit code.
func runRemote(defs *profileDefinitions) (int, error) {
	rs, err := resolveRemoteSettings(defs, *remoteCmd, os.Getenv("GOTST_REMOTE_CMD"), remoteRun.n)
	if err != nil {
		return 0, err
	}
	args, jobs, localListen, err := remoteForwardArgs(flag.CommandLine)
	if err != nil {
		return 0, err
	}

	// Without -remote-gotst, machines install this binary's version,
	// unless the remote shell command provides GOTST_BIN. If the
	// version is unknown, tell the guest why, in place of a version.
	version := ""
	if *remoteGotst == "" {
		bi, _ := debug.ReadBuildInfo()
		if version, err = gotstVersion(bi); err != nil {
			version = "unknown: can't determine the local gotst's version (" + err.Error() + ")"
		}
	}

	wd, err := os.Getwd()
	if err != nil {
		return 0, err
	}
	t0 := time.Now()
	sn, err := takeRemoteSnapshot(wd)
	if err != nil {
		return 0, fmt.Errorf("snapshotting checkout: %w", err)
	}
	local := "pushed"
	if sn.bundle != nil {
		local = fmt.Sprintf("%d KiB bundle of %d local commits", (len(sn.bundle)+1023)/1024, sn.commits)
	}
	if n := len(sn.skipped); n > 0 {
		fmt.Fprintf(os.Stderr, "# gotst -remote: not sending %d untracked files over %d KiB (git add them to send them):\n", n, maxUntrackedSize>>10)
		for i, f := range sn.skipped {
			if i == 5 {
				fmt.Fprintf(os.Stderr, "#   ... and %d more\n", n-i)
				break
			}
			fmt.Fprintf(os.Stderr, "#   %s (%.1f MiB)\n", f.path, float64(f.size)/(1<<20))
		}
	}
	fmt.Fprintf(os.Stderr, "# gotst -remote: %.12s (%s, %v); starting %d machines with %s\n",
		sn.snap, local, time.Since(t0).Round(time.Millisecond), rs.machines, strings.Join(rs.command, " "))

	if n := len(sn.bundle); n >= 1<<20 {
		fmt.Fprintf(os.Stderr, "# gotst -remote: sending %.1f MiB to each machine; this may take a while\n", float64(n)/(1<<20))
	}
	var outMu sync.Mutex
	type machineProc struct {
		cmd  *exec.Cmd
		done chan error
	}
	procs := make([]*machineProc, rs.machines)
	addrc := make(chan string, 1)
	// addrReady delivers the leader's helper address to each helper's
	// stdin writer; noAddr is closed if there will be none.
	addrReady := make(chan string, rs.machines)
	noAddr := make(chan struct{})
	for i := range procs {
		m := remoteMachine{index: i, name: fmt.Sprintf("m%d", i), leader: i == 0}
		if m.leader {
			m.name = "leader"
		}
		payload, err := remotePayload(remoteMeta(m, sn, version, *remoteGotst, jobs, args), sn)
		if err != nil {
			return 0, err
		}
		cmd := exec.Command(rs.command[0], append(rs.command[1:], remoteShellCommand(payload))...)
		cmd.Env = append(os.Environ(),
			"GOTST_REMOTE_PROTOCOL=1",
			fmt.Sprintf("GOTST_REMOTE_INDEX=%d", i),
			fmt.Sprintf("GOTST_REMOTE_COUNT=%d", rs.machines),
			"GOTST_REMOTE_REPO_URL="+sn.url,
			"GOTST_REMOTE_REPO="+sn.repo,
		)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return 0, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return 0, err
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return 0, err
		}
		if err := cmd.Start(); err != nil {
			return 0, fmt.Errorf("starting remote shell command: %w", err)
		}
		p := &machineProc{cmd: cmd, done: make(chan error, 1)}
		procs[i] = p
		// The only writer to stdin: the payload, then a helper's
		// leader address once known, then EOF.
		go func() {
			defer stdin.Close()
			t0 := time.Now()
			if _, err := stdin.Write(payload); err != nil {
				return
			}
			if len(payload) >= 1<<20 {
				outMu.Lock()
				fmt.Fprintf(os.Stderr, "# gotst -remote: sent %s %.1f MiB in %v\n", m.name, float64(len(payload))/(1<<20), time.Since(t0).Round(100*time.Millisecond))
				outMu.Unlock()
			}
			if m.leader {
				return
			}
			select {
			case addr := <-addrReady:
				io.WriteString(stdin, addr+"\n")
			case <-noAddr:
			}
		}()
		var wg sync.WaitGroup
		wg.Add(2)
		mo := &machineOutput{outMu: &outMu, prefix: "[" + m.name + "] "}
		if m.leader {
			mo.prefix = ""
			go func() {
				defer wg.Done()
				io.Copy(os.Stdout, stdout)
			}()
			go func() {
				defer wg.Done()
				copyLines(stderr, func(line string) {
					if am := helperAddrLine.FindStringSubmatch(line); am != nil {
						select {
						case addrc <- am[1]:
						default:
						}
						if !*verbose {
							return
						}
					}
					if sm := statusAddrLine.FindStringSubmatch(line); sm != nil && localListen != "" {
						if url, err := startStatusProxy(localListen, sm[1]); err != nil {
							mo.print(line)
							mo.print(fmt.Sprintf("# gotst -remote: can't proxy the status page at %s: %v", localListen, err))
						} else {
							mo.print("# gotst status: " + url)
						}
						return
					}
					mo.line(line)
				})
			}()
		} else {
			go func() { defer wg.Done(); copyLines(stdout, mo.line) }()
			go func() { defer wg.Done(); copyLines(stderr, mo.line) }()
		}
		go func() {
			wg.Wait()
			err := cmd.Wait()
			mo.exited()
			p.done <- err
		}()
	}

	leader := procs[0]
	var leaderErr error
	select {
	case addr := <-addrc:
		for range procs[1:] {
			addrReady <- addr
		}
		leaderErr = <-leader.done
		// Helpers exit once the leader completes; don't wait long
		// for stragglers.
		close(noAddr)
		deadline := time.After(5 * time.Second)
		for _, p := range procs[1:] {
			select {
			case <-p.done:
			case <-deadline:
				p.cmd.Process.Kill()
			}
		}
	case leaderErr = <-leader.done:
		// The leader never started gotst; its output above says why.
		// Stop the helpers rather than leave them waiting.
		close(noAddr)
		for _, p := range procs[1:] {
			p.cmd.Process.Kill()
		}
		for _, p := range procs[1:] {
			<-p.done
		}
		if leaderErr == nil {
			leaderErr = errors.New("exited before starting gotst")
		}
		return 0, fmt.Errorf("leader: %w (see its unprefixed output above)", leaderErr)
	}
	if leaderErr != nil {
		var ee *exec.ExitError
		if errors.As(leaderErr, &ee) && ee.ExitCode() > 0 {
			return ee.ExitCode(), nil
		}
		return 0, fmt.Errorf("leader: %w", leaderErr)
	}
	return 0, nil
}

func mainRemote() {
	defs, err := loadProfileDefinitions(*configFile)
	if err != nil {
		log.Fatal(err)
	}
	code, err := runRemote(defs)
	if err != nil {
		log.Fatalf("-remote: %v", err)
	}
	os.Exit(code)
}

// startStatusProxy serves the leader's tailcat status page at the local
// address listen and returns its URL. It runs until the process exits.
func startStatusProxy(listen string, leader string) (string, error) {
	if _, err := tailcat.ParseAddr(tailcat.Addr(leader)); err != nil {
		return "", fmt.Errorf("leader status address: %w", err)
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return "", err
	}
	logf := logger.Discard
	if *verbose {
		logf = log.Printf
	}
	client := &tailcat.Client{Server: tailcat.Addr(leader), Logf: logf}
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = "http"
			r.Out.URL.Host = "gotst-leader"
			r.Out.Host = r.In.Host
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return client.DialTCPPort(ctx, 80)
			},
		},
		ErrorLog: log.New(io.Discard, "", 0),
	}
	go http.Serve(ln, rp)
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port) + "/", nil
}
