package camoufox

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/stretchr/testify/require"
)

type fakeGoogleLoginPage struct {
	pages       []LoginPage
	index       int
	typed       []string
	captured    string
	submissions int
}

func (f *fakeGoogleLoginPage) LoginPage(context.Context, string) (LoginPage, error) {
	return f.pages[min(f.index, len(f.pages)-1)], nil
}
func (f *fakeGoogleLoginPage) FillLoginField(_ context.Context, _ LoginPage, value string) error {
	f.typed = append(f.typed, value)
	return nil
}
func (f *fakeGoogleLoginPage) ClickLoginTarget(context.Context, LoginPage, bool) error {
	f.submissions++
	f.index++
	return nil
}
func (f *fakeGoogleLoginPage) Capture(context.Context, string) (aistudio.StorageState, string, error) {
	if f.pages[min(f.index, len(f.pages)-1)].Phase == "studio" {
		return aistudio.StorageState{}, f.captured, nil
	}
	return aistudio.StorageState{}, "", errors.New("not ready")
}

func TestGoogleLoginGuardedEmailPasswordTOTPFlow(t *testing.T) {
	page := &fakeGoogleLoginPage{pages: []LoginPage{{Origin: "https://accounts.google.com", Phase: "email"}, {Origin: "https://accounts.google.com", Phase: "password"}, {Origin: "https://accounts.google.com", Phase: "totp"}, {Origin: aistudio.AIStudioOrigin, Phase: "studio"}}, captured: "owner@gmail.com"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	checkpoints := []string{}
	result, err := AutomateGoogleLogin(ctx, page, GoogleLoginCredentials{Email: "Owner@gmail.com", Password: "synthetic-password", TOTP: "JBSWY3DPEHPK3PXP"}, GoogleLoginProgress{}, false, func(step string, progress GoogleLoginProgress) error {
		require.Contains(t, progress.Submitted, step)
		require.Equal(t, len(checkpoints), len(page.typed))
		checkpoints = append(checkpoints, step)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, "owner@gmail.com", result.Email)
	require.Equal(t, []string{"email", "password", "totp"}, checkpoints)
	require.Equal(t, 3, page.submissions)
	require.Equal(t, "Owner@gmail.com", page.typed[0])
	require.Equal(t, "synthetic-password", page.typed[1])
	require.True(t, regexp.MustCompile(`^\d{6}$`).MatchString(page.typed[2]))
}

func TestGoogleLoginStopsForChallengeWrongIdentityAndUntrustedOrigin(t *testing.T) {
	for _, tc := range []struct {
		name     string
		page     LoginPage
		captured string
		reason   string
		manual   bool
	}{
		{"captcha", LoginPage{Origin: "https://accounts.google.com", Phase: "manual", Reason: "captcha"}, "", "captcha", true},
		{"unexpected origin", LoginPage{Origin: "https://example.test", Phase: "password"}, "", "unexpected_page", true},
		{"wrong password", LoginPage{Origin: "https://accounts.google.com", Phase: "failed", Reason: "invalid_credentials"}, "", "invalid_credentials", false},
		{"wrong identity", LoginPage{Origin: aistudio.AIStudioOrigin, Phase: "studio"}, "someoneelse@gmail.com", "identity_mismatch", false},
		{"missing TOTP", LoginPage{Origin: "https://accounts.google.com", Phase: "totp"}, "", "totp_required", true},
		{"unconfirmed terms", LoginPage{Origin: aistudio.AIStudioOrigin, Phase: "onboarding_check"}, "", "onboarding", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := &fakeGoogleLoginPage{pages: []LoginPage{tc.page}, captured: tc.captured}
			r, err := AutomateGoogleLogin(context.Background(), page, GoogleLoginCredentials{Email: "owner@gmail.com", Password: "never-dispatch"}, GoogleLoginProgress{}, false, func(string, GoogleLoginProgress) error { return nil })
			require.NoError(t, err)
			require.Equal(t, tc.reason, r.Reason)
			require.Equal(t, tc.manual, r.Manual)
			require.Empty(t, page.typed)
			require.Zero(t, page.submissions)
		})
	}
}

func TestGoogleLoginDoesNotRepeatAnUnconfirmedPassword(t *testing.T) {
	page := &fakeGoogleLoginPage{pages: []LoginPage{{Origin: "https://accounts.google.com", Phase: "password"}}}
	r, err := AutomateGoogleLogin(context.Background(), page, GoogleLoginCredentials{Email: "owner@gmail.com", Password: "never-resend"}, GoogleLoginProgress{Submitted: map[string]time.Time{"password": time.Now().Add(-time.Minute)}}, false, func(string, GoogleLoginProgress) error { return nil })
	require.NoError(t, err)
	require.True(t, r.Manual)
	require.Equal(t, "submission_unconfirmed", r.Reason)
	require.Empty(t, page.typed)
}

func TestGoogleLoginNewAccountOnboardingAndExistingSession(t *testing.T) {
	for _, phases := range [][]string{{"onboarding_check", "onboarding_continue", "optional_skip", "studio"}, {"studio"}} {
		pages := make([]LoginPage, len(phases))
		for i, phase := range phases {
			pages[i] = LoginPage{Origin: aistudio.AIStudioOrigin, Path: "/onboarding", Phase: phase, Key: phase}
		}
		browser := &fakeGoogleLoginPage{pages: pages, captured: "owner@gmail.com"}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		result, err := AutomateGoogleLogin(ctx, browser, GoogleLoginCredentials{Email: "owner@gmail.com"}, GoogleLoginProgress{}, true, func(string, GoogleLoginProgress) error { return nil })
		cancel()
		require.NoError(t, err)
		require.False(t, result.Manual)
		require.Equal(t, "owner@gmail.com", result.Email)
		require.Equal(t, len(phases)-1, browser.submissions)
		require.Empty(t, browser.typed)
	}
}

func TestGoogleLoginCheckpointFailurePreventsCredentialDispatch(t *testing.T) {
	page := &fakeGoogleLoginPage{pages: []LoginPage{{Origin: "https://accounts.google.com", Phase: "password"}}}
	_, err := AutomateGoogleLogin(context.Background(), page, GoogleLoginCredentials{Email: "owner@gmail.com", Password: "never-dispatch"}, GoogleLoginProgress{}, false, func(string, GoogleLoginProgress) error { return errors.New("disk full") })
	require.ErrorContains(t, err, "disk full")
	require.Empty(t, page.typed)
	require.Zero(t, page.submissions)
}

func TestGoogleKeyboardRejectsOtherOriginsBeforeUsingBrowser(t *testing.T) {
	var browser Browser
	err := browser.FillLoginField(context.Background(), LoginPage{Origin: "http://accounts.google.com", Phase: "password"}, "synthetic")
	require.ErrorContains(t, err, "not trusted")
	err = browser.FillLoginField(context.Background(), LoginPage{Origin: "https://myaccount.google.com", Phase: "password"}, "synthetic")
	require.ErrorContains(t, err, "not trusted")
	for _, page := range []LoginPage{
		{Origin: "https://myaccount.google.com", Phase: "password"},
		{Origin: "https://myaccount.google.com", Phase: "optional_skip", Key: "unknown"},
		{Origin: "https://gds.google.com", Phase: "onboarding_continue", Key: "optional-selfie-skip"},
	} {
		require.ErrorContains(t, browser.ClickLoginTarget(context.Background(), page, false), "not trusted")
	}
	require.Equal(t, "user@gmail.com", CanonicalGoogleEmail("U.s.e.r+tag@Googlemail.com"))
	require.NotEqual(t, CanonicalGoogleEmail("user@example.test"), CanonicalGoogleEmail("u.ser@example.test"))
}

func TestGoogleResumeURLsAreLimitedToVerifiedGoogleOrigins(t *testing.T) {
	for _, u := range []string{"https://accounts.google.com/signin/challenge?state=synthetic", "https://aistudio.google.com/prompts/new_chat", "https://myaccount.google.com/verification"} {
		require.True(t, validGoogleResumeURL(u))
	}
	for _, u := range []string{"http://accounts.google.com/", "https://accounts.google.com.evil.test/", "https://accounts.google.com@evil.test/", "https://accounts.google.com:8443/", "file:///etc/passwd", "javascript:alert(1)"} {
		require.False(t, validGoogleResumeURL(u))
	}
}
