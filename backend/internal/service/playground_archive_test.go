package service

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type playgroundArchiveProxyRepo struct {
	ProxyRepository
	proxy Proxy
}

func (r *playgroundArchiveProxyRepo) GetByID(context.Context, int64) (*Proxy, error) {
	return &r.proxy, nil
}

func TestPlaygroundArchiveGrantNormalizesToNewProfile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PLAYGROUND_PROFILE_DIR", root)
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	proxyID := int64(11)
	proxies := &playgroundArchiveProxyRepo{proxy: Proxy{ID: proxyID, Protocol: "http", Host: "proxy.example.test", Port: 3128}}
	browser := NewPlaygroundBrowserService(&aistudio.SessionCache{Client: client}, proxies)
	source := uuid.NewString()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	file, err := writer.Create("profiles/" + source + "/cookies.sqlite")
	require.NoError(t, err)
	_, err = file.Write([]byte("synthetic-cookie-database"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	archive, err := zip.NewReader(bytes.NewReader(buffer.Bytes()), int64(buffer.Len()))
	require.NoError(t, err)
	token, rollback, err := browser.ImportProfile(context.Background(), archive, source, "user@example.test", &proxyID)
	require.NoError(t, err)
	admin := &adminServiceImpl{playgroundBrowser: browser}
	credentials, err := admin.normalizePlaygroundCredentials(context.Background(), PlatformGemini, AccountTypePlayground, &proxyID,
		map[string]any{"playground_profile_id": source, "playground_login_token": token, "model_mapping": map[string]any{"test": "model"}}, nil)
	require.NoError(t, err)
	restored := credentials["playground_profile_id"].(string)
	require.NotEqual(t, source, restored)
	require.Equal(t, "user@example.test", credentials["playground_email"])
	require.Contains(t, credentials, "model_mapping")
	require.NotContains(t, credentials, "playground_login_token")
	require.FileExists(t, filepath.Join(root, restored, "cookies.sqlite"))
	_, err = browser.Cache.ConsumeGrant(context.Background(), token)
	require.Error(t, err)
	rollback()
	_, err = os.Stat(filepath.Join(root, restored))
	require.True(t, os.IsNotExist(err))
}
