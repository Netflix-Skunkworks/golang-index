# Indexing fixtures

Each `*.txtar` file here is one end-to-end test case, holding both the input and
the expected output. `TestIndexing`, in `internal/integration`, globs this
directory and runs a subtest per file, named after the file: adding a fixture
adds a test, and no Go code changes.

A case runs like this:

1. Serve the fixture's repos from the fake GitHub Enterprise
   (`internal/githubfake`) and run a full index cycle against a real Postgres.
2. Compare every stored row of `repo_module_versions` with the `want` file.
3. Apply the fixture's `cycle` section, if it has one, and index again.
4. Compare with `want.2` — or with `want` again, when the fixture has no `cycle`
   section and so nothing changed upstream.
5. Check that both cycles completed every repo's work item, including a repo they
   found no module versions for — otherwise the re-index period could never hold
   that repo's next pass off, and it would come back around every re-indexing TTL.
6. Check that each cycle's `/index` feed publishes a module version once, even
   where several repos claim it, so the feed is narrower than the table for a
   fixture like `sharedmodulepath`.
7. Check the `/index` feed's append-only-log properties across that second cycle:
   surviving rows keep their `indexed_at`, new rows get a later one, and a
   consumer polling from the pre-re-index cursor sees exactly the new rows.

Steps 3–7 run for every fixture, so each one also covers re-indexing being
idempotent, whether or not it describes upstream changes.

## Format

