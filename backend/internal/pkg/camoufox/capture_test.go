package camoufox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestCaptureAccountControlsAndFailureReasons(t *testing.T) {
	for _, tc := range []struct {
		name, origin, pathname, email, identity, expected, cookie, failure string
		interactive                                                        bool
	}{
		{name: "account control fallback", origin: aistudio.AIStudioOrigin, email: "selected@example.test", identity: "account-control", cookie: "synthetic"},
		{name: "page data", origin: aistudio.AIStudioOrigin, email: "selected@example.test", identity: "page-data", cookie: "synthetic", expected: "Selected@example.test"},
		{name: "login page", origin: "https://accounts.google.com", pathname: "/v3/signin/identifier", email: "selected@example.test", failure: "[not-ai-studio]", interactive: true},
		{name: "cookie check redirect", origin: "https://accounts.google.com", pathname: "/CheckCookie", failure: "[not-ai-studio]"},
		{name: "missing account", origin: aistudio.AIStudioOrigin, identity: "missing", failure: "[account-email-missing]"},
		{name: "ambiguous account", origin: aistudio.AIStudioOrigin, identity: "ambiguous", failure: "[account-ambiguous]"},
		{name: "wrong saved account", origin: aistudio.AIStudioOrigin, email: "other@example.test", expected: "selected@example.test", failure: "does not match"},
		{name: "missing cookie", origin: aistudio.AIStudioOrigin, email: "selected@example.test", failure: "[session-cookie-missing]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page, err := json.Marshal(map[string]any{"origin": tc.origin, "pathname": tc.pathname, "email": tc.email, "identity": tc.identity, "storage": []any{}})
			require.NoError(t, err)
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				for {
					var command struct {
						ID     int            `json:"id"`
						Method string         `json:"method"`
						Params map[string]any `json:"params"`
					}
					if conn.ReadJSON(&command) != nil {
						return
					}
					var result any
					switch command.Method {
					case "script.evaluate":
						result = map[string]any{"type": "success", "result": map[string]string{"type": "string", "value": string(page)}}
					case "storage.getCookies":
						if _, filtered := command.Params["partition"]; filtered {
							t.Error("cookie export must include cross-domain cookies in the isolated profile")
						}
						result = map[string]any{"cookies": []any{map[string]any{"name": "SAPISID", "value": map[string]string{"type": "string", "value": tc.cookie}, "domain": ".google.com", "path": "/", "secure": true}}}
					default:
						return
					}
					if conn.WriteJSON(map[string]any{"id": command.ID, "result": result}) != nil {
						return
					}
				}
			}))
			defer server.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			require.NoError(t, err)
			defer conn.Close()
			fingerprint, err := aistudio.NewBrowserFingerprint()
			require.NoError(t, err)
			browser := &Browser{conn: conn, tab: "test-tab", fingerprint: fingerprint}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			state, email, err := browser.Capture(ctx, tc.expected)
			if tc.failure != "" {
				require.ErrorContains(t, err, tc.failure)
				require.Empty(t, email)
				if strings.HasPrefix(tc.failure, "[") {
					require.ErrorIs(t, err, aistudio.ErrAuthenticationRequired)
					var captureErr *captureStateError
					require.ErrorAs(t, err, &captureErr)
					require.Equal(t, tc.interactive, captureErr.interactive)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, "selected@example.test", email)
			require.Len(t, state.Cookies, 1)
		})
	}
}

// Run in the existing opt-in smoke browser, which is already on a synthetic
// data: page. These fixtures never touch a real Google account or session.
func checkCaptureIdentityInBrowser(t *testing.T, ctx context.Context, browser *Browser) {
	t.Helper()
	for _, tc := range []struct{ name, html, wiz, email, identity string }{
		{"aria account", `<textarea>wrong@example.test</textarea><button aria-label="Google Account: Name (selected@example.test)">Account</button>`, `{}`, "selected@example.test", "account-control"},
		{"Chinese account", `<button aria-label="Google 账号：用户 (SELECTED@example.test)">账号</button>`, `{}`, "selected@example.test", "account-control"},
		{"localized account link", `<a role="button" href="https://accounts.google.com/SignOutOptions?hl=de" aria-label="Google-Konto: selected@example.test">Konto</a>`, `{}`, "selected@example.test", "account-control"},
		{"prompt email is not identity", `<p>selected@example.test</p><textarea>selected@example.test</textarea>`, `{}`, "", "missing"},
		{"prompt account link is not identity", `<a href="https://myaccount.google.com">selected@example.test</a>`, `{}`, "", "missing"},
		{"chat account control is not identity", `<ms-chat-turn><a role="button" href="https://accounts.google.com/SignOutOptions" aria-label="Google Account: selected@example.test">Account</a></ms-chat-turn>`, `{}`, "", "missing"},
		{"untrusted link", `<a href="https://accounts.google.com.example.test/SignOutOptions" aria-label="selected@example.test">Account</a>`, `{}`, "", "missing"},
		{"hidden account", `<button hidden aria-label="Google Account: hidden@example.test">Account</button>`, `{}`, "", "missing"},
		{"ambiguous accounts", `<button aria-label="Google Account: first@example.test">One</button><button aria-label="Google Account: second@example.test">Two</button>`, `{}`, "", "ambiguous"},
		{"multiple emails in one control", `<button aria-label="Google Account: first@example.test second@example.test">Accounts</button>`, `{}`, "", "ambiguous"},
		{"page data takes precedence", `<button aria-label="Google Account: other@example.test">Other</button>`, `{"oPEP7c":"selected@example.test"}`, "selected@example.test", "page-data"},
		{"invalid page data falls back", `<button aria-label="Google Account: selected@example.test">Account</button>`, `{"oPEP7c":123}`, "selected@example.test", "account-control"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			html, err := json.Marshal(tc.html)
			require.NoError(t, err)
			_, err = browser.Evaluate(ctx, "document.body.innerHTML="+string(html)+";window.WIZ_global_data="+tc.wiz+";'ready'")
			require.NoError(t, err)
			raw, err := browser.Evaluate(ctx, capturePageExpression)
			require.NoError(t, err)
			var page struct {
				Email    string `json:"email"`
				Identity string `json:"identity"`
			}
			require.NoError(t, json.Unmarshal([]byte(raw), &page))
			require.Equal(t, tc.email, page.Email)
			require.Equal(t, tc.identity, page.Identity)
		})
	}
}
