//go:build cgo

package service

import (
	"bytes"
	"context"
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
