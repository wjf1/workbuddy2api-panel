// index.go 面板静态资源与安全响应头。
//
// 资源经 go:embed 打进二进制（随服务部署，无外部构建步骤）：
//   - index.html  页面骨架
//   - logic.js    纯函数层（转义 / 时间解析 / 格式化），可被 node 直接单测
//   - app.js      全部前端逻辑（独立文件而非内联，为了启用无需 unsafe-inline 的严格 CSP）
//
// 安全头只作用于面板的两个静态响应（index/appScript）：CSP 限制脚本只能来自本服务，
// 禁止被 iframe 嵌套（防点击劫持），禁 MIME 嗅探，并声明不泄露 Referer 出去。
// /panel/api/* 是 JSON 数据接口，不带这组头——它们靠 Bearer 鉴权 + JSON 内容类型，
// CSP 对 JSON 无意义。（早期注释写成"页面与全部 api 统一生效"，与实现不符，已纠正。）
package panel

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

//go:embed logic.js
var logicJS []byte

//go:embed app.js
var appJS []byte

// csp 内容安全策略（严格版，无需 unsafe-inline）：
//   - default-src 'none'        默认全禁，逐个开口
//   - script-src 'self'         只跑同源脚本（app.js）；页面无内联事件处理器/内联脚本
//   - style-src 'self' 'unsafe-inline'
//     style 的内联是设计取舍：页面有少量 style="..." 属性（进度条宽度、表格列宽），
//     允许内联样式不会导致脚本执行；仍禁止外部样式域与 @import 外链。
//   - connect-src 'self'        前端 fetch 只能打本服务
//   - img-src 'self' data:      图标/内联图
//   - form-action 'none'        页面无表单提交目标（配置页是 JS 提交）
//   - frame-ancestors 'none'    禁止被任何站点 iframe 嵌套（点击劫持）
//   - base-uri 'none'          禁止注入 <base> 改写相对路径
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// setSecurityHeaders 写入面板统一安全响应头（页面与 API 都要，API 也含 JSON 数据）。
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff") // 禁 MIME 嗅探
	w.Header().Set("X-Frame-Options", "DENY")           // 老浏览器兜底（CSP frame-ancestors 的等价项）
	w.Header().Set("Referrer-Policy", "no-referrer")    // 不外泄面板地址给外部站点
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}

// noCache 单文件嵌入资源（index.html / logic.js / app.js）的缓存策略：禁止启发式
// 强缓存，每次 no-cache 协商校验。这三个文件是【同版本强耦合】的独立文件（结构 /
// 纯函数 / 交互分属三处，app.js 顶部还解构 logic.js 的导出），URL 不带版本号，
// 一旦浏览器用旧缓存拼新页面就会出现半新半旧的错乱界面——更糟的是 logic.js 旧版
// 少了新函数时，app.js 解构到 undefined 直接白屏。宁多一次 304，不容忍版本错配。
func noCache(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-cache")
}

// index 输出面板页面（静态无秘密；数据接口 /panel/api/* 才走鉴权）。
func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	noCache(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(indexHTML)
}

// logicScript 输出纯函数层。必须由 index.html 在 app.js **之前**加载：
// app.js 顶部直接解构 window.WBLogic，顺序错了整份脚本首行即 TypeError（面板白屏）。
func (p *Panel) logicScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	noCache(w)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(logicJS)
}

// appScript 输出前端逻辑（同源脚本，供 CSP script-src 'self' 加载）。
func (p *Panel) appScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	noCache(w)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(appJS)
}
