# gotst design

This document describes gotst's current architecture and the implementation
choices behind it. It is intended for contributors and for readers debugging a
run. User-facing behavior and configuration belong in [README.md](README.md);
future ideas and unfinished work belong in [NOTES.md](NOTES.md).

## Goals and boundaries

Gotst is a test runner built on the standard Go toolchain. It deliberately uses
`go list`, `go test`, and ordinary test-binary flags instead of reimplementing
package loading or `testing` package behavior.

The core choices are:

- discover and compile every selected package before running any test;
- capture the compiled test executables, then invoke them directly;
- schedule top-level tests independently with a process-wide concurrency
  limit;
- cache successful top-level tests using the executable and observed runtime
  dependencies as the identity;
- expose one synchronized state model to terminal progress and the web UI; and
- augment, but remain compatible with, stock Go's `GOCACHEPROG` support.

Gotst is intended to make two important environments fast:

- **Ephemeral CI and VM workers.** These machines may start every run with a
  fresh disk and therefore cannot benefit from a previous local Go build cache.
  A shared cache reached through `GOCACHEPROG` should let them reuse compiled
  packages and linked test executables across machines and runs.
- **Local development on macOS, Linux, and Windows.** Developers repeatedly
  edit, build, and run focused tests on a persistent workstation. In this mode
  `GOCACHEPROG` will usually be unset. Gotst must still be fast by cooperating
  with Go's ordinary local build cache, avoiding unnecessary package links and
  test-binary startup, selecting only relevant work, and reusing valid local
  per-test results and learned scheduling/resource history.

Neither environment is a fallback for the other. Features and defaults should
not require a remote cache to provide good laptop behavior, and local-only
optimizations should not assume a warm persistent disk on ephemeral workers.

Gotst does not currently distribute work between machines, batch multiple
top-level tests into one process, or provide a remote test-result cache. Those
are possible extensions, not properties of the current architecture.

## Live status page

The optional HTTP server renders the current synchronized `Server` state as a
complete HTML document. Each browser also opens `/live-ws`. The server renders
at most once per 500 milliseconds per connection and sends a compact
common-prefix/common-suffix patch relative to that connection's preceding
document. WebSocket per-message deflate with context takeover compresses the
patch stream. The browser reconstructs the next document and morphs the live
DOM in place, preserving the page rather than navigating or replacing the
whole body.

The frozen status snapshot contains only packages with test source, linked
binary counts and sizes, per-test and per-package progress, last-change times,
and failed or flaky attempts. Failure details are present in collapsed DOM
elements so they are immediately inspectable, but output is capped at 500 KiB
per test. Package columns are sorted in the browser, initially by most recent
change, without altering the server's deterministic document order.

The summary also counts the distinct package nodes in the selected test build
graph, discovered with `go list -deps -test`. Go 1.26's structured build stream
reports build output and failures, but not successful package compiles or cache
hits. Gotst therefore installs itself as a transparent `-toolexec` wrapper and
uses `TOOLEXEC_IMPORTPATH` to count distinct compile actions that actually run.
This supplies the `afresh` count. The wrapper also records a persistent mapping
from the compiler's build-ID action prefix to its package identity. On later
runs, the cache broker uses that mapping to attribute cache hits. Previously
unseen cached actions cannot be named, so the displayed remainder is the
graph-only portion rather than proof those packages need compilation. It also
includes `go list -test` identities that cmd/go replaces with test-specific
compile identities and special nodes such as `unsafe` that have no compiler
invocation.

At the end of `Server.Run`, after setting the terminal `done` or `failed`
phase, gotst sends a final patch to every connected browser. Browsers
acknowledge that sequence only after applying it. The runner waits for those
acknowledgements, with a two-second bound so an unresponsive browser cannot
prevent process exit. This finalization happens inside `Server.Run` because
the caller reports an error with `log.Fatal`, whose `os.Exit` would skip a
defer in `main`.

## Process roles

The gotst executable has five entry modes. `main` selects the four child modes
before parsing normal command-line flags.

1. **Runner.** The normal command owns configuration, discovery, compilation,
   scheduling, result caching, progress, and the optional HTTP server.
