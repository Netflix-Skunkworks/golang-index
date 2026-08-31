package indexer

import (
	"context"
	"errors"
	"time"

	"github.com/Netflix-Skunkworks/golang-index/internal/db"
	"github.com/Netflix-Skunkworks/golang-index/internal/github"
)

type fakeAllOwnersStore struct {
	nextReindexAllOwnersWork func(ctx context.Context, reindexTTL, reindexPeriod time.Duration) (bool, error)
	storeOwners              func(ctx context.Context, ownerLogins []string) error
}

func (f *fakeAllOwnersStore) NextReindexAllOwnersWork(ctx context.Context, reindexTTL, reindexPeriod time.Duration) (bool, error) {
	return f.nextReindexAllOwnersWork(ctx, reindexTTL, reindexPeriod)
}

func (f *fakeAllOwnersStore) StoreOwners(ctx context.Context, ownerLogins []string) error {
	return f.storeOwners(ctx, ownerLogins)
}

type fakeOwnerLister struct {
	owners func(ctx context.Context) ([]string, error)
}

func (f *fakeOwnerLister) Owners(ctx context.Context) ([]string, error) {
	return f.owners(ctx)
}

// ownerStoreCall is one StoreOwnerRepos call a fakeOwnerReposStore recorded.
type ownerStoreCall struct {
	OwnerLogin   string
	OrgRepoNames []string
}

type fakeOwnerReposStore struct {
	nextReindexOwnerReposWork func(ctx context.Context, reindexTTL, reindexPeriod time.Duration) (ownerToReindex string, failedAttempts int, workWasFound bool, err error)
	storeOwnerRepos           func(ctx context.Context, ownerLogin string, orgRepoNames []string) error
	completeOwnerReposWork    func(ctx context.Context, ownerLogin string) error
}

func (f *fakeOwnerReposStore) NextReindexOwnerReposWork(ctx context.Context, reindexTTL, reindexPeriod time.Duration) (string, int, bool, error) {
	return f.nextReindexOwnerReposWork(ctx, reindexTTL, reindexPeriod)
}

func (f *fakeOwnerReposStore) StoreOwnerRepos(ctx context.Context, ownerLogin string, orgRepoNames []string) error {
	return f.storeOwnerRepos(ctx, ownerLogin, orgRepoNames)
}

func (f *fakeOwnerReposStore) CompleteOwnerReposWork(ctx context.Context, ownerLogin string) error {
	return f.completeOwnerReposWork(ctx, ownerLogin)
}

type fakeOwnerRepoLister struct {
	ownerGoRepos func(ctx context.Context, ownerLogin string) ([]string, error)
}

func (f *fakeOwnerRepoLister) OwnerGoRepos(ctx context.Context, ownerLogin string) ([]string, error) {
	return f.ownerGoRepos(ctx, ownerLogin)
}

// storeCall is one StoreRepoModuleVersions call a fakeRepoTagsStore recorded.
type storeCall struct {
	OrgRepoName        string
	RepoModuleVersions []*db.RepoModuleVersion
}

type fakeRepoTagsStore struct {
	nextReindexRepoTagsWork func(ctx context.Context, reindexTTL, reindexPeriod time.Duration) (repoToReindex string, failedAttempts int, workWasFound bool, err error)
	storeRepoModuleVersions func(ctx context.Context, orgRepoName string, repoModuleVersions []*db.RepoModuleVersion) error
	completeRepoTagsWork    func(ctx context.Context, orgRepoName string) error
}

func (f *fakeRepoTagsStore) NextReindexRepoTagsWork(ctx context.Context, reindexTTL, reindexPeriod time.Duration) (string, int, bool, error) {
	return f.nextReindexRepoTagsWork(ctx, reindexTTL, reindexPeriod)
}

func (f *fakeRepoTagsStore) StoreRepoModuleVersions(ctx context.Context, orgRepoName string, repoModuleVersions []*db.RepoModuleVersion) error {
	return f.storeRepoModuleVersions(ctx, orgRepoName, repoModuleVersions)
}

func (f *fakeRepoTagsStore) CompleteRepoTagsWork(ctx context.Context, orgRepoName string) error {
	return f.completeRepoTagsWork(ctx, orgRepoName)
}

// fakeSCM is an in-memory scm: it serves canned tags, HEAD, module dirs, and
// go.mod files without talking to GitHub.
type fakeSCM struct {
	tags    []github.Tag
	headOID string
	headAt  time.Time
	// goMods maps a (ref, subdir) to that go.mod's contents. An absent key has no
	// go.mod there.
	goMods   map[goModKey]string
	goModErr error
	// moduleDirs is what ModuleDirs returns: the subdirs holding a go.mod at HEAD
	// (the repo root is ""). Nil means the repo has no go.mod at all.
	moduleDirs    []string
	moduleDirsErr error
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

func (f *fakeSCM) RepoTags(context.Context, string) ([]github.Tag, bool, error) {
	f.repoTagsCalls++
	if f.repoTagsCalls <= f.repoTagsFails {
		return nil, true, errors.New("github error")
	}
	return f.tags, false, nil
}

func (f *fakeSCM) HeadCommit(context.Context, string) (string, time.Time, bool, error) {
	return f.headOID, f.headAt, false, nil
}

func (f *fakeSCM) GoMod(_ context.Context, _, ref, subdir string) ([]byte, bool, bool, error) {
	if f.goModErr != nil {
		return nil, false, false, f.goModErr
	}
	if content, ok := f.goMods[goModKey{ref, subdir}]; ok {
		return []byte(content), true, false, nil
	}
	return nil, false, false, nil
}

func (f *fakeSCM) ModuleDirs(_ context.Context, _, _ string) ([]string, bool, error) {
	if f.moduleDirsErr != nil {
		return nil, false, f.moduleDirsErr
	}
	return f.moduleDirs, false, nil
}
