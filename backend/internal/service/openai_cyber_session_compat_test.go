package service

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCyberSessionBlockKeyWithoutContext(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	for _, body := range []string{
		`{"prompt_cache_key":"session-one"}`,
		`{"type":"response.create","response":{"prompt_cache_key":"session-one"}}`,
	} {
		key := CyberSessionBlockKey(1801, nil, []byte(body))
		require.NotEmpty(t, key)
		require.Equal(t, CyberSessionBlockKey(1801, c, []byte(body)), key)
		require.NotEqual(t, CyberSessionBlockKey(1802, nil, []byte(body)), key)
	}
	require.Empty(t, CyberSessionBlockKey(1801, nil, []byte(`{"previous_response_id":"response-one"}`)))
}