2. **Test-executable capture wrapper.** If `GOTST_EXEC_DEST` is present, gotst
   is running under `go test -exec`. It captures the executable named by
   `os.Args[1]`, emits an `ExecSnarf:` JSON record, and exits without running
   the test.
3. **Build-cache frontend.** With the private `-gotst-cache-shim` argument,
   gotst bridges cmd/go's `GOCACHEPROG` connection to the broker owned by the
   runner process.
4. **Local build-cache helper.** With the private `-gotst-local-cache-prog`
   argument, gotst serves the standard cache protocol from its persistent local
   store with read-through fallback to the ordinary Go disk cache.
5. **Tool-execution wrapper.** With the private `-gotst-toolexec` argument,
   gotst reports compiler invocations to the runner-owned broker and then
   transparently executes cmd/go's requested tool with the same arguments and
   standard streams.

Using one binary for all roles means `go test -exec`, `-toolexec`, and `GOCACHEPROG`
do not require separately installed helper programs.

## Run pipeline

`Server.Run` is a strict phase pipeline:

```text
invocation + profile
        |
        v
  discover packages  -- go list
        |
        v
  build and capture  -- go test -exec=<gotst>
        |
        v
 enumerate tests     -- captured binary -test.list=.
        |
        v
 cache check / run   -- one task per selected top-level test
        |
        v
 final progress + flaky summary
```

The corresponding `runPhase` values are `discovering`, `building`, `listing`,
`testing`, and then `done` or `failed`. A compilation failure stops the run
before any test executes. This is intentional: build errors should be reported
as build errors, rather than appearing after unrelated tests have run.

With `-build-only`, a successful run stops after the build-and-capture phase.
It does not invoke captured binaries for `-test.list` and therefore neither
executes package initialization/`TestMain` nor validates requested test names
against the compiled test inventory.

Each run gets a private directory named `pid<PID>-t<TIMESTAMP>` below the gotst
cache root. It holds captured executables, test logs, the broker socket, and
materialized external-cache executables. `Server.Cleanup` removes it. At
startup, gotst removes abandoned per-run directories whose recorded process is
no longer alive. Persistent test results live outside this directory.

## Invocation and profiles

`loadProfileDefinitions` searches upward from the working directory for
`.gotst.yml`, unless `-config` specifies a file. The configuration directory is
the root for relative package patterns and subprocess working directories. If
there is no configuration, gotst creates an implicit `default` profile rooted
at the current directory with package pattern `./...`.

Profiles may include other profiles. Resolution is a depth-first merge with
cycle detection:

- list fields append in include order and deduplicate;
- explicitly set scalar values override included values; and
- an empty final package list defaults to `./...`.

`parseInvocation` then interprets positional arguments:

- an exact profile name in the first position selects that profile;
- identifiers beginning with `Test`, `Fuzz`, or `Example` select unqualified
  tests;
- an argument whose final dotted component is such a name selects a qualified
  package/test pair; and
- remaining arguments are package patterns and replace the profile package
  list.

Qualified package specifications are resolved to exactly one import path with
`go list` and added to package discovery. The original specification is kept
for useful not-found errors.

## Package discovery and selection

Discovery uses `go list -json` with the resolved profile's tags and package
patterns. Exclusion patterns are separately resolved to import paths, so
exclusions are compared canonically rather than as strings supplied by the
user.

When unqualified test names were requested, gotst scans `_test.go` files for
those names and drops packages that cannot contain any requested test. This is
only a link-time optimization. A read error conservatively keeps the package,
qualified selections always keep their resolved package, and the captured
binary's `-test.list` output remains authoritative after build constraints and
code generation have taken effect.

The reduced `goListPackage` type contains only fields gotst needs. Package and
test status is stored by canonical import path in `Server.pkgs`.

## Building and capturing test executables

Gotst builds all packages in one command shaped like:

```text
go test -count=1 -p=<jobs> -trimpath -tags=<tags> -json \
    -exec=<path-to-gotst> <import-paths...>
```

`-count=1` disables cmd/go's package-level test-result cache. Gotst owns result
caching at top-level-test granularity. The command still compiles and links
normal test executables, but cmd/go invokes gotst as the `-exec` wrapper instead
of executing each one.

The wrapper:

1. hashes the executable with SHA-256;
2. hard-links it into the run directory under that hash, or copies it if a
   hard link is unavailable;
