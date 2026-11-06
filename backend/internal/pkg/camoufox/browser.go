// Package camoufox runs short-lived browsers over WebDriver BiDi. Persistent
// profiles, not exported cookie files, are the durable login recovery source.
package camoufox

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio/proxydial"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type Manager struct {
	Executable, Root string
	slots            chan struct{}
	mu               sync.Mutex
	active           map[*Browser]struct{}
	life             context.Context
	cancel           context.CancelFunc
}

func NewManager(executable, root string) *Manager {
	life, cancel := context.WithCancel(context.Background())
	return &Manager{Executable: executable, Root: root, slots: make(chan struct{}, 2), active: map[*Browser]struct{}{}, life: life, cancel: cancel}
}

type browserProcess struct {
	command *exec.Cmd
	tree    browserTree
}
type Browser struct {
	process     *browserProcess
	done        chan struct{}
	conn        *websocket.Conn
	mu          sync.Mutex
	id          int
	tab         string
	fingerprint map[string]any
	relay       *proxydial.Relay
	once        sync.Once
	release     func()
}

var endpointPattern = regexp.MustCompile(`ws://(?:127\.0\.0\.1|localhost|\[::1\]):[0-9]+(?:/[^\s]*)?`)

type commandError struct{ Method, Code, Detail string }

func (e *commandError) Error() string { return "Camoufox " + e.Method + ": " + e.Code }

func (m *Manager) ProfilePath(profile string) (string, error) {
	id, err := uuid.Parse(profile)
	if err != nil || id.String() != profile {
		return "", errors.New("invalid Camoufox profile ID")
	}
	root, err := filepath.Abs(m.Root)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, profile), nil
}

// RemoveProfile removes only a validated direct child of Root. RemoveAll does
// not traverse symbolic links, including links inside the profile directory.
// Callers must hold the profile's distributed session lease.
func (m *Manager) RemoveProfile(profile string) error {
	directory, err := m.ProfilePath(profile)
	if err != nil {
		return err
	}
	return os.RemoveAll(directory)
}

