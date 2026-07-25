package indexer

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/Netflix-Skunkworks/golang-index/internal/db"
	"github.com/cenkalti/backoff/v5"
)

// allReposStore is the DB access the all-repos indexer needs; [*db.DB]
// satisfies it.
type allReposStore interface {
	NextReindexAllReposWork(ctx context.Context, reindexTTL, reindexPeriod time.Duration) (shouldReindex bool, err error)
	StoreRepos(ctx context.Context, orgRepoNames []string) error
}

// repoLister lists all of the Go repos to index; [*github.GithubSCM] satisfies
// it.
type repoLister interface {
	GoRepos(ctx context.Context) ([]string, error)
}

// AllReposIndexer periodically re-indexes the full list of Go repos, feeding
// new repos into the queue that [RepoTagsIndexer] drains.
type AllReposIndexer struct {
	DB     allReposStore
	Lister repoLister

	// WorkCheckPeriod is how long to wait between checks of the work queue. There
	// is only ever one all-repos work item, so there's no point checking eagerly.
	WorkCheckPeriod time.Duration
	// ReindexTTL is how long a worker may hold the all-repos work item before it's
	// considered abandoned and eligible for another worker.
	ReindexTTL time.Duration
	// ReindexPeriod is the minimum time between full re-indexes of the repo list.
	ReindexPeriod time.Duration

	// bo, when non-nil, replaces the default GitHub retry backoff; tests set it to
	// a fast backoff so retry paths don't sleep.
	bo *backoff.ExponentialBackOff
}

// Run re-indexes the repo list whenever the work queue says it's due, until ctx
// is cancelled (returning ctx.Err()). A GitHub error is transient (back off and
// retry); a DB error is fatal (return it, tearing down the process).
func (ix *AllReposIndexer) Run(ctx context.Context) error {
	bo := ix.bo
	if bo == nil {
		bo = newGithubBackoff()
	}

	for {
		retryable, err := ix.IndexAllReposOnce(ctx)
		if err != nil {
			if !retryable {
				return err
			}
			// TODO(jbarkhuysen): Add some metrics/alerting here.
			slog.Error(err.Error())
			if err := sleep(ctx, bo.NextBackOff()); err != nil {
				return err
			}
			continue
		}
		if err := sleep(ctx, ix.WorkCheckPeriod); err != nil {
			return err
		}
	}
}

// IndexAllReposOnce performs a single all-repos pass: if the work queue says a
// reindex is due, it refreshes the stored list of Go repos. [AllReposIndexer.Run]
// calls it in a loop. A retryable error is a transient GitHub failure the caller
// should back off on; a non-retryable error is fatal.
func (ix *AllReposIndexer) IndexAllReposOnce(ctx context.Context) (retryable bool, err error) {
	shouldReindex, err := ix.DB.NextReindexAllReposWork(ctx, ix.ReindexTTL, ix.ReindexPeriod)
	if err != nil {
		return false, fmt.Errorf("error fetching next reindex all repos work: %v", err)
	}
	if !shouldReindex {
		slog.Info(fmt.Sprintf("Should re-index all Go repos: no, waiting %v to check again", ix.WorkCheckPeriod))
		return false, nil
	}

	slog.Info("Should re-index all Go repos: yes")
	allRepos, err := ix.Lister.GoRepos(ctx)
	if err != nil {
		return true, fmt.Errorf("error fetching all Go repos: %v", err)
	}
	if err := ix.DB.StoreRepos(ctx, allRepos); err != nil {
		return false, fmt.Errorf("error storing all repos: %v", err)
	}
	slog.Info(fmt.Sprintf("Finished re-indexing all Go repos, saw %d repos", len(allRepos)))
	return false, nil
}

// repoTagsStore is the DB access the repo-tags indexer needs; [*db.DB]
// satisfies it.
type repoTagsStore interface {
	NextReindexRepoTagsWork(ctx context.Context, reindexTTL, reindexPeriod time.Duration) (repoToReindex string, workWasFound bool, err error)
	StoreRepoModuleVersions(ctx context.Context, repoModuleVersions []*db.RepoModuleVersion) error
}

// RepoTagsIndexer drains the repo work queue, re-indexing one repo's module
// versions at a time. Several are run concurrently.
type RepoTagsIndexer struct {
	DB  repoTagsStore
	SCM scm

	// DefaultModuleHost is the module host used to build repo-derived module paths (e.g.
	// "github.mycompany.net").
	DefaultModuleHost string

	// WorkerID identifies this worker in its log lines.
	WorkerID int

	// WorkCheckPeriod is how long to wait before re-checking the queue after
	// finding no work; a 1-60s jitter is added. When work is found the next check
	// is eager, with no wait.
	WorkCheckPeriod time.Duration
	// ReindexTTL is how long a worker may hold a repo's work item before it's
	// considered abandoned and eligible for another worker.
	ReindexTTL time.Duration
	// ReindexPeriod is the minimum time between re-indexes of a given repo.
	ReindexPeriod time.Duration

	// bo, when non-nil, replaces the default GitHub retry backoff; tests set it to
	// a fast backoff so retry paths don't sleep.
	bo *backoff.ExponentialBackOff
}

