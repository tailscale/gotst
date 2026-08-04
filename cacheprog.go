// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

// This file implements a GOCACHEPROG shim that fills in a missing piece of
// cmd/go's external-cache support: cmd/go looks up linker outputs in an
// external cache, but does not put newly linked executables there. The shim
// forwards cmd/go's normal cache traffic and lets gotst's -exec wrappers add
// the omitted put while cmd/go's exact linker action ID is still known.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	cacheShimSocketEnv = "GOTST_CACHE_SHIM_SOCKET"
	cacheShimArg       = "-gotst-cache-shim"
	localCacheProgArg  = "-gotst-local-cache-prog"
	cacheConnCmdGo     = byte('C')
	cacheConnRegister  = byte('R')
)

type cacheProgCommand string

const (
	cacheProgGet   cacheProgCommand = "get"
	cacheProgPut   cacheProgCommand = "put"
	cacheProgClose cacheProgCommand = "close"
)

// cacheProgRequest and cacheProgResponse intentionally mirror cmd/go's
// public GOCACHEPROG JSON protocol. Body is encoded as a separate JSON value.
type cacheProgRequest struct {
	ID       int64
	Command  cacheProgCommand
	ActionID []byte `json:",omitempty"`
	OutputID []byte `json:",omitempty"`
	ObjectID []byte `json:",omitempty"` // compatibility with older helpers
	BodySize int64  `json:",omitempty"`

	body io.Reader
}

type cacheProgResponse struct {
	ID            int64
	Err           string             `json:",omitempty"`
	KnownCommands []cacheProgCommand `json:",omitempty"`
	Miss          bool               `json:",omitempty"`
	OutputID      []byte             `json:",omitempty"`
	ObjectID      []byte             `json:",omitempty"`
	Size          int64              `json:",omitempty"`
	Time          *time.Time         `json:",omitempty"`
	TimeNanos     int64              `json:",omitempty"` // older helper compatibility
	DiskPath      string             `json:",omitempty"`
}

type cacheProgClient struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	bw    *bufio.Writer
	enc   *json.Encoder
	can   map[cacheProgCommand]bool
	done  chan struct{}

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan *cacheProgResponse
	readErr error
}

