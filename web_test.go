// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"testing"

	"tailscale.com/ipn/ipnstate"
)

type fakeTailscaleStatusClient struct {
	status *ipnstate.Status
	err    error
}

func (c fakeTailscaleStatusClient) StatusWithoutPeers(context.Context) (*ipnstate.Status, error) {
	return c.status, c.err
}

func TestStartStatusListenersTailscale(t *testing.T) {
	st := &ipnstate.Status{
		BackendState: "Running",
		TailscaleIPs: []netip.Addr{
			netip.MustParseAddr("127.0.0.2"),
			netip.MustParseAddr("127.0.0.2"), // duplicates do not create duplicate listeners
		},
		Self: &ipnstate.PeerStatus{DNSName: "host.example.ts.net."},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "status page")
	})
	listeners, statusURL, err := startStatusListeners(t.Context(), "127.0.0.1:0", handler, fakeTailscaleStatusClient{status: st})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeListeners(listeners) })
	if len(listeners) != 2 {
		t.Fatalf("got %d listeners; want configured plus Tailscale listener", len(listeners))
	}
	port := listeners[0].Addr().(*net.TCPAddr).Port
	if want := fmt.Sprintf("http://host.example.ts.net:%d", port); statusURL != want {
		t.Fatalf("status URL = %q; want %q", statusURL, want)
	}
	for _, ln := range listeners {
		res, err := http.Get("http://" + ln.Addr().String())
		if err != nil {
			t.Fatalf("GET from %v: %v", ln.Addr(), err)
		}
		body, readErr := io.ReadAll(res.Body)
		res.Body.Close()
		if readErr != nil || string(body) != "status page" {
			t.Fatalf("GET from %v = %q, %v", ln.Addr(), body, readErr)
		}
	}
}

func TestStartStatusListenersWithoutTailscale(t *testing.T) {
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for _, tc := range []fakeTailscaleStatusClient{
		{err: errors.New("tailscaled unavailable")},
		{status: &ipnstate.Status{BackendState: "Stopped"}},
	} {
		listeners, statusURL, err := startStatusListeners(t.Context(), "127.0.0.1:0", handler, tc)
		if err != nil {
			t.Fatal(err)
		}
		closeListeners(listeners)
		if len(listeners) != 1 || statusURL != "" {
			t.Fatalf("listeners=%d, statusURL=%q; want configured listener only", len(listeners), statusURL)
		}
	}
}

func TestListenerCoversIP(t *testing.T) {
	for _, tt := range []struct {
		addr string
		ip   string
		want bool
	}{
		{"0.0.0.0:5525", "100.64.0.1", true},
		{"0.0.0.0:5525", "fd7a:115c:a1e0::1", false},
		{"[::]:5525", "fd7a:115c:a1e0::1", true},
		{"[::]:5525", "100.64.0.1", false},
		{"127.0.0.1:5525", "127.0.0.1", true},
		{"127.0.0.1:5525", "100.64.0.1", false},
	} {
		tcpAddr, err := net.ResolveTCPAddr("tcp", tt.addr)
		if err != nil {
			t.Fatal(err)
		}
		if got := listenerCoversIP(tcpAddr, netip.MustParseAddr(tt.ip)); got != tt.want {
			t.Errorf("listenerCoversIP(%q, %q) = %v; want %v", tt.addr, tt.ip, got, tt.want)
		}
	}
}
