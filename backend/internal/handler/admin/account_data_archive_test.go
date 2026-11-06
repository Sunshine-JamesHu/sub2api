package admin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func setupAccountArchiveRouter(t *testing.T) (*gin.Engine, *stubAdminService, *service.PlaygroundBrowserService, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	t.Setenv("PLAYGROUND_PROFILE_DIR", root)
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	browser := service.NewPlaygroundBrowserService(&aistudio.SessionCache{Client: client}, nil)
	admin := newStubAdminService()
	handler := &AccountHandler{adminService: admin, playgroundBrowser: browser}
	router := gin.New()
	router.GET("/data", handler.ExportData)
	router.POST("/data", handler.ImportData)
	return router, admin, browser, root
}

func accountArchiveUpload(t *testing.T, archive []byte) *http.Request {
	t.Helper()
	var buffer bytes.Buffer
	form := multipart.NewWriter(&buffer)
	file, err := form.CreateFormFile("file", "accounts.zip")
	require.NoError(t, err)
	_, err = file.Write(archive)
	require.NoError(t, err)
	require.NoError(t, form.Close())
	request := httptest.NewRequest("POST", "/data", &buffer)
	request.Header.Set("Content-Type", form.FormDataContentType())
	return request
}

func TestAccountArchiveRoundTripAndRollback(t *testing.T) {
	for _, failCreate := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "create-failure"}[failCreate], func(t *testing.T) {
			router, admin, browser, root := setupAccountArchiveRouter(t)
			profile := uuid.NewString()
			require.NoError(t, os.Mkdir(filepath.Join(root, profile), 0700))
			require.NoError(t, os.WriteFile(filepath.Join(root, profile, "cookies.sqlite"), []byte("synthetic-cookies"), 0600))
			admin.accounts = []service.Account{
				{Name: "playground", Platform: service.PlatformGemini, Type: service.AccountTypePlayground, Credentials: map[string]any{"playground_profile_id": profile, "playground_email": "test@example.test"}},
				{Name: "regular", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "synthetic-key"}},
			}
			export := httptest.NewRecorder()
			router.ServeHTTP(export, httptest.NewRequest("GET", "/data", nil))
			require.Equal(t, 200, export.Code, export.Body.String())
			require.Equal(t, "application/zip", export.Header().Get("Content-Type"))
			require.Contains(t, export.Header().Get("Content-Disposition"), ".zip")
			if failCreate {
				admin.createAccountErr = errors.New("synthetic database failure")
			}
			imported := httptest.NewRecorder()
			router.ServeHTTP(imported, accountArchiveUpload(t, export.Body.Bytes()))
			require.Equal(t, 200, imported.Code, imported.Body.String())
			var result struct {
				Data DataImportResult `json:"data"`
			}
			require.NoError(t, json.Unmarshal(imported.Body.Bytes(), &result))
			if failCreate {
				require.Equal(t, 2, result.Data.AccountFailed)
				entries, err := os.ReadDir(root)
				require.NoError(t, err)
				require.Len(t, entries, 1)
				return
			}
			require.Equal(t, 2, result.Data.AccountCreated)
			require.Len(t, admin.createdAccounts, 2)
			token, _ := admin.createdAccounts[0].Credentials["playground_login_token"].(string)
			grant, err := browser.Cache.ConsumeGrant(context.Background(), token)
			require.NoError(t, err)
			require.NotEqual(t, profile, grant.Profile)
			require.Equal(t, "test@example.test", grant.Email)
			copied, err := os.ReadFile(filepath.Join(root, grant.Profile, "cookies.sqlite"))
			require.NoError(t, err)
			require.Equal(t, "synthetic-cookies", string(copied))
		})
	}
}

func TestAccountArchiveRejectsMissingProfileBeforeCreatingData(t *testing.T) {
	router, admin, _, _ := setupAccountArchiveRouter(t)
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	manifest, err := writer.Create("accounts.json")
	require.NoError(t, err)
	require.NoError(t, json.NewEncoder(manifest).Encode(DataPayload{Proxies: []DataProxy{}, Accounts: []DataAccount{{Name: "missing", Platform: service.PlatformGemini, Type: service.AccountTypePlayground, Credentials: map[string]any{"playground_profile_id": uuid.NewString(), "playground_email": "test@example.test"}}}}))
	require.NoError(t, writer.Close())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, accountArchiveUpload(t, buffer.Bytes()))
	require.Equal(t, 400, response.Code)
	require.Empty(t, admin.createdAccounts)
	require.Empty(t, admin.createdProxies)
}

func TestAccountArchiveRejectsBarePlaygroundJSON(t *testing.T) {
	router, admin, _, _ := setupAccountArchiveRouter(t)
	body, err := json.Marshal(DataImportRequest{Data: DataPayload{Proxies: []DataProxy{}, Accounts: []DataAccount{{Name: "bare", Platform: service.PlatformGemini, Type: service.AccountTypePlayground, Credentials: map[string]any{"playground_profile_id": uuid.NewString()}}}}})
	require.NoError(t, err)
	request := httptest.NewRequest("POST", "/data", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, 200, response.Code)
	require.Contains(t, response.Body.String(), "require a ZIP")
	require.Empty(t, admin.createdAccounts)
}