func startCacheProgClient(command string, extraEnv ...string) (*cacheProgClient, error) {
	args, err := splitQuoted(command)
	if err != nil {
		return nil, fmt.Errorf("parsing GOCACHEPROG: %w", err)
	}
	if len(args) == 0 {
		return nil, errors.New("empty GOCACHEPROG")
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(envWithout(os.Environ(), "GOCACHEPROG", cacheShimSocketEnv,
		localCacheDirEnv, stockGoCacheDirEnv), extraEnv...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(stdout)
	var hello cacheProgResponse
	if err := dec.Decode(&hello); err != nil {
		_ = stdin.Close()
		_ = cmd.Wait()
		return nil, fmt.Errorf("reading GOCACHEPROG capabilities: %w", err)
	}
	if len(hello.KnownCommands) == 0 {
		_ = stdin.Close()
		_ = cmd.Wait()
		return nil, errors.New("GOCACHEPROG declared no supported commands")
	}
	c := &cacheProgClient{
		cmd: cmd, stdin: stdin, bw: bufio.NewWriter(stdin),
		can: make(map[cacheProgCommand]bool), done: make(chan struct{}),
		pending: make(map[int64]chan *cacheProgResponse),
	}
	c.enc = json.NewEncoder(c.bw)
	for _, command := range hello.KnownCommands {
		c.can[command] = true
	}
	go c.readLoop(dec)
	return c, nil
}

func (c *cacheProgClient) readLoop(dec *json.Decoder) {
	defer close(c.done)
	for {
		var res cacheProgResponse
		err := dec.Decode(&res)
		c.mu.Lock()
		if err != nil {
			c.readErr = err
			for id, ch := range c.pending {
				delete(c.pending, id)
				close(ch)
			}
			c.mu.Unlock()
			return
		}
		ch := c.pending[res.ID]
		delete(c.pending, res.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- &res
		}
	}
}

func (c *cacheProgClient) send(ctx context.Context, req cacheProgRequest) (*cacheProgResponse, error) {
	if !c.can[req.Command] {
		return nil, fmt.Errorf("GOCACHEPROG does not support %q", req.Command)
	}
	ch := make(chan *cacheProgResponse, 1)
	c.mu.Lock()
	if c.readErr != nil {
		err := c.readErr
		c.mu.Unlock()
		return nil, err
	}
	c.nextID++
	req.ID = c.nextID
	c.pending[req.ID] = ch
	c.mu.Unlock()

	c.writeMu.Lock()
	err := c.enc.Encode(&req)
	if err == nil && req.BodySize > 0 {
		if req.body == nil {
			err = errors.New("cache put has size but no body")
		} else {
			var wrote int64
			e := base64.NewEncoder(base64.StdEncoding, c.bw)
			if _, werr := c.bw.WriteString("\""); werr != nil {
				err = werr
			} else if wrote, err = io.Copy(e, req.body); err == nil {
				err = e.Close()
			}
			if err == nil && wrote != req.BodySize {
				err = fmt.Errorf("cache put copied %d bytes; want %d", wrote, req.BodySize)
			}
			if err == nil {
				_, err = c.bw.WriteString("\"\n")
			}
		}
	}
	if err == nil {
		err = c.bw.Flush()
	}
	c.writeMu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, req.ID)
		c.mu.Unlock()
		return nil, err
	}
	select {
	case res, ok := <-ch:
		if !ok {
			c.mu.Lock()
			err := c.readErr
			c.mu.Unlock()
			return nil, err
		}
		if res.Err != "" {
			return nil, errors.New(res.Err)
		}
		return res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *cacheProgClient) close() error {
	var retErr error
	if c.can[cacheProgClose] {
		_, retErr = c.send(context.Background(), cacheProgRequest{Command: cacheProgClose})
	}
	_ = c.stdin.Close()
	<-c.done
	if err := c.cmd.Wait(); retErr == nil && err != nil {
		retErr = err
	}
	return retErr
}

type executableRegistration struct {
	Path       string
	OutputHash string
	Size       int64
}

type cacheShim struct {
	downstream        *cacheProgClient
	execDir           string
	directExecutables bool // downstream paths are gotst-owned and executable
	listener          net.Listener
	socket            string
	endpoint          string
	socketDir         string
	acceptDone        chan struct{}
	connWG            sync.WaitGroup
	execHits          atomic.Int64
	execPuts          atomic.Int64

	mu       sync.Mutex
	misses   map[string][]byte // 120-bit build-ID prefix -> full action ID
	uploaded map[string]bool
}

// runCacheShim runs in the child process started by cmd/go. The gotst parent
// owns the actual cache helper; this process only bridges cmd/go's stdin and
// stdout to it. Keeping ownership in the parent matters because cmd/go closes
// its cache helper before all -exec wrappers have necessarily exited.
func runCacheShim() error {
	socket := os.Getenv(cacheShimSocketEnv)
	if socket == "" {
		return errors.New("missing cache shim socket")
	}
	conn, err := dialCacheEndpoint(socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{cacheConnCmdGo}); err != nil {
		return err
	}
	go func() {
		_, _ = io.Copy(conn, os.Stdin)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	_, err = io.Copy(os.Stdout, conn)
	return err
}

func startCacheShim(command, socket string, helperEnv ...string) (*cacheShim, error) {
	downstream, err := startCacheProgClient(command, helperEnv...)
	if err != nil {
		return nil, err
	}
	_ = os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		_ = downstream.close()
		return nil, err
	}
	execDir := filepath.Join(filepath.Dir(socket), "cache-executables")
	if err := os.MkdirAll(execDir, 0700); err != nil {
		_ = ln.Close()
		_ = downstream.close()
		return nil, err
	}
	shim := &cacheShim{
		downstream: downstream, execDir: execDir, listener: ln, socket: socket, endpoint: socket,
		misses: make(map[string][]byte), uploaded: make(map[string]bool),
	}
	shim.acceptDone = make(chan struct{})
	go shim.acceptLoop()
	return shim, nil
}

func startRunCacheShim(command string, helperEnv ...string) (*cacheShim, error) {
	if runtime.GOOS == "windows" {
		downstream, err := startCacheProgClient(command, helperEnv...)
		if err != nil {
			return nil, err
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			_ = downstream.close()
			return nil, err
		}
		execDir, err := os.MkdirTemp("", "gotst-cache-executables-")
		if err != nil {
			_ = ln.Close()
			_ = downstream.close()
			return nil, err
		}
		shim := &cacheShim{
			downstream: downstream, execDir: execDir, listener: ln,
			endpoint: "tcp:" + ln.Addr().String(), socketDir: execDir,
			misses: make(map[string][]byte), uploaded: make(map[string]bool),
		}
		shim.acceptDone = make(chan struct{})
		go shim.acceptLoop()
		return shim, nil
	}
	dir, err := os.MkdirTemp("", "gotst-cache-socket-")
	if err != nil {
		return nil, err
	}
	socket := filepath.Join(dir, "cache.sock")
	shim, err := startCacheShim(command, socket, helperEnv...)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	shim.socketDir = dir
	return shim, nil
}

func dialCacheEndpoint(endpoint string) (net.Conn, error) {
	if address, ok := strings.CutPrefix(endpoint, "tcp:"); ok {
		return net.Dial("tcp", address)
	}
	return net.Dial("unix", endpoint)
}

func (s *cacheShim) serveCmdGo(r io.Reader, w io.Writer) error {
	dec := json.NewDecoder(bufio.NewReader(r))
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	commands := []cacheProgCommand{cacheProgGet}
	if s.downstream.can[cacheProgPut] {
		commands = append(commands, cacheProgPut)
	}
	commands = append(commands, cacheProgClose)
	if err := enc.Encode(&cacheProgResponse{KnownCommands: commands}); err != nil {
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
		if req.Command == cacheProgPut && req.BodySize > 0 {
			var body []byte
			if err := dec.Decode(&body); err != nil {
				return err
			}
			if int64(len(body)) != req.BodySize {
				return fmt.Errorf("cache put body is %d bytes; want %d", len(body), req.BodySize)
			}
			req.body = bytes.NewReader(body)
		}
		if req.Command == cacheProgClose {
			requests.Wait()
			res := &cacheProgResponse{ID: req.ID}
			writeResponse(res)
			return nil
		}
		requests.Add(1)
		go func(req cacheProgRequest) {
			defer requests.Done()
			res, err := s.forward(context.Background(), req)
			if err != nil {
				res = &cacheProgResponse{Err: err.Error()}
			}
			res.ID = req.ID
			writeResponse(res)
		}(req)
	}
}

func (s *cacheShim) forward(ctx context.Context, req cacheProgRequest) (*cacheProgResponse, error) {
	var res *cacheProgResponse
	if req.Command == cacheProgGet && !s.downstream.can[cacheProgGet] {
		// Advertising get lets us observe cmd/go's exact link action ID even
		// when the configured downstream helper is write-only.
		res = &cacheProgResponse{Miss: true}
	} else {
		var err error
		res, err = s.downstream.send(ctx, req)
		if err != nil {
			return nil, err
		}
	}
	if req.Command != cacheProgGet {
		return res, nil
	}
	if len(res.OutputID) == 0 && len(res.ObjectID) != 0 {
		res.OutputID = res.ObjectID
	}
	if res.Miss {
		s.recordMiss(req.ActionID)
		return res, nil
	}
	if looksLikeExecutable(res.DiskPath) {
		path, ok := s.materializeExecutable(req.ActionID, res)
		if !ok {
			// Never execute an executable-looking cache object unless both its
			// content hash and embedded action ID match the response.
			s.recordMiss(req.ActionID)
			return &cacheProgResponse{Miss: true}, nil
		}
		res.DiskPath = path
		s.execHits.Add(1)
	}
	return res, nil
}

func (s *cacheShim) recordMiss(actionID []byte) {
	prefix := actionIDPrefix(actionID)
	if prefix == "" {
		return
	}
	s.mu.Lock()
	s.misses[prefix] = append([]byte(nil), actionID...)
	s.mu.Unlock()
}

func (s *cacheShim) materializeExecutable(actionID []byte, res *cacheProgResponse) (string, bool) {
	if res.DiskPath == "" || !looksLikeExecutable(res.DiskPath) {
		return "", false
	}
	buildID, err := readGoBuildID(res.DiskPath)
	parts := strings.Split(buildID, "/")
	if err != nil || len(parts) != 4 || parts[0] != actionIDPrefix(actionID) {
		return "", false
	}
	gotHash, _, err := hashFile(res.DiskPath)
	if err != nil || !bytes.Equal(gotHash, res.OutputID) {
		return "", false
	}
	if s.directExecutables {
		if runtime.GOOS == "windows" {
			return res.DiskPath, true
		}
		if fi, err := os.Stat(res.DiskPath); err == nil && fi.Mode().IsRegular() && fi.Mode()&0100 != 0 {
			return res.DiskPath, true
		}
	}
	target := filepath.Join(s.execDir, hex.EncodeToString(res.OutputID))
	if fi, err := os.Stat(target); err == nil && fi.Mode().IsRegular() {
		hash, _, hashErr := hashFile(target)
		if hashErr == nil && bytes.Equal(hash, res.OutputID) && fi.Mode()&0100 != 0 {
			return target, true
		}
	}
	if err := copyExecutable(res.DiskPath, target); err != nil {
		return "", false
	}
	return target, true
}

func (s *cacheShim) acceptLoop() {
	defer close(s.acceptDone)
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.connWG.Add(1)
		go func() {
			defer s.connWG.Done()
			defer conn.Close()
			var kind [1]byte
			if _, err := io.ReadFull(conn, kind[:]); err != nil {
				return
			}
			if kind[0] == cacheConnCmdGo {
				_ = s.serveCmdGo(conn, conn)
				return
			}
			if kind[0] != cacheConnRegister {
				return
			}
			var reg executableRegistration
			res := struct {
				Err string `json:",omitempty"`
			}{}
			if err := json.NewDecoder(conn).Decode(&reg); err == nil {
				err = s.registerExecutable(reg)
			}
			if err != nil {
				res.Err = err.Error()
			}
			_ = json.NewEncoder(conn).Encode(&res)
		}()
	}
}

