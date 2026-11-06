package admin

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestPlaygroundLoginRejectsUnknownJobWithoutExposingCredentials(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	handler := &AccountHandler{playgroundBrowser: service.NewPlaygroundBrowserService(&aistudio.SessionCache{Client: client}, nil)}
	router := gin.New()
	router.GET("/login/:loginID/frame", handler.PlaygroundLoginFrame)
	router.POST("/login/:loginID/input", handler.PlaygroundLoginInput)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest("GET", "/login/unknown/frame", nil))
	require.Equal(t, 400, recorder.Code)
	require.Contains(t, recorder.Body.String(), "expired")
	recorder = httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/login/unknown/input", strings.NewReader(`{"type":"text","text":"private-password"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	require.Equal(t, 400, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "private-password")
}
