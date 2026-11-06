package aistudio

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

func TestSessionDoesNotDecodeTransportDecodedBodyTwice(t *testing.T) {
	for _, encoding := range []string{"gzip", "deflate", "br", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			client := &http.Client{Transport: protocolRoundTrip(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Encoding": {encoding}, "Content-Length": {"100"}}, Body: io.NopCloser(strings.NewReader("decoded content")), Uncompressed: true}, nil
			})}
			session, err := NewSession(SessionOptions{Client: client})
			require.NoError(t, err)
			defer session.Close()
			response, err := session.Do(context.Background(), http.MethodGet, AIStudioOrigin, nil, nil)
			require.NoError(t, err)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, "decoded content", string(body))
			require.Empty(t, response.Header.Get("Content-Encoding"))
			require.Empty(t, response.Header.Get("Content-Length"))
			require.EqualValues(t, -1, response.ContentLength)
		})
	}
}

func TestSessionDecodesRawCompressedBody(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, err := writer.Write([]byte("decoded content"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	client := &http.Client{Transport: protocolRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Encoding": {"gzip"}}, Body: io.NopCloser(bytes.NewReader(compressed.Bytes()))}, nil
	})}
	session, err := NewSession(SessionOptions{Client: client})
	require.NoError(t, err)
	defer session.Close()
	response, err := session.Do(context.Background(), http.MethodGet, AIStudioOrigin, nil, nil)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, "decoded content", string(body))
	require.True(t, response.Uncompressed)
}

func TestSessionUsesCapturedFingerprintLocale(t *testing.T) {
	state := StorageState{}
	fingerprint, err := buildCamoufoxConfig(camoufoxFirefoxMajor, "zh-CN", "America/Los_Angeles")
	require.NoError(t, err)
	require.NoError(t, state.SetBrowserFingerprint(fingerprint))
	session, err := NewSession(SessionOptions{State: state})
	require.NoError(t, err)
	defer session.Close()
	require.Equal(t, "zh-CN", session.profile.Locale)
	require.Contains(t, session.headers.Get("Accept-Language"), "zh-CN")
}

func TestReferenceBindingPreservesWhitespaceAndPartPositions(t *testing.T) {
	req := &antigravity.GeminiRequest{
		SystemInstruction: &antigravity.GeminiContent{Parts: []antigravity.GeminiPart{{Text: "excluded system"}}},
		Contents: []antigravity.GeminiContent{
			{Role: "user", Parts: []antigravity.GeminiPart{{Text: "  first\n"}, {InlineData: &antigravity.GeminiInlineData{Data: "YWJj"}}}},
			{Role: "model", Parts: []antigravity.GeminiPart{{FunctionCall: &antigravity.GeminiFunctionCall{Name: "f"}}}},
			{Role: "user", Parts: []antigravity.GeminiPart{{Text: " last "}, {Text: "tail"}}},
		},
	}
	require.Equal(t, "  first\n YWJj   last  tail", ExtractPromptForBinding(req))
	require.Equal(t, " last  tail", PagePrompt(req))
	require.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", ComputePayloadDigest(""))
}

func TestReferenceToolStructRoundTrip(t *testing.T) {
	args := map[string]any{"text": " spaced ", "nested": map[string]any{"ok": true}, "values": []any{nil, float64(2), "x"}}
	req := &antigravity.GeminiRequest{Contents: []antigravity.GeminiContent{
		{Role: "model", Parts: []antigravity.GeminiPart{{FunctionCall: &antigravity.GeminiFunctionCall{Name: "f", Args: args, ID: "id"}, ThoughtSignature: "sig"}}},
		{Role: "tool", Parts: []antigravity.GeminiPart{{FunctionResponse: &antigravity.GeminiFunctionResponse{Name: "f", Response: args}}}},
	}, Tools: []antigravity.GeminiToolDeclaration{{FunctionDeclarations: []antigravity.GeminiFunctionDecl{{Name: "f", Parameters: map[string]any{"type": "object", "properties": map[string]any{"choice": map[string]any{"type": "string", "enum": []string{"a", "b"}}}}}}}}}
	raw, err := EncodeGenerateContentRequest("m", req, "!proof")
	require.NoError(t, err)
	var wire []any
	require.NoError(t, json.Unmarshal(raw, &wire))
	contents := wire[1].([]any)
	model := contents[0].([]any)
	part := model[0].([]any)[0]
	encoded, _ := json.Marshal(part)
	event, err := decodePart(encoded)
	require.NoError(t, err)
	require.Equal(t, "sig", event.ThoughtSignature)
	require.Equal(t, "id", event.FunctionCall.ID)
	got, _ := json.Marshal(event.FunctionCall.Args)
	want, _ := json.Marshal(args)
	require.JSONEq(t, string(want), string(got))
	require.Equal(t, "user", contents[1].([]any)[1])
	tool := wire[6].([]any)[0].([]any)
	require.Nil(t, tool[0])
	require.Len(t, tool[1].([]any), 1)
	require.NotNil(t, wire[3].([]any)[13])
}

