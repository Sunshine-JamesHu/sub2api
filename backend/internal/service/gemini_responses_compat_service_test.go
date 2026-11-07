package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGeminiForwardAsResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		for _, oauth := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{false: "json", true: "stream"}[stream], map[bool]string{false: "apikey", true: "oauth"}[oauth]}, "/"), func(t *testing.T) {
				upstream := `{"candidates":[{"content":{"parts":[{"text":"OK"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3}}`
				if oauth {
					upstream = `{"response":` + upstream + `}`
				}
				if stream || oauth {
					upstream = "data: " + upstream + "\n\n"
				}
				stub := &geminiCompatHTTPUpstreamStub{response: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(upstream))}}
				svc := &GeminiMessagesCompatService{httpUpstream: stub, cfg: &config.Config{}, tokenProvider: &GeminiTokenProvider{}}
				account := &Account{ID: 1, Platform: PlatformGemini, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test"}}
				if oauth {
					account.Type = AccountTypeOAuth
					account.Credentials = map[string]any{"access_token": "test", "project_id": "p"}
				}
				body, _ := json.Marshal(map[string]any{"model": "gemini-3.8-flash", "input": "Hi", "stream": stream})
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(body))
				result, err := svc.ForwardAsResponses(context.Background(), c, account, body)
				require.NoError(t, err)
				require.Equal(t, 200, rec.Code)
				require.Equal(t, 7, result.Usage.InputTokens)
				require.Equal(t, 3, result.Usage.OutputTokens)
				require.Contains(t, stub.lastReq.URL.String(), map[bool]string{false: "generateContent", true: "streamGenerateContent"}[stream || oauth])
				if stream {
					require.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")
					require.Contains(t, rec.Body.String(), "event: response.created\n")
					require.Contains(t, rec.Body.String(), "event: response.output_text.delta\n")
					require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.completed\n"))
					require.NotContains(t, rec.Body.String(), "chat.completion")
					require.NotContains(t, rec.Body.String(), "[DONE]")
				} else {
					require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
					var got map[string]any
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
					require.Equal(t, "response", got["object"])
					require.Equal(t, "completed", got["status"])
					require.Contains(t, rec.Body.String(), `"text":"OK"`)
				}
			})
		}
	}
}

func TestGeminiResponsesCustomToolsAndHistory(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			response := `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"apply_patch","args":{"input":"*** Begin Patch\n*** End Patch"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3}}`
			if stream {
				response = "data: " + response + "\n\n"
			}
			stub := &geminiCompatHTTPUpstreamStub{response: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}}
			svc := &GeminiMessagesCompatService{httpUpstream: stub, cfg: &config.Config{}}
			account := &Account{ID: 1, Platform: PlatformGemini, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test"}}
			body := []byte(`{"model":"gemini-3.8-flash","tools":[{"type":"custom","name":"apply_patch","format":{"type":"text"}},{"type":"web_search"}],"input":[{"role":"user","content":"Make a patch"},{"type":"custom_tool_call","call_id":"call_old","name":"apply_patch","input":"old patch"},{"type":"custom_tool_call_output","call_id":"call_old","output":"Success"}],"stream":` + map[bool]string{false: "false", true: "true"}[stream] + `}`)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(body))
			_, err := svc.ForwardAsResponses(context.Background(), c, account, body)
			require.NoError(t, err)
			sent, err := io.ReadAll(stub.lastReq.Body)
			require.NoError(t, err)
			require.Contains(t, string(sent), "functionDeclarations")
			require.Contains(t, string(sent), "functionResponse")
			require.Contains(t, string(sent), "google_search")
			require.Contains(t, rec.Body.String(), `"type":"custom_tool_call"`)
			require.Contains(t, rec.Body.String(), "*** Begin Patch")
			require.NotContains(t, rec.Body.String(), `"type":"function_call"`)
		})
	}
}

func TestGeminiResponsesTruncatedStreamFails(t *testing.T) {
	stub := &geminiCompatHTTPUpstreamStub{response: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}\n\n"))}}
	svc := &GeminiMessagesCompatService{httpUpstream: stub, cfg: &config.Config{}}
	account := &Account{ID: 1, Platform: PlatformGemini, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test"}}
	body := []byte(`{"model":"gemini-3.8-flash","input":"Hi","stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(body))
	result, err := svc.ForwardAsResponses(context.Background(), c, account, body)
	require.Error(t, err)
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), "event: response.failed\n")
	require.NotContains(t, rec.Body.String(), "event: response.completed\n")
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event))
		if event["type"] == "response.failed" {
			response := event["response"].(map[string]any)
			require.NotEmpty(t, response["id"])
			require.Contains(t, response, "created_at")
		}
	}
}
