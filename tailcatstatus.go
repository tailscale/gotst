// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"log"
	"net"
	"net/http"
	"sync"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/filter"
)

// tailcatListenValue is the -listen value that serves the status page
// over an ephemeral tailcat server instead of a local TCP listener.
const tailcatListenValue = "tailcat"

// startTailcatStatus starts a tailcat server serving the status page
// handler over HTTP on port 80, so "tailcat browse ADDR" can view it.
// The returned func shuts the server down.
func startTailcatStatus(handler http.Handler) (tailcat.Addr, func(), error) {
	logf := logger.Discard
	if *verbose {
		logf = log.Printf
	}
	ln := &connListener{
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
	srv := &tailcat.Server{
		Logf: logf,
		OnTCP: func(port uint16) func(net.Conn) {
			if port != 80 {
				return nil
			}
			return ln.deliver
		},
		ServedTCPPorts: []filter.PortRange{{First: 80, Last: 80}},
	}
	if err := srv.Start(); err != nil {
		return "", nil, err
	}
	go serveStatus(ln, handler)
	cleanup := func() {
		ln.Close()
		srv.Close()
	}
	return srv.TailcatAddr(), cleanup, nil
}

// connListener adapts tailcat's per-connection callback into a
// net.Listener for http.Serve.
type connListener struct {
	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
}

// deliver hands an accepted tailcat connection to the listener,
// closing it if the listener has shut down.
func (l *connListener) deliver(c net.Conn) {
	select {
	case l.conns <- c:
	case <-l.closed:
		c.Close()
	}
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *connListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *connListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4zero, Port: 80}
}
