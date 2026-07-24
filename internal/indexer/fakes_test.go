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

// fakeSCM is an in-memory scm: it serves canned tags, HEAD, module dirs, and
// go.mod files without talking to GitHub.
type fakeSCM struct {
	tags    []github.Tag
	headOID string
	headAt  time.Time
	// goMods maps a (ref, subdir) to that go.mod's contents. An absent key has no
	// go.mod there.
	goMods map[goModKey]string
	// moduleDirs is what ModuleDirs returns: the subdirs holding a go.mod at HEAD
	// (the repo root is "").
	moduleDirs []string
	// repoTagsFails makes the first repoTagsFails RepoTags calls return an error,
	// to exercise transient-error handling. repoTagsCalls counts calls made.
	repoTagsFails int
	repoTagsCalls int
}

// goModKey identifies a go.mod by ref (a tag name or commit oid) and the subdir
// it lives in (the repo root is "").
type goModKey struct {
	ref    string
	subdir string
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

func (f *fakeSCM) GoMod(_ context.Context, _, ref, subdir string) ([]byte, bool, error) {
	if content, ok := f.goMods[goModKey{ref, subdir}]; ok {
		return []byte(content), true, nil
	}
	return nil, false, nil
}

func (f *fakeSCM) ModuleDirs(_ context.Context, _, _ string) ([]string, error) {
	return f.moduleDirs, nil
}