3. records its working directory and cmd/go-provided test arguments; and
4. prints one `ExecSnarf:<json>` line.

The runner consumes cmd/go's JSON stream. Build output is retained separately
from wrapper output so compile failures can identify the failing package. A
successful wrapper record becomes `packageStatus.exeHash`, `workDir`, and
`exeArgs`. Wrapper diagnostics may precede the record, so parsing searches for
a complete line beginning with `ExecSnarf:` rather than assuming it is the
first output.

Captured files are content-addressed. Later code derives the executable hash
from its basename; renaming that layout requires changing the test-cache key
construction too.

## Test enumeration

After every selected package has built, gotst runs each captured executable
with `-test.list=.`. Enumeration is concurrent but bounded by the shared
`execSem`. The list is filtered through `testSelection`, and gotst then verifies
that every explicitly requested test was found.

Only names beginning with `Test`, `Fuzz`, or `Example` become scheduler tasks.
This mirrors invocation selection but is intentionally a lexical check rather
than an attempt to reproduce all internals of the `testing` package.

One consequence of direct enumeration is that package initialization and
`TestMain` execute once while listing and again for every scheduled test.
Packages containing `TestMain` but no runnable top-level name currently produce
no test task.

## Scheduling and execution

`runAllTests` creates one `testTask` for each selected `(package, test)` pair.
A fixed worker pool of size `min(-j, number of tasks)` consumes those tasks.
The `execSem`, also sized by `-j`, is the process-wide subprocess limit used by
listing and test execution.

Each task invokes its captured executable directly with an anchored
`-test.run=^<quoted-name>$`. Arguments captured from cmd/go are normalized for
direct execution: the private `-test.v=test2json` framing mode is removed, and
profile short/timeout/test flags are applied. Gotst forces each child attempt
to `-test.count=1`; repetition and retries are controlled by the parent.

For `-count=N`, every requested repetition must eventually pass. Each failed
attempt may be retried up to `-max-retries`. A task that fails and later passes
is successful but flaky, and its result is not cached as a clean pass. An
exhausted retry budget records a test failure.

`-failfast` takes effect only after retries for a test are exhausted. The first
such failure cancels `Server.ctx`, which stops dispatching queued tasks and
kills running `exec.CommandContext` children. Cancellation errors from sibling
tasks are suppressed; the primary test failure is retained.

Test output is captured in a concurrency-safe `cappedBuffer`. Failures are
printed immediately under `Server.outMu`; successful tests are quiet unless
`-vlog` is enabled.

With `-json-summary`, child binaries run in verbose mode so standard
`testing.T.Attr` lines are available. Gotst retains the attributes without
interpreting their keys and includes them in its final `gotst flaky tests JSON:`
record. Consumers can attach domain-specific meaning to attributes while gotst
remains independent of any CI or issue tracker. This mode also prints each
flaky test's retained failed-attempt output before the summary, allowing a CI
consumer to analyze the original failure even though the overall run passed.

### State and locking

`Server.mu` protects package/test status, phase, counters, and fail-fast state.
Code must not hold it while waiting for a subprocess or performing cache I/O.
`Server.outMu` prevents progress and test output from interleaving.

The principal state transitions are:

```text
package: discovered -> built -> testing -> done
test:    pending -> running -> done(pass or fail)
```

Readers such as terminal progress and the HTTP handler freeze a view while
holding `Server.mu`; they do not maintain separate execution state.

## Test-result cache

The test-result cache is independent of Go's build cache. Its narrow
`testResultCache` interface has `Get` and `Put` operations so another storage
backend can be added without coupling the scheduler to the disk layout.

The versioned cache key contains:

- captured test-binary SHA-256;
- package import path and top-level test name;
- package working directory; and
- effective test arguments, excluding the per-task `-test.run` value.

Entries are JSON below a versioned `test-results/vN` directory in the cache root and are installed by
atomic rename. Only a clean, single-repetition pass is written. Explicitly
supplying `-count`, even `-count=1`, disables result-cache reuse so it preserves
the familiar `go test -count=1` intent.

