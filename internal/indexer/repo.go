// Package indexer holds the background loops that keep the index up to date:
// one that re-indexes the full list of Go repos, and one that re-indexes an
// individual repo's tags. It also derives Go module versions from a repo's
// tags, commits, and go.mod files.
package indexer

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Netflix-Skunkworks/golang-index/internal/github"
	"github.com/Netflix-Skunkworks/golang-index/internal/mod"
)

// scm is the source-control access needed to read a repo's module versions;
// [*github.GithubSCM] satisfies it.
// Every method reports retryable alongside its error: false means a later pass
// would fail the same way, as it does for a repo the credentials may not read.
// It says nothing when err is nil.
type scm interface {
	RepoTags(ctx context.Context, orgRepoName string) (tags []github.Tag, retryable bool, err error)
	HeadCommit(ctx context.Context, orgRepoName string) (oid string, committed time.Time, retryable bool, err error)
	GoMod(ctx context.Context, orgRepoName, ref, subdir string) (content []byte, found, retryable bool, err error)
	ModuleDirs(ctx context.Context, orgRepoName, ref string) (subdirs []string, retryable bool, err error)
}

// moduleVersionsForRepo returns a module version for each of a repo's semver
// tags, skipping tags that don't resolve to one. A version is not always identical
// to its tag: a v2+ tag on a repo with no go.mod is recorded as +incompatible. A
// repo with no versions from tags falls back to pseudo-versions synthesized from
// HEAD, one per module, each built on the highest tag that names it: tags that
// yield no version still set where those pseudo-versions sort. host is the module
// host used to build repo-derived module paths (e.g. "github.mycompany.net").
func moduleVersionsForRepo(ctx context.Context, scm scm, host, orgRepoName string) (versions []*mod.ModuleVersion, retryable bool, err error) {
	tags, retryable, err := scm.RepoTags(ctx, orgRepoName)
	if err != nil {
		return nil, retryable, err
	}

	for _, t := range tags {
		subdir, version, ok := mod.ModuleVersionFromTag(t.Name)
		if !ok {
			slog.Debug(fmt.Sprintf("Skipping tag %q for %s: not a module version", t.Name, orgRepoName))
			continue
		}

		modulePath := mod.RepoModulePath(host, orgRepoName, subdir)

		declaredPath, hasGoMod, retryable, err := modulePathForTag(ctx, scm, orgRepoName, t.Name, subdir, version)
		switch {
		case err != nil && !retryable:
			// Every remaining tag would fail the same way, and defaulting to the
			// repo-derived module path would record the whole repo at a path no go.mod
			// confirms.
			return nil, false, err
		case err != nil:
			slog.Error(fmt.Sprintf("Error getting go.mod file for %s (tag %q): %v; defaulting to GitHub URL for module path", orgRepoName, t.Name, err))
		case !hasGoMod:
			slog.Info(fmt.Sprintf("Unable to find go.mod file for %s (tag %q); defaulting to GitHub URL for module path", orgRepoName, t.Name))
			version = mod.IncompatibleVersion(subdir, version)
		case declaredPath == "":
			slog.Debug(fmt.Sprintf("Skipping tag %q for %s: its go.mod declares no module path", t.Name, orgRepoName))
			continue
		default:
			modulePath = declaredPath
		}

		if err := mod.Check(modulePath, version); err != nil {
			slog.Debug(fmt.Sprintf("Skipping tag %q for %s: %v", t.Name, orgRepoName, err))
			continue
		}

		versions = append(versions, &mod.ModuleVersion{
			Version:    version,
			Created:    t.Date,
			ModulePath: modulePath,
		})
	}

	if len(versions) == 0 {
		pseudos, retryable, err := headPseudoVersions(ctx, scm, host, orgRepoName, tagNames(tags))
		if err != nil {
			return nil, retryable, err
		}
		versions = append(versions, pseudos...)
	}

	return versions, false, nil
}

