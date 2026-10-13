// Package websearch 提供 /v1/messages 服务端 web_search 工具的检索后端。
//
// 设计取向：Anthropic 语义里 web_search 是「服务端工具」——由服务端执行检索、
// 把结果作为 web_search_tool_result 块发回。上游（WorkBuddy/CodeBuddy）不实现
// 任何服务端工具，因此网关自己执行检索。本包是一个独立、可替换（Searcher
// 接口）的检索层：
//   - 默认实现走 360 搜索（www.so.com）HTML 抓取：结果页每条自带 data-mdurl
//     真实目标 URL，无需跟随 /link?m= 跳转链（跳转链既慢又容易被风控计数）；
//   - 超时/条数由调用方（server 侧配置）控制，本包不做全局限流；
//   - 解析只依赖结果页稳定的 class 名（res-list / res-title / res-desc）与
//     data-* 属性；页面结构调整时集中在本文件修正（附解析测试 fixture）。
//
// 为什么不用搜索引擎 API：无 key 依赖、部署零配置；检索量级是单机个人网关的
// 频率，HTML 抓取足够。若将来接入正式 API，实现 Searcher 接口替换即可。
package websearch

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Result 单条检索结果（字段与 Anthropic web_search_result 条目对齐）。
type Result struct {
	Title   string
	URL     string
	Snippet string
	PageAge string // 形如 "2026-09-30"；结果页无日期时留空
}

// Searcher 检索后端接口。server 层经此接口调用（测试注入 fake）。
type Searcher interface {
	// Search 执行一次检索，返回至多 max 条结果（max<=0 时按实现默认值）。
	Search(ctx context.Context, query string, max int) ([]Result, error)
}

// DefaultUserAgent 默认 UA：手机/桌面均可，这里用桌面 Chrome——360 搜索对
// 无 UA 的裸请求会降低结果页完整度（结果仍出，但偶发精简版页面）。
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// So 360 搜索（www.so.com）实现。
type So struct {
	// HTTP 检索用 client（不设 Timeout，超时由 ctx 控制；调用方给单次检索
	// 设置 deadline）。
	HTTP *http.Client
	// BaseURL 结果页地址，缺省 https://www.so.com/s（测试可指向 httptest）。
	BaseURL string
	// UserAgent 覆盖默认 UA（空 = DefaultUserAgent）。
	UserAgent string
}

// NewSo 构造默认实现。timeout<=0 时不设 client 级超时（由 ctx 兜底）。
func NewSo(timeout time.Duration, ua string) *So {
	return &So{
		HTTP:      &http.Client{Timeout: timeout},
		BaseURL:   "https://www.so.com/s",
		UserAgent: ua,
	}
}

// Search 实现 Searcher：抓取结果页 → 解析 → 去重截断。
func (s *So) Search(ctx context.Context, query string, max int) ([]Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("empty query")
	}
	if max <= 0 {
		max = 5
	}
	base := s.BaseURL
	if base == "" {
		base = "https://www.so.com/s"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"?q="+url.QueryEscape(query), nil)
	if err != nil {
		return nil, err
	}
	ua := s.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search http status %d", resp.StatusCode)
	}
	// 结果页约 200-400KB；4MB 上限足够容纳（防止异常响应打爆内存）。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("search read body: %w", err)
	}
	results := ParseSoHTML(string(raw), max)
	if len(results) == 0 && looksBlocked(string(raw)) {
		// 结果页出现验证/风控文案 → 明确报错（让调用方回 web_search_tool_result_error，
		// 而不是把「被拦」伪装成「无结果」）。
		return nil, fmt.Errorf("search blocked by engine (verification page)")
	}
	return results, nil
}

// blockedMarkers 命中任一即视为被风控/验证页拦截（无结果时的判据）。
var blockedMarkers = []string{"验证码", "安全验证", "请输入验证", "访问过于频繁"}

func looksBlocked(page string) bool {
	for _, m := range blockedMarkers {
		if strings.Contains(page, m) {
			return true
		}
	}
	return false
}