func TestCookieRotationIsolationDeletionAndRestart(t *testing.T) {
	now := time.Unix(1800000000, 0)
	state := StorageState{}
	require.NoError(t, state.MergeSetCookieHeaders([]string{"s=domain; Domain=google.com; Path=/; Secure; HttpOnly; Max-Age=60", "h=host; Path=/", "p=private; Path=/private"}, "https://aistudio.google.com/private/a", now))
	header, err := state.CookieHeader("https://aistudio.google.com/private/x", now)
	require.NoError(t, err)
	require.Equal(t, "p=private; s=domain; h=host", header)
	header, _ = state.CookieHeader("https://other.google.com/private/x", now)
	require.Equal(t, "s=domain", header)
	header, _ = state.CookieHeader("http://aistudio.google.com/private-other", now)
	require.Equal(t, "h=host", header)
	require.NoError(t, state.MergeSetCookieHeaders([]string{"s=new; Domain=.google.com; Path=/; Secure; Max-Age=120; Expires=Thu, 01 Jan 1970 00:00:00 GMT", "h=gone; Path=/; Max-Age=0"}, "https://aistudio.google.com/", now))
	require.Len(t, state.Cookies, 2)
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	var restored StorageState
	require.NoError(t, json.Unmarshal(raw, &restored))
	header, _ = restored.CookieHeader("https://aistudio.google.com/", now.Add(65*time.Second))
	require.Equal(t, "s=new", header)
	header, _ = restored.CookieHeader("https://aistudio.google.com/", now.Add(125*time.Second))
	require.Empty(t, header)
	require.Error(t, restored.MergeSetCookieHeaders([]string{"bad=1; Domain=evil.com"}, "https://aistudio.google.com/", now))
	require.Error(t, restored.MergeSetCookieHeaders([]string{"bad=1; Domain=com"}, "https://aistudio.google.com/", now))
	require.Error(t, restored.MergeSetCookieHeaders([]string{"invalid"}, "https://aistudio.google.com/", now))
}

type protocolRoundTrip func(*http.Request) (*http.Response, error)

