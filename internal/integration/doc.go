// Package integration holds end-to-end tests that wire the real database layer,
// GitHub SCM client, and indexers together against a real Postgres (configured
// by the POSTGRES_* env vars) and a fake GitHub Enterprise serving txtar-defined
// repos. Every case is a txtar fixture in internal/testcases holding both the
// repos to index and the rows they must produce; see that directory's README.md
// for the format. The package has no runtime code.
package integration
