package indexer

import (
	"context"
	"errors"
	"time"

	"github.com/Netflix-Skunkworks/golang-index/internal/db"
	"github.com/Netflix-Skunkworks/golang-index/internal/github"
)

// fakeAllReposStore implements allReposStore.
type fakeAllReposStore struct {
	nextReindexAllReposWork func(ctx context.Context, reindexTTL, reindexPeriod time.Duration) (bool, error)
	storeRepos              func(ctx context.Context, orgRepoNames []string) error
}

func (f *fakeAllReposStore) NextReindexAllReposWork(ctx context.Context, reindexTTL, reindexPeriod time.Duration) (bool, error) {
	return f.nextReindexAllReposWork(ctx, reindexTTL, reindexPeriod)
}

func (f *fakeAllReposStore) StoreRepos(ctx context.Context, orgRepoNames []string) error {
	return f.storeRepos(ctx, orgRepoNames)
}

// fakeRepoLister implements repoLister.
type fakeRepoLister struct {
	goRepos func(ctx context.Context) ([]string, error)
}

func (f *fakeRepoLister) GoRepos(ctx context.Context) ([]string, error) {
	return f.goRepos(ctx)
}

// fakeRepoTagsStore implements repoTagsStore.
type fakeRepoTagsStore struct {
	nextReindexRepoTagsWork func(ctx context.Context, reindexTTL, reindexPeriod time.Duration) (repoToReindex string, workWasFound bool, err error)
	storeRepoTags           func(ctx context.Context, repoTags []*db.RepoTag) error
}

func (f *fakeRepoTagsStore) NextReindexRepoTagsWork(ctx context.Context, reindexTTL, reindexPeriod time.Duration) (string, bool, error) {
	return f.nextReindexRepoTagsWork(ctx, reindexTTL, reindexPeriod)
}

func (f *fakeRepoTagsStore) StoreRepoTags(ctx context.Context, repoTags []*db.RepoTag) error {
	return f.storeRepoTags(ctx, repoTags)
}

// fakeSCM is an in-memory scm: it serves canned tags, HEAD, and go.mod files
// without talking to GitHub.
type fakeSCM struct {
	tags    []github.Tag
	headOID string
	headAt  time.Time
	// goMods maps a ref (a tag name or commit oid) to its go.mod contents. A ref
	// that's absent has no go.mod.
	goMods map[string]string
	// repoTagsFails makes the first repoTagsFails RepoTags calls return an error,
	// to exercise transient-error handling. repoTagsCalls counts calls made.
	repoTagsFails int
	repoTagsCalls int
}

func (f *fakeSCM) RepoTags(context.Context, string) ([]github.Tag, error) {
	f.repoTagsCalls++
	if f.repoTagsCalls <= f.repoTagsFails {
		return nil, errors.New("github error")
	}
	return f.tags, nil
}

func (f *fakeSCM) HeadCommit(context.Context, string) (string, time.Time, error) {
	return f.headOID, f.headAt, nil
}

func (f *fakeSCM) GoMod(_ context.Context, _, ref, _ string) ([]byte, bool, error) {
	if content, ok := f.goMods[ref]; ok {
		return []byte(content), true, nil
	}
	return nil, false, nil
}
