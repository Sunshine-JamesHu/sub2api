package admin

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/camoufox"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *AccountHandler) batchOwner(c *gin.Context) (int64, bool) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	role, _ := middleware.GetUserRoleFromContext(c)
	if !ok || subject.UserID <= 0 || role != "admin" || adminaccess.FromContext(c.Request.Context()) != nil {
		response.Error(c, http.StatusForbidden, "批量上号需要完整管理员权限")
		return 0, false
	}
	if h.playgroundBatch == nil {
		response.Error(c, http.StatusServiceUnavailable, "批量上号暂不可用，请检查加密密钥和任务存储")
		return 0, false
	}
	c.Header("Cache-Control", "no-store")
	return subject.UserID, true
}

func (h *AccountHandler) PlaygroundBatches(c *gin.Context) {
	owner, ok := h.batchOwner(c)
	if !ok {
		return
	}
	response.Success(c, h.playgroundBatch.View(owner, c.Query("batch_id")))
}
func (h *AccountHandler) CreatePlaygroundBatch(c *gin.Context) {
	owner, ok := h.batchOwner(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2<<20)
	var request service.PlaygroundBatchRequest
	if c.ShouldBindJSON(&request) != nil {
		response.BadRequest(c, "导入内容格式不正确或超过 2MiB")
		return
	}
	id, err := h.playgroundBatch.Create(c.Request.Context(), owner, request)
	request.Accounts = ""
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, gin.H{"id": id})
}
func (h *AccountHandler) ConfigurePlaygroundBatches(c *gin.Context) {
	owner, ok := h.batchOwner(c)
	if !ok {
		return
	}
	var req struct {
		Concurrency int `json:"concurrency"`
	}
	if c.ShouldBindJSON(&req) != nil {
		response.BadRequest(c, "并发参数不正确")
		return
	}
	if err := h.playgroundBatch.Configure(owner, req.Concurrency); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, gin.H{"ok": true})
}
func (h *AccountHandler) PlaygroundBatchAction(c *gin.Context) {
	owner, ok := h.batchOwner(c)
	if !ok {
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if c.ShouldBindJSON(&req) != nil {
		response.BadRequest(c, "操作参数不正确")
		return
	}
	if err := h.playgroundBatch.Action(owner, c.Param("batchID"), c.Param("jobID"), req.Action); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, gin.H{"ok": true})
}
func (h *AccountHandler) PlaygroundBatchFrame(c *gin.Context) {
	owner, ok := h.batchOwner(c)
	if !ok {
		return
	}
	image, live, err := h.playgroundBatch.ManualFrame(c.Request.Context(), owner, c.Param("batchID"), c.Param("jobID"))
	if err != nil {
		response.Error(c, http.StatusConflict, err.Error())
		return
	}
	response.Success(c, gin.H{"image": image, "live": live})
}
func (h *AccountHandler) PlaygroundBatchInput(c *gin.Context) {
	owner, ok := h.batchOwner(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	var input camoufox.Input
	if c.ShouldBindJSON(&input) != nil {
		response.BadRequest(c, "浏览器操作格式不正确")
		return
	}
	if err := h.playgroundBatch.ManualInput(c.Request.Context(), owner, c.Param("batchID"), c.Param("jobID"), input); err != nil {
		response.Error(c, http.StatusConflict, err.Error())
		return
	}
	response.Success(c, gin.H{"ok": true})
}
func (h *AccountHandler) FinishPlaygroundBatchJob(c *gin.Context) {
	owner, ok := h.batchOwner(c)
	if !ok {
		return
	}
	if err := h.playgroundBatch.ManualFinish(c.Request.Context(), owner, c.Param("batchID"), c.Param("jobID")); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, gin.H{"ok": true})
}
