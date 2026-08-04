# gotst design and development notes

This file is a scratch pad for the design, current implementation status, and
next steps. It is intentionally more candid and less polished than README.md.

## Overall goal

`gotst` should become a test orchestration layer around the standard Go build
and test machinery, usable both interactively and in CI. It is not merely a
different formatter for `go test`.

The central idea is to let the Go command do package loading and compilation,
but separate test-binary construction from test execution. Once test binaries
are explicit artifacts, gotst can schedule them, inspect their tests, retry
failures, shard work, collect statistics, and expose live state without
reimplementing the Go build system.

## Core design

The intended pipeline is:

1. Discover packages with `go list`.
2. Ask `go test` to compile all applicable test binaries up front. If any test
   package cannot compile, abort the run before intentionally executing tests.
3. Use `go test -exec=gotst` to intercept the point where the Go command would
   execute each binary. In this child mode, gotst hashes and captures the
   binary, records its working directory and arguments, and exits successfully.
4. Store binaries by content hash. `-trimpath` helps make those artifacts
   reproducible and permits identical binaries to be deduplicated, including
   eventually between build-tag configurations.
5. Enumerate tests from each captured binary with `-test.list`.
6. Schedule execution with explicit concurrency control, updating shared
   package and test state as events arrive.
7. Use that scheduler and state model for retries, flake measurement,
   distributed sharding, statistics, and the web UI.

The existing state types already sketch the desired model: packages progress
from discovered to built to testing to done, while each test records whether
it is running or complete, its successful duration, and multiple failures with
their duration and output. The HTTP server reads a frozen view of this same
state.

The experiments with `-test.testlogfile` appear to point toward richer cache
semantics based on test inputs (files and environment variables), rather than
only caching package-wide results. `testing.T.Attr` may provide another useful
annotation channel.

## Current implementation status

Implemented:

- Package discovery using `go list -json`.
- Up-front compilation and extraction of structured build failures.
- Self-wrapping via `go test -exec` to capture test binaries.
- Per-run, content-addressed binary storage using SHA-256, hard links when
  possible, and copying as a fallback.
- Concurrent enumeration of names with `-test.list`.
- A basic in-process HTML status page showing packages and build state.
- Initial package/test state and execution-semaphore scaffolding.
- Cleanup of abandoned per-process cache directories.

Not implemented yet:

- Running the captured tests. The current `Server.Run` stops after discovery.
- Correct pass/fail output and process exit status for a test run.
- A scheduler that keeps available CPU busy without exhausting memory.
- Retry policy and flakiness measurement.
- Sharding between machines.
- Multiple build-tag configurations and deduplication across them.
- Persistent artifact/result caching.
- Statistics storage and historical speed/flakiness reporting.
- User-declared cache properties or annotations.
- A useful live progress UI; the current HTML is a debug table.
- Meaningful automated coverage. Existing tests are manual fixtures.

This is therefore an architectural prototype: the binary-capture mechanism
and a plausible state model exist, but most user-visible value begins after
the current endpoint.

## Memory/OOM note

There is a remembered OOM during parallel test discovery. The exact profile or
reproducer is not currently recorded, so the cause needs verification.

The leading hypothesis is multiplicative concurrency: while `go test` is still
compiling packages with its own CPU-scaled `-p` default, `addTestBinary` starts
each newly captured binary to run `-test.list`. That second layer is currently
limited to `2 * runtime.NumCPU()`, which can be extremely high on CI builders.
Large test binaries may have expensive initialization even when only listing
tests. Compilation and listing therefore overlap at two independently scaled
concurrency levels. Unbounded captured output buffers are another possible
memory contributor.

Initial mitigation should put one conservative, configurable bound on build,
discovery, and execution concurrency, and avoid unbounded output retention.
After that, reproduce on a large repository and inspect peak RSS before making
the scheduler more aggressive or adaptive.

The first mitigation is now implemented: build, listing, and execution are
separate phases; `-j` defaults to at most four processes and is passed to the
Go command as `-p`; listing and execution use the same limit; and retained
package/build output is capped. The original OOM has not yet been reproduced or
profiled, so this item is mitigated but not considered closed.

