package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type playgroundDeleteRepository struct {
	AccountRepository
	mu          sync.Mutex
	account     *Account
	deleteErr   error
	getErr      error
	deleted     chan struct{}
	afterDelete func()
}

func (r *playgroundDeleteRepository) GetByID(context.Context, int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return nil, r.getErr
	}
	if r.account == nil {
		return nil, ErrAccountNotFound
	}
	copy := *r.account
	return &copy, nil
}
func (r *playgroundDeleteRepository) ListShadowsByParent(context.Context, int64) ([]*Account, error) {
	return nil, nil
}
func (r *playgroundDeleteRepository) Delete(context.Context, int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deleteErr != nil {
		return r.deleteErr
	}
	r.account = nil
	if r.afterDelete != nil {
		r.afterDelete()
	}
	if r.deleted != nil {
		close(r.deleted)
		r.deleted = nil
	}
	return nil
}

func playgroundCleanupFixture(t *testing.T) (*adminServiceImpl, *playgroundDeleteRepository, *miniredis.Miniredis, string, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("DATA_DIR", root)
	t.Setenv("PLAYGROUND_PROFILE_DIR", "")
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	browser := NewPlaygroundBrowserService(&aistudio.SessionCache{Client: client}, nil)
	t.Cleanup(browser.Close)
	profile := uuid.NewString()
	path, err := browser.manager.ProfilePath(profile)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "aistudio", "profiles", profile), path)
	require.NoError(t, os.MkdirAll(path, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(path, "cookies.sqlite"), []byte("synthetic"), 0600))
	repo := &playgroundDeleteRepository{account: &Account{ID: 37, Platform: PlatformGemini, Type: AccountTypePlayground, Credentials: map[string]any{"playground_profile_id": profile}}}
	return &adminServiceImpl{accountRepo: repo, playgroundBrowser: browser}, repo, server, profile, path
}

func TestPlaygroundDeleteAccountRemovesProfileAndAllCacheVariants(t *testing.T) {
	admin, _, server, profile, path := playgroundCleanupFixture(t)
	neighbor := filepath.Join(filepath.Dir(path), uuid.NewString())
	require.NoError(t, os.Mkdir(neighbor, 0700))
	for _, key := range []string{playgroundCacheKey(profile, "proxy-a", "a"), playgroundCacheKey(profile, "proxy-b", "a") + ":login-required"} {
		require.NoError(t, server.Set(key, "synthetic"))
	}
	otherKey := playgroundCacheKey(uuid.NewString(), "", "neighbor")
	require.NoError(t, server.Set(otherKey, "keep"))
	require.NoError(t, admin.DeleteAccount(context.Background(), 37))
	require.NoDirExists(t, path)
	require.DirExists(t, neighbor)
	require.Equal(t, []string{otherKey}, server.Keys())
	marker, err := admin.playgroundBrowser.cleanupPath(profile)
	require.NoError(t, err)
	require.NoFileExists(t, marker)
}

func TestPlaygroundDeleteDatabaseFailurePreservesProfile(t *testing.T) {
	admin, repo, _, profile, path := playgroundCleanupFixture(t)
	repo.deleteErr = errors.New("database unavailable")
	require.ErrorContains(t, admin.DeleteAccount(context.Background(), 37), "database unavailable")
	admin.playgroundBrowser.retryProfileCleanup(context.Background(), repo)
	require.FileExists(t, filepath.Join(path, "cookies.sqlite"))
	// A retry must be able to replace the existing cleanup intent on Windows too.
	repo.deleteErr = nil
	require.NoError(t, admin.DeleteAccount(context.Background(), 37))
	require.NoDirExists(t, path)
	marker, err := admin.playgroundBrowser.cleanupPath(profile)
	require.NoError(t, err)
	require.NoFileExists(t, marker)
}

