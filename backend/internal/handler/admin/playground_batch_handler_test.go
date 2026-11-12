package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type batchHandlerProxies struct{ service.ProxyRepository }

func (*batchHandlerProxies) GetByID(context.Context, int64) (*service.Proxy, error) {
	return &service.Proxy{ID: 1, Protocol: "http", Host: "198.51.100.1", Port: 8080, Status: service.StatusActive}, nil
}
func (*batchHandlerProxies) ListActive(context.Context) ([]service.Proxy, error) { return nil, nil }

type batchHandlerAdmin struct{ service.AdminService }

func (*batchHandlerAdmin) ValidateAccountGroupBindings(context.Context, []int64) error { return nil }
func (*batchHandlerAdmin) ListAccountsForSchedulerScoreFilter(context.Context, string, string, string, string, int64, string) ([]service.Account, error) {
	return nil, nil
}

func TestPlaygroundBatchHTTPRequiresFullAdminAndIsolatesOwners(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("PLAYGROUND_PROFILE_DIR", "")
	redisServer := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	defer client.Close()
	browser := service.NewPlaygroundBrowserService(&aistudio.SessionCache{Client: client}, &batchHandlerProxies{})
	defer browser.Close()
	cfg := &config.Config{}
	cfg.Totp.EncryptionKey = strings.Repeat("41", 32)
	cfg.Totp.EncryptionKeyConfigured = true
	batch, err := service.NewPlaygroundBatchService(browser, &batchHandlerAdmin{}, cfg)
	require.NoError(t, err)
	defer batch.Close()
	h := &AccountHandler{playgroundBatch: batch}
	owner := int64(1)
	role := "admin"
	scoped := false
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: owner})
		c.Set(string(middleware.ContextKeyUserRole), role)
		if scoped {
			c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), &adminaccess.Principal{CreatorID: 1}))
		}
	})
	r.POST("/batches", h.CreatePlaygroundBatch)
	r.GET("/batches", h.PlaygroundBatches)
	r.POST("/batches/:batchID/actions", h.PlaygroundBatchAction)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		r.ServeHTTP(out, request)
		return out
	}
	body := `{"accounts_text":"owner@example.test----private-password-marker----JBSWY3DPEHPK3PXP","proxy_ids":[1],"concurrency":20}`
	role = "user"
	require.Equal(t, http.StatusForbidden, call("POST", "/batches", body).Code)
	role = "admin"
	scoped = true
	require.Equal(t, http.StatusForbidden, call("POST", "/batches", body).Code)
	scoped = false
	created := call("POST", "/batches", body)
	require.Equal(t, 200, created.Code, created.Body.String())
	var decoded struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &decoded))
	visible := call("GET", "/batches", "")
	require.Equal(t, 200, visible.Code)
	require.Contains(t, visible.Body.String(), "owner@example.test")
	require.NotContains(t, visible.Body.String(), "private-password-marker")
	require.NotContains(t, visible.Body.String(), "JBSWY3DPEHPK3PXP")
	owner = 2
	hidden := call("GET", "/batches", "")
	require.NotContains(t, hidden.Body.String(), "owner@example.test")
	require.Equal(t, 400, call("POST", "/batches/"+decoded.Data.ID+"/actions", `{"action":"cancel"}`).Code)
}
