package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"net/http"
)

func (h *AccountHandler) warmupService(c *gin.Context) (*service.PlaygroundWarmupService, bool) {
	// Account-scoped admin keys must not inspect or change a process-wide pool.
	subject, authenticated := middleware.GetAuthSubjectFromContext(c)
	role, _ := middleware.GetUserRoleFromContext(c)
	if !authenticated || subject.UserID <= 0 || role != "admin" || adminaccess.FromContext(c.Request.Context()) != nil {
		response.Error(c, http.StatusForbidden, "预热设置需要完整管理员权限")
		return nil, false
	}
	w, err := h.playgroundBrowser.WarmupService()
	if err != nil {
		response.Error(c, http.StatusServiceUnavailable, err.Error())
		return nil, false
	}
	c.Header("Cache-Control", "no-store")
	return w, true
}
func (h *AccountHandler) PlaygroundWarmup(c *gin.Context) {
	w, ok := h.warmupService(c)
	if !ok {
		return
	}
	response.Success(c, w.View())
}
func (h *AccountHandler) ConfigurePlaygroundWarmup(c *gin.Context) {
	w, ok := h.warmupService(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	var config service.PlaygroundWarmupConfig
	if c.ShouldBindJSON(&config) != nil {
		response.BadRequest(c, "预热设置格式不正确")
		return
	}
	if err := w.Configure(c.Request.Context(), config); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, w.View())
}
