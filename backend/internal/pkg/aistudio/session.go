package aistudio

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio/waa"
	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio/waa/mozjs"
	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sync/errgroup"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	waaCreateURL  = "https://waa-pa.clients6.google.com/$rpc/google.internal.waa.v1.Waa/Create"
	waaRequestKey = "lmnUSbltwc5ULv48iKLX"
	waaAPIKey     = "AIzaSyBGb5fGAyC-pRcRU6MUHb__b_vKha71HRE"
	waaTTL        = 12 * time.Hour
)

var makerSuiteAPIKeyPattern = regexp.MustCompile(`"WIu0Nc":"([^"]+)"`)

type SessionOptions struct {
	State    StorageState
	ProxyURL string
	AuthUser int
	Version  string
	OnChange func(StorageState) (string, error)
	Client   *http.Client
}
type Session struct {
	Version          string
	client           *http.Client
	proxyURL         string
	authUser         int
	stateMu          sync.Mutex
	state            StorageState
	onChange         func(StorageState) (string, error)
	headers          http.Header
	profile          waa.Profile
	pageURL          string
	pageReady        bool
	catalog          map[string][]byte
	vmMu             sync.Mutex
	vm               *waa.Runtime
	expires          time.Time
	interpreterHash  string
	interpreter      string
	closed           bool
	refreshTimer     *time.Timer
	incidentsTimer   *time.Timer
	networkError     error
	epoch            atomic.Uint64
	pendingChallenge *waa.Challenge
	challengeTime    time.Time
}