// Run re-indexes repos' tags as the work queue hands them out, until ctx is
// cancelled (returning ctx.Err()). A GitHub error is transient (back off and
// retry); a DB error is fatal (return it, tearing down the process).
func (ix *RepoTagsIndexer) Run(ctx context.Context) error {
	logger := slog.With("workerID", ix.WorkerID)
	bo := ix.bo
	if bo == nil {
		bo = newGithubBackoff()
	}

	for {
		gotWork, retryable, err := ix.IndexNextRepoOnce(ctx)
		if err != nil {
			if !retryable {
				return err
			}
			// TODO(jbarkhuysen): Add some metrics/alerting here.
			logger.Error(err.Error())
			if err := sleep(ctx, bo.NextBackOff()); err != nil {
				return err
			}
			continue
		}
		if gotWork {
			// Eagerly check for new work rather than waiting again.
			continue
		}

		// Jitter the wait so the workers don't all poll in lockstep.
		jitter := time.Duration(rand.Intn(60)+1) * time.Second
		waitTime := ix.WorkCheckPeriod + jitter
		logger.Info(fmt.Sprintf("Repo tags re-indexing: no work, waiting %v to check again", waitTime))
		if err := sleep(ctx, waitTime); err != nil {
			return err
		}
	}
}

// IndexNextRepoOnce re-indexes at most one queued repo's module versions.
// gotWork reports whether a repo was claimed from the queue. [RepoTagsIndexer.Run]
// calls it in a loop. A retryable error is a transient GitHub failure the caller
// should back off on; a non-retryable error is fatal.
func (ix *RepoTagsIndexer) IndexNextRepoOnce(ctx context.Context) (gotWork, retryable bool, err error) {
	logger := slog.With("workerID", ix.WorkerID)

	repoToReindex, gotWork, err := ix.DB.NextReindexRepoTagsWork(ctx, ix.ReindexTTL, ix.ReindexPeriod)
	if err != nil {
		return false, false, fmt.Errorf("error fetching next reindex repo tags work: %v", err)
	}
	if !gotWork {
		return false, false, nil
	}

	logger.Info(fmt.Sprintf("Repo tags re-indexing: got work for repo %s", repoToReindex))
	repoModuleVersions, err := ix.repoModuleVersions(ctx, repoToReindex)
	if err != nil {
		return true, true, fmt.Errorf("error fetching repo tags for %s: %v", repoToReindex, err)
	}
	if len(repoModuleVersions) == 0 {
		return true, false, nil
	}

	logger.Info(fmt.Sprintf("Repo tags re-indexing: storing %d module versions for repo %s", len(repoModuleVersions), repoToReindex))
	if err := ix.DB.StoreRepoModuleVersions(ctx, repoModuleVersions); err != nil {
		return true, false, fmt.Errorf("error storing repo tags: %v", err)
	}
	logger.Info(fmt.Sprintf("Repo tags re-indexing: stored %d module versions for repo %s", len(repoModuleVersions), repoToReindex))
	return true, false, nil
}

// repoModuleVersions reads a repo's module versions and shapes them into the DB rows to
// store. The error is a transient GitHub error for the caller to back off on.
func (ix *RepoTagsIndexer) repoModuleVersions(ctx context.Context, orgRepoName string) ([]*db.RepoModuleVersion, error) {
	moduleVersions, err := moduleVersionsForRepo(ctx, ix.SCM, ix.DefaultModuleHost, orgRepoName)
	if err != nil {
		return nil, err
	}

	repoModuleVersions := make([]*db.RepoModuleVersion, 0, len(moduleVersions))
	for _, mv := range moduleVersions {
		repoModuleVersions = append(repoModuleVersions, &db.RepoModuleVersion{
			OrgRepoName: orgRepoName,
			Version:     mv.Version,
			ModulePath:  mv.ModulePath,
			Created:     mv.Created,
		})
	}
	return repoModuleVersions, nil
}

// newGithubBackoff returns a fresh retry backoff for GitHub errors, ranging from
// 30s to 5m and growing 1.5x per attempt. Each worker uses its own, so their
// retry state stays independent.
func newGithubBackoff() *backoff.ExponentialBackOff {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = 30 * time.Second
	b.Multiplier = 1.5
	b.MaxInterval = 5 * time.Minute
	return b
}

// sleep waits for d, returning ctx.Err() if ctx is cancelled first.
func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
