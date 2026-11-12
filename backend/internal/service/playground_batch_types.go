package service

import (
	"encoding/base32"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode"

	"github.com/Wei-Shaw/sub2api/internal/pkg/camoufox"
)

type PlaygroundBatchRequest struct {
	Backend        string  `json:"backend"`
	Accounts       string  `json:"accounts_text"`
	ProxyIDs       []int64 `json:"proxy_ids"`
	GroupIDs       []int64 `json:"group_ids"`
	Concurrency    int     `json:"concurrency"`
	AutoOnboarding bool    `json:"auto_onboarding"`
}

type batchSecret struct {
	Password string `json:"password"`
	TOTP     string `json:"totp"`
}
type parsedBatchAccount struct {
	Email  string
	Secret batchSecret
}

func parsePlaygroundBatch(text string) ([]parsedBatchAccount, error) {
	if len(text) > 2<<20 {
		return nil, fmt.Errorf("导入内容过大，每批最多 1000 个账号")
	}
	result := []parsedBatchAccount{}
	seen := map[string]bool{}
	for i, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "----")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, fmt.Errorf("第 %d 行格式不正确，请使用 邮箱----密码----2FA密钥", i+1)
		}
		email := strings.ToLower(strings.TrimSpace(parts[0]))
		address, err := mail.ParseAddress(email)
		if err != nil || address.Address != email || len(email) > 254 {
			return nil, fmt.Errorf("第 %d 行邮箱格式不正确", i+1)
		}
		if len(parts[1]) == 0 || len(parts[1]) > 1024 {
			return nil, fmt.Errorf("第 %d 行密码为空或过长", i+1)
		}
		secret := batchSecret{Password: parts[1]}
		if len(parts) == 3 {
			secret.TOTP = strings.ToUpper(strings.Map(func(r rune) rune {
				if unicode.IsSpace(r) {
					return -1
				}
				return r
			}, parts[2]))
			secret.TOTP = strings.TrimRight(secret.TOTP, "=")
			if secret.TOTP != "" {
				decoded, e := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret.TOTP)
				if e != nil || len(decoded) < 10 || len(decoded) > 64 {
					return nil, fmt.Errorf("第 %d 行 2FA 密钥不是有效的 Base32 密钥", i+1)
				}
			}
		}
		key := camoufox.CanonicalGoogleEmail(email)
		if seen[key] {
			return nil, fmt.Errorf("第 %d 行邮箱在本批次中重复", i+1)
		}
		seen[key] = true
		result = append(result, parsedBatchAccount{Email: email, Secret: secret})
		if len(result) > 1000 {
			return nil, fmt.Errorf("每批最多 1000 个账号")
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("请填写要导入的账号")
	}
	return result, nil
}

type playgroundBatchState struct {
	Version     int                `json:"version"`
	Concurrency int                `json:"concurrency"`
	Batches     []*playgroundBatch `json:"batches"`
	Cleanup     []string           `json:"cleanup,omitempty"`
}
type playgroundBatch struct {
	Backend        string                `json:"backend,omitempty"`
	ID             string                `json:"id"`
	Owner          int64                 `json:"owner"`
	Paused         bool                  `json:"paused"`
	ProxyIDs       []int64               `json:"proxy_ids"`
	GroupIDs       []int64               `json:"group_ids"`
	AutoOnboarding bool                  `json:"auto_onboarding"`
	Created        time.Time             `json:"created"`
	Jobs           []*playgroundBatchJob `json:"jobs"`
}
type playgroundBatchJob struct {
	ID              string                       `json:"id"`
	Email           string                       `json:"email"`
	Status          string                       `json:"status"`
	Step            string                       `json:"step"`
	Reason          string                       `json:"reason,omitempty"`
	Secret          *batchSecret                 `json:"secret,omitempty"`
	SecretExpires   time.Time                    `json:"secret_expires"`
	Profile         string                       `json:"profile"`
	ProxyID         int64                        `json:"proxy_id"`
	ProxyName       string                       `json:"proxy_name"`
	ProxyDigest     string                       `json:"proxy_digest,omitempty"`
	Attempts        int                          `json:"attempts"`
	Progress        camoufox.GoogleLoginProgress `json:"progress"`
	AccountID       int64                        `json:"account_id,omitempty"`
	Updated         time.Time                    `json:"updated"`
	LastFrame       string                       `json:"last_frame,omitempty"`
	ResumeURL       string                       `json:"resume_url,omitempty"`
	NextTry         time.Time                    `json:"next_try"`
	ManualRequested bool                         `json:"-"`
}

