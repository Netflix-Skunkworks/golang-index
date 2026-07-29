// Package mod reads git tags, commits, and go.mod files as Go module versions.
package mod

import (
	"path"
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

// MajorSubdir returns the major-version subdirectory a v2+ module's go.mod may
// live in, below the subdir its tags are prefixed with: "tracing/v2" for a v2
// version tagged under "tracing", or "v2" for one tagged at the root. It returns
// "" when there is no such directory, which is the case for v0 and v1.
//
// See https://go.dev/ref/mod#vcs-dir.
func MajorSubdir(subdir, version string) string {
	if !needsMajorSuffix(version) {
		return ""
	}
	return path.Join(subdir, semver.Major(version))
}

// IncompatibleVersion returns the form a canonical semver version takes for a
// module that has no go.mod, and so no /vN path suffix to match: v2 and later get
// the "+incompatible" suffix. Only a module in the repository root can be
// versioned this way, so a version from a subdir comes back unchanged, as do v0
// and v1, which need no suffix.
//
// See https://go.dev/ref/mod#incompatible-versions.
func IncompatibleVersion(subdir, version string) string {
	if subdir != "" || !needsMajorSuffix(version) {
		return version
	}
	return version + "+incompatible"
}

// needsMajorSuffix reports whether a canonical version requires a matching /vN
// module path suffix, which v2 and later do. It is false for anything that isn't a
// canonical semver version.
func needsMajorSuffix(version string) bool {
	switch semver.Major(version) {
	case "", "v0", "v1":
		return false
	}
	return true
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