func ValidateProxy(value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("proxy must be an http, https, socks5 or socks5h URL")
	}
	return nil
}
func NewProxyHTTPClient(proxyURL string) (*http.Client, error) {
	if err := ValidateProxy(proxyURL); err != nil {
		return nil, err
	}
	transport, err := newBrowserRoundTripper(proxyURL)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
func NewSession(options SessionOptions) (*Session, error) {
	client := options.Client
	if client == nil {
		var err error
		client, err = NewProxyHTTPClient(options.ProxyURL)
		if err != nil {
			return nil, err
		}
	}
	s := &Session{Version: options.Version, client: client, proxyURL: options.ProxyURL, authUser: options.AuthUser, state: options.State, onChange: options.OnChange, catalog: map[string][]byte{}}
	var fingerprint map[string]any
	if raw := options.State.Fingerprint; len(raw) > 0 {
		if err := json.Unmarshal(raw, &fingerprint); err != nil {
			return nil, fmt.Errorf("parse browser fingerprint: %w", err)
		}
	}
	if len(fingerprint) == 0 {
		var err error
		fingerprint, err = NewBrowserFingerprint()
		if err != nil {
			return nil, err
		}
		if err := s.state.SetBrowserFingerprint(fingerprint); err != nil {
			return nil, err
		}
	}
	s.profile, _, _ = profileFromFingerprint(fingerprint)
	s.profile.Locale, _ = fingerprint["navigator.language"].(string)
	s.profile.TimeZone, _ = fingerprint["timezone"].(string)
	s.pageURL = AIStudioOrigin + "/prompts/new_chat"
	if options.AuthUser > 0 {
		s.pageURL = fmt.Sprintf("%s/u/%d/prompts/new_chat", AIStudioOrigin, options.AuthUser)
	}
	s.headers = make(http.Header)
	ua, _ := fingerprint["navigator.userAgent"].(string)
	s.headers.Set("User-Agent", ua)
	language, _ := fingerprint["headers.Accept-Language"].(string)
	s.headers.Set("Accept-Language", language)
	return s, nil
}

func (s *Session) StorageState() StorageState {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	raw, _ := json.Marshal(s.state)
	var clone StorageState
	_ = json.Unmarshal(raw, &clone)
	return clone
}
func (s *Session) setPersistence(callback func(StorageState) (string, error)) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.onChange = callback
}
func (s *Session) version() string { s.stateMu.Lock(); defer s.stateMu.Unlock(); return s.Version }
func (s *Session) pendingNetworkError() error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.networkError
}
func (s *Session) rememberNetworkError(err error) {
	if err != nil {
		s.stateMu.Lock()
		defer s.stateMu.Unlock()
		s.networkError = err
	}
}
func (s *Session) persistLocked() error {
	if s.onChange == nil {
		return nil
	}
	version, err := s.onChange(s.state)
	if err != nil {
		s.networkError = err
		return err
	}
	s.Version = version
	return nil
}
func (s *Session) Commit() error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.networkError != nil {
		return s.networkError
	}
	return s.persistLocked()
}
func (s *Session) ReplaceState(state StorageState) error {
	s.vmMu.Lock()
	defer s.vmMu.Unlock()
	s.stopTimers()
	if s.vm != nil {
		s.vm.Close()
		s.vm = nil
	}
	s.pageReady = false
	s.interpreter = ""
	s.interpreterHash = ""
	s.pendingChallenge = nil
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	state.Fingerprint = s.state.Fingerprint
	s.state = state
	s.networkError = nil
	return s.persistLocked()
}
func (s *Session) Do(ctx context.Context, method, target string, body []byte, headers http.Header) (*http.Response, error) {
	epoch := s.epoch.Load()
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	for _, key := range []string{"User-Agent", "Accept-Language"} {
		if req.Header.Get(key) == "" {
			req.Header.Set(key, s.headers.Get(key))
		}
	}
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	s.stateMu.Lock()
	cookies, err := s.state.CookieHeader(target, time.Now())
	if err == nil && req.Header.Get("Authorization") != "" {
		for _, item := range signatureCookies {
			if _, ok := s.state.CookieValue(item.name, AIStudioReferer, time.Now()); !ok {
				err = fmt.Errorf("%w: missing valid %s for AI Studio origin", ErrAuthenticationRequired, item.name)
				break
			}
		}
		if err == nil {
			var authorization string
			originCookies, cookieErr := s.state.CookieHeader(AIStudioReferer, time.Now())
			if cookieErr != nil {
				err = cookieErr
			} else {
				authorization, err = BuildAuthorizationHeader(originCookies, AIStudioOrigin, time.Now())
			}
			req.Header.Set("Authorization", authorization)
		}
	}
	s.stateMu.Unlock()
	if err != nil {
		return nil, err
	}
	req.Header.Del("Cookie")
	if cookies != "" {
		req.Header.Set("Cookie", cookies)
	}
	response, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if values := response.Header.Values("Set-Cookie"); len(values) > 0 {
		s.stateMu.Lock()
		if epoch != s.epoch.Load() {
			err = errors.New("AI Studio session was replaced during the request")
		} else {
			err = s.state.MergeSetCookieHeaders(values, target, time.Now())
		}
		if err == nil {
			err = s.persistLocked()
		}
		s.stateMu.Unlock()
		if err != nil {
			response.Body.Close()
			return nil, fmt.Errorf("persist AI Studio cookies: %w", err)
		}
	}
	var reader io.Reader = response.Body
	encoding := response.Header.Get("Content-Encoding")
	// fhttp decodes the body while retaining the original Content-Encoding.
	// Its Uncompressed flag is authoritative; decoding again corrupts the stream.
	if response.Uncompressed {
		encoding = ""
	}
	switch encoding {
	case "gzip":
		reader, err = gzip.NewReader(response.Body)
	case "deflate":
		reader, err = zlib.NewReader(response.Body)
	case "br":
		reader = brotli.NewReader(response.Body)
	case "zstd":
		var decoder *zstd.Decoder
		decoder, err = zstd.NewReader(response.Body)
		if err == nil {
			reader = decoder.IOReadCloser()
		}
	}
	if err != nil {
		response.Body.Close()
		return nil, err
	}
	if reader != response.Body {
		response.Body = &decodedBody{Reader: reader, source: response.Body}
		response.Uncompressed = true
	}
	if response.Uncompressed {
		response.Header.Del("Content-Encoding")
		response.Header.Del("Content-Length")
		response.ContentLength = -1
	}
	return response, nil
}

type decodedBody struct {
	io.Reader
	source io.Closer
}

