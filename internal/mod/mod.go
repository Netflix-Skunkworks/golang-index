// Package mod reads git tags, commits, and go.mod files as Go module versions.
package mod

import (
	"strings"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// ModuleVersion is a Go module version: its path, semantic version, and the
// time of the tag or commit it came from.
type ModuleVersion struct {
	Version    string
	Created    time.Time
	ModulePath string
}

// ModuleVersionFromTag reads a git tag as a Go module version.
//
// It is multi-module aware: a "vX.Y.Z" tag is the root module, whereas a
// "<subdir>/vX.Y.Z" tag is the module in <subdir>.
//
// ok is false when the tag isn't a canonical semver, like v1", "foo", etc.
func ModuleVersionFromTag(tag string) (subdir, version string, ok bool) {
	version = tag
	if i := strings.LastIndex(tag, "/"); i >= 0 {
		subdir, version = tag[:i], tag[i+1:]
	}
	if !semver.IsValid(version) || semver.Canonical(version) != version {
		return "", "", false
	}
	return subdir, version, true
}

// PseudoVersion builds a Go pseudo-version for a module at a commit:
// "vMAJOR.0.0-<yyyymmddhhmmss>-<12-char-commit>". MAJOR comes from the module
// path's /vN suffix, or v0 when it has none.
func PseudoVersion(modulePath, commitOID string, committed time.Time) string {
	rev := commitOID
	if len(rev) > 12 {
		rev = rev[:12]
	}
	_, pathMajor, _ := module.SplitPathVersion(modulePath)
	return module.PseudoVersion(module.PathMajorPrefix(pathMajor), "", committed, rev)
}

// ModulePath returns the module path declared in a go.mod file, or "" when the
// bytes have no module line.
func ModulePath(goMod []byte) string {
	return modfile.ModulePath(goMod)
}

// RepoModulePath builds a repo's module path from the module host and the repo's
// "org/name": "<host>/<orgRepoName>", plus "/<subdir>" for a subdir module.
func RepoModulePath(host, orgRepoName, subdir string) string {
	modulePath := host + "/" + orgRepoName
	if subdir != "" {
		modulePath += "/" + subdir
	}
	return modulePath
}

// Check reports whether version is a valid release of the module at modulePath,
// e.g. that a v2+ version has a matching /vN path suffix.
func Check(modulePath, version string) error {
	return module.Check(modulePath, version)
}