var (
	// reMdURL 结果条目内的真实目标 URL（360 结果页在 <a> 上挂 data-mdurl，
	// href 只是 /link?m= 跳转链）。
	reMdURL = regexp.MustCompile(`data-mdurl="([^"]*)"`)
	// reResultType 条目类型（normal/richsum/ad 等）。广告（含 "ad"）与
	// spread 位跳过；normal（经典网页结果）与 richsum（富摘要卡片，
	// 2026-10 实测的新闻/聚合卡）等其余类型照常解析。
	reResultType = regexp.MustCompile(`data-result-type="([^"]*)"`)
	// reTitleA 标题：res-title 下的首个 <a> 文本（含 <em> 高亮标签，稍后剥掉）。
	// \s+ 容忍 "<h3   class=...>"（richsum 卡实测有多个空格，曾漏解析整类结果）。
	reTitleA = regexp.MustCompile(`(?s)<h3\s+class="res-title[^"]*"[^>]*>\s*<a[^>]*>(.*?)</a>`)
	// reAnchorTitle 标题兜底：带 data-mdurl 的锚点自身文本（res-title 结构
	// 变化时仍能取到标题）。
	reAnchorTitle = regexp.MustCompile(`(?s)<a[^>]*data-mdurl="[^"]*"[^>]*>(.*?)</a>`)
	// reSummary 摘要：data-result-summary 容器（首选，避开前缀日期文本）。
	reSummary = regexp.MustCompile(`(?s)<span[^>]*data-result-summary[^>]*>(.*?)</span>`)
	// reDesc 摘要兜底：res-desc 段落整体（其内可能含灰色日期前缀，剥掉）。
	reDesc = regexp.MustCompile(`(?s)<p class="res-desc"[^>]*>(.*?)</p>`)
	// reDate 中文日期（结果页摘要前缀，如 "2026年9月30日"）。
	reDate = regexp.MustCompile(`(\d{4})年(\d{1,2})月(\d{1,2})日`)
	// reDescDatePrefix 兜底摘要开头的 "日期 - " 前缀。
	reDescDatePrefix = regexp.MustCompile(`^\s*\d{4}年\d{1,2}月\d{1,2}日\s*[-–—]\s*`)
	// reTag HTML 标签（剥标签用）。
	reTag = regexp.MustCompile(`(?s)<[^>]*>`)
	// reWS 连续空白（压缩为单空格）。
	reWS = regexp.MustCompile(`\s+`)
)

// soSegmentCap 单条目解析窗口上限（字节）：用于把「下一条结果的正文」挡在
// 窗口外。360 结果页条目实测 < 4KB（含高亮/图标），8KB 宽松兜住。
const soSegmentCap = 8 << 10

// ParseSoHTML 解析 360 搜索结果页，返回至多 max 条结果。
//
// 实现要点：按 `<li class="res-list` 切段，而不是用非贪婪正则一次抓到底——
// 条目内部可能嵌套 <li>（相关搜索/子列表），非贪婪 `.*?</li>` 会在第一个内层
// 闭合处截断，丢掉摘要尾部。切段 + 窗口封顶没有这个毛病，且天然容忍残缺 HTML。
func ParseSoHTML(page string, max int) []Result {
	if max <= 0 {
		max = 5
	}
	const marker = `<li class="res-list`
	segs := strings.Split(page, marker)
	out := make([]Result, 0, max)
	seen := map[string]bool{}
	for _, seg := range segs[1:] { // segs[0] 是首个条目之前的页面头
		if len(seg) > soSegmentCap {
			seg = seg[:soSegmentCap]
		}
		if m := reResultType.FindStringSubmatch(seg); m != nil {
			typ := strings.ToLower(m[1])
			// 只按广告特征做黑名单：normal（经典结果）/richsum（富摘要卡）/
			// 未知新类型照常尝试解析——白名单会把新形态整类漏掉（richsum
			// 就是这么漏的）。
			if typ == "spread" || strings.Contains(typ, "ad") {
				continue
			}
		}
		mm := reMdURL.FindStringSubmatch(seg)
		if mm == nil {
			continue
		}
		link := cleanURL(mm[1])
		if link == "" || seen[link] {
			continue
		}
		title := ""
		if tm := reTitleA.FindStringSubmatch(seg); tm != nil {
			title = plainText(tm[1])
		}
		if title == "" {
			if am := reAnchorTitle.FindStringSubmatch(seg); am != nil {
				title = plainText(am[1])
			}
		}
		if title == "" {
			continue // 无标题条目不是可用的检索结果
		}
		snippet := ""
		if sm := reSummary.FindStringSubmatch(seg); sm != nil {
			snippet = plainText(sm[1])
		} else if dm := reDesc.FindStringSubmatch(seg); dm != nil {
			snippet = plainText(dm[1])
			snippet = reDescDatePrefix.ReplaceAllString(snippet, "")
		}
		res := Result{
			Title:   runeCut(title, 220),
			URL:     link,
			Snippet: runeCut(snippet, 400),
		}
		if dm := reDate.FindStringSubmatch(seg); dm != nil {
			res.PageAge = dm[1] + "-" + pad2(dm[2]) + "-" + pad2(dm[3])
		}
		seen[link] = true
		out = append(out, res)
		if len(out) >= max {
			break
		}
	}
	return out
}

// pad2 把 "9" 补成 "09"（月/日两位化）。
func pad2(s string) string {
	if len(s) == 1 {
		return "0" + s
	}
	return s
}

// cleanURL 规范化 data-mdurl 值：HTML 反转义 + 去空白；只接受 http/https。
func cleanURL(raw string) string {
	u := strings.TrimSpace(html.UnescapeString(raw))
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return ""
	}
	return u
}

// plainText 剥 HTML 标签 + 反转义实体 + 压缩空白。
func plainText(s string) string {
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return strings.TrimSpace(reWS.ReplaceAllString(s, " "))
}

// runeCut 按 rune 截断（不切断多字节字符）。
func runeCut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