`-debug-uncached` diagnoses result-cache instability with two phases. The seed
phase bypasses all existing entries, runs every selected top-level test, and
writes successful cache entries normally. The verification phase does not run
test binaries: it reads each just-written entry and fingerprints its recorded
inputs against the settled filesystem and environment after the seed phase.
This read-only second phase avoids cascading false misses from one diagnostic
rerun mutating another test's inputs. Missing entries, capture/write errors,
validation errors, and changed dependencies are reported per test; any miss
makes the command unsuccessful.

For an executed test, gotst asks the test binary to write the standard
`-test.testlogfile`. `parseTestLog` snapshots dependencies reported by the Go
runtime:

- `getenv`: presence plus a SHA-256 of the value;
- `open` on a regular file: stat/lstat metadata and full content hash;
- `open` on a directory: metadata and a deterministic entry fingerprint; and
- `stat` and `chdir`: filesystem metadata, resolving relative paths against the
  working directory as it changes.

As in cmd/go's native test cache, `open` and `stat` paths outside the package's
module, GOPATH, or GOROOT root are not rechecked. This prevents incidental
runtime bookkeeping such as `t.TempDir` opening the shared temporary directory,
and volatile pseudo-files such as `/proc/net/route`, from invalidating results.
`chdir` remains tracked regardless of its destination because it changes how
subsequent relative paths are interpreted.

`GODEBUG` is added explicitly because the runtime reads it without reporting it
in the test log. Environment values are not stored in plaintext.

On lookup, gotst recomputes every dependency fingerprint. A changed or
unreadable dependency makes the entry unusable and the test runs normally.
Cache errors are generally soft failures: with `-vlog` they are diagnosed, but
they do not replace test execution.

This mechanism can only invalidate on dependencies reported by Go's test-log
hooks. Network services, subprocess behavior, time, randomness, and unreported
system state are outside its current model.

## Test history

Test history is scheduling evidence, not a result cache. The importable,
dependency-light `github.com/tailscale/gotst/history` package defines the
`history.Store` interface and all value and HTTP wire types. `Store.Lookup`
takes a named `LookupOptions` value; `RecentPerKey` bounds the number of newest
observations returned for each key. The interface is available for embedding
an in-process backend, while external services normally implement the HTTP
protocol using the same package's `LookupRequest`, `LookupResponse`, and
`RecordRequest` types.

Gotst performs one batched lookup before execution and one batched record at
the end of a run. The default implementation is local; an HTTP implementation
supports a shared production service. Backend errors are reported with `-vlog`
and otherwise ignored so history availability cannot affect whether tests run
or whether a run passes.

The scheduler uses the newest observation for each test. Tests whose newest
outcome is `fail` run before the rest, giving likely regressions an early start
and faster feedback. Within the failed and non-failed groups, tests run from
longest to shortest newest observed duration. Tests without history have a
zero estimate and sort last. This longest-processing-time-first order reduces
the chance that a slow test becomes a straggler after other workers go idle.
History remains advisory: missing or unavailable history falls back to stable
package-and-test ordering and never changes the result of a run.

### Identity and observations

A history key contains the package import path, top-level test name, GOOS,
GOARCH, sorted build tags, and effective test arguments. Its ID is the SHA-256
of a versioned canonical encoding. It deliberately omits the test-binary hash,
source revision, and checkout directory: duration, memory, and dependency-shape
evidence should survive an edit and should be shareable between machines. A
binary hash remains mandatory in the separate correctness-sensitive result
cache.

Each completed, actually executed test contributes an observation containing:

- a unique idempotency ID and UTC timestamp;
- `pass`, `fail`, or `flaky` outcome;
- total duration and number of attempts;
- observed test-log dependency shape; and
- a reserved peak-RSS field for memory-aware admission.

A result-cache hit does not fabricate a duration observation. Dependency
history retains operations and environment-variable names but not environment
values or file contents. Package-relative and module-relative paths use
`$PACKAGE` and `$MODULE` prefixes. External paths are represented by a
non-reversible short hash plus basename, avoiding usernames and absolute paths
while conservatively preventing unlike machine layouts from matching. A null
dependency list means capture failed; an empty list means capture succeeded
and saw no inputs.

When retries or explicit repetitions produce multiple input logs, the
observation records their union. If any attempt's input capture fails, the
whole observation's dependency shape is unknown rather than pretending that a
partial union is complete.

