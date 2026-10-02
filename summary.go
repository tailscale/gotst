// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

// fleetSummary describes how a distributed run went: how long each phase
// took and how much of the testing phase each worker spent running tests.
type fleetSummary struct {
	Phases  []phaseDuration
	Workers []workerSummary
}

type phaseDuration struct {
	Phase    runPhase
	Duration time.Duration
}

type workerSummary struct {
	ID, Name, Error                  string
	Slots, Completed, Cached, Failed int
	Joined                           time.Duration // since the run started
	TestTime                         time.Duration

	// Busy is the fraction of the worker's slot time spent running tests
	// while it was connected during the testing phase. It is negative if
	// the worker was never connected during that phase.
	Busy float64
}

func (s *Server) fleetSummaryLocked(end time.Time) fleetSummary {
	var sum fleetSummary
	var testStart, testEnd time.Time
	for i, ps := range s.phaseStarts {
		if ps.phase == phaseDone || ps.phase == phaseFailed {
			break
		}
		next := end
		if i+1 < len(s.phaseStarts) {
			next = s.phaseStarts[i+1].at
		}
		sum.Phases = append(sum.Phases, phaseDuration{ps.phase, next.Sub(ps.at)})
		if ps.phase == phaseTesting {
			testStart, testEnd = ps.at, next
		}
	}
	for _, w := range s.fleet.workers {
		ws := workerSummary{
			ID: w.ID, Name: w.Name, Error: w.Error, Slots: w.Slots,
			Completed: w.Completed, Cached: w.Cached, Failed: w.Failed,
			Joined: w.Joined.Sub(s.start), TestTime: w.Duration, Busy: -1,
		}
		from, to := w.Joined, testEnd
		if from.Before(testStart) {
			from = testStart
		}
		if !w.Left.IsZero() && w.Left.Before(to) {
			to = w.Left
		}
		if !testStart.IsZero() && to.After(from) && w.Slots > 0 {
			ws.Busy = float64(w.Duration) / (float64(to.Sub(from)) * float64(w.Slots))
		}
		sum.Workers = append(sum.Workers, ws)
	}
	return sum
}

// printFleetSummary prints the distributed run summary to stdout and, under
// GitHub Actions, appends it as Markdown to the job summary.
func (s *Server) printFleetSummary() {
	if s.fleet == nil {
		return
	}
	s.mu.Lock()
	sum := s.fleetSummaryLocked(time.Now())
	s.mu.Unlock()

	s.outMu.Lock()
	fmt.Fprintf(os.Stdout, "\nDISTRIBUTED RUN SUMMARY:\n")
	sum.writeText(os.Stdout)
	s.outMu.Unlock()

	if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
		if err == nil {
			sum.writeMarkdown(f)
			err = f.Close()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "gotst: writing GitHub job summary: %v\n", err)
		}
	}
}

func (sum fleetSummary) phasesString() string {
	var parts []string
	for _, p := range sum.Phases {
		parts = append(parts, fmt.Sprintf("%s %v", p.Phase, roundDuration(p.Duration)))
	}
	return strings.Join(parts, ", ")
}

func (sum fleetSummary) writeText(w io.Writer) {
	fmt.Fprintf(w, "  phases: %s\n", sum.phasesString())
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  WORKER\tNAME\tSLOTS\tJOINED\tTESTS\tCACHED\tFAILED\tTEST TIME\tBUSY\tERROR\n")
	for _, ws := range sum.Workers {
		fmt.Fprintf(tw, "  %s\t%s\t%d\t%v\t%d\t%d\t%d\t%v\t%s\t%s\n",
			ws.ID, ws.Name, ws.Slots, roundDuration(ws.Joined), ws.Completed, ws.Cached, ws.Failed,
			roundDuration(ws.TestTime), ws.busyString(), ws.Error)
	}
	tw.Flush()
}

func (sum fleetSummary) writeMarkdown(w io.Writer) {
	fmt.Fprintf(w, "### gotst distributed run\n\n")
	fmt.Fprintf(w, "Phases: %s\n\n", sum.phasesString())
	fmt.Fprintf(w, "| Worker | Name | Slots | Joined | Tests | Cached | Failed | Test time | Busy | Error |\n")
	fmt.Fprintf(w, "|---|---|--:|--:|--:|--:|--:|--:|--:|---|\n")
	for _, ws := range sum.Workers {
		fmt.Fprintf(w, "| %s | %s | %d | %v | %d | %d | %d | %v | %s | %s |\n",
			ws.ID, markdownCell(ws.Name), ws.Slots, roundDuration(ws.Joined), ws.Completed, ws.Cached, ws.Failed,
			roundDuration(ws.TestTime), ws.busyString(), markdownCell(ws.Error))
	}
	fmt.Fprintln(w)
}

func (ws workerSummary) busyString() string {
	if ws.Busy < 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", ws.Busy*100)
}

func roundDuration(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(time.Millisecond)
	}
	return d.Round(time.Second)
}

// markdownCell makes s safe to put in one Markdown table cell.
func markdownCell(s string) string {
	const limit = 200
	if len(s) > limit {
		s = strings.ToValidUTF8(s[:limit], "") + "…"
	}
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.Join(strings.Fields(s), " ")
}
