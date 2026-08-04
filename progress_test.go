// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"strings"
	"testing"
	"time"
)

func TestProgressLine(t *testing.T) {
	for _, tt := range []struct {
		name string
		p    progressSnapshot
		want []string
	}{
		{
			name: "discovering",
			p:    progressSnapshot{Phase: phaseDiscovering, PackagesDiscovered: 123, PackagesTotal: 45, Elapsed: 2 * time.Second},
			want: []string{"discovering", "123 pkgs matched", "45 with tests", "2s"},
		},
		{
			name: "building",
			p: progressSnapshot{
				Phase: phaseBuilding, PackagesDiscovered: 200, PackagesBuilt: 30,
				PackagesCached: 12, PackagesTotal: 80,
			},
			want: []string{"building", "80/200 test pkgs", "30/80 built", "12 cached, 40.0%"},
		},
		{
			name: "listing",
			p:    progressSnapshot{Phase: phaseListing, PackagesDiscovered: 200, PackagesListed: 40, PackagesTotal: 80, TestsTotal: 456},
			want: []string{"listing", "200 pkgs matched", "80 with tests", "40/80 test pkgs listed", "456 tests found"},
		},
		{
			name: "testing partial cache",
			p: progressSnapshot{
				Phase: phaseTesting, PackagesDone: 12, PackagesTotal: 80,
				TestsDone: 345, TestsTotal: 1000, TestsRunning: 4,
				TestsFlaky: 2, CacheEnabled: true, CacheChecks: 400, CacheHits: 300,
			},
			want: []string{"testing", "12/80 test pkgs", "345/1000 tests", "4 running", "2 flaky", "cache hits 300/400 (75.0%)"},
		},
		{
			name: "done cache off",
			p:    progressSnapshot{Phase: phaseDone, PackagesDone: 2, PackagesTotal: 2, TestsDone: 10, TestsTotal: 10},
			want: []string{"done", "2/2 test pkgs", "10/10 tests", "0 running", "cache off"},
		},
		{
			name: "build only done",
			p: progressSnapshot{
				Phase: phaseDone, BuildOnly: true, PackagesDiscovered: 300,
				PackagesTotal: 282, PackagesBuilt: 282, PackagesCached: 282, CacheEnabled: true,
			},
			want: []string{"done", "282/300 test pkgs", "282/282 built", "282 cached, 100.0%"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.p.line()
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("line %q does not contain %q", got, want)
				}
			}
		})
	}
}
