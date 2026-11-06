package aistudio

import (
	"encoding/json"
	"fmt"
	"golang.org/x/net/publicsuffix"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// StateCookie 表示 Playwright storage state 中的 Cookie
type StateCookie struct {
	Name         string  `json:"name"`
	Value        string  `json:"value"`
	Domain       string  `json:"domain"`
	Path         string  `json:"path"`
	Expires      float64 `json:"expires"`
	HTTPOnly     bool    `json:"httpOnly"`
	Secure       bool    `json:"secure"`
	SameSite     string  `json:"sameSite"`
	PartitionKey string  `json:"partitionKey,omitempty"`
}

// StorageItem 表示浏览器本地存储项
type StorageItem struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// StorageOrigin 表示 Playwright storage state 中的站点数据
type StorageOrigin struct {
	Origin       string        `json:"origin"`
	LocalStorage []StorageItem `json:"localStorage"`
}

// StorageState 保存浏览器导出到 Redis 的 Cookie、本地存储和指纹。
type StorageState struct {
	Cookies     []StateCookie   `json:"cookies"`
	Origins     []StorageOrigin `json:"origins"`
	Fingerprint json.RawMessage `json:"playground_fingerprint,omitempty"`
}

// Validate 校验 storage state 的浏览器字段
func (s StorageState) Validate() error {
	for index, cookie := range s.Cookies {
		if strings.TrimSpace(cookie.Name) == "" || strings.TrimSpace(cookie.Domain) == "" {
			return fmt.Errorf("storage state Cookie %d 缺少名称或域", index)
		}
		if cookie.Path == "" || cookie.Path[0] != '/' {
			return fmt.Errorf("storage state Cookie %s 的路径无效", cookie.Name)
		}
		switch cookie.SameSite {
		case "", "Lax", "Strict", "None":
		default:
			return fmt.Errorf("storage state Cookie %s 的 SameSite 无效", cookie.Name)
		}
	}
	for index, origin := range s.Origins {
		parsed, err := url.Parse(origin.Origin)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("storage state origin %d 无效", index)
		}
	}
	return nil
}

// CookieHeader 为目标 URL 构造当前有效的 Cookie 请求头
func (s StorageState) CookieHeader(targetURL string, now time.Time) (string, error) {
	target, err := url.Parse(targetURL)
	if err != nil {
		return "", err
	}
	type candidate struct {
		cookie StateCookie
		index  int
	}
	candidates := make([]candidate, 0, len(s.Cookies))
	for index, cookie := range s.Cookies {
		if cookie.Value == "" || cookie.Expires >= 0 && cookie.Expires <= float64(now.UnixNano())/1e9 {
			continue
		}
		if cookie.Secure && !strings.EqualFold(target.Scheme, "https") {
			continue
		}
		if !cookieDomainMatches(cookie.Domain, target.Hostname()) || !cookiePathMatches(cookie.Path, target.EscapedPath()) {
			continue
		}
		candidates = append(candidates, candidate{cookie: cookie, index: index})
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		if len(candidates[left].cookie.Path) == len(candidates[right].cookie.Path) {
			return candidates[left].index < candidates[right].index
		}
		return len(candidates[left].cookie.Path) > len(candidates[right].cookie.Path)
	})
	parts := make([]string, 0, len(candidates))
	for _, item := range candidates {
		parts = append(parts, item.cookie.Name+"="+item.cookie.Value)
	}
	return strings.Join(parts, "; "), nil
}

// CookieValue 返回目标 URL 下最具体的同名 Cookie
func (s StorageState) CookieValue(name string, targetURL string, now time.Time) (string, bool) {
	target, err := url.Parse(targetURL)
	if err != nil {
		return "", false
	}
	selectedPath := -1
	selected := ""
	for _, cookie := range s.Cookies {
		if cookie.Name != name || cookie.Value == "" {
			continue
		}
		if cookie.Expires >= 0 && cookie.Expires <= float64(now.UnixNano())/1e9 {
			continue
		}
		if cookie.Secure && !strings.EqualFold(target.Scheme, "https") {
			continue
		}
		if !cookieDomainMatches(cookie.Domain, target.Hostname()) || !cookiePathMatches(cookie.Path, target.EscapedPath()) {
			continue
		}
		if len(cookie.Path) > selectedPath {
			selected = cookie.Value
			selectedPath = len(cookie.Path)
		}
	}
	return selected, selectedPath >= 0
}