func (b *decodedBody) Close() error {
	if closer, ok := b.Reader.(io.Closer); ok {
		_ = closer.Close()
	}
	return b.source.Close()
}
func (s *Session) rpcHeaders(apiKey string) (http.Header, error) {
	state := s.StorageState()
	cookie, err := state.CookieHeader(AIStudioReferer, time.Now())
	if err != nil {
		return nil, err
	}
	for _, item := range signatureCookies {
		if _, ok := state.CookieValue(item.name, AIStudioReferer, time.Now()); !ok {
			return nil, fmt.Errorf("%w: missing valid %s", ErrAuthenticationRequired, item.name)
		}
	}
	headers, err := BuildRequestHeaders(cookie, s.headers.Get("User-Agent"))
	if err != nil {
		return nil, err
	}
	headers.Del("Cookie")
	headers.Set("Accept", "*/*")
	headers.Set("X-Goog-Api-Key", apiKey)
	headers.Set("X-Goog-AuthUser", strconv.Itoa(s.authUser))
	headers.Set("Sec-Fetch-Dest", "empty")
	headers.Set("Sec-Fetch-Mode", "cors")
	headers.Set("Sec-Fetch-Site", "same-site")
	for _, key := range []string{"X-AIStudio-Visit-Id", "X-Goog-Ext-519733851-Bin"} {
		if value := s.headers.Get(key); value != "" {
			headers.Set(key, value)
		}
	}
	return headers, nil
}
func checkResponse(response *http.Response, body []byte) error {
	return CheckRPCResponse("", response.StatusCode, body)
}
func (s *Session) RPC(ctx context.Context, method string, body []byte) (*http.Response, error) {
	if err := s.pendingNetworkError(); err != nil {
		return nil, err
	}
	if err := s.EnsurePage(ctx, ""); err != nil {
		return nil, err
	}
	headers, err := s.rpcHeaders(s.headers.Get("X-Goog-Api-Key"))
	if err != nil {
		return nil, err
	}
	if method == "GenerateContent" {
		headers.Set("Priority", "u=0")
		if tier := s.headers.Get("X-AIStudio-G1-Tier"); tier != "" {
			headers.Set("X-AIStudio-G1-Tier", tier)
		}
	}
	return s.Do(ctx, http.MethodPost, MakerSuiteRPCBase+method, body, headers)
}
func (s *Session) EnsurePage(ctx context.Context, model string) error {
	if s.pageReady {
		return nil
	}
	if err := s.Commit(); err != nil {
		return err
	}
	target := s.pageURL
	if model != "" {
		target += "?model=" + url.QueryEscape(model) + "&temporary=true"
	}
	s.profile.PageURL = target
	s.profile.HasFocus = true
	s.profile.NavigationStart = time.Now()
	var document []byte
	var response *http.Response
	for redirects := 0; redirects < 10; redirects++ {
		headers := http.Header{"Accept": {"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"}, "Upgrade-Insecure-Requests": {"1"}, "Sec-Fetch-Dest": {"document"}, "Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Site": {"none"}, "Sec-Fetch-User": {"?1"}, "Priority": {"u=0, i"}}
		var err error
		response, err = s.Do(ctx, http.MethodGet, target, nil, headers)
		if err != nil {
			return err
		}
		document, err = io.ReadAll(io.LimitReader(response.Body, 16<<20))
		response.Body.Close()
		if err != nil {
			return err
		}
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			location, err := response.Location()
			if err != nil {
				return err
			}
			if location.Hostname() == "accounts.google.com" {
				return fmt.Errorf("AI Studio page redirected to Google sign-in: %w", ErrAuthenticationRequired)
			}
			if !googleCookieDomain(location.Hostname()) {
				return errors.New("unexpected AI Studio redirect")
			}
			target = location.String()
			continue
		}
		break
	}
	if err := checkResponse(response, document); err != nil {
		return err
	}
	match := makerSuiteAPIKeyPattern.FindSubmatch(document)
	if len(match) != 2 {
		return errors.New("AI Studio page has no WIu0Nc API key")
	}
	s.headers.Set("X-Goog-Api-Key", string(match[1]))
	visitID, err := newVisitID()
	if err != nil {
		return err
	}
	s.headers.Set("X-AIStudio-Visit-Id", visitID)
	s.profile.Globals = pageGlobals(document)
	s.profile.TrustedTypes = requiresTrustedScripts(response.Header.Values("Content-Security-Policy"))
	state := s.StorageState()
	s.profile.Document = map[string]any{"cookie": s.documentCookies(target), "lastModified": time.Now().Format("01/02/2006 15:04:05")}
	for _, origin := range state.Origins {
		if origin.Origin == AIStudioOrigin {
			s.profile.LocalStorage = map[string]string{}
			for _, item := range origin.LocalStorage {
				s.profile.LocalStorage[item.Name] = item.Value
			}
		}
	}
	parsed := parsePageDocument(document, target)
	for _, resource := range parsed.resources {
		accept, destination, priority := "*/*", "script", "u=2"
		if resource.kind == pageStylesheet {
			accept, destination = "text/css,*/*;q=0.1", "style"
		}
		if resource.kind == pagePreloadScript {
			priority = "u=1"
		}
		r, err := s.Do(ctx, http.MethodGet, resource.url, nil, subresourceHeaders(resource.url, accept, destination, priority))
		if err != nil {
			return err
		}
		_, err = io.Copy(io.Discard, io.LimitReader(r.Body, 16<<20))
		r.Body.Close()
		if err != nil {
			return err
		}
		if r.StatusCode != http.StatusOK {
			return fmt.Errorf("AI Studio page resource HTTP %d", r.StatusCode)
		}
	}
	// Mirror the page: auth warmup, logging, startup RPCs and Waa/Create
	// run concurrently. The logging extension is installed for followup requests.
	startup := []struct{ method, body string }{{"GetLoggingContext", "[]"}, {"GetUserPreferences", "[]"}, {"GetAiStudioBenefitTier", "[]"}, {"ListModels", "[]"}, {"GenerateAccessToken", `["users/me"]`}, {"ListIncidentsHistory", "[]"}, {"ListPromos", "[]"}, {"GenerateAccessToken", `["users/me"]`}}
	group, groupCtx := errgroup.WithContext(ctx)
	results := make([][]byte, len(startup))
	var challenge waa.Challenge
	group.Go(func() error {
		var err error
		challenge, err = s.fetchChallenge(groupCtx, nil)
		if err != nil {
			return fmt.Errorf("Waa/Create: %w", err)
		}
		return nil
	})
	group.Go(func() error {
		_, err := s.navigateFrame(groupCtx, "https://console.cloud.google.com/onboarding/auth_warmup?origin="+url.QueryEscape(AIStudioOrigin)+"&authuser="+strconv.Itoa(s.authUser))
		return err
	})
	for index, rpc := range startup {
		group.Go(func() error {
			raw, err := s.fetchPageRPC(groupCtx, rpc.method, rpc.body)
			if err != nil {
				return fmt.Errorf("AI Studio startup %s: %w", rpc.method, err)
			}
			results[index] = raw
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	for index, rpc := range startup {
		s.catalog[rpc.method] = results[index]
	}
	tier, err := benefitTierHeader(s.catalog["GetAiStudioBenefitTier"])
	if err != nil {
		return err
	}
	s.headers.Del("X-AIStudio-G1-Tier")
	if tier != "" {
		s.headers.Set("X-AIStudio-G1-Tier", tier)
	}
	normalized, err := io.ReadAll(newSparseJSONReader(bytes.NewReader(results[0])))
	if err != nil {
		return err
	}
	encoded, err := encodeJSPBFields(normalized)
	if err != nil {
		return err
	}
	s.headers.Set("X-Goog-Ext-519733851-Bin", base64.StdEncoding.EncodeToString(encoded))
	s.pendingChallenge = &challenge
	s.challengeTime = time.Now()
	followup, followupCtx := errgroup.WithContext(ctx)
	for _, rpc := range []struct{ method, body string }{{"ListRecentApplets", "[]"}, {"ListPrompts", "[100]"}} {
		followup.Go(func() error { _, err := s.fetchPageRPC(followupCtx, rpc.method, rpc.body); return err })
	}
	if err := followup.Wait(); err != nil {
		return err
	}

	s.pageReady = true
	s.vmMu.Lock()
	s.scheduleIncidents()
	s.vmMu.Unlock()
	return nil
}
func (s *Session) navigateFrame(ctx context.Context, target string) (bool, error) {
	for redirects := 0; redirects < 20; redirects++ {
		headers := http.Header{"Accept": {"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"}, "Referer": {AIStudioReferer}, "Sec-Fetch-Dest": {"iframe"}, "Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Site": {fetchSite(target)}, "Priority": {"u=4"}}
		response, err := s.Do(ctx, http.MethodGet, target, nil, headers)
		if err != nil {
			return false, err
		}
		_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<20))
		response.Body.Close()
		if err != nil {
			return false, err
		}
		if response.StatusCode < 300 || response.StatusCode >= 400 {
			return response.StatusCode == http.StatusOK, nil
		}
		location, err := response.Location()
		if err != nil {
			return false, err
		}
		target = location.String()
	}
	return false, errors.New("AI Studio frame redirect limit exceeded")
}

