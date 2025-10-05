// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

func mustNewCacheDir() string {
	cacheRoot := mustCacheRoot()
	cleanOldCaches(cacheRoot)
	pid := os.Getpid()
	cacheDir := filepath.Join(cacheRoot, fmt.Sprintf("pid%d-t%d", pid, time.Now().UnixNano()))
	if err := os.Mkdir(cacheDir, 0700); err != nil {
		log.Fatalf("creating per-run cache dir %q: %v", cacheDir, err)
	}
	return cacheDir
}

func mustCacheRoot() string {
	ucd, err := os.UserCacheDir()
	if err != nil {
		log.Fatalf("getting user cache dir: %v", err)
	}
	s := filepath.Join(ucd, "gotst")
	if err := os.MkdirAll(s, 0700); err != nil {
		log.Fatalf("creating cache dir %q: %v", s, err)
	}
	return s
}

func cleanOldCaches(cacheRoot string) {
	if cacheRoot == "" {
		return
	}
	ents, err := os.ReadDir(cacheRoot)
	if err != nil {
		log.Fatalf("error reading cache dir %q to clean it: %v", cacheRoot, err)
		return
	}
	dirRx := regexp.MustCompile(`^pid(\d+)-t(\d+)$`)
	for _, ent := range ents {
		name := ent.Name()
		if !ent.IsDir() {
			continue
		}
		m := dirRx.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		pid, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		timestamp, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			continue
		}
		if pidStillrunning(pid) {
			continue
		}
		age := time.Since(time.Unix(0, timestamp)).Round(time.Second)
		log.Printf("removing old cache dir %q (pid %d, age %v)", name, pid, age)
		os.RemoveAll(filepath.Join(cacheRoot, name))
	}
}