Gotst requests `-test.testlogfile` whenever either history or result caching is
enabled. It parses the log even after a failed test, because failure history is
also useful for batching and flake policy. The local store keeps the newest 32
observations per key under the gotst user cache:

```text
history/v1/<first-key-hash-byte>/<key-hash>/<observation-id-hash>.json
```

One atomically renamed file per observation makes concurrent gotst processes
safe without a cross-platform file lock or lossy read-modify-write. Writes are
idempotent by observation ID. Retention pruning is opportunistic; a concurrent
writer may temporarily leave more than 32 files, and a later write converges
the directory to the bound.

### HTTP API

`-history=https://host/base` selects the remote store. Requests are JSON POSTs,
have a five-second client timeout, and include `Authorization: Bearer
<GOTST_HISTORY_TOKEN>` when that environment variable is set. The base URL may
include a path. Version 1 has two operations:

```http
POST /base/v1/history/lookup
Content-Type: application/json

{"version":1,"keys":[{"package":"example.com/p","test":"TestX","goos":"linux","goarch":"amd64"}],"limit":32}
```

```json
{"version":1,"histories":[{"version":1,"key":{"package":"example.com/p","test":"TestX","goos":"linux","goarch":"amd64"},"observations":[{"id":"...","observed_at":"2026-08-08T12:00:00Z","outcome":"pass","duration_ns":1200000,"attempts":1,"dependencies":[{"operation":"getenv","name":"GODEBUG"}]}]}]}
```

```http
POST /base/v1/history/record
Content-Type: application/json

{"version":1,"observations":[...]}
```

Lookup returns at most `limit` newest observations for each key. Record is
idempotent by observation ID and may return `204 No Content`. The server must
reject unsupported versions and invalid outcomes, sizes, or key/observation
associations. Request authentication and tenant/repository authorization are
deployment concerns; the wire format intentionally contains no
Tailscale-specific fields.

### PostgreSQL schema and queries

`cmd/testhistoryd` implements the HTTP service with the same
`github.com/jackc/pgx/v5` driver used by Tailscale's cfgdb PostgreSQL backend.
It uses `pgxpool` directly, requires PostgreSQL major version 17 at startup,
embeds the schema in the binary, and refuses an unknown schema version. Each
process is configured with one UUID `scope_id`; the value comes from trusted
daemon configuration rather than client input. Writes are transactional and
idempotent by `(scope_id, observation_id)`.

The HTTP service can compute the same canonical key hash as the client and use
the following initial schema. `scope_id` is derived from authentication or the
configured service/base URL rather than trusted client JSON; it isolates
repositories or tenants that happen to use the same import path. JSON arrays
preserve the protocol representation without requiring joins for fields that
are only returned as key metadata.

```sql
CREATE TABLE test_history_key (
    scope_id uuid NOT NULL,
    key_hash bytea NOT NULL CHECK (octet_length(key_hash) = 32),
    package text NOT NULL,
    test_name text NOT NULL,
    goos text NOT NULL,
    goarch text NOT NULL,
    tags jsonb NOT NULL,
    args jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_observed_at timestamptz NOT NULL,
    PRIMARY KEY (scope_id, key_hash)
);

CREATE TABLE test_history_observation (
    scope_id uuid NOT NULL,
    observation_id text NOT NULL,
    key_hash bytea NOT NULL,
    FOREIGN KEY (scope_id, key_hash)
        REFERENCES test_history_key(scope_id, key_hash)
        ON DELETE CASCADE,
    observed_at timestamptz NOT NULL,
    outcome text NOT NULL CHECK (outcome IN ('pass', 'fail', 'flaky')),
    duration_ns bigint NOT NULL CHECK (duration_ns >= 0),
    attempts integer NOT NULL CHECK (attempts > 0),
    peak_rss_bytes bigint CHECK (peak_rss_bytes IS NULL OR peak_rss_bytes >= 0),
    dependencies jsonb,
    PRIMARY KEY (scope_id, observation_id)
);

CREATE INDEX test_history_observation_key_time
    ON test_history_observation
       (scope_id, key_hash, observed_at DESC, observation_id)
    INCLUDE (outcome, duration_ns, attempts, peak_rss_bytes);

CREATE INDEX test_history_key_last_observed
    ON test_history_key (scope_id, last_observed_at);

CREATE INDEX test_history_observation_time
    ON test_history_observation (scope_id, observed_at);
```

