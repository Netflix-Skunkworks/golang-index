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
	subdir, version = splitTag(tag)
	if !semver.IsValid(version) || semver.Canonical(version) != version {
		return "", "", false
	}
	return subdir, version, true
}

// splitTag divides a tag into the module subdirectory it names and its version
// part: "tracing/v2.0.0" is v2.0.0 of the module in tracing.
func splitTag(tag string) (subdir, version string) {
	if i := strings.LastIndex(tag, "/"); i >= 0 {
		return tag[:i], tag[i+1:]
	}
	return "", tag
}

// PseudoVersion builds a Go pseudo-version for a module at a commit. base is the
// canonical version it builds on, and its form follows from that:
//
//	""            -> v0.0.0-20260102030405-abcdef012345
//	"v1.2.3"      -> v1.2.4-0.20260102030405-abcdef012345
//	"v1.2.3-rc.1" -> v1.2.3-rc.1.0.20260102030405-abcdef012345
//
// With an empty base the major version comes from the module path's /vN suffix, or
// is v0 when it has none.
//
// See https://go.dev/ref/mod#pseudo-versions.
func PseudoVersion(modulePath, base string, committed time.Time, commitOID string) string {
	rev := commitOID
	if len(rev) > 12 {
		rev = rev[:12]
	}
	_, pathMajor, _ := module.SplitPathVersion(modulePath)
	return module.PseudoVersion(module.PathMajorPrefix(pathMajor), base, committed, rev)
}

// PseudoVersionBaseFromTags picks the version a module's pseudo-version builds on:
// the highest version among the repo's tags prefixed with tagPrefix ("" for a
// module in the repo root). It returns "" when no tag qualifies, which yields the
// vMAJOR.0.0 form.
//
// The base is assumed to be an ancestor of the commit being versioned. A list of
// tag names can't establish that, so a base taken from a tag outside the commit's
// history yields a pseudo-version the go tool rejects as not descending from it.
func PseudoVersionBaseFromTags(modulePath, tagPrefix string, tags []string) string {
	var base string
	for _, tag := range tags {
		prefix, candidate, ok := tagBaseVersion(tag)
		if !ok || prefix != tagPrefix {
			continue
		}
		// A base has to be a version of this module path, major version included.
		if Check(modulePath, candidate) != nil {
			continue
		}
		if semver.Compare(candidate, base) > 0 {
			base = candidate
		}
	}
	return base
}

// tagBaseVersion reads the canonical version a tag can base a pseudo-version on,
// which is looser than [ModuleVersionFromTag]: build metadata is ignored, so
// "tracing/v1.2.3+meta" bases v1.2.3 even though it names no version of its own.
// Shorthand like "v1" or "v1.2" bases nothing, and neither does a tag that already
// looks like a pseudo-version.
func tagBaseVersion(tag string) (tagPrefix, base string, ok bool) {
	tagPrefix, version := splitTag(tag)
	base = semver.Canonical(version)
	if base == "" || version != base+semver.Build(version) || module.IsPseudoVersion(version) {
		return "", "", false
	}
	return tagPrefix, base, true
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

// TagPrefix returns the prefix a module's tags carry, given the subdirectory its
// go.mod sits in. That is the subdirectory itself, minus a major-version element:
// the module in "client/v3" declaring ".../client/v3" is tagged "client/v3.5.0", so
// its prefix is "client". It is the inverse of [MajorSubdir].
func TagPrefix(subdir, modulePath string) string {
	_, pathMajor, _ := module.SplitPathVersion(modulePath)
	if pathMajor == "" {
		return subdir
	}
	dir, last := path.Split(subdir)
	if last != strings.TrimPrefix(pathMajor, "/") {
		return subdir
	}
	return strings.TrimSuffix(dir, "/")
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
