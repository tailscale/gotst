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
