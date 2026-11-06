package service

import (
	"archive/zip"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
)

func (s *PlaygroundBrowserService) ExportProfile(ctx context.Context, writer *zip.Writer, profile string) error {
	if s == nil || s.manager == nil || s.Cache == nil {
		return errors.New("Playground browser service is unavailable")
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
	if err = s.manager.WriteProfileArchive(leaseCtx, writer, profile); err != nil {
		return err
	}
	return lease.Renew(leaseCtx)
}

// ImportProfile creates a new durable profile and uses the normal one-time grant
// path to authorize account creation. The rollback is only for failed creation.
func (s *PlaygroundBrowserService) ImportProfile(ctx context.Context, archive *zip.Reader, source, email string, proxyID *int64) (token string, rollback func(), err error) {
	if s == nil || s.manager == nil || s.Cache == nil {
		return "", nil, errors.New("Playground browser service is unavailable")
	}
	if !strings.Contains(email, "@") {
		return "", nil, errors.New("Playground archive is missing the account email")
	}
	proxy, err := s.proxyURL(ctx, proxyID)
	if err != nil {
		return "", nil, err
	}
	profile, err := s.manager.RestoreProfileArchive(ctx, archive, source)
	if err != nil {
		return "", nil, err
	}
	token, err = s.Cache.IssueGrant(ctx, aistudio.BrowserGrant{Profile: profile, Email: email, Proxy: aistudio.ComputePayloadDigest(proxy)})
	if err != nil {
		_ = s.manager.RemoveProfile(profile)
		return "", nil, err
	}
	rollback = func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Cache.Client.Del(cleanupCtx, "playground:login:"+token).Err()
		_ = s.manager.RemoveProfile(profile)
	}
	return token, rollback, nil
}