## TODO

Near-term functional path:

- [ ] Reproduce and profile the remembered OOM on a representative large
  repository; verify the new phase separation, conservative `-j` limit, and
  bounded output resolve it; then tune or adapt concurrency if appropriate.
- [x] Conservatively and configurably cap build, discovery, and execution.
- [x] Bound retained build and package failure output.
- [x] Find `go` through `PATH` as well as repository/Tailscale-specific paths.
- [x] Treat external-only test packages (`XTestGoFiles`) as packages with tests.
- [x] Handle zero discovered test packages without waiting forever.
- [x] Preserve each captured binary's original arguments and package working
  directory when executing it.
- [x] Run captured test binaries and return failure when any package fails.
- [ ] Consume structured `test2json` events to update package/test state.
- [x] Keep concurrent package failure output non-interleaved and memory-bounded;
  richer streaming and formatting remain future work.
- [ ] Add cancellation and signal handling; terminate child processes cleanly.
- [ ] Add timeouts for discovery and execution failure modes.

Correctness and hardening:

- [ ] Replace process-wide `log.Fatal` calls in worker goroutines with errors
  propagated to the owning run.
- [ ] Make binary capture race-safe when two packages produce the same hash,
  including on Windows.
- [ ] Preserve executable modes and close/remove temporary files correctly on
  all copy errors.
- [ ] Avoid the default 64 KiB `bufio.Scanner` token limit, or report oversized
  tool output clearly.
- [ ] Bound stderr/build-output collection as well as test-output collection.
- [ ] Decide how benchmarks, examples, fuzz targets, skipped tests, subtests,
  package-level panics, and `TestMain` are represented and scheduled.
- [ ] Decide whether scheduling is package-grained, individual-test-grained,
  or hybrid. Per-test processes improve scheduling/retry precision but change
  package-level semantics and can repeat expensive `TestMain` setup.
- [ ] Pass through the useful `go test` and test-binary flags with a coherent
  CLI, including `-run`, timeout, count, race, cover, and build flags.
- [ ] Make package ordering deterministic.
- [ ] Add unit tests for event parsing, command failures, state transitions,
  cache cleanup, binary capture, and status snapshots.
- [ ] Add integration tests for success, test failure, build failure, no tests,
  external tests, hangs/timeouts, large output, and cancellation.
- [ ] Run race-detector and cross-platform CI, especially Windows.

Scheduler, retries, and distribution:

- [ ] Define a versioned protocol/API for a statistics and history store. Before
  a run, gotst should be able to query estimated durations for the discovered
  tests so it can compute an approximate uncached-work denominator, show useful
  overall progress, schedule longest work first, and balance shards. After each
  run, it should record successful test durations and attempt outcomes needed
  to measure flakes over time. Define test identity, build/configuration keys,
  aggregation/windowing, missing-history behavior, schema evolution, batching,
  and behavior when the store is unavailable.
- [ ] Decide how duration estimates compose into progress. Parallel work means
  summed test CPU-time is not wall-clock time; distinguish total estimated work
  from estimated critical-path/wall time, and update estimates as uncached tests
  finish or previously unseen tests are learned.
- [ ] Record duration observations on passes (including retry passes) and use
  robust historical estimates for longest-first scheduling rather than letting
  flakes, timeouts, or outliers poison estimates.
- [x] Add an initial retry policy: `-max-retries=N` permits N additional
  attempts, fail-then-pass tests are reported as flaky and not cached as clean
  passes, exhausted retries fail the run, and `-failfast` cancels other work
  after retries are exhausted.
- [ ] Further distinguish test failures from timeouts, signals, malformed test
  protocol, and other infrastructure failures; define which are retryable.
- [ ] Add a `gotst -retry` mode that persists the previous run's test outcomes
  and quickly selects only tests that failed on that run, skipping packages and
  tests that passed. Define the scope/identity of “previous run” across profiles,
  build tags, binary changes, working trees, interrupted runs, and multiple
  concurrent gotst invocations. This is failure-set selection, not a cache hit:
  it must work for failed and otherwise non-cacheable tests.