`observation_id` is opaque text so protocol evolution does not bind the server
to one UUID representation. Batched lookup uses the key/time index and one
index-limited scan per requested hash:

```sql
SELECT requested.ordinality, o.*
FROM unnest($1::bytea[]) WITH ORDINALITY AS requested(key_hash, ordinality)
CROSS JOIN LATERAL (
    SELECT observation_id, key_hash, observed_at, outcome, duration_ns,
           attempts, peak_rss_bytes, dependencies
    FROM test_history_observation
    WHERE scope_id = $2 AND key_hash = requested.key_hash
    ORDER BY observed_at DESC, observation_id
    LIMIT $3
) AS o
ORDER BY requested.ordinality, o.observed_at DESC, o.observation_id;
```

Recording first upserts key metadata and monotonically advances its last-seen
time, then inserts observations with `ON CONFLICT (scope_id, observation_id) DO
NOTHING`. The service must verify that a conflicting ID was not previously
associated with a different key. Old observations can be deleted in bounded
batches using the observation-time index; keys with no remaining observations
can then be deleted using `last_observed_at`. The per-key time index also
supports trimming to the newest N observations if retention is count-based.

No GIN index on `dependencies` is planned initially. Online scheduler queries
are by key hash and retrieve a small recent window; they do not search inside
dependency JSON. Dependencies are deliberately not an included index column
because shapes can be large; the bounded lookup fetches them from the table.
If server-side dependency grouping becomes useful, add a canonical
`dependency_shape_hash bytea` column and a targeted B-tree index instead of
broadly indexing JSON. Recent observations can likewise feed
median/p95 duration, flake-rate, and peak-RSS estimates; precomputed aggregate
tables should be added only after those query windows and write volume are
measured.

### Conservative future batching

History-driven batching must remain speculative. Only tests whose prior
dependency shapes match may share a test-binary invocation. If the batch
fails, gotst reruns its tests individually. If the batch's observed union of
dependencies differs from the expected historical shape, gotst also splits
and reruns, even if the batch passed. Individual passing observations or cache
entries are recorded from a batch only when the whole batch passes and its
dependencies match history; otherwise only the isolated reruns establish new
per-test evidence. Unknown, slow, or historically flaky tests can remain
isolated. This preserves attributable cache inputs and outcomes while allowing
the fast, stable long tail to amortize process startup later.

## External Go build-cache broker

This subsystem is separate from the test-result cache. The broker is used in
both performance environments, with different downstream helpers:

- when the runner inherits `GOCACHEPROG`, that configured helper remains the
  downstream cache for fresh-disk and cross-machine reuse; and
- when `GOCACHEPROG` is unset, gotst automatically starts its own persistent
  local helper below `build-cache/v1` in the gotst cache root.

The automatic helper is a two-tier cache. It writes new package artifacts and
linked executables into gotst's cache. On a miss, it reads the stable v1 action
record from the ordinary `GOCACHE` read-only. This preserves the value of a
developer's already-warm Go cache without requiring gotst to write cmd/go's
internal disk format or locking protocol. The first run pays additional I/O to
persist linked executables; later unchanged runs can skip their link actions.

The runner and its frontend/capture children communicate over a private
Unix-domain socket on Unix and a loopback-only TCP listener on Windows.

Stock cmd/go asks an external cache for linker outputs but does not upload a
newly linked executable to it. Gotst fills that gap without patching Go. The
runner starts the configured helper itself and listens on a private Unix socket.
The cmd/go child is given `GOCACHEPROG=<gotst> -gotst-cache-shim`; that shim is a
byte bridge to the parent. Capture wrappers use a second connection type on the
same socket to register linked executables.

```text
                         private Unix socket
cmd/go <-> gotst shim  --------------------------+
                                                   |
capture wrapper --------------------------------> broker <-> real GOCACHEPROG
                                                   |
gotst runner owns lifecycle -----------------------+
```

The parent must own the real helper. Cmd/go can close its cache frontend before
the final `-exec` wrapper registers its executable. Treating cmd/go's `close` as
only the end of that frontend connection lets late registrations finish; the
runner sends the real helper's `close` after the complete `go test` command has
exited.

### Cold executable path

