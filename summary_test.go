// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestFleetSummary(t *testing.T) {
	t0 := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	at := func(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }
	s := &Server{
		start: t0,
		phaseStarts: []phaseStart{
			{phaseDiscovering, at(0)},
			{phaseBuilding, at(5)},
			{phaseListing, at(65)},
			{phaseTesting, at(70)},
			{phaseDone, at(170)},
		},
	}
	s.fleet = &fleet{s: s, workers: []*fleetWorker{
		// Joined during the build; connected for the whole 100s testing phase.
		{ID: "h1", Name: "a|b", Slots: 4, Joined: at(20), Completed: 30, Duration: 200 * time.Second, Left: at(171)},
		{ID: "leader", Name: "local", Slots: 2, Joined: at(70), Completed: 50, Duration: 150 * time.Second},
		// Connected for only the last 50s of testing.
		{ID: "h2", Name: "late", Slots: 1, Joined: at(120), Completed: 5, Duration: 25 * time.Second},
		// Left before testing started.
		{ID: "h3", Name: "broken", Slots: 4, Joined: at(10), Left: at(30), Error: "binary mismatch"},
	}}
	sum := s.fleetSummaryLocked(at(200))

	wantPhases := "discovering 5s, building 1m0s, listing 5s, testing 1m40s"
	if got := sum.phasesString(); got != wantPhases {
		t.Errorf("phases = %q; want %q", got, wantPhases)
	}
	wantBusy := map[string]float64{"h1": 0.5, "leader": 0.75, "h2": 0.5, "h3": -1}
	for _, ws := range sum.Workers {
		if math.Abs(ws.Busy-wantBusy[ws.ID]) > 1e-9 {
			t.Errorf("%s busy = %v; want %v", ws.ID, ws.Busy, wantBusy[ws.ID])
		}
	}
	if got, want := sum.Workers[0].Joined, 20*time.Second; got != want {
		t.Errorf("h1 joined = %v; want %v", got, want)
	}

	var md strings.Builder
	sum.writeMarkdown(&md)
	for _, want := range []string{
		"| h1 | a\\|b | 4 | 20s | 30 | 0 | 0 | 3m20s | 50% |  |\n",
		"| h3 | broken | 4 | 10s | 0 | 0 | 0 | 0s | - | binary mismatch |\n",
	} {
		if !strings.Contains(md.String(), want) {
			t.Errorf("markdown missing %q; got:\n%s", want, md.String())
		}
	}
	var text strings.Builder
	sum.writeText(&text)
	if !strings.Contains(text.String(), "phases: "+wantPhases) {
		t.Errorf("text summary missing phases; got:\n%s", text.String())
	}
}