- [ ] Record attempt outcomes and enough context to compute flake rates and
  trends over time, including fail-then-pass retries, without allowing
  output/history to grow without bounds.
- [ ] Design hybrid scheduling for `t.Parallel` and packages with thousands of
  top-level tests. Executing the binary once per top-level test gives precise
  attribution, isolation, retries, and cross-package balancing, but repeats
  package initialization and `TestMain`, loses useful in-process parallelism,
  and can make a 5,000-test package very slow.
- [ ] Experiment with batching top-level tests into a smaller number of
  test-binary invocations, using anchored `-test.run` regular expressions. Use
  history to schedule known-slow or known-flaky tests separately, while packing
  the unknown/fast long tail into batches that retain `t.Parallel` concurrency
  inside each process. Define how batches are sized and rebalanced as timings
  become known.
- [ ] Decide how gotst controls the two concurrency layers: concurrent test
  processes and `-test.parallel` within each process. A single NumCPU-sized
  process semaphore is insufficient if every process may itself run NumCPU
  parallel tests; the scheduler needs a global CPU/resource budget or an
  intentional oversubscription policy.
- [ ] Determine how retries interact with batching and flakes. On a batch
  failure, identify the failing top-level test(s), then rerun those separately;
  known flaky tests may be excluded from normal batches from the outset so
  their attempts and output are independently attributable.
- [ ] Use `-test.list` as the authoritative inventory of top-level tests for
  each binary. If `-test.skip` is useful, reserve it for excluding known tests
  from broad batch invocations rather than for discovery; compare that with
  constructing an explicit `-test.run` expression for each batch.
- [ ] Define a stable work-item/artifact protocol suitable for machine shards.
- [ ] Ensure sharding is deterministic and balanced using duration estimates.
- [ ] Determine how binaries and required runtime files reach remote workers.
- [ ] Integrate a statistics store for duration and flakiness trends.

Build tags and caching:

- [ ] Accept multiple build-tag sets in one logical run.
- [ ] Detect when tag sets produce identical test binaries and run those tests
  only once while attributing the result to each applicable configuration.
- [x] Implement an initial persistent local test-result cache keyed by test
  binary SHA-256, package, top-level test, working directory, and test arguments.
  Successful entries live as inspectable JSON under
  `~/.cache/gotst/test-results/v1`; transient captured binaries remain per-run.
- [x] Implement `-test.testlogfile` collection to learn runtime test inputs such
  as opened files and consulted environment variables. Understand cmd/go's
  testlog format and cache rules, including path normalization and environment
  hashing.
- [x] Implement initial cache invalidation for those learned inputs. Environment
  presence/value hashes, file contents and metadata, directory listings, stat,
  and chdir dependencies are recorded and revalidated before reuse. gotst uses
  full regular-file content hashes rather than cmd/go's mtime/size shortcut.
- [ ] Finish auditing cmd/go's test-result cache semantics and edge cases for
  those learned inputs: a cached result is reusable only when the binary/configuration
  and every recorded file/environment dependency still match. Define behavior
  for inputs that cannot be tracked, tests that escape the supported sandbox or
  module roots, and dependency sets that differ between attempts.
- [ ] Decide whether cache dependency metadata belongs to a whole package
  invocation, a scheduled batch, or an individual top-level test. Per-test
  precision may require isolated executions, while batching naturally produces
  a union of dependencies.
- [ ] Handle packages that define `TestMain` but have no runnable top-level
  tests, and decide how package-level `TestMain` behavior participates in cache
  identity and batching. The current per-test execution repeats `TestMain` for
  every top-level test and does not execute a package containing only
  `TestMain`.
- [ ] Design test annotations for cacheability, isolation, resource needs, and
  other scheduling properties.
