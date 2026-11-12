package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlaygroundBatchProxyPreflightFailsClosed(t *testing.T) {
	var called atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		require.Equal(t, http.MethodConnect, r.Method)
		require.Equal(t, "accounts.google.com:443", r.Host)
		require.NotEmpty(t, r.Header.Get("Proxy-Authorization"))
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	port, _ := strconv.Atoi(u.Port())
	proxy := &Proxy{Protocol: "http", Host: u.Hostname(), Port: port, Username: "synthetic-user", Password: "proxy-private-marker", Status: StatusActive}
	err = checkPlaygroundBatchProxy(context.Background(), proxy)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "proxy-private-marker")
	require.EqualValues(t, 1, called.Load())
	require.Error(t, checkPlaygroundBatchProxy(context.Background(), nil))
	proxy.Status = "inactive"
	require.Error(t, checkPlaygroundBatchProxy(context.Background(), proxy))
	require.EqualValues(t, 1, called.Load())
}
