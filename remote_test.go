// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
)

func TestGithubRepo(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/tailscale/gotst/":    "tailscale/gotst",
		"https://github.com/tailscale/corp.git":  "tailscale/corp",
		"git@github.com:tailscale/tailscale.git": "tailscale/tailscale",
		"ssh://git@github.com/tailscale/x.y":     "tailscale/x.y",
		"https://gitlab.com/foo/bar":             "",
	} {
		if got, _ := githubRepo(url); got != want {
			t.Errorf("githubRepo(%q) = %q; want %q", url, got, want)
		}
	}
}

func TestRemoteForwardArgs(t *testing.T) {
	newFS := func() *flag.FlagSet {
		fs := flag.NewFlagSet("x", flag.ContinueOnError)
		fs.Var(new(remoteFlag), "remote", "")
		fs.Int("j", 4, "")
		fs.Bool("race", false, "")
		fs.String("listen", "127.0.0.1:5525", "")
		fs.Bool("dist", false, "")
		fs.String("config", "", "")
		return fs
	}
	tests := []struct {
		args       []string
		want       []string
		wantJobs   string
		wantListen string
		wantErr    bool
	}{
		{args: []string{"-remote=4", "./..."}, want: []string{"-listen=tailcat", "./..."}, wantListen: "127.0.0.1:5525"},
		{args: []string{"-remote=4", "-j=16", "-race", "-listen=", "x", "TestFoo"}, want: []string{"-race=true", "-listen=", "x", "TestFoo"}, wantJobs: "16"},
		{args: []string{"-remote=4", "-listen=tailcat"}, want: []string{"-listen=tailcat"}},
		{args: []string{"-remote=4", "-listen=:8080"}, want: []string{"-listen=tailcat"}, wantListen: ":8080"},
		{args: []string{"-remote=4", "--", "-weird"}, want: []string{"-listen=tailcat", "--", "-weird"}, wantListen: "127.0.0.1:5525"},
		{args: []string{"-remote=4", "-dist"}, wantErr: true},
		{args: []string{"-remote=4", "-config=/abs/.gotst.yml"}, wantErr: true},
	}
	for _, tt := range tests {
		fs := newFS()
		if err := fs.Parse(tt.args); err != nil {
			t.Fatal(err)
		}
		got, jobs, listen, err := remoteForwardArgs(fs)
		if (err != nil) != tt.wantErr {
			t.Errorf("%q: err = %v", tt.args, err)
			continue
		}
		if err == nil && (!reflect.DeepEqual(got, tt.want) || jobs != tt.wantJobs || listen != tt.wantListen) {
			t.Errorf("%q: got %q, %q, %q; want %q, %q, %q", tt.args, got, jobs, listen, tt.want, tt.wantJobs, tt.wantListen)
		}
	}
}

