# gotst

`gotst` is a wrapper around `go test` designed for both humans and CI.

This README covers usage and behavior. See [DESIGN.md](DESIGN.md) for the
execution architecture, subprocess protocols, scheduler, and cache internals.

Running `gotst` selects the `default` profile from the nearest `.gotst.yml` in
the current directory or one of its parents. Running `gotst NAME` selects a
named profile. If no configuration exists, the implicit `default` profile runs
`./...` from the current directory.

Package and test arguments can further select what to run:

```
gotst ./foo/...                    # packages, using the default profile
gotst TestFoo TestBar              # named tests in the default package set
gotst tailscale.io/foo/bar.TestBaz # one fully qualified test
gotst ./foo/bar.TestBaz            # the same with a relative package
gotst full TestFreeBSDSubnetRouter # a named profile and test subset
```

An exact profile name in the first position selects that profile. Other
non-test arguments are package patterns and replace the profile's configured
package set. Test names begin with `Test`, `Fuzz`, or `Example`. For
unqualified names, gotst scans test source files to avoid compiling
unrelated test binaries, then always uses each compiled binary's `-test.list`
output as the authoritative check after build-tag evaluation. The command
fails if any requested test is not found.

Use `-build-only` to discover packages and build/link the selected test
binaries without listing or running their tests. This is useful for warming the
Go build cache or an external `GOCACHEPROG` cache. Because test binaries are not
listed in this mode, requested test names are only source-level build-pruning
hints and are not authoritatively checked for existence.

Example configuration:

```yaml
version: 1
profiles:
  all:
    packages: [./...]
    short: true

  default:
    include: [all]
    tags: [integration]
    timeout: 10m
    exclude_packages:
      - ./some/slow/package

  vm:
    packages: [./integration/vmtests]
    timeout: 60m
    test_flags: [-run-vm-tests]
```

Included profiles are merged in order. List fields are appended and
deduplicated; `short` and `timeout` are inherited and can be overridden by the
including profile. `tags` is one build-tag set passed to both package discovery
and compilation. `test_flags` are passed to captured test binaries; common
`-testing.*` spellings are normalized to the test binary's `-test.*` flags.
`-config=PATH` selects an explicit configuration file.

## Test result caching

gotst disables cmd/go's package test-result cache with `go test -count=1` and
maintains its own per-top-level-test cache. Successful results are stored below
`~/.cache/gotst/test-results/vN` by test-binary SHA-256 and test name. Use
`-cache=false` to bypass result caching or `-cache-dir=PATH` to select a
different cache root.

Cache entries include dependencies observed by Go's test-log support. A result
is reused only while those environment and filesystem inputs still match.
Unreported inputs such as network services, time, and randomness cannot
invalidate an entry automatically.

The current scheduler executes each top-level test in its own process. This
makes cache entries and invalidation attributable to individual tests, at the
cost of repeating package initialization and `TestMain`.

## Test history

Gotst records advisory per-test history by default below the operating system's
user cache directory (`~/.cache/gotst/history/v1` on typical Linux systems), or
the selected `-cache-dir`. History contains recent outcomes, durations, attempt
counts, and portable dependency shapes. It is separate from the test-result
cache: history can inform scheduling, but never causes a test to be treated as
passing.

Use `-history=off` to disable history. In a shared production environment,
`-history=https://history.example/base` uses the versioned HTTP API instead;
`GOTST_HISTORY_TOKEN` supplies an optional bearer token. History service and
local history failures are soft failures and do not stop tests.

The current implementation collects and retrieves history but does not yet
change scheduling. It establishes the storage and protocol needed for future
duration- and memory-aware scheduling and conservative batching. See
[DESIGN.md](DESIGN.md#test-history) for identity, protocol, and database
details. Go implementations can import
`github.com/tailscale/gotst/history` for the store interface and versioned HTTP
wire types.

## Progress output

By default gotst prints one aggregate status line per second and a final
summary. During the build it distinguishes test binaries restored from the
linked-executable cache from newly built binaries. During execution it reports
completed packages and tests, currently running tests, and result-cache
hits/checks with a hit percentage. Successful
individual tests are suppressed; failures and their output are printed
immediately. `-vlog` restores per-test success/cache-hit lines and internal
diagnostics. Use `-progress=DURATION` to change the update interval or
`-progress=0` to print only the final summary.

Use `-failfast` to stop after the first test exhausts its retries. gotst stops
dispatching queued tests, cancels currently running test binaries, and also
passes `-test.failfast=true` to each binary so subtest scheduling stops
promptly.

Omitting `-count` runs each top-level test once and permits result-cache reuse.
Explicitly setting `-count=N` disables gotst result caching and runs each test
N times; in particular, `-count=1` means one uncached run, matching the usual
`go test -count=1` intent.

Failed tests are retried up to `-max-retries` additional times (default 3). A
test that fails one or more attempts and ultimately passes is considered flaky:
the run succeeds, the test is not stored as a clean cache result, and gotst
prints a `FLAKY TESTS` section in the final output. A test that still fails
after all retries fails the run. `-failfast` takes effect after retries are
exhausted. Automatic flake detection only observes tests that execute; a valid
cache hit is skipped, so use explicit `-count=1` when actively investigating
nondeterministic behavior.

Use `-json-summary` when another tool needs structured flaky-test results.
Gotst then emits a `gotst flaky tests JSON:` record after the human-readable
summary. The record includes arbitrary attributes set with `testing.T.Attr`;
capturing those attributes requires verbose test-binary mode, so tests observe
`testing.Verbose()` as true when this flag is enabled. Before the summary,
gotst also prints the retained output from every failed attempt of a test that
subsequently passed, preserving diagnostics for CI log analysis.

Use `-debug-uncached` to investigate unexpectedly low result-cache hit rates.
Gotst first runs every selected test while bypassing existing result entries and
seeds successful results, then performs a read-only verification pass. For each
miss it reports whether the entry was absent or invalid, could not be created,
or which recorded environment or filesystem inputs changed. The command fails
if any seeded result cannot be reused. It cannot be combined with `-count`,
`-cache=false`, `-build-only`, or `-failfast`.

## External build caching

Stock cmd/go does not normally retain linked test executables. When
`GOCACHEPROG` is unset, gotst automatically adds a persistent local build cache
below its cache root. It reads through to the ordinary Go build cache, so an
existing warm `GOCACHE` remains useful, while retaining linked test executables
for later gotst runs. The first run writes those large artifacts and can be
slower; subsequent unchanged runs avoid linking them.

When `GOCACHEPROG` is set, gotst instead places the same broker in front of the
configured helper. This allows a machine with an empty local `GOCACHE` to reuse
executables linked by another run or machine. Cached executables are verified
before execution. The private broker transport uses Unix-domain sockets on
Unix and loopback TCP on Windows.

Its goals are:

* keep the CPU busy
* build all test binaries up front, aborting early if any fail to compile
* support flaky tests, and re-running them to compute flakiness stats
* sharding test execution between multiple machines
* support multiple sets of build tags, without running tests redundantly
  if build tags don't affect a particular package
* have a pretty HTML status page with progress bars
* integrating with statistics stores, to track test speed & flakiness
  trends over time
* integrating well with [gomodfs](https://github.com/tailscale/gomodfs/)
  and [gocached](https://pkg.go.dev/github.com/bradfitz/go-tool-cache/cmd/gocached)
  as needed. (not much work should be required)
* giving users more control over declaring their test caching properties
  via annotations in their tests
