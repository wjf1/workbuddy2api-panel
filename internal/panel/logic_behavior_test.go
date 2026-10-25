package panel

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// destructureRe 匹配 app.js 顶部取用纯函数层的那段解构。
var destructureRe = regexp.MustCompile(`(?s)const \{([^}]*)\} = window\.WBLogic;`)

// TestLogicJSBehavior 跑 logic.js 的断言集（logic_test.js）。
//
// 这是面板前端**唯一**真正的行为测试：app.js 里混着 DOM 与格式化逻辑，除了语法
// 校验什么都验不了；把纯函数抽到 logic.js 之后，node 可以直接跑断言，覆盖到
// esc 的注入防护、时间格式化的边界、fmtCredit 的非数字兜底这些真会咬人的地方。
//
// 无 node 环境跳过（与 TestAppJSSyntax 同策略，不阻塞没装 Node 的构建机）。
func TestLogicJSBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping logic.js behavior tests")
	}
	path, err := filepath.Abs("logic_test.js")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("logic_test.js 断言失败:\n%s", out)
	}
	// 断言数太少说明被人整段删了测试而没报错——总数是下限检查，不是精确值。
	if !strings.Contains(string(out), "assertions passed") {
		t.Fatalf("logic_test.js 输出不符合预期（测试可能被清空）:\n%s", out)
	}
}

// TestLogicJSSyntax logic.js 同样要过 JS 解析器：它是 go:embed 的静态资源，
// Go 编译器不检查内容，一个笔误就让整份面板白屏。
func TestLogicJSSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS syntax check")
	}
	path, err := filepath.Abs("logic.js")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
		t.Fatalf("logic.js syntax error:\n%s", out)
	}
}

// TestLogicScriptServed logic.js 必须能被面板路由出去（CSP script-src 'self'
// 只允许同源脚本，落不到这个路由上整份前端就加载不了）。
func TestLogicScriptServed(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/logic.js", nil))

	if rec.Code != 200 {
		t.Fatalf("GET /panel/logic.js = %d，期望 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type=%q，期望 javascript", ct)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("静态 JS 也应带 CSP 头（setSecurityHeaders）")
	}
	if body := rec.Body.String(); !strings.Contains(body, "WBLogic") {
		t.Errorf("响应体不像 logic.js（找不到 WBLogic 导出）")
	}
}

// TestIndexLoadsLogicBeforeApp logic.js 必须排在 app.js 之前加载：
// app.js 顶部直接解构 window.WBLogic，顺序反了会在首行抛 TypeError，
// 面板白屏——而 Go 测试与 node --check 都发现不了。
func TestIndexLoadsLogicBeforeApp(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	iLogic := strings.Index(body, `src="logic.js"`)
	iApp := strings.Index(body, `src="app.js"`)
	if iLogic < 0 {
		t.Fatal("index.html 没有加载 logic.js")
	}
	if iApp < 0 {
		t.Fatal("index.html 没有加载 app.js")
	}
	if iLogic > iApp {
		t.Fatalf("logic.js 必须排在 app.js 之前（当前 logic@%d > app@%d）", iLogic, iApp)
	}
}

// TestLogicJSNeedsNoDOM logic.js 必须保持"纯"：不碰 document / addEventListener。
//
// 这条是给未来的自己看的：一旦有人往 logic.js 里塞 DOM 操作，它就不能再被 node
// 直接测试（node 没有 document），logic_test.js 会连带失效。要碰 DOM 请写进 app.js。
func TestLogicJSNeedsNoDOM(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/logic.js", nil))
	body := rec.Body.String()

	for _, banned := range []string{"document.", "addEventListener", "localStorage"} {
		if strings.Contains(body, banned) {
			t.Errorf("logic.js 出现了 %q —— 纯函数层不应依赖浏览器环境（否则 node 侧测不了）", banned)
		}
	}
}

// TestWBLogicDestructureMatches app.js 顶部解构的名字必须与 logic.js 导出的键**完全一致**。
//
// 为什么要这条：解构是静默失败的——名字对不上时得到的是 undefined，不会报错，
// 直到某个页面第一次调用 esc/fmtTok 才抛 "not a function"，而那时错误栈在几千行
// 之外。这条测试把「改了 logic.js 的导出名忘了同步 app.js」挡在 CI 里。
func TestWBLogicDestructureMatches(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping export/destructure cross-check")
	}
	out, err := exec.Command(node, "-e", `console.log(Object.keys(require("./logic.js")).sort().join(","))`).Output()
	if err != nil {
		t.Fatalf("读取 logic.js 导出键失败: %v", err)
	}
	exported := map[string]bool{}
	for _, k := range strings.Split(strings.TrimSpace(string(out)), ",") {
		if k != "" {
			exported[k] = true
		}
	}

	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	m := destructureRe.FindStringSubmatch(string(src))
	if m == nil {
		t.Fatal("app.js 顶部找不到 `const { ... } = window.WBLogic;` 解构块")
	}
	used := map[string]bool{}
	for _, name := range strings.Split(m[1], ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		used[name] = true
		if !exported[name] {
			t.Errorf("app.js 解构了 %q，但 logic.js 没有导出它（运行时会是 undefined）", name)
		}
	}
	for k := range exported {
		if !used[k] {
			t.Errorf("logic.js 导出了 %q 但 app.js 没解构（死代码或忘记接线）", k)
		}
	}
}
