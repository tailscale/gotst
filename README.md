# gotst

`gotst` is a wrapper around `go test` designed for both humans and CI.

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
