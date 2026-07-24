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
// tags. A repo with no semver tags falls back to a single pseudo-version
// synthesized from HEAD. host is the module host used to build repo-derived
// module paths (e.g. "github.mycompany.net").
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

		goModModulePath, found, err := declaredModulePath(ctx, scm, orgRepoName, t.Name, subdir)
		switch {
		case err != nil:
			slog.Error(fmt.Sprintf("Error getting go.mod file for %s (tag %s): %v; defaulting to GitHub URL for module path", orgRepoName, t.Name, err))
		case found:
			modulePath = goModModulePath
		default:
			slog.Info(fmt.Sprintf("Unable to find go.mod file for %s (tag %s); defaulting to GitHub URL for module path", orgRepoName, t.Name))
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
		pseudos, err := headPseudoVersions(ctx, scm, orgRepoName)
		if err != nil {
			return nil, err
		}
		versions = append(versions, pseudos...)
	}

	return versions, nil
}

// headPseudoVersions synthesizes a pseudo-version for every module in the repo
// at HEAD, used when a repo has no semver tags. Multi-module repos get one per
// module, not just the root. It returns nil when the repo has no commit or no
// module with a valid module path (an empty repo, or one with no go.mod).
func headPseudoVersions(ctx context.Context, scm scm, orgRepoName string) ([]*mod.ModuleVersion, error) {
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
	for _, subdir := range subdirs {
		modulePath, found, err := declaredModulePath(ctx, scm, orgRepoName, oid, subdir)
		if err != nil {
			return nil, fmt.Errorf("error getting go.mod at HEAD for %s (subdir %q): %v", orgRepoName, subdir, err)
		}
		if !found {
			continue
		}

		version := mod.PseudoVersion(modulePath, oid, committedAt)
		if err := mod.Check(modulePath, version); err != nil {
			slog.Debug(fmt.Sprintf("Skipping HEAD pseudo-version for %s (subdir %q): %v", orgRepoName, subdir, err))
			continue
		}

		versions = append(versions, &mod.ModuleVersion{
			Version:    version,
			Created:    committedAt,
			ModulePath: modulePath,
		})
	}
	return versions, nil
}

// declaredModulePath returns the module path declared in the go.mod at subdir
// (the repo root when subdir is "") for the given ref (a tag or commit). found
// is false, with a nil error, when there's no go.mod or it declares no module.
func declaredModulePath(ctx context.Context, scm scm, orgRepoName, ref, subdir string) (string, bool, error) {
	content, found, err := scm.GoMod(ctx, orgRepoName, ref, subdir)
	if err != nil {
		return "", false, err
	}
	if !found {
		return "", false, nil
	}

	modulePath := mod.ModulePath(content)
	if modulePath == "" {
		return "", false, nil
	}

	return modulePath, true, nil
}
