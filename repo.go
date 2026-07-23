package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Netflix-Skunkworks/golang-index/internal/github"
	"github.com/Netflix-Skunkworks/golang-index/internal/mod"
)

// scm is the source-control access needed to read a repo's module versions;
// *github.GithubSCM satisfies it.
type scm interface {
	RepoTags(ctx context.Context, orgRepoName string) ([]github.Tag, error)
	HeadCommit(ctx context.Context, orgRepoName string) (oid string, committed time.Time, err error)
	GoMod(ctx context.Context, orgRepoName, ref, subdir string) (content []byte, found bool, err error)
}

// moduleVersionsForRepo returns a module version for each of a repo's semver
// tags. A repo with no semver tags falls back to a single pseudo-version
// synthesized from HEAD.
func moduleVersionsForRepo(ctx context.Context, scm scm, orgRepoName string) ([]*mod.ModuleVersion, error) {
	tags, err := scm.RepoTags(ctx, orgRepoName)
	if err != nil {
		return nil, err
	}

	var versions []*mod.ModuleVersion
	for _, t := range tags {
		subdir, version, ok := mod.ModuleVersionFromTag(t.Name)
		if !ok {
			slog.Debug(fmt.Sprintf("skipping tag %q for %s: not a module version", t.Name, orgRepoName))
			continue
		}

		modulePath := mod.RepoModulePath(*githubHostName, orgRepoName, subdir)

		goModModulePath, found, err := declaredModulePath(ctx, scm, orgRepoName, t.Name, subdir)
		switch {
		case err != nil:
			slog.Error(fmt.Sprintf("error getting go.mod file for %s (tag %s): %v. Defaulting to github url for module path", orgRepoName, t.Name, err))
		case found:
			modulePath = goModModulePath
		default:
			slog.Info(fmt.Sprintf("unable to find go.mod file for %s (tag %s). Defaulting to github url for module path", orgRepoName, t.Name))
		}

		if err := mod.Check(modulePath, version); err != nil {
			slog.Debug(fmt.Sprintf("skipping tag %q for %s: %v", t.Name, orgRepoName, err))
			continue
		}

		versions = append(versions, &mod.ModuleVersion{
			Version:    version,
			Created:    t.Date,
			ModulePath: modulePath,
		})
	}

	if len(versions) == 0 {
		pseudo, err := headPseudoVersion(ctx, scm, orgRepoName)
		if err != nil {
			return nil, err
		}
		if pseudo != nil {
			versions = append(versions, pseudo)
		}
	}

	return versions, nil
}

// headPseudoVersion builds a ModuleVersion for the root module at HEAD, used
// when a repo has no semver tags. It returns nil when the repo has no commit, no
// root go.mod, or a go.mod with an invalid module path.
func headPseudoVersion(ctx context.Context, scm scm, orgRepoName string) (*mod.ModuleVersion, error) {
	oid, committedAt, err := scm.HeadCommit(ctx, orgRepoName)
	if err != nil {
		return nil, err
	}
	if oid == "" {
		return nil, nil
	}

	modulePath, found, err := declaredModulePath(ctx, scm, orgRepoName, oid, "")
	if err != nil {
		return nil, fmt.Errorf("error getting go.mod at HEAD for %s: %v", orgRepoName, err)
	}
	if !found {
		return nil, nil
	}

	version := mod.PseudoVersion(modulePath, oid, committedAt)
	if err := mod.Check(modulePath, version); err != nil {
		slog.Debug(fmt.Sprintf("skipping HEAD pseudo-version for %s: %v", orgRepoName, err))
		return nil, nil
	}

	return &mod.ModuleVersion{
		Version:    version,
		Created:    committedAt,
		ModulePath: modulePath,
	}, nil
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
