package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/camoufox"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

func (h *AccountHandler) StartPlaygroundLogin(c *gin.Context) {
	var req struct {
		ProxyID *int64 `json:"proxy_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid login request")
		return
	}
	if h.playgroundBrowser == nil {
		response.BadRequest(c, "Camoufox login service unavailable")
		return
	}
	id, err := h.playgroundBrowser.Begin(c.Request.Context(), req.ProxyID)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, gin.H{"id": id})
}
func (h *AccountHandler) PlaygroundLoginFrame(c *gin.Context) {
	frame, err := h.playgroundBrowser.Frame(c.Request.Context(), c.Param("loginID"))
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, gin.H{"image": frame})
}
func (h *AccountHandler) PlaygroundLoginInput(c *gin.Context) {
	var input camoufox.Input
	if err := c.ShouldBindJSON(&input); err != nil {
		response.BadRequest(c, "Invalid browser input")
		return
	}
	if err := h.playgroundBrowser.Input(c.Request.Context(), c.Param("loginID"), input); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, gin.H{"ok": true})
}
func (h *AccountHandler) FinishPlaygroundLogin(c *gin.Context) {
	token, email, err := h.playgroundBrowser.Finish(c.Request.Context(), c.Param("loginID"))
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, gin.H{"token": token, "email": email})
}
func (h *AccountHandler) CancelPlaygroundLogin(c *gin.Context) {
	h.playgroundBrowser.Cancel(c.Param("loginID"))
	response.Success(c, gin.H{"ok": true})
}
