# gotst

`gotst` is a wrapper around `go test` designed for both humans and CI.

Running `gotst` selects the `default` profile from the nearest `.gotst.yml` in
the current directory or one of its parents. Running `gotst NAME` selects a
named profile. If no configuration exists, the implicit `default` profile runs
`./...` from the current directory.

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
`~/.cache/gotst/test-results/v1` by test-binary SHA-256 and test name. Use
`-cache=false` to bypass result caching or `-cache-dir=PATH` to select a
different cache root.

Each entry is readable JSON and records the test identity, arguments, passing
duration, and dependencies learned through `-test.testlogfile`. Environment
dependencies include presence and a SHA-256 of the value (not the plaintext
value). Opened regular files include a full content SHA-256 as well as
stat/lstat metadata; directory opens include a deterministic directory-listing
fingerprint. `stat` and `chdir` operations record filesystem metadata. A result
is reused only if all recorded dependency fingerprints still match.

The current scheduler executes each top-level test in its own process. This
makes cache entries and invalidation attributable to individual tests, but it
also repeats package initialization and `TestMain` and does not retain
in-process `t.Parallel` scheduling across top-level tests. Hybrid batching is
planned.

## Progress output

By default gotst prints one aggregate status line per second and a final
summary. During execution it reports completed packages and tests, currently
running tests, and cache hits/checks with a hit percentage. Successful
individual tests are suppressed; failures and their output are printed
immediately. `-vlog` restores per-test success/cache-hit lines and internal
diagnostics. Use `-progress=DURATION` to change the update interval or
`-progress=0` to print only the final summary.

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
