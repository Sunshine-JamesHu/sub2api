package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
)

// Cleanup intents survive a crash between database commit and filesystem
// removal. They contain identifiers only and live in the same durable volume.
type playgroundProfileCleanup struct {
	AccountID int64 `json:"account_id"`
}

func (s *PlaygroundBrowserService) cleanupPath(profile string) (string, error) {
	directory, err := s.manager.ProfilePath(profile)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(directory), ".pending-deletions", profile+".json"), nil
}

func (s *PlaygroundBrowserService) prepareProfileCleanup(profile string, accountID int64) error {
	path, err := s.cleanupPath(profile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	raw, err := json.Marshal(playgroundProfileCleanup{AccountID: accountID})
	if err != nil {
		return err
	}
	// Publish atomically: a retry worker must never observe a partial record.
	file, err := os.CreateTemp(filepath.Dir(path), ".cleanup-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(raw)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func (s *PlaygroundBrowserService) cleanupProfile(ctx context.Context, repo AccountRepository, profile string, accountID int64) error {
	path, err := s.cleanupPath(profile)
	if err != nil {
		return err
	}
	lease, err := s.Cache.Acquire(ctx, profile)
	if err != nil {
		return err
	}
	leaseCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(8 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				renewCtx, stop := context.WithTimeout(leaseCtx, 5*time.Second)
				err := lease.Renew(renewCtx)
				stop()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		<-done
		releaseCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		lease.Release(releaseCtx)
	}()
	// Always verify against committed state (callers must not pass a transaction
	// context). A failed/rolled-back deletion must preserve the browser profile.
	_, err = repo.GetByID(leaseCtx, accountID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrAccountNotFound) {
		return err
	}
	if err := aistudio.DefaultWAAManager().CloseCached(leaseCtx, strconv.FormatInt(accountID, 10)); err != nil {
		return err
	}
	if err := lease.DeleteProfileSessions(leaseCtx); err != nil {
		return err
	}
	if err := lease.Renew(leaseCtx); err != nil {
		return err
	}
	if err := s.manager.RemoveProfile(profile); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *PlaygroundBrowserService) retryProfileCleanup(ctx context.Context, repo AccountRepository) {
	entries, err := os.ReadDir(filepath.Join(s.manager.Root, ".pending-deletions"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		slog.Warn("read pending Playground profile deletions", "error", err)
		return
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		profile := strings.TrimSuffix(entry.Name(), ".json")
		path, err := s.cleanupPath(profile)
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var record playgroundProfileCleanup
		if json.Unmarshal(raw, &record) != nil || record.AccountID <= 0 {
			continue
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err = s.cleanupProfile(attemptCtx, repo, profile, record.AccountID)
		cancel()
		if err != nil {
			slog.Warn("Playground profile deletion pending retry", "account_id", record.AccountID, "error", err)
		}
	}
}

func (s *PlaygroundBrowserService) startProfileCleanup(repo AccountRepository) {
	ctx, cancel := context.WithCancel(context.Background())
	s.cleanupCancel, s.cleanupDone = cancel, make(chan struct{})
	go func() {
		defer close(s.cleanupDone)
		s.retryProfileCleanup(ctx, repo)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.retryProfileCleanup(ctx, repo)
			}
		}
	}()
}

func (s *adminServiceImpl) deleteAccountAndProfile(ctx context.Context, id int64) error {
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	profile := account.GetCredential("playground_profile_id")
	if account.Platform != PlatformGemini || account.Type != AccountTypePlayground || profile == "" {
		return s.accountRepo.Delete(ctx, id)
	}
	if s.playgroundBrowser == nil || s.playgroundBrowser.Cache == nil {
		return errors.New("Playground profile cleanup service unavailable")
	}
	if err := s.playgroundBrowser.prepareProfileCleanup(profile, id); err != nil {
		return fmt.Errorf("prepare Playground profile deletion: %w", err)
	}
	if err := s.accountRepo.Delete(ctx, id); err != nil {
		return err
	}
	var cleanupErr error
	AfterAccountCommit(ctx, func(context.Context) {
		// Finish even if the HTTP client disconnects, using a fresh context so
		// repository reads cannot accidentally reuse the committed transaction.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cleanupErr = s.playgroundBrowser.cleanupProfile(cleanupCtx, s.accountRepo, profile, id)
		if cleanupErr != nil {
			slog.Warn("Playground account deleted; profile cleanup queued for retry", "account_id", id, "error", cleanupErr)
		}
	})
	if cleanupErr != nil {
		return fmt.Errorf("account deleted; Playground profile cleanup pending automatic retry: %w", cleanupErr)
	}
	return nil
}
