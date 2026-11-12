package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestPlaygroundBatchAndKeyboardAuditNeverStoreCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repository := &auditCaptureRepository{}
	audit := service.NewAuditLogService(repository, nil)
	audit.Start()
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(ContextKeyUser), AuthSubject{UserID: 1})
		c.Set(string(ContextKeyUserRole), "admin")
		c.Next()
	})
	router.Use(gin.HandlerFunc(NewAuditLogMiddleware(audit)))
	routes := []struct{ pattern, path string }{
		{"/api/v1/admin/accounts/playground/batches", "/api/v1/admin/accounts/playground/batches"},
		{"/api/v1/admin/accounts/playground/batches/:batchID/jobs/:jobID/input", "/api/v1/admin/accounts/playground/batches/b/jobs/j/input"},
		{"/api/v1/admin/accounts/playground/login/:loginID/input", "/api/v1/admin/accounts/playground/login/j/input"},
		{"/api/v1/admin/proxies/batch", "/api/v1/admin/proxies/batch"},
	}
	body := `{"accounts_text":"owner@example.test----audit-password-canary----JBSWY3DPEHPK3PXP","text":"audit-password-canary"}`
	for _, route := range routes {
		router.POST(route.pattern, func(c *gin.Context) {
			received, err := io.ReadAll(c.Request.Body)
			require.NoError(t, err)
			require.Equal(t, body, string(received))
			c.JSON(200, gin.H{"ok": true})
		})
		request := httptest.NewRequest(http.MethodPost, route.path, bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		require.Equal(t, 200, recorder.Code)
	}
	audit.Stop()
	repository.mu.Lock()
	defer repository.mu.Unlock()
	require.Len(t, repository.logs, len(routes))
	raw, err := json.Marshal(repository.logs)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "audit-password-canary")
	require.NotContains(t, string(raw), "JBSWY3DPEHPK3PXP")
	for _, entry := range repository.logs {
		require.Equal(t, "<credential-bearing body omitted>", entry.RequestBody)
	}
}
