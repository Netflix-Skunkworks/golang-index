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

// allOwnersStore is the DB access the all-owners indexer needs; [*db.DB]
// satisfies it.
type allOwnersStore interface {
	NextReindexAllOwnersWork(ctx context.Context, reindexTTL, reindexPeriod time.Duration) (shouldReindex bool, err error)
	// StoreOwners also completes the work item; that's what gates the next pass on
	// ReindexPeriod rather than only ReindexTTL.
	StoreOwners(ctx context.Context, ownerLogins []string) error
}

// ownerLister lists all of the repository owners to index; [*github.GithubSCM]
// satisfies it.
type ownerLister interface {
	Owners(ctx context.Context) ([]string, error)
}

// AllOwnersIndexer periodically re-indexes the full list of repository owners,
// feeding new owners into the queue that [OwnerReposIndexer] drains.
type AllOwnersIndexer struct {
	DB     allOwnersStore
	Lister ownerLister

	// WorkCheckPeriod is the base wait between queue checks that find no work.
	// There is only ever one all-owners work item, so most checks find nothing.
	WorkCheckPeriod time.Duration
	// ReindexTTL is how long a worker may hold the all-owners work item before it's
	// considered abandoned and eligible for another worker.
	ReindexTTL time.Duration
	// ReindexPeriod is the minimum time between full re-indexes of the owner list.
	ReindexPeriod time.Duration

	// bo, when non-nil, overrides the default GitHub retry backoff; a test seam.
	bo *backoff.ExponentialBackOff
}

// Run re-indexes the owner list whenever the work queue says it's due, until ctx
// is cancelled (returning ctx.Err()).
func (ix *AllOwnersIndexer) Run(ctx context.Context) error {
	return runQueue(ctx, slog.Default(), ix.bo, ix.WorkCheckPeriod, ix.IndexAllOwnersOnce)
}

// IndexAllOwnersOnce performs a single all-owners pass: if the work queue says a
// reindex is due, it refreshes the stored list of repository owners. A retryable
// error is a transient GitHub failure to back off on; a non-retryable one is fatal.
func (ix *AllOwnersIndexer) IndexAllOwnersOnce(ctx context.Context) (gotWork, retryable bool, err error) {
	shouldReindex, err := ix.DB.NextReindexAllOwnersWork(ctx, ix.ReindexTTL, ix.ReindexPeriod)
	if err != nil {
		return false, false, fmt.Errorf("error fetching next reindex all owners work: %v", err)
	}
	if !shouldReindex {
		return false, false, nil
	}

	slog.Info("Should re-index all repository owners: yes")
	owners, err := ix.Lister.Owners(ctx)
	if err != nil {
		return true, true, fmt.Errorf("error fetching all repository owners: %v", err)
	}
	if err := ix.DB.StoreOwners(ctx, owners); err != nil {
		return true, false, fmt.Errorf("error storing all owners: %v", err)
	}
	slog.Info(fmt.Sprintf("Finished re-indexing all repository owners, saw %d owners", len(owners)))
	return true, false, nil
}

// ownerReposStore is the DB access the owner-repos indexer needs; [*db.DB]
// satisfies it.
type ownerReposStore interface {
	NextReindexOwnerReposWork(ctx context.Context, reindexTTL, reindexPeriod time.Duration) (ownerToReindex string, workWasFound bool, err error)
	// StoreOwnerRepos also completes the owner's work item, deferring its next
	// re-index by ReindexPeriod. An empty repos slice is legal.
	StoreOwnerRepos(ctx context.Context, ownerLogin string, orgRepoNames []string) error
}

// ownerRepoLister lists one owner's Go repos; [*github.GithubSCM] satisfies it.
type ownerRepoLister interface {
	OwnerGoRepos(ctx context.Context, ownerLogin string) ([]string, error)
}

// OwnerReposIndexer drains the owner work queue, re-indexing one owner's Go repos
// at a time and feeding them into the queue that [RepoTagsIndexer] drains. Several
// are run concurrently.
type OwnerReposIndexer struct {
	DB     ownerReposStore
	Lister ownerRepoLister

	// WorkerID identifies this worker in its log lines.
	WorkerID int

	// WorkCheckPeriod is the base wait between queue checks that find no work.
	WorkCheckPeriod time.Duration
	// ReindexTTL is how long a worker may hold an owner's work item before it's
	// considered abandoned and eligible for another worker.
	ReindexTTL time.Duration
	// ReindexPeriod is the minimum time between re-indexes of a given owner.
	ReindexPeriod time.Duration

	// bo, when non-nil, overrides the default GitHub retry backoff; a test seam.
	bo *backoff.ExponentialBackOff
}

// Run re-indexes owners' repos as the work queue hands them out, until ctx is
// cancelled (returning ctx.Err()).
func (ix *OwnerReposIndexer) Run(ctx context.Context) error {
	return runQueue(ctx, slog.With("workerID", ix.WorkerID), ix.bo, ix.WorkCheckPeriod, ix.IndexNextOwnerOnce)
}

