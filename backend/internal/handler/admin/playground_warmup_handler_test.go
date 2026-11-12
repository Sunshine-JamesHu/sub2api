package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPlaygroundWarmupRequiresFullAdmin(t *testing.T) {
	for _, tc := range []struct {
		name, role string
		user       int64
		scoped     bool
	}{{"anonymous", "", 0, false}, {"user", "user", 1, false}, {"scoped admin", "admin", 1, true}} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/warmup", nil)
			if tc.user > 0 {
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: tc.user})
			}
			c.Set(string(middleware.ContextKeyUserRole), tc.role)
			if tc.scoped {
				c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), &adminaccess.Principal{CreatorID: tc.user}))
			}
			h := &AccountHandler{}
			h.PlaygroundWarmup(c)
			require.Equal(t, http.StatusForbidden, c.Writer.Status())
		})
	}
}