func (s *cacheShim) close() error {
	_ = s.listener.Close()
	<-s.acceptDone
	s.connWG.Wait()
	if s.socket != "" {
		_ = os.Remove(s.socket)
	}
	if s.socketDir != "" {
		_ = os.RemoveAll(s.socketDir)
	}
	return s.downstream.close()
}

func (s *cacheShim) registerExecutable(reg executableRegistration) error {
	buildID, err := readGoBuildID(reg.Path)
	if err != nil {
		return err
	}
	parts := strings.Split(buildID, "/")
	if len(parts) != 4 {
		return fmt.Errorf("test executable has unexpected Go build ID %q", buildID)
	}
	s.mu.Lock()
	actionID := append([]byte(nil), s.misses[parts[0]]...)
	s.mu.Unlock()
	if len(actionID) == 0 || !s.downstream.can[cacheProgPut] {
		return nil
	}
	outputID, err := hex.DecodeString(reg.OutputHash)
	if err != nil || len(outputID) != sha256.Size {
		return fmt.Errorf("invalid executable hash %q", reg.OutputHash)
	}
	gotOutputID, gotSize, err := hashFile(reg.Path)
	if err != nil {
		return err
	}
	if !bytes.Equal(gotOutputID, outputID) || gotSize != reg.Size {
		return fmt.Errorf("captured executable changed before cache upload")
	}
	s.mu.Lock()
	if s.uploaded[parts[0]] {
		s.mu.Unlock()
		return nil
	}
	s.uploaded[parts[0]] = true
	s.mu.Unlock()
	uploaded := false
	defer func() {
		if !uploaded {
			s.mu.Lock()
			delete(s.uploaded, parts[0])
			s.mu.Unlock()
		}
	}()
	f, err := os.Open(reg.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	res, err := s.downstream.send(context.Background(), cacheProgRequest{
		Command: cacheProgPut, ActionID: actionID, OutputID: outputID,
		BodySize: reg.Size, body: f,
	})
	if err != nil {
		return err
	}
	if res.DiskPath == "" {
		return errors.New("GOCACHEPROG executable put returned no disk path")
	}
	if s.directExecutables && runtime.GOOS != "windows" {
		if err := os.Chmod(res.DiskPath, 0700); err != nil {
			return fmt.Errorf("marking cached executable: %w", err)
		}
	}
	uploaded = true
	s.execPuts.Add(1)
	return nil
}

func registerTestExecutable(socket, path, outputHash string, size int64) error {
	conn, err := dialCacheEndpoint(socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{cacheConnRegister}); err != nil {
		return err
	}
	if err := json.NewEncoder(conn).Encode(executableRegistration{
		Path: path, OutputHash: outputHash, Size: size,
	}); err != nil {
		return err
	}
	var res struct {
		Err string `json:",omitempty"`
	}
	if err := json.NewDecoder(conn).Decode(&res); err != nil {
		return err
	}
	if res.Err != "" {
		return errors.New(res.Err)
	}
	return nil
}

func actionIDPrefix(actionID []byte) string {
	if len(actionID) < 15 {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(actionID[:15])
}

func readGoBuildID(path string) (string, error) {
	out, err := exec.Command(goCmd(), "tool", "buildid", path).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func hashFile(path string) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return h.Sum(nil), n, err
}

func looksLikeExecutable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var b [4]byte
	if _, err := io.ReadFull(f, b[:]); err != nil {
		return false
	}
	return bytes.Equal(b[:], []byte("\x7fELF")) || // ELF
		bytes.Equal(b[:2], []byte("MZ")) || // PE
		bytes.Equal(b[:], []byte("\x00asm")) || // WebAssembly
		bytes.Equal(b[:], []byte{0xfe, 0xed, 0xfa, 0xce}) || // Mach-O
		bytes.Equal(b[:], []byte{0xfe, 0xed, 0xfa, 0xcf}) ||
		bytes.Equal(b[:], []byte{0xce, 0xfa, 0xed, 0xfe}) ||
		bytes.Equal(b[:], []byte{0xcf, 0xfa, 0xed, 0xfe})
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(dst), ".gotst-executable-")
	if err != nil {
		return err
	}
	tmp := out.Name()
	ok := false
	defer func() {
		out.Close()
		if !ok {
			os.Remove(tmp)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Chmod(0700); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		if _, statErr := os.Stat(dst); statErr == nil {
			ok = true
			return nil
		}
		return err
	}
	ok = true
	return nil
}

func envWithout(env []string, names ...string) []string {
	drop := make(map[string]bool, len(names))
	for _, name := range names {
		drop[name] = true
	}
	ret := env[:0:0]
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !drop[name] {
			ret = append(ret, kv)
		}
	}
	return ret
}