A [txtar](https://pkg.go.dev/golang.org/x/tools/txtar) archive is a block of
free-form text followed by files introduced by `-- name --` lines. Here the
leading block holds the repo directives, plus the `#` comments describing what the
case exercises, and the files hold the repo trees and the expected output.

```
# The plain case: one root module with two semver tags.
repo someorg/thing
head 0123456789abcdef0123456789abcdef01234567 2026-01-02T03:04:05Z
tag v1.0.0 2025-01-02T03:04:05Z
tag tracing/v2.0.0 2025-03-04T05:06:07Z
-- someorg/thing/go.mod --
module go.example.com/thing
-- someorg/thing/tracing/go.mod --
module go.example.com/thing/tracing/v2
-- want --
someorg/thing go.example.com/thing v1.0.0 2025-01-02T03:04:05Z
someorg/thing go.example.com/thing/tracing/v2 v2.0.0 2025-03-04T05:06:07Z
```

### The directives

One directive per line, applying to the repo named by the most recent `repo`
line. Blank lines and `#` comments are ignored, so the comment describing the case
goes wherever it reads best.

| Directive | Arguments | Meaning |
| --- | --- | --- |
| `repo` | `org/name` | Starts a repo. A fixture may declare several. |
| `head` | oid, RFC 3339 date | The default branch's HEAD commit and its date. Omit for a repo with no commits. |
| `tag` | name, RFC 3339 date | A git tag and its date. Subdirectory tags (`tracing/v2.0.0`) are fine. |
| `untag` | name | Removes a tag. Only useful in a `cycle` section. |
| `cycle` | — | Splits the block: everything after it is applied between the two index cycles. |
| `skip` | reason | Reports the case as skipped instead of running it. |

`repo`, `head`, `tag`, and `untag` apply to a repo; `cycle` and `skip` apply to the
whole case.

### `skip`

A skipped fixture still has to parse, so its `want` rows keep describing what the
case should produce — the indexer just isn't asked yet. Use it for a case the
indexer does not handle, with the reason as the argument:

```
skip a subdir tag's go.mod is read only at HEAD, so a moved module resolves wrong
```

The reason is what `go test -v` prints, so make it say what is missing rather than
that something fails. `grep -l '^skip ' *.txtar` lists the skipped cases; nothing
is skipped today.

### The files: repo trees

A file's path is `<org>/<repo>/<path within the repo>`, so the repo it belongs to
is unambiguous even when a fixture declares several. A path that matches no
declared repo is an error, which catches typos. Only `go.mod` contents really
matter to the indexer; other files just give the tree something to report.

A repo may carry an `@ref` suffix, making that file the content **at that ref
only**, with the unsuffixed path serving every other ref:

```
-- someorg/thing/go.mod --
module go.example.com/thing/v2
-- someorg/thing@v1.3.0/go.mod --
module go.example.com/thing
```

That is how a fixture models a file that changed between refs — a root `go.mod`
whose module path gained a `/v2` suffix, say, where the older tags still point at
commits that predate the bump (`majortagversions.txtar`). The ref must be a tag or
head the fixture declares; a typo is a parse error rather than an override nothing
reads. Slashed refs work, since the ref is matched against the declared ones
longest-first — `someorg/thing@kafka/v3.0.0/kafka/go.mod` is how a subdirectory
module's tag gets its own content (`majorsuffixnosubdir.txtar`).

Note the override is additive: it cannot model a file being *deleted* at HEAD. A
fixture like `staletagsubdir.txtar` therefore reads as "this module exists only at
that tag," which is close enough, since a tag's module is resolved by reading its
own ref rather than by listing HEAD.

### The files: `want` and `want.2`

One line per expected row of `repo_module_versions`:

```
<org/repo> <module path> <version> <created>
```

`created` is RFC 3339 — the tag's date for a tag version, the HEAD commit's date
for a pseudo-version. Both sides are normalized to UTC before comparison, so any
spelling of the right instant works; the fixtures all use UTC. `indexed_at` is
deliberately absent: it is wall-clock time, so the test asserts on it structurally
instead (step 7 above).

Order doesn't matter (both sides are sorted), and blank lines and `#` comments
are ignored. A `want` file is required even when the fixture expects no rows —
leave it empty. `want.2` is required if and only if the fixture has a `cycle`
section.

### `cycle` sections

Directives after `cycle` mutate repos the fixture already declared — the fake
serves them by reference, so the second index cycle sees the change. This is how
a fixture models upstream churn; see `tagchurn.txtar`.

```
cycle
repo someorg/thing
untag v1.0.0
tag v1.2.0 2025-03-03T03:03:03Z
```

## What the fake does with a fixture

`githubfake.NewServer` serves the four GitHub surfaces the indexer reads over an
in-memory `*http.Client`, so a real `*github.GithubSCM` runs unmodified against it:
GraphQL (owner repositories, HEAD, tags), the accounts listing, raw file content,
and the recursive git-trees REST endpoint. The fixtures therefore exercise the production
GraphQL queries, module-path derivation, and pseudo-version synthesis; only the
network is fake. Postgres is real (see below).

Simplifications worth knowing when writing a fixture:

- **The fake's host is `github.fake.test`.** It is also the indexer's
  `DefaultModuleHost`, so it shows up in the want file of a repo with no `go.mod`
  (`nogomod.txtar`).
- **Refs share one tree by default.** Every ref — the HEAD oid or any tag — sees
  the same files unless an `@ref` path overrides one.
- **No paging.** Every GraphQL response is a single final page, so keep a fixture
  under GitHub's 100-per-page limit for tags, and this directory under it for
  repos.
- **Every repo lists Go.** Which repos a sweep picks out is not what these
  fixtures exercise, so the fake reports Go for all of them.
- **Every tree entry is a blob.** `ModuleDirs` only looks at paths, so no fixture
  needs to model directories.

## Running the tests

The suite needs a real Postgres, configured through the same environment
variables the `internal/db` tests use: `POSTGRES_USERNAME`, `POSTGRES_PASSWORD`,
`POSTGRES_HOST`, `POSTGRES_PORT`, `POSTGRES_DB`. Each subtest creates and drops
its own database (`gi_integ_testindexing_<fixture>`), so the suite is safe to run
alongside the `internal/db` tests, which reset a shared one.

```
go test ./internal/integration/                       # everything
go test ./internal/integration/ -run TestIndexing/tagchurn
```

## The fixtures

Every fixture opens with a `#` comment saying what it exercises, so the list of
cases is `head *.txtar`.