- [ ] Verify integration requirements for gomodfs and gocached.
- [ ] Define a versioned cache-service protocol behind the existing internal
  `testResultCache` interface. Support a long-lived child process in the style
  of `GOCACHEPROG`, then a network/database implementation, without exposing
  disk-layout details to the scheduler. Include batched lookup/write,
  cancellation, capability negotiation, cache-miss/error distinctions,
  dependency inspection, authentication, and backpressure.

Web UI and usability:

- [ ] Separate the runner/backend from presentation. The gotst backend should
  own execution and expose a coherent status snapshot plus a stream of state
  changes; the TUI is only one in-process client of that interface. Do not make
  terminal rendering part of scheduler state or require gotst to be a daemon.
  Define stable event identities, ordering, snapshot/resume behavior, and
  backpressure so slow clients cannot block test execution.
- [ ] Add an interactive TUI that presents the whole run as phase-aware
  progress: package patterns expanded (for example `./...`) and total packages
  discovered; distinct package/configuration builds queued and compiled;
  binaries enumerated with `-test.list`; total tests learned and completed;
  currently running tests; retries/flakes/failures; elapsed time and estimated
  remaining work. It should degrade cleanly to stable line-oriented output when
  stdout is not a terminal or in CI.
- [ ] Make TUI totals reflect build-tag deduplication rather than naively
  multiplying packages by tag sets. For example, if profiles request `./...`
  with tag configurations `foo` and `bar`, show how many packages were found,
  how many distinct builds/configurations actually result, and which packages
  are shared because those tags do not affect them. Define denominators while
  discovery and deduplication are still changing the known total.
- [ ] Show build, discovery, queued, running, retrying, passed, flaky, failed,
  and skipped states with live progress.
- [ ] Add per-test attempt history, durations, and bounded failure output.
- [ ] Add automatic updates (SSE, WebSocket, or polling) and useful CI links.
- [ ] Preserve the optional web status mode as another client of the same
  backend interface. Serve an initial status snapshot and live updates over a
  WebSocket (with reconnect/resynchronization behavior), rather than coupling
  the HTTP handlers directly to mutable scheduler internals. The web listener
  should remain optional and should not imply a persistent daemon.
- [ ] Shut down the HTTP listener cleanly and define whether it remains alive
  after a run for interactive inspection.
- [ ] Expand README.md with installation, examples, supported behavior, and an
  explicit experimental-status warning.

Profiles and configuration:

- [x] Define and implement a versioned repository `.gotst.yml` format containing named
  test profiles, so `gotst PROFILE` selects a reusable set of package patterns,
  build-tag configurations, test filters, timeouts, concurrency/resource
  policy, retry policy, and other build/test options. The initial schema
  supports package patterns, exclusions, one build-tag set, short mode,
  timeout, arbitrary test flags, and recursive includes with strict validation.
- [x] Define initial profile composition: included profiles merge in order,
  list values append with deduplication, and the including profile overrides
  explicitly set scalar values. With no positional argument, `default` is used.
- [ ] Add profile fields for concurrency/resource policy, retry policy, and
  multiple distinct build-tag configurations in one logical run.
- [ ] Add commands to list profiles and explain the fully resolved profile.
- [ ] Define broader CLI precedence and an unambiguous ad-hoc package-pattern
  mode; currently positional arguments always name profiles, while `-tags`
  appends tags to the selected profile.

## Open design questions

- What is the smallest scheduling unit that preserves normal Go test semantics
  closely enough? Running a whole package once is compatible and cheap;
  executing top-level tests separately enables precise retries and balancing
  but repeats package initialization and `TestMain`.
- Should compilation and test execution overlap once early build-failure
  detection is a stated goal? Waiting for every build gives a clean phase
  boundary; pipelining reduces latency but may run tests before a later package
  fails to compile.
- Should the concurrency limit represent processes, estimated memory, weighted
  resources, or separate build/test pools? A process count is the safest first
  implementation but likely not the final scheduler.
- Is the content-addressed binary alone sufficient as a runnable artifact, or
  must a work item also describe source-relative files, environment, platform,
  and sandbox requirements?
- Which result should CI report when a test fails and then passes on retry?
  That policy should be explicit and independent of retaining attempt data.