func (m *Manager) Start(ctx context.Context, profile, proxy string) (_ *Browser, err error) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.life, cancel)
	defer func() {
		if err != nil {
			stop()
			cancel()
		}
	}()
	executable := m.Executable
	if executable == "" {
		executable, err = FindExecutable(ctx, proxy)
	} else {
		executable, err = validateCamoufoxExecutable(executable)
	}
	if err != nil {
		return nil, err
	}
	select {
	case m.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	b := &Browser{done: make(chan struct{})}
	b.release = func() {
		stop()
		cancel()
		m.mu.Lock()
		delete(m.active, b)
		m.mu.Unlock()
		<-m.slots
	}
	defer func() {
		if err != nil {
			b.Close()
		}
	}()
	directory, err := m.ProfilePath(profile)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	fingerprintPath := filepath.Join(directory, "sub2-fingerprint.json")
	raw, readErr := os.ReadFile(fingerprintPath)
	if errors.Is(readErr, os.ErrNotExist) {
		b.fingerprint, err = aistudio.NewBrowserFingerprint()
		if err != nil {
			return nil, err
		}
		raw, err = json.Marshal(b.fingerprint)
		if err != nil {
			return nil, err
		}
		if err = os.WriteFile(fingerprintPath, raw, 0600); err != nil {
			return nil, err
		}
	} else if readErr != nil {
		return nil, readErr
	} else if err = json.Unmarshal(raw, &b.fingerprint); err != nil {
		return nil, err
	}
	prefs := map[string]any{"remote.active-protocols": 1, "browser.shell.checkDefaultBrowser": false, "browser.startup.page": 0, "browser.startup.homepage": "about:blank", "browser.sessionstore.resume_from_crash": false, "browser.privatebrowsing.autostart": false, "network.proxy.type": 0, "signon.rememberSignons": false}
	if proxy != "" {
		if err = aistudio.ValidateProxy(proxy); err != nil {
			return nil, err
		}
		parsed, _ := url.Parse(proxy)
		b.relay, err = proxydial.StartRelay(parsed)
		if err != nil {
			return nil, err
		}
		prefs["network.proxy.type"] = 1
		prefs["network.proxy.socks"] = "127.0.0.1"
		prefs["network.proxy.socks_port"] = b.relay.Addr().Port
		prefs["network.proxy.socks_version"] = 5
		prefs["network.proxy.socks_remote_dns"] = true
		prefs["network.proxy.no_proxies_on"] = "localhost,127.0.0.1"
	}
	var userJS strings.Builder
	for k, v := range prefs {
		key, _ := json.Marshal(k)
		value, _ := json.Marshal(v)
		fmt.Fprintf(&userJS, "user_pref(%s,%s);\n", key, value)
	}
	if err = os.WriteFile(filepath.Join(directory, "user.js"), []byte(userJS.String()), 0600); err != nil {
		return nil, err
	}
	command := exec.Command(executable, "-no-remote", "-headless", "--remote-debugging-port=0", "-profile", directory)
	configureBrowserProcess(command, true)
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "CAMOU_CONFIG_") {
			command.Env = append(command.Env, item)
		}
	}
	for offset, index := 0, 1; offset < len(raw); offset, index = offset+2047, index+1 {
		end := offset + 2047
		if end > len(raw) {
			end = len(raw)
		}
		command.Env = append(command.Env, fmt.Sprintf("CAMOU_CONFIG_%d=%s", index, raw[offset:end]))
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err = command.Start(); err != nil {
		return nil, fmt.Errorf("start Camoufox: %w", err)
	}
	b.process = &browserProcess{command: command}
	b.process.tree, err = attachBrowserProcess(command)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		b.process = nil
		return nil, err
	}
	go func() { _ = command.Wait(); close(b.done) }()
	endpoints := make(chan string, 1)
	scan := func(reader io.Reader) {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			if endpoint := endpointPattern.FindString(scanner.Text()); endpoint != "" {
				select {
				case endpoints <- endpoint:
				default:
				}
			}
		}
	}
	go scan(stdout)
	go scan(stderr)
	timer := time.NewTimer(45 * time.Second)
	defer timer.Stop()
	var endpoint string
	select {
	case endpoint = <-endpoints:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.done:
		return nil, errors.New("Camoufox exited before its control endpoint was ready")
	case <-timer.C:
		return nil, errors.New("Camoufox startup timed out")
	}
	// Firefox advertises the listener root; WebDriver BiDi is served at /session.
	if u, _ := url.Parse(endpoint); u.Path == "" || u.Path == "/" {
		endpoint = strings.TrimRight(endpoint, "/") + "/session"
	}
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	b.conn, _, err = dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("connect Camoufox: %w", err)
	}
	b.conn.SetReadLimit(16 << 20)
	if _, err = b.Command(ctx, "session.new", map[string]any{"capabilities": map[string]any{}}); err != nil {
		return nil, err
	}
	tree, err := b.Command(ctx, "browsingContext.getTree", map[string]any{})
	if err != nil {
		return nil, err
	}
	var roots struct {
		Contexts []struct {
			Context string `json:"context"`
		} `json:"contexts"`
	}
	if err = json.Unmarshal(tree, &roots); err != nil || len(roots.Contexts) == 0 {
		return nil, errors.New("Camoufox has no browser context")
	}
	b.tab = roots.Contexts[0].Context
	_, _ = b.Command(ctx, "browsingContext.setViewport", map[string]any{"context": b.tab, "viewport": map[string]int{"width": 1100, "height": 780}})
	if _, err = b.Command(ctx, "browsingContext.navigate", map[string]any{"context": b.tab, "url": aistudio.AIStudioOrigin + "/prompts/new_chat", "wait": "none"}); err != nil {
		var failure *commandError
		if !errors.As(err, &failure) || !strings.Contains(failure.Detail, "NS_ERROR_ABORT") {
			return nil, err
		}
		err = nil
	}
	m.mu.Lock()
	m.active[b] = struct{}{}
	m.mu.Unlock()
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	go func() {
		select {
		case <-ctx.Done():
			b.Close()
		case <-b.done:
		}
	}()
	return b, nil
}

func (m *Manager) Close() {
	m.cancel()
	m.mu.Lock()
	active := make([]*Browser, 0, len(m.active))
	for b := range m.active {
		active = append(active, b)
	}
	m.mu.Unlock()
	for _, b := range active {
		b.Close()
	}
}

