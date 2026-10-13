package websearch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// soSamplePage 真实 360 搜索结果页的结构化摘录（2026-10 实测形态）：
// 两条 normal 结果（含 data-mdurl / data-result-summary / 中文日期前缀）+
// 一条广告位（data-result-type="ad"）+ 一条缺 data-mdurl 的畸形条目。
const soSamplePage = `<html><head><style>.res-desc p{line-height:21px}</style></head><body>
<ul class="result">
<li class="res-list" data-result-type="normal" data-result-pos="1"><h3 class="res-title " > <a href="https://www.so.com/link?m=xxx" data-mdurl="https://www.west.cn/web/dengbao/" rel="noopener" target="_blank"><em>等保测评</em>_等级保护测评 - 西部数码</a></h3> <p class="res-desc"><span class="gray g-c-gray">2026年9月30日&nbsp;-&nbsp;</span><span data-result-summary>西部数码<em>等保测评</em>服务,联合各地机构,提供专业的等级保护测评服务。协助完成测评整改，提升信息系统安全防护能力。</span></p></li>
<li class="res-list" data-result-type="normal" data-result-pos="2"><h3 class="res-title " > <a href="https://www.so.com/link?m=yyy" data-mdurl="https://www.toutiao.com/article/7638886106234159668/" rel="noopener" target="_blank">浅谈<em>等保测评</em> - 今日头条</a></h3> <p class="res-desc"><span class="gray g-c-gray">2026年5月12日&nbsp;-&nbsp;</span><span data-result-summary><em>等保</em>测评的核心宗旨在于验证与确认，不仅是合规要求。</span></p></li>
<li class="res-list spread" data-result-type="ad" data-result-pos="3"><h3 class="res-title " > <a href="https://www.so.com/link?m=zzz" data-mdurl="https://ad.example.com/promo" target="_blank">广告位：某厂商促销</a></h3> <p class="res-desc"><span data-result-summary>广告内容不应进入检索结果。</span></p></li>
<li class="res-list" data-result-type="normal" data-result-pos="4"><h3 class="res-title " > <a href="https://www.so.com/link?m=www" target="_blank">无 data-mdurl 的畸形条目</a></h3></li>
<li class="res-list" data-result-type="richsum" data-result-pos="5"><h3   class="res-title"><a href="https://www.so.com/link?m=r1" data-mdurl="https://news.sohu.com/a/942917935_122404798" rel="noopener" target="_blank">企业如何同步完成密评和<em>等保测评</em>_整改</a></h3><!-- 富摘要 --><div class='res-rich so-rich-news clearfix'><p></p><span class="g-c-gray">2025年10月11日&nbsp;-&nbsp;</span><span class="res-list-summary" data-result-summary>金融客户普遍担心,密评重在密码应用与<em>密钥管理</em>,等保更强调整体安全体系。</span></div></li>
<li class="res-list" data-result-type="normal" data-result-pos="6"><h3 class="res-title " > <a href="https://www.so.com/link?m=v1" data-mdurl="https://docs.example.org/standard?id=1&amp;lang=zh" target="_blank">标准文档 &amp; 释义</a></h3> <p class="res-desc">2026年10月1日 - 这里是兜底摘要正文。</p></li>
</ul></body></html>`

func TestParseSoHTML(t *testing.T) {
	results := ParseSoHTML(soSamplePage, 5)
	if len(results) != 4 {
		t.Fatalf("want 4 results (skip ad + malformed), got %d: %+v", len(results), results)
	}
	// 第 1 条：标题剥 em 高亮、URL 用 data-mdurl、摘要取 summary、日期转 ISO。
	if results[0].URL != "https://www.west.cn/web/dengbao/" {
		t.Errorf("result0 url = %q", results[0].URL)
	}
	if results[0].Title != "等保测评_等级保护测评 - 西部数码" {
		t.Errorf("result0 title = %q", results[0].Title)
	}
	if !strings.Contains(results[0].Snippet, "西部数码等保测评服务") {
		t.Errorf("result0 snippet = %q", results[0].Snippet)
	}
	if results[0].PageAge != "2026-09-30" {
		t.Errorf("result0 pageAge = %q", results[0].PageAge)
	}
	// richsum 卡片（h3 多空格 + res-list-summary 形态）必须被解析。
	var rich *Result
	for i := range results {
		if strings.Contains(results[i].URL, "sohu.com") {
			rich = &results[i]
		}
	}
	if rich == nil {
		t.Fatalf("richsum 条目漏解析: %+v", results)
	}
	if !strings.Contains(rich.Title, "企业如何同步完成密评") {
		t.Errorf("richsum title = %q", rich.Title)
	}
	if !strings.Contains(rich.Snippet, "密钥管理") {
		t.Errorf("richsum snippet = %q", rich.Snippet)
	}
	if rich.PageAge != "2025-10-11" {
		t.Errorf("richsum pageAge = %q", rich.PageAge)
	}
	// 末条（含兜底摘要的分支）：URL 实体反转义、日期前缀被剥掉。
	last := results[len(results)-1]
	if last.URL != "https://docs.example.org/standard?id=1&lang=zh" {
		t.Errorf("last url = %q", last.URL)
	}
	if strings.Contains(last.Snippet, "2026年10月1日") {
		t.Errorf("last snippet still has date prefix: %q", last.Snippet)
	}
	if !strings.Contains(last.Snippet, "这里是兜底摘要正文") {
		t.Errorf("last snippet = %q", last.Snippet)
	}
}

func TestParseSoHTMLDedupeAndCap(t *testing.T) {
	page := strings.ReplaceAll(soSamplePage, "https://www.toutiao.com/article/7638886106234159668/", "https://www.west.cn/web/dengbao/")
	results := ParseSoHTML(page, 5)
	if len(results) != 3 { // 两条同 URL 去重（west + sohu + docs）
		t.Fatalf("want 3 results after dedupe, got %d", len(results))
	}
	if got := ParseSoHTML(soSamplePage, 1); len(got) != 1 {
		t.Fatalf("want 1 result with max=1, got %d", len(got))
	}
	if got := ParseSoHTML("", 5); len(got) != 0 {
		t.Fatalf("empty page should parse to 0 results, got %d", len(got))
	}
}

func TestSearchSoEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.RawQuery, "q=") {
			t.Errorf("missing q param: %s", r.URL.RawQuery)
		}
		if r.Header.Get("User-Agent") == "" {
			t.Error("missing User-Agent")
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(soSamplePage))
	}))
	defer srv.Close()

	s := &So{HTTP: srv.Client(), BaseURL: srv.URL + "/s", UserAgent: "test-ua"}
	results, err := s.Search(context.Background(), "等保测评", 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %d", len(results))
	}
}

func TestSearchSoBlockedPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><body>请输入验证码后继续访问</body></html>`))
	}))
	defer srv.Close()

	s := &So{HTTP: srv.Client(), BaseURL: srv.URL + "/s"}
	if _, err := s.Search(context.Background(), "q", 5); err == nil {
		t.Fatal("verification page should surface an error, got nil")
	}
}

func TestSearchSoEmptyQuery(t *testing.T) {
	s := &So{}
	if _, err := s.Search(context.Background(), "  ", 5); err == nil {
		t.Fatal("empty query should error")
	}
}
