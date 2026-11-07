package camoufox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRecoveryLoadingTimeoutIsRetryable(t *testing.T) {
	_, err := pollRecoveredState(context.Background(), 20*time.Millisecond, time.Millisecond, func(context.Context) (aistudio.StorageState, string, error) {
		return aistudio.StorageState{}, "", &captureStateError{reason: "account-email-missing", detail: "page is still loading"}
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotErrorIs(t, err, aistudio.ErrAuthenticationRequired)
	require.NotErrorIs(t, err, ErrInteractiveLoginRequired)
	require.ErrorContains(t, err, "account-email-missing")
}

func TestRecoveryConfirmedLoginAndTransientRedirect(t *testing.T) {
	t.Run("confirmed sign-in", func(t *testing.T) {
		_, err := pollRecoveredState(context.Background(), 20*time.Millisecond, time.Millisecond, func(context.Context) (aistudio.StorageState, string, error) {
			return aistudio.StorageState{}, "", &captureStateError{reason: "not-ai-studio", interactive: true}
		})
		require.ErrorIs(t, err, ErrInteractiveLoginRequired)
		require.ErrorIs(t, err, aistudio.ErrAuthenticationRequired)
	})
	t.Run("redirect completes", func(t *testing.T) {
		calls := 0
		state, err := pollRecoveredState(context.Background(), time.Second, time.Millisecond, func(context.Context) (aistudio.StorageState, string, error) {
			calls++
			if calls == 1 {
				return aistudio.StorageState{}, "", &captureStateError{reason: "not-ai-studio", interactive: true}
			}
			return aistudio.StorageState{Cookies: []aistudio.StateCookie{{Name: "synthetic"}}}, "test@example.test", nil
		})
		require.NoError(t, err)
		require.Len(t, state.Cookies, 1)
	})
	t.Run("redirect ends on slow workbench", func(t *testing.T) {
		calls := 0
		_, err := pollRecoveredState(context.Background(), 20*time.Millisecond, time.Millisecond, func(context.Context) (aistudio.StorageState, string, error) {
			calls++
			return aistudio.StorageState{}, "", &captureStateError{reason: "loading", interactive: calls == 1}
		})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.NotErrorIs(t, err, ErrInteractiveLoginRequired)
	})
}

func TestRecoveryCancellationAndNetworkErrorNeverRequireLogin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	_, err := pollRecoveredState(ctx, time.Second, time.Millisecond, func(context.Context) (aistudio.StorageState, string, error) {
		cancel()
		return aistudio.StorageState{}, "", &captureStateError{reason: "not-ai-studio", interactive: true}
	})
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrInteractiveLoginRequired)
	networkErr := errors.New("proxy connection unavailable")
	_, err = pollRecoveredState(context.Background(), time.Second, time.Millisecond, func(context.Context) (aistudio.StorageState, string, error) {
		return aistudio.StorageState{}, "", networkErr
	})
	require.ErrorIs(t, err, networkErr)
	require.NotErrorIs(t, err, ErrInteractiveLoginRequired)
}

func TestRecoveryUnobservablePageDoesNotReuseEarlierLoginEvidence(t *testing.T) {
	calls := 0
	_, err := pollRecoveredState(context.Background(), 100*time.Millisecond, time.Millisecond, func(ctx context.Context) (aistudio.StorageState, string, error) {
		calls++
		if calls == 1 {
			return aistudio.StorageState{}, "", &captureStateError{reason: "not-ai-studio", interactive: true}
		}
		<-ctx.Done()
		return aistudio.StorageState{}, "", ctx.Err()
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotErrorIs(t, err, ErrInteractiveLoginRequired)
}

func TestRecoveryMissingProfileDoesNotCreateAnEmptyProfile(t *testing.T) {
	manager := NewManager("", t.TempDir())
	defer manager.Close()
	profile := uuid.NewString()
	_, err := manager.Recover(context.Background(), profile, "", "test@example.test")
	require.ErrorContains(t, err, "[profile-missing]")
	require.NotErrorIs(t, err, aistudio.ErrAuthenticationRequired)
	directory, err := manager.ProfilePath(profile)
	require.NoError(t, err)
	require.NoDirExists(t, directory)
	require.NoError(t, os.Mkdir(directory, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "cookies.sqlite"), nil, 0600))
	require.ErrorContains(t, manager.requireRecoveryProfile(profile), "[profile-empty]")
}

func TestInteractiveLoginPageClassification(t *testing.T) {
	for _, pathname := range []string{"/v3/signin/identifier", "/signin/v2/challenge", "/ServiceLogin", "/AccountChooser", "/challenge/pwd"} {
		require.True(t, isInteractiveLoginPage("https://accounts.google.com", pathname), pathname)
	}
	for _, page := range [][2]string{{"about:blank", ""}, {"https://aistudio.google.com", "/prompts/new_chat"}, {"https://accounts.google.com", "/CheckCookie"}, {"https://accounts.google.com.attacker.test", "/signin"}} {
		require.False(t, isInteractiveLoginPage(page[0], page[1]))
	}
}