// splitQuoted matches the deliberately small quoting language accepted by
// cmd/go for GOCACHEPROG: whitespace-separated words with optional whole-word
// single or double quotes and no escaping.
func splitQuoted(s string) ([]string, error) {
	var ret []string
	for len(s) > 0 {
		s = strings.TrimLeft(s, " \t\r\n")
		if s == "" {
			break
		}
		if s[0] == '\'' || s[0] == '"' {
			quote := s[0]
			s = s[1:]
			i := strings.IndexByte(s, quote)
			if i < 0 {
				return nil, fmt.Errorf("unterminated %c string", quote)
			}
			ret = append(ret, s[:i])
			s = s[i+1:]
			continue
		}
		i := strings.IndexAny(s, " \t\r\n")
		if i < 0 {
			return append(ret, s), nil
		}
		ret = append(ret, s[:i])
		s = s[i:]
	}
	return ret, nil
}

func quoteCacheProgArg(s string) string {
	if !strings.ContainsAny(s, " \t\r\n'\"") {
		return s
	}
	if !strings.Contains(s, "'") {
		return "'" + s + "'"
	}
	if !strings.Contains(s, "\"") {
		return "\"" + s + "\""
	}
	// cmd/go's GOCACHEPROG grammar cannot represent a word containing both
	// quote characters. os.Executable paths like that are exceedingly unusual;
	// returning the raw path produces a clear startup error rather than silently
	// running another program.
	return s
}
