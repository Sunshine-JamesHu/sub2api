//go:build cgo

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGeminiResponsesPlaygroundUsesCookieGateway(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			repo, account := newPlaygroundTestAccount(t)
			gateway, _ := newPlaygroundTestGateway(t, repo)
			account.Credentials["model_mapping"] = map[string]any{"alias": "gemini-3.8-flash"}
			calls := 0
			gateway.generateContent = func(_ context.Context, _ *aistudio.Session, model string, body []byte) (*http.Response, error) {
				calls++
				require.Equal(t, "gemini-3.8-flash", model)
				require.Contains(t, string(body), "Hi")
				return playgroundSuccessResponse(), nil
			}
			// No HTTP upstream or OAuth token provider: Playground must use its cookie gateway.
			svc := &GeminiMessagesCompatService{cfg: &config.Config{}, playgroundGatewayService: gateway}
			body := []byte(`{"model":"alias","input":"Hi","stream":` + map[bool]string{false: "false", true: "true"}[stream] + `}`)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(body))
			result, err := svc.ForwardAsResponses(context.Background(), c, account, body)
			require.NoError(t, err)
			require.Equal(t, 1, calls)
			require.Equal(t, "alias", result.Model)
			require.Equal(t, "gemini-3.8-flash", result.UpstreamModel)
			require.Contains(t, rec.Body.String(), "OK")
			if stream {
				require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.completed\n"))
			} else {
				require.Contains(t, rec.Body.String(), `"object":"response"`)
			}
		})
	}
}

func TestGeminiResponsesPlaygroundRestoresLongToolNames(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			repo, account := newPlaygroundTestAccount(t)
			gateway, _ := newPlaygroundTestGateway(t, repo)
			name := "mcp__plugin_cloudbase_skills_cloudbase__" + strings.Repeat("long_tool_", 5)
			alias := ""
			gateway.generateContent = func(_ context.Context, _ *aistudio.Session, model string, body []byte) (*http.Response, error) {
				request, err := decodePlaygroundRequest(body)
				require.NoError(t, err)
				raw, err := aistudio.EncodeGenerateContentRequest(model, request, "proof")
				require.NoError(t, err)
				var wire []any
				require.NoError(t, json.Unmarshal(raw, &wire))
				alias = wire[6].([]any)[0].([]any)[1].([]any)[0].([]any)[0].(string)
				require.LessOrEqual(t, len(alias), 63)
				require.NotEqual(t, name, alias)
				part := make([]any, 11)
				part[10] = []any{alias, []any{}, "call_test"}
				candidate := []any{[]any{[]any{part}, "model"}, 1}
				frame := []any{[]any{candidate}}
				raw, err = json.Marshal([]any{[]any{frame}, []any{0}})
				require.NoError(t, err)
				return playgroundResponse(string(raw)), nil
			}
			svc := &GeminiMessagesCompatService{cfg: &config.Config{}, playgroundGatewayService: gateway}
			body, err := json.Marshal(map[string]any{
				"model": "gemini-4-argon", "input": "Call the tool.", "stream": stream,
				"tools": []any{map[string]any{"type": "function", "name": name, "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}},
			})
			require.NoError(t, err)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(body))
			_, err = svc.ForwardAsResponses(context.Background(), c, account, body)
			require.NoError(t, err)
			require.Contains(t, rec.Body.String(), `"name":"`+name+`"`)
			require.NotContains(t, rec.Body.String(), alias)
			if stream {
				require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.completed\n"))
			}
		})
	}
}