func newVisitID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	value[6] = value[6]&15 | 64
	value[8] = value[8]&63 | 128
	uuid := fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[:4], value[4:6], value[6:8], value[8:10], value[10:])
	return "v1_" + base64.StdEncoding.EncodeToString([]byte(uuid)), nil
}

var runtimeCheckOnce sync.Once
var runtimeCheckError error

// CheckRuntime verifies context creation and execution, not just library linking.
func CheckRuntime() error {
	runtimeCheckOnce.Do(func() {
		done := make(chan error, 1)
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			cx, err := mozjs.NewContext(mozjs.Options{HeapBytes: 32 << 20, Locale: "en-US"})
			if err != nil {
				done <- err
				return
			}
			defer cx.Close()
			global, err := cx.NewGlobal(0, "UTC")
			if err != nil {
				done <- err
				return
			}
			value, err := cx.Evaluate(global, "21 * 2", "runtime-check.js", 1)
			if err == nil && (value.Kind != mozjs.KindNumber || value.Number != 42) {
				err = errors.New("SpiderMonkey evaluation returned an invalid result")
			}
			done <- err
		}()
		if err := <-done; err != nil {
			runtimeCheckError = fmt.Errorf("Playground requires a CGO_ENABLED=1 build with SpiderMonkey: %w", err)
		}
	})
	return runtimeCheckError
}