func TestGotstVersion(t *testing.T) {
	bi := func(v string, settings ...string) *debug.BuildInfo {
		b := &debug.BuildInfo{Main: debug.Module{Path: "github.com/tailscale/gotst", Version: v}}
		for i := 0; i < len(settings); i += 2 {
			b.Settings = append(b.Settings, debug.BuildSetting{Key: settings[i], Value: settings[i+1]})
		}
		return b
	}
	for _, tt := range []struct {
		bi   *debug.BuildInfo
		want string
	}{
		{bi("v0.0.0-20260910050716-a2a5b2b4fcdf"), "v0.0.0-20260910050716-a2a5b2b4fcdf"},
		{bi("v0.0.0-20260910050716-a2a5b2b4fcdf+dirty"), ""},
		{bi("v1.2.3"), "v1.2.3"},
		{bi("(devel)", "vcs.revision", "abc123", "vcs.modified", "false"), "abc123"},
		{bi("(devel)", "vcs.revision", "abc123", "vcs.modified", "true"), ""},
		{bi("(devel)"), ""},
		{nil, ""},
	} {
		got, err := gotstVersion(tt.bi)
		if got != tt.want || (err == nil) != (tt.want != "") {
			t.Errorf("gotstVersion(%+v) = %q, %v; want %q", tt.bi, got, err, tt.want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	vals := []string{"", "plain", "-run=Test.*", "it's", "a b", `$HOME`, "`x`", "\n"}
	var script strings.Builder
	script.WriteString("set --")
	for _, v := range vals {
		script.WriteString(" " + shellQuote(v))
	}
	script.WriteString(`; for a; do printf '%s\0' "$a"; done`)
	out, err := exec.Command("sh", "-c", script.String()).Output()
	if err != nil {
		t.Skip(err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if !reflect.DeepEqual(got, vals) {
		t.Errorf("got %q; want %q", got, vals)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitOutput(dir, []string{
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
	}, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRemoteSnapshotRoundTrip snapshots a checkout with local commits,
// modified, deleted, and untracked files, then reconstructs it in a
// shallow clone the way the guest script does.
func TestRemoteSnapshotRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	tmp := t.TempDir()
	upstream := filepath.Join(tmp, "tailscale", "proj")
	laptop := filepath.Join(tmp, "laptop")
	git(t, tmp, "init", "-q", "-b", "main", upstream)
	write := func(dir, name, data string) {
		t.Helper()
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0755)
		if err := os.WriteFile(p, []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(upstream, "a.txt", "a\n")
	write(upstream, "gone.txt", "gone\n")
	write(upstream, ".gitignore", "*.ignored\n")
	git(t, upstream, "add", ".")
	git(t, upstream, "commit", "-q", "-m", "one")
	git(t, tmp, "clone", "-q", upstream, laptop)
	git(t, laptop, "remote", "set-url", "origin", "https://github.com/tailscale/proj")

	// Clean pushed checkout: nothing to bundle.
	sn, err := takeRemoteSnapshot(laptop)
	if err != nil {
		t.Fatal(err)
	}
	head := git(t, laptop, "rev-parse", "HEAD")
	if sn.repo != "tailscale/proj" || sn.sourceURL() != "https://github.com/tailscale/proj" || sn.snap != head || sn.bundle != nil || !reflect.DeepEqual(sn.bases, []string{head}) || sn.rel != "." {
		t.Fatalf("clean snapshot = %+v", sn)
	}

	write(laptop, "b.txt", "local commit\n")
	git(t, laptop, "add", "b.txt")
	git(t, laptop, "commit", "-q", "-m", "local")
	write(laptop, "a.txt", "modified\n")
	write(laptop, "sub/new.txt", "untracked\n")
	write(laptop, "x.ignored", "ignored\n")
	write(laptop, "huge.bin", strings.Repeat("x", maxUntrackedSize+1))
	os.Remove(filepath.Join(laptop, "gone.txt"))
	statusBefore := git(t, laptop, "status", "--porcelain")

	sn, err = takeRemoteSnapshot(filepath.Join(laptop, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if sn.rel != "sub" || sn.bundle == nil || !reflect.DeepEqual(sn.bases, []string{head}) || sn.commits != 2 ||
		len(sn.skipped) != 1 || sn.skipped[0].path != "huge.bin" {
		t.Fatalf("dirty snapshot = %+v", sn)
	}
	if got := git(t, laptop, "status", "--porcelain"); got != statusBefore {
		t.Errorf("snapshot changed status from %q to %q", statusBefore, got)
	}
	if refs := git(t, laptop, "for-each-ref", "refs/gotst-remote"); refs != "" {
		t.Errorf("leftover refs: %s", refs)
	}

	// Reconstruct as the guest does: shallow fetch of bases, then bundle.
	guest := filepath.Join(tmp, "guest")
	bundle := filepath.Join(tmp, "src.bundle")
	if err := os.WriteFile(bundle, sn.bundle, 0644); err != nil {
		t.Fatal(err)
	}
	git(t, tmp, "init", "-q", guest)
	git(t, guest, "remote", "add", "origin", "file://"+upstream)
	git(t, guest, append([]string{"fetch", "-q", "--depth=1", "origin"}, sn.bases...)...)
	git(t, guest, "fetch", "-q", bundle, remoteBundleRef)
	git(t, guest, "checkout", "-q", "--detach", sn.snap)
	for name, want := range map[string]string{"a.txt": "modified\n", "b.txt": "local commit\n", "sub/new.txt": "untracked\n"} {
		if got, err := os.ReadFile(filepath.Join(guest, name)); err != nil || string(got) != want {
			t.Errorf("guest %s = %q, %v; want %q", name, got, err, want)
		}
	}
	for _, name := range []string{"gone.txt", "x.ignored", "huge.bin"} {
		if _, err := os.Stat(filepath.Join(guest, name)); err == nil {
			t.Errorf("guest has %s", name)
		}
	}
}

func TestRemoteFlag(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want remoteFlag
	}{
		{nil, remoteFlag{}},
		{[]string{"-remote"}, remoteFlag{on: true}},
		{[]string{"-remote=8"}, remoteFlag{on: true, n: 8}},
		{[]string{"-remote=0"}, remoteFlag{}},
		{[]string{"-remote=1"}, remoteFlag{on: true, n: 1}},
		{[]string{"-remote=false"}, remoteFlag{}},
	} {
		var f remoteFlag
		fs := flag.NewFlagSet("x", flag.ContinueOnError)
		fs.Var(&f, "remote", "")
		if err := fs.Parse(append(tt.args, "./...")); err != nil {
			t.Fatal(err)
		}
		if f != tt.want || fs.Arg(0) != "./..." {
			t.Errorf("%q: got %+v, args %q", tt.args, f, fs.Args())
		}
	}
	var f remoteFlag
	if err := f.Set("lots"); err == nil {
		t.Error("Set(lots) succeeded")
	}
}

func TestResolveRemoteSettings(t *testing.T) {
	defs := &profileDefinitions{path: "/repo/.gotst.yml", root: "/repo/sub",
		remote: &remoteConfig{Command: []string{"./tool/remote", "-x"}, Machines: 6}}
	rs, err := resolveRemoteSettings(defs, "", "", 0)
	if err != nil || !reflect.DeepEqual(rs.command, []string{"/repo/tool/remote", "-x"}) || rs.machines != 6 {
		t.Errorf("config: %+v, %v", rs, err)
	}
	rs, err = resolveRemoteSettings(defs, "", "ssh host", 3)
	if err != nil || !reflect.DeepEqual(rs.command, []string{"ssh", "host"}) || rs.machines != 3 {
		t.Errorf("env: %+v, %v", rs, err)
	}
	rs, err = resolveRemoteSettings(defs, "my-cmd", "ssh host", 0)
	if err != nil || !reflect.DeepEqual(rs.command, []string{"my-cmd"}) {
		t.Errorf("flag: %+v, %v", rs, err)
	}
	rs, err = resolveRemoteSettings(&profileDefinitions{}, "ssh h", "", 0)
	if err != nil || rs.machines != defaultRemoteMachines {
		t.Errorf("default count: %+v, %v", rs, err)
	}
	if _, err := resolveRemoteSettings(&profileDefinitions{}, "", "", 0); err == nil {
		t.Error("no command: no error")
	}
}

// TestRemoteSnapshotStaleOrigin checks that a HEAD whose history is on
// origin, but not in the local remote-tracking refs, is not bundled.
func TestRemoteSnapshotStaleOrigin(t *testing.T) {
	tmp := t.TempDir()
	upstream := filepath.Join(tmp, "upstream")
	laptop := filepath.Join(tmp, "laptop")
	git(t, tmp, "init", "-q", "-b", "main", upstream)
	git(t, upstream, "commit", "-q", "--allow-empty", "-m", "first")
	first := git(t, upstream, "rev-parse", "HEAD")
	for i := range remoteFetchThreshold + 5 {
		git(t, upstream, "commit", "-q", "--allow-empty", "-m", fmt.Sprint(i))
	}
	head := git(t, upstream, "rev-parse", "HEAD")
	git(t, tmp, "clone", "-q", upstream, laptop)
	// Make origin/main stale, as if fetched long ago.
	git(t, laptop, "update-ref", "refs/remotes/origin/main", first)

	sn, err := takeRemoteSnapshot(laptop)
	if err != nil {
		t.Fatal(err)
	}
	if sn.bundle != nil || !reflect.DeepEqual(sn.bases, []string{head}) {
		t.Errorf("snapshot = bases %q, %d-byte bundle; want bases [%s] and no bundle", sn.bases, len(sn.bundle), head)
	}
}

func TestLoadRemoteOnlyConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".gotst.yml")
	if err := os.WriteFile(cfg, []byte("version: 1\nremote:\n  command: [./x]\n  machines: 3\n"), 0644); err != nil {
		t.Fatal(err)
	}
	defs, err := loadProfileDefinitions(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if defs.remote == nil || defs.remote.Machines != 3 || !defs.implicit {
		t.Errorf("defs = %+v", defs)
	}
	if _, err := defs.resolve(""); err != nil {
		t.Error(err)
	}
}

// TestRemoteEndToEnd runs gotst -remote with a remote shell command that
// runs each "machine" locally under its own $HOME. It needs network
// access for tailcat, so it only runs with GOTST_TEST_REMOTE=1.
func TestRemoteEndToEnd(t *testing.T) {
	if os.Getenv("GOTST_TEST_REMOTE") != "1" {
		t.Skip("set GOTST_TEST_REMOTE=1 to run")
	}
	tmp := t.TempDir()
	gotst := filepath.Join(tmp, "gotst")
	goroot := strings.TrimSpace(string(must(exec.Command("go", "env", "GOROOT").Output())))
	gocache := strings.TrimSpace(string(must(exec.Command("go", "env", "GOCACHE").Output())))
	if out, err := exec.Command("go", "build", "-o", gotst, ".").CombinedOutput(); err != nil {
		t.Fatalf("building gotst: %v\n%s", err, out)
	}

	upstream := filepath.Join(tmp, "upstream")
	laptop := filepath.Join(tmp, "laptop")
	write := func(dir, name, data string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, tmp, "init", "-q", upstream)
	write(upstream, "go.mod", "module example.com/proj\n\ngo 1.24\n")
	write(upstream, "a/a_test.go", "package a\nimport \"testing\"\nfunc TestPushed(t *testing.T) {}\n")
	git(t, upstream, "add", ".")
	git(t, upstream, "commit", "-q", "-m", "one")
	git(t, tmp, "clone", "-q", upstream, laptop)
	write(laptop, "a/b_test.go", "package a\nimport \"testing\"\nfunc TestLocalCommit(t *testing.T) {}\n")
	git(t, laptop, "add", ".")
	git(t, laptop, "commit", "-q", "-m", "local")
	write(laptop, "a/c_test.go", "package a\nimport \"testing\"\nfunc TestUntracked(t *testing.T) { t.Fatal(\"untracked fails\") }\n")

	provider := filepath.Join(tmp, "provider")
	write(tmp, "provider", `#!/bin/sh
h="`+tmp+`/machine$GOTST_REMOTE_INDEX"
mkdir -p "$h" && cd "$h" || exit 1
echo "provider: machine $GOTST_REMOTE_INDEX of $GOTST_REMOTE_COUNT" >&2
echo "provider: boot noise" >&2
exec env HOME="$h" PATH="`+goroot+`/bin:$PATH" GOCACHE="`+gocache+`" GOTST_BIN="`+gotst+`" sh -c "$1"
`)
	os.Chmod(provider, 0755)

	cmd := exec.Command(gotst, "-remote=2", "-remote-cmd="+provider, "-max-retries=0", "-history=off", "-listen=127.0.0.1:0", "./...")
	cmd.Dir = filepath.Join(laptop, "a")
	out, err := cmd.CombinedOutput()
	t.Logf("output:\n%s", out)
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("err = %v; want exit status 1", err)
	}
	if strings.Contains(string(out), "boot noise") {
		t.Errorf("output shows the remote shell command's output from before the guest script started")
	}
	for _, want := range []string{
		"gotst-remote[leader]: started on",
		"[m1] gotst-remote[m1]: joining leader",
		"# gotst status: http://127.0.0.1:",
		"FAILED: example.com/proj/a.TestUntracked",
		"3/3 tests",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q", want)
		}
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestMachineOutputHoldsProvisioning(t *testing.T) {
	var mu sync.Mutex
	o := &machineOutput{outMu: &mu, prefix: "[m1] "}
	o.line("Creating linux VM...")
	o.line("Error: no VMs for you")
	if len(o.held) != 2 || o.started {
		t.Fatalf("held = %q", o.held)
	}
	o.line(guestLinePrefix + "m1]: started")
	if len(o.held) != 0 || !o.started {
		t.Fatalf("after start: held = %q", o.held)
	}
	for range maxHeldLines + 10 {
		(&machineOutput{outMu: &mu}).line("x")
	}
	o2 := &machineOutput{outMu: &mu}
	for range maxHeldLines + 10 {
		o2.line("x")
	}
	if len(o2.held) != maxHeldLines {
		t.Errorf("held %d lines; want %d", len(o2.held), maxHeldLines)
	}
}