For a linker `get` miss, the broker records cmd/go's full 32-byte action ID,
indexed by its first 15 bytes. Go embeds those same 120 bits as the first
component of the executable build ID. When the capture wrapper registers the
finished executable, the broker reads its build ID, recovers the full action
ID, verifies the wrapper-provided hash and size, and sends a synthetic `put` to
the real helper.

Gotst never attempts to calculate the action ID. Cmd/go's value already covers
the toolchain and all link inputs. If no matching miss was observed, the
executable is simply not uploaded.

### Warm executable path

On a cache hit, ordinary non-executable objects pass through unchanged. For an
executable-looking object, the broker verifies:

- the object SHA-256 equals the helper's output ID; and
- the first Go build-ID component equals the requested action-ID prefix.

It then copies the object to a private executable path, applies executable
permissions, and returns that path to cmd/go. A failed verification is converted
to a cache miss, so an unverified cache artifact is never executed.

The broker translates request IDs because concurrent cmd/go requests and
synthetic puts share one downstream helper. JSON request bodies follow the
standard base64-on-a-separate-line `GOCACHEPROG` framing. Compatibility fields
for older helper names are accepted where practical.

## Progress and HTTP status

The progress reporter periodically obtains a `progressSnapshot` derived from
the synchronized package/test state. It reports phase, package and test counts,
linked-executable cache hits, running tests, flakes, and test-result-cache hit
rates. The final snapshot is always printed, even when periodic progress is
disabled.

The optional HTTP server calls the same `Server` state through `statusData` and
renders `root.tmpl.html`. It starts on the configured address and, when the
local Tailscale daemon reports a running node, also binds the same port on each
assigned Tailscale IP and advertises `Self.DNSName`. LocalAPI lookup and
additional-listener failures are soft; the explicitly configured listener
remains authoritative. The page is currently a polling snapshot view, not an
API or event stream, and its presentation state is intentionally secondary to
the runner state.

## Source map

| File | Responsibility |
| --- | --- |
| `gotst.go` | Entry modes, `Server`, phase pipeline, discovery, capture, listing, scheduler, retries, and argument normalization |
| `profile.go` | Configuration discovery, strict YAML decoding, includes, and resolved profiles |
| `invocation.go` | Positional package/profile/test interpretation and selection matching |
| `cache.go` | Per-run directories and cleanup of abandoned runs |
| `testcache.go` | Persistent result-cache interface, disk backend, test-log parsing, and dependency fingerprints |
| `history/history.go` | Public advisory-history model, store interface, and versioned HTTP wire types |
| `history.go` | Gotst integration, portable dependency shapes, and local/HTTP store implementations |
| `cmd/testhistoryd` | PostgreSQL 17 history service, embedded schema, and HTTP handlers |
| `cacheprog.go` | Parent-owned build-cache client, private broker transport, cmd/go frontend, executable association, and verification |
| `localcache.go` | Automatic persistent local build-cache helper and read-only fallback to the ordinary Go disk cache |
| `progress.go` | Immutable progress snapshots, terminal reporting, and flaky summary |
| `web.go` / `root.tmpl.html` | HTTP status projection and rendering |
| `util.go` | Subprocess-output plumbing, Go binary discovery, and process checks |
| `NOTES.md` | Known limitations and future design work |

Tests generally live beside the subsystem they exercise. `cacheprog_test.go`
uses a subprocess helper to test the real streaming protocol and, importantly,
the lifecycle where cmd/go closes before a late executable registration.

## Correctness invariants

Changes should preserve these properties:

- no selected test starts until every selected test package compiles;
- source scanning may reduce work but never determines the authoritative test
  inventory;
- captured executable contents agree with their content-addressed filename;
- a test-result hit is used only after all recorded dependencies validate;
- flaky or failed executions are never stored as clean passing results;
- history failures never suppress execution or change a test outcome;
- fail-fast cancellation does not turn sibling cancellation into additional
  test failures;
- an executable from an external build cache is never run without content-hash
  and Go build-ID verification; and
- the real `GOCACHEPROG` helper outlives cmd/go's frontend and all capture
  registrations.

Run `go test ./...`, `go test -race ./...`, `go vet ./...`, and
`git diff --check` after changes that affect concurrency, process protocols, or
cache identity.