func TestPlaygroundDeleteWaitsForActiveProfileLease(t *testing.T) {
	admin, repo, _, profile, path := playgroundCleanupFixture(t)
	lease, err := admin.playgroundBrowser.Cache.Acquire(context.Background(), profile)
	require.NoError(t, err)
	defer lease.Release(context.Background())
	deleted := make(chan struct{})
	repo.deleted = deleted
	result := make(chan error, 1)
	go func() { result <- admin.DeleteAccount(context.Background(), 37) }()
	select {
	case <-deleted:
	case <-time.After(3 * time.Second):
		t.Fatal("account was not deleted")
	}
	select {
	case err := <-result:
		t.Fatalf("cleanup did not wait for the active request: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	require.DirExists(t, path)
	lease.Release(context.Background())
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup did not resume")
	}
	require.NoDirExists(t, path)
}

func TestPlaygroundPendingDeletionSurvivesRestartAndReadFailure(t *testing.T) {
	admin, repo, _, profile, path := playgroundCleanupFixture(t)
	require.NoError(t, admin.playgroundBrowser.prepareProfileCleanup(profile, 37))
	// Model a crash after DB commit and before filesystem cleanup.
	repo.account = nil
	repo.getErr = errors.New("database temporarily unavailable")
	admin.playgroundBrowser.retryProfileCleanup(context.Background(), repo)
	require.DirExists(t, path)
	repo.getErr = nil
	restarted := NewPlaygroundBrowserService(admin.playgroundBrowser.Cache, nil)
	restarted.startProfileCleanup(repo)
	defer restarted.Close()
	require.Eventually(t, func() bool { _, err := os.Stat(path); return errors.Is(err, os.ErrNotExist) }, 3*time.Second, 20*time.Millisecond)
}

func TestPlaygroundDeleteRejectsInvalidProfileBeforeDatabaseDeletion(t *testing.T) {
	admin, repo, _, _, path := playgroundCleanupFixture(t)
	repo.account.Credentials["playground_profile_id"] = "../runtime"
	require.ErrorContains(t, admin.DeleteAccount(context.Background(), 37), "invalid Camoufox profile ID")
	require.NotNil(t, repo.account)
	require.DirExists(t, path)
}

func TestPlaygroundDeleteRedisFailureQueuesRetry(t *testing.T) {
	admin, repo, server, profile, path := playgroundCleanupFixture(t)
	repo.afterDelete = func() { server.SetError("ERR unavailable") }
	require.ErrorContains(t, admin.DeleteAccount(context.Background(), 37), "cleanup pending automatic retry")
	require.Nil(t, repo.account)
	require.DirExists(t, path)
	marker, err := admin.playgroundBrowser.cleanupPath(profile)
	require.NoError(t, err)
	require.FileExists(t, marker)
	server.SetError("")
	admin.playgroundBrowser.retryProfileCleanup(context.Background(), repo)
	require.NoDirExists(t, path)
	require.NoFileExists(t, marker)
}

func TestPlaygroundDeleteOnlyCleansAfterTransactionCommit(t *testing.T) {
	for _, mode := range []string{"commit", "rollback", "commit_failure"} {
		t.Run(mode, func(t *testing.T) {
			admin, repo, _, _, path := playgroundCleanupFixture(t)
			original := repo.account
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			mock.ExpectBegin()
			tx, err := client.Tx(context.Background())
			require.NoError(t, err)
			require.NoError(t, admin.DeleteAccount(dbent.NewTxContext(context.Background(), tx), 37))
			require.DirExists(t, path, "uncommitted deletion must not remove login state")
			switch mode {
			case "commit":
				mock.ExpectCommit()
				require.NoError(t, tx.Commit())
				require.NoDirExists(t, path)
			case "rollback":
				mock.ExpectRollback()
				require.NoError(t, tx.Rollback())
			case "commit_failure":
				mock.ExpectCommit().WillReturnError(errors.New("commit failed"))
				require.Error(t, tx.Commit())
			}
			if mode != "commit" {
				repo.account = original // model the database's rolled-back state
				admin.playgroundBrowser.retryProfileCleanup(context.Background(), repo)
				require.DirExists(t, path)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