// IndexNextOwnerOnce re-indexes at most one queued owner's Go repos. A retryable
// error is a transient GitHub failure to back off on; a non-retryable one is fatal.
func (ix *OwnerReposIndexer) IndexNextOwnerOnce(ctx context.Context) (gotWork, retryable bool, err error) {
	logger := slog.With("workerID", ix.WorkerID)

	ownerToReindex, gotWork, err := ix.DB.NextReindexOwnerReposWork(ctx, ix.ReindexTTL, ix.ReindexPeriod)
	if err != nil {
		return false, false, fmt.Errorf("error fetching next reindex owner repos work: %v", err)
	}
	if !gotWork {
		return false, false, nil
	}

	logger.Info(fmt.Sprintf("Owner repos re-indexing: got work for owner %s", ownerToReindex))
	orgRepoNames, err := ix.Lister.OwnerGoRepos(ctx, ownerToReindex)
	if err != nil {
		return true, true, fmt.Errorf("error fetching repos for %s: %v", ownerToReindex, err)
	}
	// An owner with no Go repos is stored too, which completes its work item.
	if err := ix.DB.StoreOwnerRepos(ctx, ownerToReindex, orgRepoNames); err != nil {
		return true, false, fmt.Errorf("error storing owner repos: %v", err)
	}
	logger.Info(fmt.Sprintf("Owner repos re-indexing: stored %d Go repos for owner %s", len(orgRepoNames), ownerToReindex))
	return true, false, nil
}

// repoTagsStore is the DB access the repo-tags indexer needs; [*db.DB]
// satisfies it.
type repoTagsStore interface {
	NextReindexRepoTagsWork(ctx context.Context, reindexTTL, reindexPeriod time.Duration) (repoToReindex string, workWasFound bool, err error)
	// StoreRepoModuleVersions also completes the repo's work item, which is what
	// holds its next re-index off for ReindexPeriod rather than only ReindexTTL. It
	// takes no module versions when the repo has none.
	StoreRepoModuleVersions(ctx context.Context, orgRepoName string, repoModuleVersions []*db.RepoModuleVersion) error
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

	// WorkCheckPeriod is the base wait between queue checks that find no work.
	WorkCheckPeriod time.Duration
	// ReindexTTL is how long a worker may hold a repo's work item before it's
	// considered abandoned and eligible for another worker.
	ReindexTTL time.Duration
	// ReindexPeriod is the minimum time between re-indexes of a given repo.
	ReindexPeriod time.Duration

	// bo, when non-nil, overrides the default GitHub retry backoff; a test seam.
	bo *backoff.ExponentialBackOff
}

// Run re-indexes repos' tags as the work queue hands them out, until ctx is
// cancelled (returning ctx.Err()).
func (ix *RepoTagsIndexer) Run(ctx context.Context) error {
	return runQueue(ctx, slog.With("workerID", ix.WorkerID), ix.bo, ix.WorkCheckPeriod, ix.IndexNextRepoOnce)
}

// IndexNextRepoOnce re-indexes at most one queued repo's module versions. A
// retryable error is a transient GitHub failure to back off on; a non-retryable
// one is fatal.
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
	repoModuleVersions, retryable, err := ix.repoModuleVersions(ctx, repoToReindex)
	switch {
	case err != nil && retryable:
		return true, true, fmt.Errorf("error fetching repo tags for %s: %v", repoToReindex, err)
	case err != nil:
		// A later pass would fail the same way, so the repo is indexed as holding
		// nothing rather than handed back every ReindexTTL.
		logger.Warn(fmt.Sprintf("Repo tags re-indexing: %v; indexing no module versions for it", err))
		repoModuleVersions = nil
	}
	// A repo with no module versions is stored too, which completes its work item and
	// drops any rows it used to have.
	logger.Info(fmt.Sprintf("Repo tags re-indexing: storing %d module versions for repo %s", len(repoModuleVersions), repoToReindex))
	if err := ix.DB.StoreRepoModuleVersions(ctx, repoToReindex, repoModuleVersions); err != nil {
		return true, false, fmt.Errorf("error storing repo tags: %v", err)
	}
	logger.Info(fmt.Sprintf("Repo tags re-indexing: stored %d module versions for repo %s", len(repoModuleVersions), repoToReindex))
	return true, false, nil
}

// repoModuleVersions reads a repo's module versions and shapes them into the DB
// rows to store.
func (ix *RepoTagsIndexer) repoModuleVersions(ctx context.Context, orgRepoName string) (rows []*db.RepoModuleVersion, retryable bool, err error) {
	moduleVersions, retryable, err := moduleVersionsForRepo(ctx, ix.SCM, ix.DefaultModuleHost, orgRepoName)
	if err != nil {
		return nil, retryable, err
	}

	rows = make([]*db.RepoModuleVersion, 0, len(moduleVersions))
	for _, mv := range moduleVersions {
		rows = append(rows, &db.RepoModuleVersion{
			Version:    mv.Version,
			ModulePath: mv.ModulePath,
			Created:    mv.Created,
		})
	}
	return rows, false, nil
}

// runQueue drives one stage: it calls once repeatedly until ctx is cancelled
// (returning ctx.Err()). A retryable error is logged and backed off on; a
// non-retryable one is returned. A pass that doesn't error resets the backoff,
// which otherwise only ever grows, leaving a worker that saw a few retryable
// errors sleeping MaxInterval between passes for the rest of the process's life.
// A check that found work is followed by an eager one, and a check that didn't
// waits workCheckPeriod plus a 1-60s jitter, so the workers don't all poll in
// lockstep.
func runQueue(ctx context.Context, logger *slog.Logger, bo *backoff.ExponentialBackOff, workCheckPeriod time.Duration, once func(context.Context) (gotWork, retryable bool, err error)) error {
	if bo == nil {
		bo = newGithubBackoff()
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		gotWork, retryable, err := once(ctx)
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
		bo.Reset()
		if gotWork {
			continue
		}

		waitTime := workCheckPeriod + time.Duration(rand.Intn(60)+1)*time.Second
		logger.Info(fmt.Sprintf("No work, waiting %v to check again", waitTime))
		if err := sleep(ctx, waitTime); err != nil {
			return err
		}
	}
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