func (s *Session) Proof(ctx context.Context, model, digest, prompt string) (string, error) {
	if err := CheckRuntime(); err != nil {
		return "", err
	}
	if err := s.pendingNetworkError(); err != nil {
		return "", err
	}
	if err := s.EnsurePage(ctx, model); err != nil {
		return "", err
	}
	s.vmMu.Lock()
	defer s.vmMu.Unlock()
	if s.vm == nil || !time.Now().Before(s.expires) {
		if err := s.refreshVM(ctx); err != nil {
			return "", err
		}
	}
	proof, err := s.vm.RunProof(ctx, digest, prompt)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(proof, "!") {
		return "", errors.New("invalid WAA proof prefix")
	}
	return proof, nil
}
func (s *Session) fetchPageRPC(ctx context.Context, method, body string) ([]byte, error) {
	headers, err := s.rpcHeaders(s.headers.Get("X-Goog-Api-Key"))
	if err != nil {
		return nil, err
	}
	response, err := s.Do(ctx, http.MethodPost, MakerSuiteRPCBase+method, []byte(body), headers)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	response.Body.Close()
	if err != nil {
		return nil, err
	}
	if err = CheckRPCResponse(method, response.StatusCode, raw); err != nil {
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) && rpcErr.DriveAuthorizationMissing() {
			return nil, nil
		}
		return nil, err
	}
	return raw, nil
}