func (f protocolRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSessionPersistsSetCookieBeforeNextRequest(t *testing.T) {
	calls := 0
	persisted := StorageState{}
	client := &http.Client{Transport: protocolRoundTrip(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 2 {
			require.Contains(t, req.Header.Get("Cookie"), "SIDCC=rotated")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Set-Cookie": {"SIDCC=rotated; Domain=.google.com; Path=/; Secure; HttpOnly"}}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	s, err := NewSession(SessionOptions{State: StorageState{}, Client: client, OnChange: func(state StorageState) (string, error) { persisted = state; return "v2", nil }})
	require.NoError(t, err)
	defer s.Close()
	for i := 0; i < 2; i++ {
		response, err := s.Do(context.Background(), "GET", AIStudioReferer, nil, nil)
		require.NoError(t, err)
		response.Body.Close()
	}
	require.Len(t, persisted.Cookies, 1)
	require.Equal(t, "v2", s.version())
}

func TestSessionRejectsFailedCredentialPersistence(t *testing.T) {
	rejected := errors.New("credentials changed")
	s, err := NewSession(SessionOptions{Client: &http.Client{Transport: protocolRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Set-Cookie": {"a=b; Path=/"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}, OnChange: func(StorageState) (string, error) { return "", rejected }})
	require.NoError(t, err)
	defer s.Close()
	_, err = s.Do(context.Background(), "GET", AIStudioReferer, nil, nil)
	require.ErrorIs(t, err, rejected)
	require.ErrorIs(t, s.Commit(), rejected)
}

func TestLateCookieResponseCannotOverwriteRenewedSession(t *testing.T) {
	started := make(chan struct{})
	allowResponse := make(chan struct{})
	client := &http.Client{Transport: protocolRoundTrip(func(*http.Request) (*http.Response, error) {
		close(started)
		<-allowResponse
		return &http.Response{StatusCode: 200, Header: http.Header{"Set-Cookie": {"SAPISID=obsolete; Domain=.google.com; Path=/; Secure"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	s, err := NewSession(SessionOptions{State: StorageState{}, Client: client})
	require.NoError(t, err)
	defer s.Close()
	result := make(chan error, 1)
	go func() {
		response, err := s.Do(context.Background(), "GET", AIStudioReferer, nil, nil)
		if response != nil {
			response.Body.Close()
		}
		result <- err
	}()
	<-started
	require.NoError(t, s.ReplaceState(StorageState{Cookies: []StateCookie{{Name: "SAPISID", Value: "renewed", Domain: ".google.com", Path: "/", Expires: -1, Secure: true}}}))
	close(allowResponse)
	require.Error(t, <-result)
	value, found := s.StorageState().CookieValue("SAPISID", AIStudioReferer, time.Now())
	require.True(t, found)
	require.Equal(t, "renewed", value)
}

func generationFixture(t *testing.T, finish any, trailer any) string {
	t.Helper()
	candidate := []any{[]any{[]any{[]any{nil, "first"}}, "model"}, finish}
	frame := []any{[]any{candidate}, nil, []any{2, 3, 5}}
	raw, err := json.Marshal([]any{[]any{frame}, trailer})
	require.NoError(t, err)
	return string(raw)
}
func TestDecoderRejectsIncompleteAndRPCErrorResponses(t *testing.T) {
	valid := generationFixture(t, 1, []any{0})
	cases := []struct {
		name, input string
		auth        bool
	}{
		{"missing finish", generationFixture(t, nil, []any{0}), false},
		{"truncated", strings.TrimSuffix(valid, "]"), false},
		{"trailing roots", valid + "[]", false},
		{"auth trailer", generationFixture(t, 1, []any{16, "expired"}), true},
		{"permission trailer", generationFixture(t, 1, []any{7, "denied"}), false},
		{"decode failure", `[[[[[[[null,7]],"model"],1]]],[0]]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := DecodeStream(strings.NewReader(tc.input), func(StreamEvent) error { return nil })
			require.Error(t, err)
			require.Equal(t, tc.auth, errors.Is(err, ErrAuthenticationRequired))
		})
	}
	var text strings.Builder
	require.NoError(t, DecodeStream(strings.NewReader(valid), func(e StreamEvent) error { text.WriteString(e.Text); return nil }))
	require.Equal(t, "first", text.String())
}
func TestDecoderThoughtEnumAndModels(t *testing.T) {
	part := setWireField([]any{nil, "thinking"}, 12, 1)
	raw, _ := json.Marshal(part)
	e, err := decodePart(raw)
	require.NoError(t, err)
	require.True(t, e.Thought)
	models, err := ParseModels([]byte(`[[["models/a",1],["models/b",2]]]`))
	require.NoError(t, err)
	require.Equal(t, []string{"models/a", "models/b"}, models)
	_, err = ParseModels([]byte(`[[[["models/wrong"]]]]`))
	require.Error(t, err)
}

func TestManagerLoadsFreshCredentialsAfterLease(t *testing.T) {
	manager := &WAAManager{}
	options := SessionOptions{Version: "v1", Client: &http.Client{}}
	first, release, err := manager.AcquireFresh(context.Background(), "account", func() (SessionOptions, error) { return options, nil })
	require.NoError(t, err)
	entered := make(chan struct{})
	loaded := make(chan string, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(entered)
		s, done, err := manager.AcquireFresh(context.Background(), "account", func() (SessionOptions, error) { options.Version = "v2"; return options, nil })
		if err != nil {
			loaded <- err.Error()
			return
		}
		defer done()
		defer s.Close()
		loaded <- s.version()
	}()
	<-entered
	require.Empty(t, loaded)
	first.Version = "v1"
	release()
	wg.Wait()
	require.Equal(t, "v2", <-loaded)
	require.True(t, first.closed)
}

func TestSparseJSONPreservesStrings(t *testing.T) {
	raw, err := io.ReadAll(newSparseJSONReader(bytes.NewBufferString(`[[,"[,]",,],]`)))
	require.NoError(t, err)
	require.JSONEq(t, `[[null,"[,]",null,null],null]`, string(raw))
}

func TestRPCResponseClassifiesAuthenticationAndRegion(t *testing.T) {
	require.NoError(t, CheckRPCResponse("GetLoggingContext", 200, []byte(`[1,"L"]`)))
	for _, tc := range []struct {
		name, method, body  string
		status              int
		auth, drive, region bool
	}{
		{"region", "ListModels", `[7,"Region not supported."]`, 403, false, false, true},
		{"drive scope", "GenerateAccessToken", `[16,"OAuth error: unauthorized_client."]`, 401, false, true, false},
		{"drive applets", "ListRecentApplets", `[16,"OAuth error: unauthorized_client."]`, 401, false, true, false},
		{"drive prompts", "ListPrompts", `[16,"OAuth error: unauthorized_client."]`, 401, false, true, false},
		{"invalid session", "GenerateAccessToken", `[16,"Request had invalid authentication credentials."]`, 401, true, false, false},
		{"generation unauthorized", "GenerateContent", `[16,"OAuth error: unauthorized_client."]`, 401, true, false, false},
		{"HTTP200 authentication", "ListModels", `[null,[16,"expired"]]`, 200, true, false, false},
		{"permission", "ListModels", `[null,[7,"Permission denied"]]`, 403, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckRPCResponse(tc.method, tc.status, []byte(tc.body))
			require.Error(t, err)
			require.Equal(t, tc.auth, errors.Is(err, ErrAuthenticationRequired))
			var rpcErr *RPCError
			require.ErrorAs(t, err, &rpcErr)
			require.Equal(t, tc.drive, rpcErr.DriveAuthorizationMissing())
			require.Equal(t, tc.region, rpcErr.RegionNotSupported())
			require.Contains(t, err.Error(), tc.method)
		})
	}
	_, err := ParseModels([]byte(`[null,[16,"expired"]]`))
	require.ErrorIs(t, err, ErrAuthenticationRequired)
}

func TestSessionSignsWithOriginCookiesAndSendsTargetCookies(t *testing.T) {
	state := StorageState{Cookies: []StateCookie{
		{Name: "__Secure-1PSID", Value: "sid", Domain: ".google.com", Path: "/", Expires: -1, Secure: true},
		{Name: "SAPISID", Value: "origin", Domain: ".google.com", Path: "/", Expires: -1, Secure: true},
		{Name: "__Secure-1PAPISID", Value: "one", Domain: ".google.com", Path: "/", Expires: -1, Secure: true},
		{Name: "__Secure-3PAPISID", Value: "three", Domain: ".google.com", Path: "/", Expires: -1, Secure: true},
	}}
	state.Cookies = append(state.Cookies, StateCookie{Name: "SAPISID", Value: "target", Domain: "alkalimakersuite-pa.clients6.google.com", Path: "/", Expires: -1, Secure: true})
	client := &http.Client{Transport: protocolRoundTrip(func(req *http.Request) (*http.Response, error) {
		origin, err := state.CookieHeader(AIStudioReferer, time.Now())
		require.NoError(t, err)
		want, err := BuildAuthorizationHeader(origin, AIStudioOrigin, time.Now())
		require.NoError(t, err)
		require.Equal(t, want, req.Header.Get("Authorization"))
		require.Contains(t, req.Header.Get("Cookie"), "SAPISID=target")
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("[]"))}, nil
	})}
	session, err := NewSession(SessionOptions{State: state, Client: client})
	require.NoError(t, err)
	defer session.Close()
	response, err := session.Do(context.Background(), http.MethodPost, MakerSuiteRPCBase+"ListModels", []byte("[]"), http.Header{"Authorization": {"refresh-signature"}})
	require.NoError(t, err)
	response.Body.Close()
}

func TestPlaygroundSOCKS5HProxy(t *testing.T) {
	require.NoError(t, ValidateProxy("socks5h://127.0.0.1:7895"))
	_, err := NewProxyHTTPClient("socks5h://127.0.0.1:7895")
	require.NoError(t, err)
}

func TestPageBootstrapUsesDynamicHeadersAndParallelChallenge(t *testing.T) {
	state := StorageState{Cookies: []StateCookie{
		{Name: "__Secure-1PSID", Value: "sid", Domain: ".google.com", Path: "/", Expires: -1, Secure: true},
		{Name: "SAPISID", Value: "sap", Domain: ".google.com", Path: "/", Expires: -1, Secure: true},
		{Name: "__Secure-1PAPISID", Value: "one", Domain: ".google.com", Path: "/", Expires: -1, Secure: true},
		{Name: "__Secure-3PAPISID", Value: "three", Domain: ".google.com", Path: "/", Expires: -1, Secure: true},
	}}
	challengeStarted := make(chan struct{})
	var once sync.Once
	tierResponse := `[1]`
	wantTier := "TIER1"
	client := &http.Client{Transport: protocolRoundTrip(func(req *http.Request) (*http.Response, error) {
		body := "[]"
		status := 200
		headers := make(http.Header)
		switch {
		case req.URL.Path == "/prompts/new_chat":
			body = `<html><script>var cfg={"WIu0Nc":"dynamic-page-key"}</script></html>`
			headers.Add("Set-Cookie", "SIDCC=page-rotated; Domain=.google.com; Path=/; Secure")
		case req.URL.Host == "waa-pa.clients6.google.com":
			once.Do(func() { close(challengeStarted) })
			body = `[["message",["interpreter"],[],"hash","program","global",null,"experiments"]]`
		case strings.HasSuffix(req.URL.Path, "/ListModels"):
			select {
			case <-challengeStarted:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			body = `[[["models/dynamic-model"]]]`
		case strings.HasSuffix(req.URL.Path, "/GetLoggingContext"):
			body = `["logging",7]`
		case strings.HasSuffix(req.URL.Path, "/GetAiStudioBenefitTier"):
			body = tierResponse
		case strings.HasSuffix(req.URL.Path, "/GenerateContent"):
			if req.Header.Get("X-AIStudio-G1-Tier") != wantTier {
				return nil, errors.New("generation did not use the account's current benefit tier")
			}
		case strings.HasSuffix(req.URL.Path, "/GenerateAccessToken"), strings.HasSuffix(req.URL.Path, "/ListRecentApplets"), strings.HasSuffix(req.URL.Path, "/ListPrompts"):
			status, body = 401, `[16,"OAuth error: unauthorized_client."]`
		}
		if req.Method == http.MethodPost && strings.Contains(req.URL.Host, "alkalimakersuite") {
			if !strings.HasSuffix(req.URL.Path, "/GenerateContent") && req.Header.Get("X-AIStudio-G1-Tier") != "" {
				return nil, errors.New("benefit tier leaked into a non-generation RPC")
			}
			if req.Header.Get("X-Goog-Api-Key") != "dynamic-page-key" || req.Header.Get("X-AIStudio-Visit-Id") == "" || !strings.Contains(req.Header.Get("Cookie"), "SIDCC=page-rotated") {
				return nil, errors.New("page bootstrap did not share dynamic context and cookies")
			}
		}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	s, err := NewSession(SessionOptions{State: state, Client: client})
	require.NoError(t, err)
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response, err := s.RPC(ctx, "ListModels", []byte("[]"))
	require.NoError(t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	models, err := ParseModels(raw)
	require.NoError(t, err)
	require.Equal(t, []string{"models/dynamic-model"}, models)
	require.NotEmpty(t, s.headers.Get("X-Goog-Ext-519733851-Bin"))
	require.NotNil(t, s.pendingChallenge)
	response, err = s.RPC(ctx, "GenerateContent", []byte("[]"))
	require.NoError(t, err)
	response.Body.Close()

	// A recovered session must reload its benefit and discard a previous paid tier.
	tierResponse, wantTier = `[0]`, ""
	require.NoError(t, s.ReplaceState(state))
	response, err = s.RPC(ctx, "GenerateContent", []byte("[]"))
	require.NoError(t, err)
	response.Body.Close()
}

func TestBenefitTierHeader(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		wantError bool
	}{
		{`[]`, "", false},
		{`[null]`, "", false},
		{`[0]`, "", false},
		{`[1]`, "TIER1", false},
		{`[2]`, "TIER2", false},
		{`[3]`, "TIER0", false},
		{`[1,null,"extra"]`, "TIER1", false},
		{`[4]`, "", true},
		{`[-1]`, "", true},
		{`["1"]`, "", true},
		{`[1.5]`, "", true},
		{`{}`, "", true},
		{``, "", true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := benefitTierHeader([]byte(tc.raw))
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