// headPseudoVersions synthesizes a pseudo-version for every module in the repo
// at HEAD, used when a repo has no semver tags. tags are the repo's tag names,
// which set what each pseudo-version builds on. Multi-module repos get one per
// module, not just the root. A repo with no module directory at HEAD still gets
// one, at the repo URL, which is the path a tag on such a repo resolves to. It
// returns nil for a repo with no commit, and skips any module whose go.mod
// declares no path or whose pseudo-version isn't valid for that path.
func headPseudoVersions(ctx context.Context, scm scm, host, orgRepoName string, tags []string) (versions []*mod.ModuleVersion, retryable bool, err error) {
	oid, committedAt, retryable, err := scm.HeadCommit(ctx, orgRepoName)
	if err != nil {
		return nil, retryable, err
	}
	if oid == "" {
		return nil, false, nil
	}

	subdirs, retryable, err := scm.ModuleDirs(ctx, orgRepoName, oid)
	if err != nil {
		return nil, retryable, err
	}

	if len(subdirs) == 0 {
		slog.Info(fmt.Sprintf("No module directory at HEAD for %s; defaulting to GitHub URL for module path", orgRepoName))
		rootPath := mod.RepoModulePath(host, orgRepoName, "")
		base := mod.PseudoVersionBaseFromTags(rootPath, "", tags)
		if pseudo := headPseudoVersion(orgRepoName, rootPath, base, oid, committedAt); pseudo != nil {
			versions = append(versions, pseudo)
		}
		return versions, false, nil
	}

	for _, subdir := range subdirs {
		modulePath, _, retryable, err := declaredModulePath(ctx, scm, orgRepoName, oid, subdir)
		if err != nil {
			return nil, retryable, fmt.Errorf("getting go.mod at HEAD for %s (subdir %q): %v", orgRepoName, subdir, err)
		}
		if modulePath == "" {
			continue
		}
		base := mod.PseudoVersionBaseFromTags(modulePath, mod.TagPrefix(subdir, modulePath), tags)
		if pseudo := headPseudoVersion(orgRepoName, modulePath, base, oid, committedAt); pseudo != nil {
			versions = append(versions, pseudo)
		}
	}
	return versions, false, nil
}

// headPseudoVersion builds the pseudo-version of modulePath at a commit, or nil
// when mod.Check rejects the result, as it does for a malformed module path like
// "tools".
func headPseudoVersion(orgRepoName, modulePath, base, oid string, committedAt time.Time) *mod.ModuleVersion {
	version := mod.PseudoVersion(modulePath, base, committedAt, oid)
	if err := mod.Check(modulePath, version); err != nil {
		slog.Debug(fmt.Sprintf("Skipping HEAD pseudo-version for %s (module %q): %v", orgRepoName, modulePath, err))
		return nil
	}
	return &mod.ModuleVersion{
		Version:    version,
		Created:    committedAt,
		ModulePath: modulePath,
	}
}

// modulePathForTag returns the module path declared for a tag's version. A v2+
// module may keep its go.mod either in the subdir its tags are prefixed with or in
// a major-version subdirectory below it, so when the subdir's own go.mod doesn't
// account for the version, the major-version subdirectory is read too. Keeping the
// subdir first means only the rarer layout pays for a second read.
//
// The results are declaredModulePath's, for whichever directory answered. A go.mod
// that doesn't match the version reads as if it weren't there. A version both
// directories account for takes the subdir's path; the go tool rejects such a repo
// outright, so neither choice would be fetchable anyway.
func modulePathForTag(ctx context.Context, scm scm, orgRepoName, tag, subdir, version string) (modulePath string, hasGoMod, retryable bool, err error) {
	modulePath, hasGoMod, retryable, err = declaredModulePath(ctx, scm, orgRepoName, tag, subdir)
	if err != nil {
		return "", false, retryable, err
	}
	if hasGoMod && mod.Check(modulePath, version) == nil {
		return modulePath, hasGoMod, false, nil
	}

	majorSubdir := mod.MajorSubdir(subdir, version)
	if majorSubdir == "" {
		return modulePath, hasGoMod, false, nil
	}
	majorPath, majorHasGoMod, retryable, err := declaredModulePath(ctx, scm, orgRepoName, tag, majorSubdir)
	if err != nil {
		return "", false, retryable, err
	}
	if majorHasGoMod && mod.Check(majorPath, version) == nil {
		return majorPath, true, false, nil
	}
	return modulePath, hasGoMod, false, nil
}

// tagNames lists the names of a repo's tags.
func tagNames(tags []github.Tag) []string {
	names := make([]string, 0, len(tags))
	for _, t := range tags {
		names = append(names, t.Name)
	}
	return names
}

// declaredModulePath returns the module path declared in the go.mod at subdir
// (the repo root when subdir is "") for the given ref (a tag or commit). hasGoMod
// is false when there's no go.mod there; modulePath is empty when the go.mod
// declares no module.
func declaredModulePath(ctx context.Context, scm scm, orgRepoName, ref, subdir string) (modulePath string, hasGoMod, retryable bool, err error) {
	content, found, retryable, err := scm.GoMod(ctx, orgRepoName, ref, subdir)
	if err != nil {
		return "", false, retryable, err
	}
	if !found {
		return "", false, false, nil
	}
	return mod.ModulePath(content), true, false, nil
}