func (s *Session) documentCookies(target string) string {
	state := s.StorageState()
	visible := StorageState{}
	for _, cookie := range state.Cookies {
		if !cookie.HTTPOnly {
			visible.Cookies = append(visible.Cookies, cookie)
		}
	}
	value, _ := visible.CookieHeader(target, time.Now())
	return value
}
func (s *Session) fetchChallenge(ctx context.Context, refresh []string) (waa.Challenge, error) {
	request := append([]string{waaRequestKey}, refresh...)
	body, err := json.Marshal(request)
	if err != nil {
		return waa.Challenge{}, err
	}
	headers, err := s.rpcHeaders(waaAPIKey)
	if err != nil {
		return waa.Challenge{}, err
	}
	response, err := s.Do(ctx, http.MethodPost, waaCreateURL, body, headers)
	if err != nil {
		return waa.Challenge{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	response.Body.Close()
	if err != nil {
		return waa.Challenge{}, err
	}
	if err = CheckRPCResponse("Waa/Create", response.StatusCode, raw); err != nil {
		return waa.Challenge{}, err
	}
	return waa.ParseChallenge(raw)
}
func (s *Session) refreshVM(ctx context.Context) error {
	var challenge waa.Challenge
	var err error
	if s.vm == nil && s.pendingChallenge != nil && time.Since(s.challengeTime) < time.Minute {
		challenge = *s.pendingChallenge
	} else {
		var refresh []string
		if s.vm != nil {
			snapshot, err := s.vm.RefreshSnapshot(ctx)
			if err != nil {
				return err
			}
			refresh = []string{s.interpreterHash, snapshot}
		}
		challenge, err = s.fetchChallenge(ctx, refresh)
		if err != nil {
			return err
		}
	}
	s.pendingChallenge = nil

	interpreter := challenge.InterpreterJavaScript
	if interpreter == "" && s.interpreterHash == challenge.InterpreterHash {
		interpreter = s.interpreter
	}
	if interpreter == "" {
		r, err := s.Do(ctx, http.MethodGet, challenge.InterpreterURL, nil, subresourceHeaders(challenge.InterpreterURL, "*/*", "script", ""))
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
		r.Body.Close()
		if err != nil {
			return err
		}
		if r.StatusCode != http.StatusOK {
			return fmt.Errorf("WAA interpreter HTTP %d", r.StatusCode)
		}
		interpreter = string(data)
	}
	if waa.InterpreterHash([]byte(interpreter)) != challenge.InterpreterHash {
		return errors.New("WAA interpreter SHA-256 mismatch")
	}
	s.profile.Document["cookie"] = s.documentCookies(s.profile.PageURL)
	epoch := s.epoch.Load()
	runtime, err := waa.NewRuntime(ctx, waa.Options{Challenge: challenge, Interpreter: interpreter, Profile: s.profile, LoadFrame: func(target string) bool {
		if s.epoch.Load() != epoch {
			return false
		}
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		ok, err := s.navigateFrame(c, target)
		if s.epoch.Load() == epoch {
			s.rememberNetworkError(err)
		}
		return ok
	}, LoadImage: func(target string) bool {
		if s.epoch.Load() != epoch {
			return false
		}
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		r, err := s.Do(c, http.MethodGet, target, nil, subresourceHeaders(target, "image/avif,image/webp,image/png,image/*;q=0.8,*/*;q=0.5", "image", "u=5, i"))
		if err != nil {
			if s.epoch.Load() == epoch {
				s.rememberNetworkError(err)
			}
			return false
		}
		defer r.Body.Close()
		_, err = io.Copy(io.Discard, io.LimitReader(r.Body, 4<<20))
		return err == nil && r.StatusCode == http.StatusOK
	}})
	if err != nil {
		return err
	}
	if err = runtime.Settle(ctx); err != nil {
		runtime.Close()
		return err
	}
	if s.vm != nil {
		s.vm.Close()
	}
	s.vm = runtime
	s.interpreterHash = challenge.InterpreterHash
	s.interpreter = interpreter
	s.expires = time.Now().Add(waaTTL)
	s.scheduleRefresh()
	return s.pendingNetworkError()
}

// Timer operations hold vmMu so refresh/close cannot race with an active proof.
func (s *Session) stopTimers() {
	s.epoch.Add(1)
	if s.refreshTimer != nil {
		s.refreshTimer.Stop()
	}
	if s.incidentsTimer != nil {
		s.incidentsTimer.Stop()
	}
}
func (s *Session) scheduleRefresh() {
	epoch := s.epoch.Load()
	if s.refreshTimer != nil {
		s.refreshTimer.Stop()
	}
	s.refreshTimer = time.AfterFunc(waaTTL, func() {
		s.vmMu.Lock()
		defer s.vmMu.Unlock()
		if s.closed || s.epoch.Load() != epoch {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.refreshVM(ctx); err != nil {
			s.rememberNetworkError(err)
		}
	})
}
func (s *Session) scheduleIncidents() {
	epoch := s.epoch.Load()
	if s.incidentsTimer != nil {
		s.incidentsTimer.Stop()
	}
	s.incidentsTimer = time.AfterFunc(10*time.Minute, func() {
		s.vmMu.Lock()
		defer s.vmMu.Unlock()
		if s.closed || s.epoch.Load() != epoch {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		headers, err := s.rpcHeaders(s.headers.Get("X-Goog-Api-Key"))
		if err == nil {
			var response *http.Response
			response, err = s.Do(ctx, http.MethodPost, MakerSuiteRPCBase+"ListIncidentsHistory", []byte("[]"), headers)
			if err == nil {
				_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<20))
				response.Body.Close()
				if err == nil {
					err = checkResponse(response, nil)
				}
			}
		}
		if err != nil {
			s.rememberNetworkError(err)
		}
		s.scheduleIncidents()
	})
}
func (s *Session) Close() {
	s.vmMu.Lock()
	defer s.vmMu.Unlock()
	s.closed = true
	s.stopTimers()
	if s.vm != nil {
		s.vm.Close()
		s.vm = nil
	}
	s.client.CloseIdleConnections()
}
