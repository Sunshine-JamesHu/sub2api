package camoufox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
)

// Only a confirmed Google sign-in/challenge page may suppress further browser
// recoveries. Page loading and network deadlines are retryable failures.
var ErrInteractiveLoginRequired = fmt.Errorf("%w: [browser-login-required] Google sign-in or verification is required; reauthorize the account", aistudio.ErrAuthenticationRequired)

type captureStateError struct {
	reason      string
	detail      string
	interactive bool
}

func (e *captureStateError) Error() string {
	return fmt.Sprintf("%s: [%s] %s", aistudio.ErrAuthenticationRequired, e.reason, e.detail)
}

func (e *captureStateError) Unwrap() error { return aistudio.ErrAuthenticationRequired }

func isInteractiveLoginPage(origin, pathname string) bool {
	if origin != "https://accounts.google.com" {
		return false
	}
	for _, prefix := range []string{"signin", "v3/signin", "v3/signin/challenge", "ServiceLogin", "AccountChooser", "InteractiveLogin", "challenge"} {
		if pathname == "/"+prefix || strings.HasPrefix(pathname, "/"+prefix+"/") {
			return true
		}
	}
	return false
}

// Recovery must not turn an unavailable volume or a changed data path into a
// fresh empty profile and then tell the operator that Google logged them out.
func (m *Manager) requireRecoveryProfile(profile string) error {
	directory, err := m.ProfilePath(profile)
	if err != nil {
		return err
	}
	info, err := os.Stat(filepath.Join(directory, "cookies.sqlite"))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("Camoufox [profile-missing]: saved browser cookie database is missing at %s; check DATA_DIR/PLAYGROUND_PROFILE_DIR and the persistent volume, or import the account ZIP backup", directory)
	}
	if err != nil {
		return fmt.Errorf("Camoufox [profile-unreadable]: check browser profile volume permissions: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("Camoufox [profile-empty]: saved browser cookie database is empty; restore the account ZIP or reauthorize it")
	}
	return nil
}

func pollRecoveredState(parent context.Context, timeout, interval time.Duration, capture func(context.Context) (aistudio.StorageState, string, error)) (aistudio.StorageState, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var last *captureStateError
	for {
		state, _, err := capture(ctx)
		if err == nil {
			return state, nil
		}
		// Preserve external cancellation even if an earlier capture saw sign-in.
		if parent.Err() != nil {
			return aistudio.StorageState{}, parent.Err()
		}
		if ctx.Err() == nil && !errors.Is(err, aistudio.ErrAuthenticationRequired) {
			return aistudio.StorageState{}, err
		}
		var current *captureStateError
		// An interrupted/unreadable page is not evidence that a previous
		// sign-in redirect is still the current page.
		errors.As(err, &current)
		last = current
		select {
		case <-ctx.Done():
			if parent.Err() != nil {
				return aistudio.StorageState{}, parent.Err()
			}
			if last != nil && last.interactive {
				return aistudio.StorageState{}, ErrInteractiveLoginRequired
			}
			reason := "page-not-ready"
			if last != nil {
				reason = last.reason
			}
			return aistudio.StorageState{}, fmt.Errorf("Camoufox [recovery-timeout:%s]: browser session recovery timed out; check proxy connectivity and retry: %w", reason, ctx.Err())
		case <-ticker.C:
		}
	}
}