// MergeSetCookieHeaders 将响应轮换的 Cookie 合并到 storage state，替换的 Cookie 保留原有位置
func (s *StorageState) MergeSetCookieHeaders(headers []string, sourceURL string, now time.Time) error {
	source, err := url.Parse(sourceURL)
	if err != nil {
		return err
	}
	for _, header := range headers {
		response := http.Response{Header: http.Header{"Set-Cookie": []string{header}}}
		cookies := response.Cookies()
		if len(cookies) != 1 {
			return fmt.Errorf("Set-Cookie 格式无效")
		}
		incoming := cookies[0]
		domain := strings.ToLower(incoming.Domain)
		host := strings.ToLower(source.Hostname())
		if domain == "" {
			domain = host
		} else {
			domain = strings.TrimPrefix(domain, ".")
			if host != domain && !strings.HasSuffix(host, "."+domain) {
				return fmt.Errorf("Set-Cookie domain does not match response host")
			}
			if suffix, _ := publicsuffix.PublicSuffix(domain); suffix == domain {
				return fmt.Errorf("Set-Cookie cannot target a public suffix")
			}
			domain = "." + domain
		}
		if incoming.Secure && source.Scheme != "https" {
			continue
		}
		if strings.HasPrefix(incoming.Name, "__Secure-") && (!incoming.Secure || source.Scheme != "https") {
			continue
		}
		if strings.HasPrefix(incoming.Name, "__Host-") && (!incoming.Secure || incoming.Domain != "" || incoming.Path != "/") {
			continue
		}
		cookiePath := incoming.Path
		if cookiePath == "" || cookiePath[0] != '/' {
			cookiePath = defaultCookiePath(source.EscapedPath())
		}
		index := -1
		for existingIndex, existing := range s.Cookies {
			if existing.Name == incoming.Name && strings.EqualFold(strings.TrimPrefix(existing.Domain, "."), strings.TrimPrefix(domain, ".")) && existing.Path == cookiePath && existing.PartitionKey == "" {
				index = existingIndex
				break
			}
		}
		if incoming.MaxAge < 0 || incoming.MaxAge == 0 && !incoming.Expires.IsZero() && !incoming.Expires.After(now) {
			if index >= 0 {
				s.Cookies = append(s.Cookies[:index], s.Cookies[index+1:]...)
			}
			continue
		}
		expires := float64(-1)
		if incoming.MaxAge > 0 {
			expires = float64(now.Add(time.Duration(incoming.MaxAge)*time.Second).UnixNano()) / 1e9
		} else if !incoming.Expires.IsZero() {
			expires = float64(incoming.Expires.UnixNano()) / 1e9
		}
		cookie := StateCookie{
			Name:     incoming.Name,
			Value:    incoming.Value,
			Domain:   domain,
			Path:     cookiePath,
			Expires:  expires,
			HTTPOnly: incoming.HttpOnly,
			Secure:   incoming.Secure,
			SameSite: sameSiteText(incoming.SameSite),
		}
		if index >= 0 {
			s.Cookies[index] = cookie
		} else {
			s.Cookies = append(s.Cookies, cookie)
		}
	}
	return nil
}

func googleCookieDomain(domain string) bool {
	domain = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(domain), "."))
	return domain == "google.com" || strings.HasSuffix(domain, ".google.com")
}

func cookieDomainMatches(cookieDomain string, hostname string) bool {
	domain := strings.TrimPrefix(strings.ToLower(cookieDomain), ".")
	host := strings.ToLower(hostname)
	return host == domain || strings.HasPrefix(cookieDomain, ".") && strings.HasSuffix(host, "."+domain)
}

func cookiePathMatches(cookiePath string, requestPath string) bool {
	if requestPath == "" {
		requestPath = "/"
	}
	if cookiePath == "/" {
		return true
	}
	if requestPath == cookiePath {
		return true
	}
	return strings.HasPrefix(requestPath, cookiePath) && (strings.HasSuffix(cookiePath, "/") || len(requestPath) > len(cookiePath) && requestPath[len(cookiePath)] == '/')
}

func defaultCookiePath(requestPath string) string {
	if requestPath == "" || requestPath[0] != '/' {
		return "/"
	}
	index := strings.LastIndex(requestPath, "/")
	if index <= 0 {
		return "/"
	}
	return requestPath[:index]
}

func sameSiteText(value http.SameSite) string {
	switch value {
	case http.SameSiteStrictMode:
		return "Strict"
	case http.SameSiteNoneMode:
		return "None"
	default:
		return "Lax"
	}
}
