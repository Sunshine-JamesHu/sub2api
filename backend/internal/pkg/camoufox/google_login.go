package camoufox

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/mail"
	"net/url"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/pquerna/otp/totp"
)

//go:embed google_login.js
var googleLoginInspector string

type LoginPage struct {
	Origin string `json:"origin"`
	Path   string `json:"path"`
	Phase  string `json:"phase"`
	Reason string `json:"reason"`
	Token  string `json:"token"`
	Key    string `json:"key"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
}

type GoogleLoginCredentials struct{ Email, Password, TOTP string }
type GoogleLoginProgress struct {
	Submitted map[string]time.Time `json:"submitted"`
}
type GoogleLoginResult struct {
	State  aistudio.StorageState
	Email  string
	Manual bool
	Reason string
}

type GoogleLoginBrowser interface {
	LoginPage(context.Context, string) (LoginPage, error)
	FillLoginField(context.Context, LoginPage, string) error
	ClickLoginTarget(context.Context, LoginPage, bool) error
	Capture(context.Context, string) (aistudio.StorageState, string, error)
}

func validGoogleResumeURL(value string) bool {
	if len(value) > 16384 {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	switch u.Hostname() {
	case "accounts.google.com", "aistudio.google.com", "gemini.google.com", "myaccount.google.com", "gds.google.com":
		return true
	}
	return false
}
func (b *Browser) LoginURL(ctx context.Context) (string, error) {
	value, err := b.Evaluate(ctx, `location.href`)
	if err != nil {
		return "", err
	}
	if !validGoogleResumeURL(value) {
		return "", nil
	}
	return value, nil
}
func (m *Manager) StartManualResume(ctx context.Context, profile, proxy, resume string) (*Browser, error) {
	if !validGoogleResumeURL(resume) {
		resume = aistudio.AIStudioOrigin + "/prompts/new_chat"
	}
	return m.start(ctx, profile, proxy, 1, resume)
}

func CanonicalGoogleEmail(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	parts := strings.Split(value, "@")
	if len(parts) == 2 && (parts[1] == "gmail.com" || parts[1] == "googlemail.com") {
		parts[0] = strings.SplitN(parts[0], "+", 2)[0]
		return strings.ReplaceAll(parts[0], ".", "") + "@gmail.com"
	}
	return value
}

func (b *Browser) LoginPage(ctx context.Context, email string) (LoginPage, error) {
	var result LoginPage
	encoded, _ := json.Marshal(email)
	raw, err := b.Evaluate(ctx, "("+googleLoginInspector+")("+string(encoded)+")")
	if err != nil {
		return result, err
	}
	err = json.Unmarshal([]byte(raw), &result)
	return result, err
}

func (b *Browser) loginTarget(ctx context.Context, page LoginPage, focus bool, next bool) (bool, error) {
	args, _ := json.Marshal(map[string]any{"token": page.Token, "origin": page.Origin, "focus": focus, "next": next})
	expression := `(() => {
 const a = ` + string(args) + `, t = window.__sub2LoginTarget;
 if (!t || t.token !== a.token || location.href !== t.href || location.origin !== a.origin || window !== window.top) return 'false';
 let e=t.element;
 if (a.next) e=document.querySelector(t.next) || [...document.querySelectorAll('button')].find(x=>/^(Next|下一步|下一步驟)$/i.test(x.innerText.trim()));
 if (!e || !e.isConnected || e.disabled || !e.getClientRects().length) return 'false';
 if (a.focus) { if (location.origin !== 'https://accounts.google.com') return 'false'; e.focus(); e.select(); }
 else { e.scrollIntoView({block:'center'}); const r=e.getBoundingClientRect(); window.__sub2ClickPoint={x:Math.round(r.left+r.width/2),y:Math.round(r.top+r.height/2)}; }
 return 'true';
})()`
	value, err := b.Evaluate(ctx, expression)
	return value == "true", err
}

func (b *Browser) FillLoginField(ctx context.Context, page LoginPage, value string) error {
	if page.Origin != "https://accounts.google.com" || (page.Phase != "email" && page.Phase != "password" && page.Phase != "totp") {
		return errors.New("login origin is not trusted")
	}
	ok, err := b.loginTarget(ctx, page, true, false)
	if err != nil || !ok {
		return errors.New("login field changed before keyboard input")
	}
	if err = b.Input(ctx, Input{Type: "key", Key: "Backspace"}); err != nil {
		return errors.New("login keyboard unavailable")
	}
	check := `(() => { const t=window.__sub2LoginTarget; return t && location.origin==='https://accounts.google.com' && location.href===t.href && document.activeElement===t.element && t.element.value.length===0 ? 'true':'false' })()`
	if v, e := b.Evaluate(ctx, check); e != nil || v != "true" {
		return errors.New("login field lost focus")
	}
	if err = b.Input(ctx, Input{Type: "text", Text: value}); err != nil {
		return errors.New("login keyboard delivery uncertain")
	}
	length := len(utf16.Encode([]rune(value)))
	want, _ := json.Marshal(length)
	check = `(() => { const t=window.__sub2LoginTarget; return t && location.origin==='https://accounts.google.com' && location.href===t.href && t.element.isConnected && t.element.value.length===` + string(want) + ` ? 'true':'false' })()`
	if v, e := b.Evaluate(ctx, check); e != nil || v != "true" {
		return errors.New("login keyboard delivery uncertain")
	}
	return b.Input(ctx, Input{Type: "key", Key: "Tab"})
}

func (b *Browser) ClickLoginTarget(ctx context.Context, page LoginPage, next bool) error {
	optionalSettings := !next && page.Phase == "optional_skip" && page.Key == "optional-selfie-skip" &&
		(page.Origin == "https://myaccount.google.com" || page.Origin == "https://gds.google.com")
	if page.Origin != "https://accounts.google.com" && page.Origin != aistudio.AIStudioOrigin && !optionalSettings {
		return errors.New("login origin is not trusted")
	}
	ok, err := b.loginTarget(ctx, page, false, next)
	if err != nil || !ok {
		return errors.New("login button changed")
	}
	raw, err := b.Evaluate(ctx, `JSON.stringify(window.__sub2ClickPoint)`)
	if err != nil {
		return err
	}
	var point struct {
		X int `json:"x"`
		Y int `json:"y"`
	}
	if json.Unmarshal([]byte(raw), &point) != nil {
		return errors.New("login button unavailable")
	}
	return b.Input(ctx, Input{Type: "click", X: point.X, Y: point.Y})
}

// AutomateGoogleLogin ports the workbench's guarded, one-shot keyboard flow.
// Checkpoint runs before credential dispatch; a crash cannot silently resubmit
// the same password or OTP. Challenges are handed to the human operator.
func AutomateGoogleLogin(ctx context.Context, browser GoogleLoginBrowser, credentials GoogleLoginCredentials, progress GoogleLoginProgress, autoOnboarding bool, checkpoint func(string, GoogleLoginProgress) error) (GoogleLoginResult, error) {
	if _, err := mail.ParseAddress(credentials.Email); err != nil {
		return GoogleLoginResult{}, errors.New("invalid account email")
	}
	if progress.Submitted == nil {
		progress.Submitted = map[string]time.Time{}
	}
	wait := func(d time.Duration) error {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	unknownSince := time.Now()
	previous := ""
	for {
		if err := ctx.Err(); err != nil {
			return GoogleLoginResult{}, err
		}
		page, err := browser.LoginPage(ctx, credentials.Email)
		if err != nil {
			return GoogleLoginResult{Manual: true, Reason: "browser_unavailable"}, nil
		}
		if page.Phase != previous {
			previous = page.Phase
			unknownSince = time.Now()
		}
		if (page.Origin == aistudio.AIStudioOrigin || page.Origin == GeminiOrigin) && page.Phase == "studio" {
			state, email, captureErr := browser.Capture(ctx, "")
			if captureErr == nil {
				if CanonicalGoogleEmail(email) != CanonicalGoogleEmail(credentials.Email) {
					return GoogleLoginResult{Reason: "identity_mismatch"}, nil
				}
				return GoogleLoginResult{State: state, Email: email}, nil
			}
		}
		switch page.Phase {
		case "manual":
			return GoogleLoginResult{Manual: true, Reason: page.Reason}, nil
		case "failed":
			return GoogleLoginResult{Reason: page.Reason}, nil
		case "email", "password", "totp":
			if page.Origin != "https://accounts.google.com" {
				return GoogleLoginResult{Manual: true, Reason: "unexpected_page"}, nil
			}
			if at, sent := progress.Submitted[page.Phase]; sent {
				if time.Since(at) > 40*time.Second {
					return GoogleLoginResult{Manual: true, Reason: "submission_unconfirmed"}, nil
				}
				break
			}
			value := credentials.Email
			if page.Phase == "password" {
				value = credentials.Password
			}
			if page.Phase == "totp" {
				if credentials.TOTP == "" {
					return GoogleLoginResult{Manual: true, Reason: "totp_required"}, nil
				}
				remaining := 30 - time.Now().Unix()%30
				if remaining < 8 {
					if err = wait(time.Duration(remaining)*time.Second + 300*time.Millisecond); err != nil {
						return GoogleLoginResult{}, err
					}
				}
				value, err = totp.GenerateCode(credentials.TOTP, time.Now())
				if err != nil {
					return GoogleLoginResult{Reason: "invalid_totp_secret"}, nil
				}
			}
			if value == "" {
				return GoogleLoginResult{Manual: true, Reason: "credentials_required"}, nil
			}
			progress.Submitted[page.Phase] = time.Now()
			if err = checkpoint(page.Phase, progress); err != nil {
				return GoogleLoginResult{}, err
			}
			if err = browser.FillLoginField(ctx, page, value); err != nil {
				return GoogleLoginResult{Manual: true, Reason: "submission_unconfirmed"}, nil
			}
			if err = browser.ClickLoginTarget(ctx, page, true); err != nil {
				return GoogleLoginResult{Manual: true, Reason: "submission_unconfirmed"}, nil
			}
		case "choose_account", "other_account", "choose_totp", "optional_skip", "onboarding_check", "onboarding_continue":
			if page.Phase == "choose_totp" && credentials.TOTP == "" {
				return GoogleLoginResult{Manual: true, Reason: "totp_required"}, nil
			}
			if strings.HasPrefix(page.Phase, "onboarding_") && !autoOnboarding {
				return GoogleLoginResult{Manual: true, Reason: "onboarding"}, nil
			}
			key := page.Phase + ":" + page.Path + ":" + page.Key
			if at, done := progress.Submitted[key]; done {
				if time.Since(at) > 40*time.Second {
					return GoogleLoginResult{Manual: true, Reason: "submission_unconfirmed"}, nil
				}
				break
			}
			progress.Submitted[key] = time.Now()
			if err = checkpoint(page.Phase, progress); err != nil {
				return GoogleLoginResult{}, err
			}
			if err = browser.ClickLoginTarget(ctx, page, false); err != nil {
				return GoogleLoginResult{Manual: true, Reason: "page_changed"}, nil
			}
		case "waiting", "studio":
			if time.Since(unknownSince) > 45*time.Second {
				return GoogleLoginResult{Manual: true, Reason: "login_not_ready"}, nil
			}
		case "onboarding_wait":
			if !autoOnboarding || time.Since(unknownSince) > 40*time.Second {
				return GoogleLoginResult{Manual: true, Reason: "onboarding"}, nil
			}
		default:
			return GoogleLoginResult{Manual: true, Reason: "unexpected_page"}, nil
		}
		if err = wait(1500 * time.Millisecond); err != nil {
			return GoogleLoginResult{}, err
		}
	}
}