func (b *Browser) Command(ctx context.Context, method string, params any) (json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(45 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = b.conn.SetWriteDeadline(deadline)
	_ = b.conn.SetReadDeadline(deadline)
	b.id++
	if err := b.conn.WriteJSON(map[string]any{"id": b.id, "method": method, "params": params}); err != nil {
		return nil, fmt.Errorf("Camoufox %s: connection closed: %w", method, err)
	}
	for {
		var reply struct {
			ID      int             `json:"id"`
			Result  json.RawMessage `json:"result"`
			Error   string          `json:"error"`
			Message string          `json:"message"`
		}
		if err := b.conn.ReadJSON(&reply); err != nil {
			return nil, fmt.Errorf("Camoufox %s: connection closed or timed out: %w", method, err)
		}
		if reply.ID != b.id {
			continue
		}
		if reply.Error != "" {
			return nil, &commandError{Method: method, Code: reply.Error, Detail: reply.Message}
		}
		return reply.Result, nil
	}
}
func (b *Browser) Evaluate(ctx context.Context, expression string) (string, error) {
	raw, err := b.Command(ctx, "script.evaluate", map[string]any{"expression": expression, "target": map[string]string{"context": b.tab}, "awaitPromise": true})
	if err != nil {
		return "", err
	}
	var value struct {
		Type   string `json:"type"`
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err = json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	if value.Type == "exception" {
		return "", errors.New("Camoufox page evaluation failed")
	}
	return value.Result.Value, nil
}
func (b *Browser) Screenshot(ctx context.Context) (string, error) {
	raw, err := b.Command(ctx, "browsingContext.captureScreenshot", map[string]any{"context": b.tab, "origin": "viewport", "format": map[string]any{"type": "image/jpeg", "quality": 0.7}})
	if err != nil {
		return "", err
	}
	var v struct {
		Data string `json:"data"`
	}
	err = json.Unmarshal(raw, &v)
	return v.Data, err
}

type Input struct {
	Type   string `json:"type"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Text   string `json:"text"`
	Key    string `json:"key"`
	DeltaY int    `json:"delta_y"`
}

func (b *Browser) Input(ctx context.Context, in Input) error {
	var action map[string]any
	switch in.Type {
	case "click":
		if in.X < 0 || in.X > 1100 || in.Y < 0 || in.Y > 780 {
			return errors.New("invalid pointer coordinates")
		}
		action = map[string]any{"type": "pointer", "id": "mouse", "parameters": map[string]string{"pointerType": "mouse"}, "actions": []any{map[string]any{"type": "pointerMove", "x": in.X, "y": in.Y, "origin": "viewport"}, map[string]any{"type": "pointerDown", "button": 0}, map[string]any{"type": "pointerUp", "button": 0}}}
	case "text":
		if len(in.Text) > 4096 {
			return errors.New("input too long")
		}
		actions := make([]any, 0, len(in.Text)*2)
		for _, r := range in.Text {
			value := string(r)
			actions = append(actions, map[string]string{"type": "keyDown", "value": value}, map[string]string{"type": "keyUp", "value": value})
		}
		action = map[string]any{"type": "key", "id": "keyboard", "actions": actions}
	case "key":
		keys := map[string]string{"Enter": "\ue007", "Tab": "\ue004", "Backspace": "\ue003", "Escape": "\ue00c", "ArrowDown": "\ue015", "ArrowUp": "\ue013"}
		key, ok := keys[in.Key]
		if !ok {
			return errors.New("unsupported key")
		}
		action = map[string]any{"type": "key", "id": "keyboard", "actions": []any{map[string]string{"type": "keyDown", "value": key}, map[string]string{"type": "keyUp", "value": key}}}
	case "scroll":
		if in.DeltaY < -1000 || in.DeltaY > 1000 {
			return errors.New("invalid scroll")
		}
		action = map[string]any{"type": "wheel", "id": "wheel", "actions": []any{map[string]any{"type": "scroll", "x": 550, "y": 390, "deltaX": 0, "deltaY": in.DeltaY, "duration": 100, "origin": "viewport"}}}
	default:
		return errors.New("unsupported browser input")
	}
	_, err := b.Command(ctx, "input.performActions", map[string]any{"context": b.tab, "actions": []any{action}})
	return err
}

//go:embed capture.js
var capturePageExpression string

// Capture uses AI Studio's signed-in account data or Google account controls,
// never arbitrary page text (which may include emails in prompts or replies).
func (b *Browser) Capture(ctx context.Context, expected string) (aistudio.StorageState, string, error) {
	var state aistudio.StorageState
	value, err := b.Evaluate(ctx, capturePageExpression)
	if err != nil {
		return state, "", err
	}
	var page struct {
		Origin   string                 `json:"origin"`
		Email    string                 `json:"email"`
		Identity string                 `json:"identity"`
		Storage  []aistudio.StorageItem `json:"storage"`
	}
	if err = json.Unmarshal([]byte(value), &page); err != nil {
		return state, "", err
	}
	if page.Origin != aistudio.AIStudioOrigin {
		return state, "", fmt.Errorf("%w: [not-ai-studio] finish Google login and open the AI Studio workbench before completing authorization", aistudio.ErrAuthenticationRequired)
	}
	if page.Identity == "ambiguous" {
		return state, "", fmt.Errorf("%w: [account-ambiguous] multiple Google accounts are visible; close the account switcher and select the intended AI Studio account", aistudio.ErrAuthenticationRequired)
	}
	if !strings.Contains(page.Email, "@") {
		return state, "", fmt.Errorf("%w: [account-email-missing] AI Studio is open but its signed-in email is not available; wait for the page to load or open the Google account menu, then complete authorization again", aistudio.ErrAuthenticationRequired)
	}
	if expected != "" && !strings.EqualFold(expected, page.Email) {
		return state, "", errors.New("Camoufox account does not match the saved account")
	}
	// Export the isolated profile, not only the current page's storage partition:
	// Google login spans accounts.google.com, AI Studio and clients6.google.com.
	raw, err := b.Command(ctx, "storage.getCookies", map[string]any{})
	if err != nil {
		return state, "", err
	}
	var result struct {
		Cookies []struct {
			Name  string `json:"name"`
			Value struct {
				Type  string `json:"type"`
				Value string `json:"value"`
			} `json:"value"`
			Domain   string   `json:"domain"`
			Path     string   `json:"path"`
			Expiry   *float64 `json:"expiry"`
			HTTPOnly bool     `json:"httpOnly"`
			Secure   bool     `json:"secure"`
			SameSite string   `json:"sameSite"`
		} `json:"cookies"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		return state, "", err
	}
	for _, c := range result.Cookies {
		domain := strings.TrimPrefix(c.Domain, ".")
		if domain != "google.com" && !strings.HasSuffix(domain, ".google.com") && domain != "clients6.google.com" {
			continue
		}
		if c.Value.Type != "string" {
			continue
		}
		expires := float64(-1)
		if c.Expiry != nil {
			expires = *c.Expiry
		}
		sameSite := ""
		switch c.SameSite {
		case "lax":
			sameSite = "Lax"
		case "strict":
			sameSite = "Strict"
		case "none":
			sameSite = "None"
		}
		state.Cookies = append(state.Cookies, aistudio.StateCookie{Name: c.Name, Value: c.Value.Value, Domain: c.Domain, Path: c.Path, Expires: expires, HTTPOnly: c.HTTPOnly, Secure: c.Secure, SameSite: sameSite})
	}
	state.Origins = []aistudio.StorageOrigin{{Origin: page.Origin, LocalStorage: page.Storage}}
	if _, ok := state.CookieValue("SAPISID", aistudio.AIStudioReferer, time.Now()); !ok {
		return state, "", fmt.Errorf("%w: [session-cookie-missing] the browser has no valid SAPISID cookie for AI Studio; finish Google login before completing authorization", aistudio.ErrAuthenticationRequired)
	}
	if err = state.SetBrowserFingerprint(b.fingerprint); err != nil {
		return state, "", err
	}
	if err = state.Validate(); err != nil {
		return state, "", err
	}
	return state, strings.ToLower(page.Email), nil
}
func (b *Browser) Close() {
	b.once.Do(func() {
		if b.conn != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, _ = b.Command(ctx, "browser.close", map[string]any{})
			cancel()
			_ = b.conn.Close()
		}
		if b.process != nil {
			select {
			case <-b.done:
			case <-time.After(5 * time.Second):
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = terminateBrowserProcess(ctx, b.process)
				cancel()
				select {
				case <-b.done:
				case <-time.After(5 * time.Second):
					_ = b.process.command.Process.Kill()
				}
			}
			releaseBrowserTree(b.process.tree)
		}
		if b.relay != nil {
			_ = b.relay.Close()
		}
		if b.release != nil {
			b.release()
		}
	})
}

func (m *Manager) Recover(ctx context.Context, profile, proxy, email string) (aistudio.StorageState, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	b, err := m.Start(ctx, profile, proxy)
	if err != nil {
		return aistudio.StorageState{}, err
	}
	defer b.Close()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		state, _, err := b.Capture(ctx, email)
		if err == nil {
			return state, nil
		}
		if !errors.Is(err, aistudio.ErrAuthenticationRequired) {
			return state, err
		}
		select {
		case <-ctx.Done():
			return state, fmt.Errorf("%w: open the account's Camoufox login window", aistudio.ErrAuthenticationRequired)
		case <-ticker.C:
		}
	}
}
