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
type scm interface {
	RepoTags(ctx context.Context, orgRepoName string) ([]github.Tag, error)
	HeadCommit(ctx context.Context, orgRepoName string) (oid string, committed time.Time, err error)
	GoMod(ctx context.Context, orgRepoName, ref, subdir string) (content []byte, found bool, err error)
	ModuleDirs(ctx context.Context, orgRepoName, ref string) (subdirs []string, err error)
}

// moduleVersionsForRepo returns a module version for each of a repo's semver
// tags, skipping tags that don't resolve to one. A repo with no versions from
// tags falls back to pseudo-versions synthesized from HEAD, one per module.
// host is the module host used to build repo-derived module paths
// (e.g. "github.mycompany.net").
func moduleVersionsForRepo(ctx context.Context, scm scm, host, orgRepoName string) ([]*mod.ModuleVersion, error) {
	tags, err := scm.RepoTags(ctx, orgRepoName)
	if err != nil {
		return nil, err
	}

	var versions []*mod.ModuleVersion
	for _, t := range tags {
		subdir, version, ok := mod.ModuleVersionFromTag(t.Name)
		if !ok {
			slog.Debug(fmt.Sprintf("Skipping tag %q for %s: not a module version", t.Name, orgRepoName))
			continue
		}

		modulePath := mod.RepoModulePath(host, orgRepoName, subdir)

		declaredPath, hasGoMod, err := declaredModulePath(ctx, scm, orgRepoName, t.Name, subdir)
		switch {
		case err != nil:
			slog.Error(fmt.Sprintf("Error getting go.mod file for %s (tag %q): %v; defaulting to GitHub URL for module path", orgRepoName, t.Name, err))
		case !hasGoMod:
			slog.Info(fmt.Sprintf("Unable to find go.mod file for %s (tag %q); defaulting to GitHub URL for module path", orgRepoName, t.Name))
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
		pseudos, err := headPseudoVersions(ctx, scm, host, orgRepoName)
		if err != nil {
			return nil, err
		}
		versions = append(versions, pseudos...)
	}

	return versions, nil
}

// headPseudoVersions synthesizes a pseudo-version for every module in the repo
// at HEAD, used when a repo has no semver tags. Multi-module repos get one per
// module, not just the root. A repo with no module directory at HEAD still gets
// one, at the repo URL, which is the path a tag on such a repo resolves to. It
// returns nil for a repo with no commit, and skips any module whose go.mod
// declares no path or whose pseudo-version isn't valid for that path.
func headPseudoVersions(ctx context.Context, scm scm, host, orgRepoName string) ([]*mod.ModuleVersion, error) {
	oid, committedAt, err := scm.HeadCommit(ctx, orgRepoName)
	if err != nil {
		return nil, err
	}
	if oid == "" {
		return nil, nil
	}

	subdirs, err := scm.ModuleDirs(ctx, orgRepoName, oid)
	if err != nil {
		return nil, err
	}

	var versions []*mod.ModuleVersion
	if len(subdirs) == 0 {
		slog.Info(fmt.Sprintf("No module directory at HEAD for %s; defaulting to GitHub URL for module path", orgRepoName))
		rootPath := mod.RepoModulePath(host, orgRepoName, "")
		if pseudo := headPseudoVersion(orgRepoName, rootPath, oid, committedAt); pseudo != nil {
			versions = append(versions, pseudo)
		}
		return versions, nil
	}

	for _, subdir := range subdirs {
		modulePath, _, err := declaredModulePath(ctx, scm, orgRepoName, oid, subdir)
		if err != nil {
			return nil, fmt.Errorf("getting go.mod at HEAD for %s (subdir %q): %v", orgRepoName, subdir, err)
		}
		if modulePath == "" {
			continue
		}
		if pseudo := headPseudoVersion(orgRepoName, modulePath, oid, committedAt); pseudo != nil {
			versions = append(versions, pseudo)
		}
	}
	return versions, nil
}

// headPseudoVersion builds the pseudo-version of modulePath at a commit, or nil
// when mod.Check rejects the result, as it does for a malformed module path like
// "tools".
func headPseudoVersion(orgRepoName, modulePath, oid string, committedAt time.Time) *mod.ModuleVersion {
	version := mod.PseudoVersion(modulePath, oid, committedAt)
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

// declaredModulePath returns the module path declared in the go.mod at subdir
// (the repo root when subdir is "") for the given ref (a tag or commit). hasGoMod
// is false when there's no go.mod there; modulePath is empty when the go.mod
// declares no module.
func declaredModulePath(ctx context.Context, scm scm, orgRepoName, ref, subdir string) (modulePath string, hasGoMod bool, err error) {
	content, found, err := scm.GoMod(ctx, orgRepoName, ref, subdir)
	if err != nil {
		return "", false, err
	}
	if !found {
		return "", false, nil
	}
	return mod.ModulePath(content), true, nil
}
