package aistudio

import (
	"bytes"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// pageResourceKind 是官网文档 HTML 引用、浏览器解析文档时请求的子资源类别
type pageResourceKind int

const (
	// pageStylesheet 是 <link rel="stylesheet">
	pageStylesheet pageResourceKind = iota
	// pagePreloadScript 是 <link rel="preload" as="script">
	pagePreloadScript
	// pageScript 是没有预载的同步 <script src>
	pageScript
)

// pageResource 是官网文档 HTML 引用的一个子资源
type pageResource struct {
	url  string
	kind pageResourceKind
}

// pageIcon 是官网文档 HTML 中的一个图标链接，width 为 sizes 中第一个有效尺寸的宽度，没有时为 -1
type pageIcon struct {
	url   string
	width int
	// rich 表示 apple-touch-icon、apple-touch-icon-precomposed 或 fluid-icon
	rich bool
}

// pageDocument 是从官网文档 HTML 中解析出的子资源与图标，顺序与文档相同
type pageDocument struct {
	resources []pageResource
	icons     []pageIcon
}

// iconSizePattern 匹配 sizes 属性中的一个有效尺寸
var iconSizePattern = regexp.MustCompile(`(?i)^([1-9][0-9]*)x[1-9][0-9]*$`)

// parsePageDocument 按 Firefox 解析文档的规则取出官网文档 HTML 引用的样式表、预载脚本、同步脚本与图标，地址按文档 base 解析
func parsePageDocument(body []byte, pageURL string) pageDocument {
	base, _ := url.Parse(pageURL)
	resolve := func(raw string) string {
		target, err := base.Parse(strings.TrimSpace(raw))
		if err != nil {
			return ""
		}
		return target.String()
	}
	var document pageDocument
	preloaded := map[string]bool{}
	tokenizer := html.NewTokenizer(bytes.NewReader(body))
	noscript := false
	for {
		kind := tokenizer.Next()
		if kind == html.ErrorToken {
			return document
		}
		token := tokenizer.Token()
		if token.DataAtom == atom.Noscript {
			noscript = kind == html.StartTagToken
			continue
		}
		if noscript || kind != html.StartTagToken && kind != html.SelfClosingTagToken {
			continue
		}
		attributes := map[string]string{}
		for _, attribute := range token.Attr {
			attributes[attribute.Key] = attribute.Val
		}
		switch token.DataAtom {
		case atom.Base:
			if href, ok := attributes["href"]; ok {
				if parsed, err := base.Parse(href); err == nil {
					base = parsed
				}
			}
		case atom.Link:
			href := resolve(attributes["href"])
			if href == "" {
				continue
			}
			rels := strings.Fields(strings.ToLower(attributes["rel"]))
			switch {
			case slices.Contains(rels, "stylesheet"):
				document.resources = append(document.resources, pageResource{url: href, kind: pageStylesheet})
			case slices.Contains(rels, "preload") && strings.EqualFold(attributes["as"], "script"):
				document.resources = append(document.resources, pageResource{url: href, kind: pagePreloadScript})
				preloaded[href] = true
			default:
				rich := slices.Contains(rels, "apple-touch-icon") || slices.Contains(rels, "apple-touch-icon-precomposed") || slices.Contains(rels, "fluid-icon")
				if rich || slices.Contains(rels, "icon") {
					document.icons = append(document.icons, pageIcon{url: href, width: iconWidth(attributes["sizes"]), rich: rich})
				}
			}
		case atom.Script:
			_, async := attributes["async"]
			_, deferred := attributes["defer"]
			source := resolve(attributes["src"])
			if attributes["src"] == "" || async || deferred || attributes["type"] == "module" || preloaded[source] {
				continue
			}
			document.resources = append(document.resources, pageResource{url: source, kind: pageScript})
		}
	}
}

// iconWidth 按 Firefox 读取 sizes 中第一个有效尺寸的宽度，any 或没有有效尺寸时为 -1
func iconWidth(sizes string) int {
	for _, size := range strings.Fields(sizes) {
		if strings.EqualFold(size, "any") {
			return -1
		}
		if match := iconSizePattern.FindStringSubmatch(size); match != nil {
			width, _ := strconv.Atoi(match[1])
			return width
		}
	}
	return -1
}

// firefoxRichIconMinWidth 是 Firefox 把普通图标也算作富图标的最小宽度
const firefoxRichIconMinWidth = 96

// selectPageIcons 按 Firefox FaviconLoader 的 selectIcons 选出富图标与标签页图标，preferredWidth 为 16 乘以向上取整的 devicePixelRatio
func selectPageIcons(icons []pageIcon, preferredWidth int) (rich, tab string) {
	var largestRich, bestSized, fallback *pageIcon
	for index := range icons {
		icon := &icons[index]
		if icon.rich || icon.width >= firefoxRichIconMinWidth {
			if largestRich == nil || largestRich.width < icon.width {
				largestRich = icon
			}
		} else {
			fallback = icon
		}
		if icon.width >= preferredWidth && (bestSized == nil || bestSized.width >= icon.width) {
			bestSized = icon
		}
	}
	if largestRich != nil {
		rich = largestRich.url
	}
	switch {
	case bestSized != nil:
		tab = bestSized.url
	case fallback != nil:
		tab = fallback.url
	}
	return rich, tab
}

// fetchSite 返回从官网页面发起的请求的 Sec-Fetch-Site
func fetchSite(target string) string {
	parsed, err := url.Parse(target)
	if err != nil {
		return "cross-site"
	}
	host := parsed.Hostname()
	switch {
	case host == "aistudio.google.com":
		return "same-origin"
	case host == "google.com" || strings.HasSuffix(host, ".google.com"):
		return "same-site"
	}
	return "cross-site"
}

// subresourceHeaders 返回 Firefox 从官网页面请求无 CORS 子资源的请求头
func subresourceHeaders(target, accept, destination, priority string) http.Header {
	headers := http.Header{}
	headers.Set("Accept", accept)
	headers.Set("Referer", aiStudioOrigin+"/")
	site := fetchSite(target)
	if site == "cross-site" {
		headers.Set("Sec-Fetch-Storage-Access", "none")
	}
	headers.Set("Sec-Fetch-Dest", destination)
	headers.Set("Sec-Fetch-Mode", "no-cors")
	headers.Set("Sec-Fetch-Site", site)
	if priority != "" {
		headers.Set("Priority", priority)
	}
	return headers
}