type PlaygroundBatchJobView struct {
	ID             string    `json:"id"`
	Email          string    `json:"email"`
	Status         string    `json:"status"`
	Step           string    `json:"step"`
	Reason         string    `json:"reason,omitempty"`
	ProxyID        int64     `json:"proxy_id"`
	ProxyName      string    `json:"proxy_name"`
	Attempts       int       `json:"attempts"`
	AccountID      int64     `json:"account_id,omitempty"`
	HasCredentials bool      `json:"has_credentials"`
	ManualOpen     bool      `json:"manual_open"`
	Opening        bool      `json:"opening"`
	Updated        time.Time `json:"updated"`
}
type PlaygroundBatchView struct {
	Backend        string                   `json:"backend"`
	ID             string                   `json:"id"`
	Paused         bool                     `json:"paused"`
	Created        time.Time                `json:"created"`
	ProxyIDs       []int64                  `json:"proxy_ids"`
	GroupIDs       []int64                  `json:"group_ids"`
	AutoOnboarding bool                     `json:"auto_onboarding"`
	Counts         map[string]int           `json:"counts"`
	Jobs           []PlaygroundBatchJobView `json:"jobs"`
}
type PlaygroundBatchViewState struct {
	Concurrency int                      `json:"concurrency"`
	Running     int                      `json:"running"`
	Capacity    camoufox.BrowserCapacity `json:"capacity"`
	Error       string                   `json:"error,omitempty"`
	Batches     []PlaygroundBatchView    `json:"batches"`
}

func batchTerminal(status string) bool {
	return status == "succeeded" || status == "skipped" || status == "cancelled"
}

func batchReason(code string) string {
	values := map[string]string{
		"account_saved_check_groups": "账号已保存，请检查分组绑定", "totp_rejected": "动态验证码未通过，请人工核对密钥或时间", "owner_inactive": "创建任务的管理员已停用或权限已变化", "captcha": "需要人机验证", "security_verification": "Google 要求额外安全验证", "onboarding": "需要确认 AI Studio 首次引导", "unexpected_page": "出现未识别的登录页面，请人工确认", "invalid_credentials": "邮箱或密码被拒绝", "account_disabled": "账号已被停用", "totp_required": "需要人工提供动态验证码", "invalid_totp_secret": "动态验证码密钥无效", "credentials_required": "凭据已清除，请重新导入", "identity_mismatch": "登录邮箱与导入账号不一致，未保存", "submission_unconfirmed": "登录步骤已提交但未确认成功，已停止重复提交", "browser_unavailable": "浏览器连接中断，请重新处理", "page_changed": "登录页面已变化，请人工确认", "login_not_ready": "登录页面未完成，请人工处理", "network_error": "代理网络或页面连接失败", "proxy_unavailable": "代理不可用，已停止直连回退", "storage_error": "任务保存失败，自动任务已暂停", "account_save_failed": "登录成功，但账号保存失败；可重试保存", "already_exists": "账号已存在，已跳过", "active_duplicate": "已有同邮箱的上号任务，已跳过", "expired": "保存的上号凭据已过期，请重新导入", "cancelled": "任务已取消", "timeout": "登录等待超时，请人工处理", "waiting_proxy": "等待可用代理", "waiting_memory": "等待可用浏览器内存", "interrupted": "服务重启，继续已保存的任务", "manual_closed": "人工窗口已关闭，可重新打开", "manual_opening": "正在打开人工处理窗口", "account_saved": "账号已保存", "paused": "已暂停新任务",
	}
	if value, ok := values[code]; ok {
		return value
	}
	return code
}
