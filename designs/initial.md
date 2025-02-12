# Initial design

2025-01-01

## Background

https://index.golang.org/index is the Go package index. It indexes VCS and
presents a simple JSON feed of module / version (tag) updates for
[pkgsite](https://github.com/golang/pkgsite/blob/master/doc/design.md). pkgsite
workers then AST parse code and documentation to populate a database, with the
pkgsite frontend providing a documentation server frontend for users
(ex [pkg.go.dev](https://pkg.go.dev/)).

The Go team provides the code for pkgsite at
https://go.googlesource.com/pkgsite, but there is no open source code for the
Golang index. This project provides a golang index for users that need to set up
their own pkgsite stack: for example, to host documentation for a private
source code repository (ex GitHub enterprise).

There are some public details about the shape of the Golang index at
https://index.golang.org/. But, there are few design details, forcing us to
design our own.

## High level design

The initial MVP design simply trawls GitHub for Go repositories, queries GitHub
for their tags, and stores the information as an index in-memory.

Once the index is built, an HTTP listener is spun up that serves this index in
the form described at https://index.golang.org/.

## Future

In the future, we'll need to solve for:

- Not re-indexing on every application startup.
- Resumable indexing.
- Distributed indexing.
- Large SCMs (ex indexing github.com).
- etc.
