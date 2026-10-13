// dsml.go 剥离模型正文里的「伪工具调用」标记。
//
// 背景（2026-10 E2E 实测）：WorkBuddy 上游的 deepseek 系模型在「想调用一个
// 未注册/不可用的工具」时，会把工具调用语法以**纯文本**形式吐进 content，
// 实测到两种形态：
//
//	A. 全角竖线 DSML 标记：尖括号包裹、以两枚全角竖线（U+FF5C）分隔标签名
//	   （开块 + 参数 + 闭块）。竖线与标签名之间常带空格；闭合标记形如
//	   "/" + 双竖线 + 空格 + invoke + ">"。
//	B. 伪 XML 工具调用：<tool_call>{"name":"web_search","arguments":{…}}</tool_call>
//	   （有时是 <tool_calls> / <function_call>）。实测同一提示词约 1/3 的次数
//	   吐出该形态——正文里就是一坨 JSON 与标签，客户端直接显示为乱码。
//
// 两条入口：
//   - StripDSMLText：聚合 / 非流式路径整段清洗（完整块 + 独立标记 + 未闭合
//     残段一次剥净，两种形态都处理）；
//   - dsmlStreamFilter：流式路径的增量过滤（锚点可能跨多个 delta 帧，用
//     holdback 缓冲处理"疑似起始前缀"）。
//
// 源码约定：形态 A 的标记含全角竖线，本文件一律用 \uFF5C（Go 字符串）与
// \x{FF5C}（正则）转义书写——保持文件纯 ASCII，避免肉眼不可辨字符混入。
package server

import (
	"encoding/json"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// 形态 A：全角竖线 DSML 标记
// ---------------------------------------------------------------------------

// dsmlMark 全角竖线（U+FF5C）。含此字符的文本才进入形态 A 清洗路径（快速判定）。
const dsmlMark = "\uFF5C"

// dsmlTagPattern 形态 A 标记前缀的容忍式片段：尖括号 + 可选斜杠 + 两枚全角
// 竖线 + "DSML" + 两枚全角竖线，各段之间允许任意空白。实测泄漏形态里竖线与
// 标签名之间常带空格（形如 "/" + 双竖线 + 空格 + "invoke>"），不留 \s* 会整类
// 漏匹配。注意：Go 的 RE2 不支持 \u 转义，全角竖线必须写 \x{FF5C}。
const dsmlTagPattern = `<[[:space:]]*/?[[:space:]]*\x{FF5C}{2}[[:space:]]*DSML[[:space:]]*\x{FF5C}{2}[[:space:]]*`

// dsmlOpenPattern 与 dsmlTagPattern 同形，但不允许斜杠——只认「起始（非闭合）
// 标记」。未闭合尾规则必须用它：通用型会把完整块剥离后残留的闭合标签（如
// 仅剩的闭合 calls 标签）当成新的起始标记，从那里截到文末，把块之后的正常
// 正文整段吞掉。
const dsmlOpenPattern = `<[[:space:]]*\x{FF5C}{2}[[:space:]]*DSML[[:space:]]*\x{FF5C}{2}[[:space:]]*`

// dsmlLeadPattern 形态 A 的「标记起点」：尖括号 + 可选空白 + 可选斜杠 + 可选
// 空白 + 两枚全角竖线。开、闭标签都是标记起点——闭合标签漏判过（流式路径只
// 认「尖括号紧跟双竖线」，闭合标签的斜杠挡在前面，导致残留的闭合标签被当正文
// 放行，实测 dsml-st 流式用例正文只剩一个闭合 calls 标签）。
const dsmlLeadPattern = `<[[:space:]]*/?[[:space:]]*\x{FF5C}{2}`

var (
	// reDSMLBlock 完整标记块：起始标签到闭合标签（invoke/calls）之间的全部内容。
	reDSMLBlock = regexp.MustCompile(`(?s)` + dsmlTagPattern + `(?:calls|invoke|parameter)?[^>]*>.*?` + dsmlTagPattern + `(?:calls|invoke)[^>]*>`)
	// reDSMLTag 独立标记（开/闭标签自身，如闭合后残留的空标签）。
	reDSMLTag = regexp.MustCompile(`(?s)` + dsmlTagPattern + `[^>]*>`)
	// reDSMLTail 未闭合残段：**起始**标记直到文末（模型在标记中途被停止/截断；
	// 在完整块剥离之后应用于残余文本）。用 dsmlOpenPattern 而非 dsmlTagPattern，
	// 否则剥离完整块后剩下的闭合标签会把其后的正文一起截掉。
	reDSMLTail = regexp.MustCompile(`(?s)` + dsmlOpenPattern + `.*$`)
	// reBlankRun 清洗后可能出现的三连以上空行，压回双空行。
	reBlankRun = regexp.MustCompile(`\n{3,}`)
	// reDSMLEnd 流式吞弃态用的「块结束特征」：闭合标签（竖线 + invoke/calls + ">"，
	// 各段允许空白）。只写竖线紧邻 ">" 永远匹配不到实测形态，吞弃态会一直持续
	// 到 64KB 上限（把后续正文整段吞掉）。
	reDSMLEnd = regexp.MustCompile(`\x{FF5C}{2}[[:space:]]*(?:invoke|calls)[[:space:]]*>[[:space:]]*\n?`)
	// reDSMLLead 流式路径的形态 A 起点锚（开/闭标签均可）。
	reDSMLLead = regexp.MustCompile(dsmlLeadPattern)
	// reDSMLLeadBlank 只有「尖括号 + 空白 + 可选斜杠 + 空白」、竖线尚未到达的
	// 情况：也属于未完成起点尾，先扣住。
	reDSMLLeadBlank = regexp.MustCompile(`^<[[:space:]]*/?[[:space:]]*$`)
)

// aAnchors 形态 A 的字节级锚（用于流式 holdback：部分 UTF-8 竖线字节天然是
// 这些锚的前缀，按 rune 匹配反而会把半截竖线当正文放行）。
var aAnchors = []string{"<" + dsmlMark + dsmlMark, "</" + dsmlMark + dsmlMark}

// ---------------------------------------------------------------------------
// 形态 B：伪 XML 工具调用（<tool_call> 包裹的工具调用 JSON）
// ---------------------------------------------------------------------------

// bAnchors 形态 B 的起始锚（按 ASCII 小写比较；<tool_call 亦覆盖 <tool_calls）。
var bAnchors = []string{"<tool_call", "<function_call"}

var (
	// reToolCallOpen 形态 B 的完整开标签（允许属性、大小写混合）。
	reToolCallOpen = regexp.MustCompile(`(?is)<[[:space:]]*(?:tool_calls?|function_call)(?:[[:space:]][^>]*)?>`)
	// reToolCallClose 形态 B 的闭标签（tool_call / tool_calls / function_call 通用）。
	reToolCallClose = regexp.MustCompile(`(?is)</[[:space:]]*(?:tool_calls?|function_call)[[:space:]]*>`)
)

// asciiLower 仅折叠 ASCII 大写字母。不能用 strings.ToLower：流式缓冲里可能含
// 半个多字节字符（逐字节喂入），ToLower 会把非法字节替换成 U+FFFD，长度与原
// 索引全部错位。
func asciiLower(s string) string {
	hasUpper := false
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			hasUpper = true
			break
		}
	}
	if !hasUpper {
		return s
	}
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// looksLikeToolCallJSON 判定块内载荷是否为「工具调用形状的 JSON」。
// 严格到「先能解析成 JSON，再看形状」：普通 JSON 或正文里出现的 XML 片段
// （模型解释语法、用户贴代码等）原样保留，只清真正的伪调用。
func looksLikeToolCallJSON(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || (s[0] != '{' && s[0] != '[') {
		return false
	}
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return false
	}
	return toolCallShaped(v)
}

// toolCallShaped 工具调用形状：对象含 name / function / tool_calls / tool_call
// 任一键；数组则任一元素为工具调用形状。
func toolCallShaped(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range []string{"name", "function", "tool_calls", "tool_call"} {
			if _, ok := t[k]; ok {
				return true
			}
		}
	case []any:
		for _, it := range t {
			if toolCallShaped(it) {
				return true
			}
		}
	}
	return false
}

// looksLikeToolCallJSONPrefix 判定「可能是（未写完的）工具调用载荷」：JSON 尚不
// 完整无法解析，退化为看首字符 + 关键键名。仅用于截断/流结束的兜底判断。
func looksLikeToolCallJSONPrefix(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || (s[0] != '{' && s[0] != '[') {
		return false
	}
	l := asciiLower(s)
	return strings.Contains(l, `"name"`) || strings.Contains(l, `"function"`) ||
		strings.Contains(l, `"tool_call`)
}

// stripToolCallBlocks 移除形态 B 的伪调用块：载荷是工具调用形状才丢，否则
// 原样保留（防误伤）。未闭合的开标签只在载荷疑似工具调用时才截到文末。
func stripToolCallBlocks(s string) string {
	var out strings.Builder
	rest := s
	for {
		loc := reToolCallOpen.FindStringIndex(rest)
		if loc == nil {
			out.WriteString(rest)
			return out.String()
		}
		out.WriteString(rest[:loc[0]])
		inner := rest[loc[1]:]
		cl := reToolCallClose.FindStringIndex(inner)
		if cl == nil {
			if looksLikeToolCallJSONPrefix(inner) {
				return out.String() // 截断的伪调用：连同残段一起丢
			}
			out.WriteString(rest[loc[0]:])
			return out.String()
		}
		payload := inner[:cl[0]]
		if !looksLikeToolCallJSON(payload) {
			// 不是工具调用：原样保留（含标签），继续找下一处。
			out.WriteString(rest[loc[0]:loc[1]])
			out.WriteString(payload)
			out.WriteString(inner[cl[0]:cl[1]])
		}
		rest = inner[cl[1]:]
	}
}

// indexToolCallAnchor 返回形态 B 起始锚最早出现的位置（大小写不敏感，返回
// [start,end)；无则 nil）。ASCII 小写不改变字节长度，索引可直接复用。
func indexToolCallAnchor(s string) []int {
	lower := asciiLower(s)
	best := -1
	for _, a := range bAnchors {
		if i := strings.Index(lower, a); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	// 锚长度取「小写后仍是其前缀」的最长锚，覆盖 <tool_call 与 <tool_calls。
	n := 0
	for _, a := range bAnchors {
		if strings.HasPrefix(lower[best:], a) && len(a) > n {
			n = len(a)
		}
	}
	return []int{best, best + n}
}

// isAnchorPrefix 尾部片段是否为某个锚的前缀（大小写不敏感）。
func isAnchorPrefix(tail string) bool {
	l := asciiLower(tail)
	for _, a := range bAnchors {
		if strings.HasPrefix(a, l) {
			return true
		}
	}
	for _, a := range aAnchors {
		if strings.HasPrefix(a, l) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 聚合清洗
// ---------------------------------------------------------------------------

// StripDSMLText 整段清洗：先剥离形态 A 的完整块，再从残余文本里找未闭合的
// 起始标记（找到则截到文末——残段之后的正文不可信），清理独立标签；随后剥离
// 形态 B 的伪调用块；最后规整空行。顺序关键：A 的尾部截断必须在完整块剥离
// 之后，否则一个完整块之后的正常正文会被一并截掉。两种形态的标记都不含的
// 文本零开销原样返回。
func StripDSMLText(s string) string {
	if s == "" {
		return s
	}
	lower := asciiLower(s)
	if !strings.Contains(s, dsmlMark) &&
		!strings.Contains(lower, bAnchors[0]) && !strings.Contains(lower, bAnchors[1]) {
		return s
	}
	out := s
	if strings.Contains(out, dsmlMark) {
		out = reDSMLBlock.ReplaceAllString(out, "")
		out = reDSMLTail.ReplaceAllString(out, "")
		out = reDSMLTag.ReplaceAllString(out, "")
	}
	if l := asciiLower(out); strings.Contains(l, bAnchors[0]) || strings.Contains(l, bAnchors[1]) {
		out = stripToolCallBlocks(out)
	}
	out = reBlankRun.ReplaceAllString(out, "\n\n")
	return strings.TrimSpace(out)
}

// ---------------------------------------------------------------------------
// 流式增量过滤
// ---------------------------------------------------------------------------

const (
	filterNormal = iota // 常态：透传与标记无关的前缀
	filterDropA         // 吞弃态（形态 A）：丢弃到块结束特征
	filterHoldB         // 挂起态（形态 B）：缓冲到闭标签后按载荷判定
)

// dsmlStreamFilter 流式路径的增量过滤器。
//
// 状态机：
//   - 常态：把文本并入缓冲，输出「确定与标记无关」的前缀，尾部保留可能是
//     不完整锚点的部分（形态 A 的 <[/]｜｜ 前缀尾、形态 B 的 <tool_call 前缀尾）。
//   - 吞弃态（形态 A）：累积内容直到探到结束特征，然后整体丢弃、回到常态；
//     若缓冲超过 dsmlDropCap 仍无结束特征（异常/超长），也丢弃并回常态。
//   - 挂起态（形态 B）：缓冲到闭标签；载荷是工具调用 JSON 则整块丢弃，否则
//     原样放行；超过 dsmlDropCap 同样放行兜底。
//   - Flush（流结束）：常态输出残余缓冲（含扣住的不完整前缀——它不是标记，
//     照常放行）；吞弃态丢弃残余；挂起态按载荷形状决定丢弃或放行。
type dsmlStreamFilter struct {
	buf  strings.Builder
	mode int
}

const dsmlDropCap = 64 << 10 // 吞弃 / 挂起态缓冲上限（异常形态的兜底）

func newDSMLStreamFilter() *dsmlStreamFilter {
	return &dsmlStreamFilter{mode: filterNormal}
}

// set 重置缓冲并切换状态。
func (f *dsmlStreamFilter) set(rest string, mode int) {
	f.buf.Reset()
	f.buf.WriteString(rest)
	f.mode = mode
}

// holdbackLen 常态下允许保留的尾长：尾部若整体是某个锚的前缀（可能跨帧长成
// 锚，含半个多字节竖线的字节片段），先扣住不输出。取满足条件的最长尾，避免漏判。
func holdbackLen(cur string) int {
	max := len(bAnchors[1]) // <function_call 最长
	if len(cur) < max {
		max = len(cur)
	}
	for k := max; k > 0; k-- {
		if isAnchorPrefix(cur[len(cur)-k:]) {
			return k
		}
	}
	// 形态 A 的「只有尖括号与空白/斜杠、竖线尚未到达」尾。
	maxA := 8
	if len(cur) < maxA {
		maxA = len(cur)
	}
	for k := maxA; k > 0; k-- {
		if reDSMLLeadBlank.MatchString(cur[len(cur)-k:]) {
			return k
		}
	}
	return 0
}

// Write 吸入一段增量文本，返回可安全下发的部分（可能为空串）。
func (f *dsmlStreamFilter) Write(text string) string {
	if f.mode == filterNormal && f.buf.Len() == 0 && !strings.Contains(text, "<") {
		// 快路径：无 '<'（两种锚都以 '<' 起头）且无挂起缓冲 → 直接放行。
		return text
	}
	f.buf.WriteString(text)
	var out strings.Builder
	for {
		cur := f.buf.String()
		switch f.mode {
		case filterDropA:
			if m := reDSMLEnd.FindStringIndex(cur); m != nil {
				// 丢弃到结束特征（含），回到常态继续处理剩余。
				f.set(cur[m[1]:], filterNormal)
				continue
			}
			if len(cur) > dsmlDropCap {
				f.set("", filterNormal)
			}
		case filterHoldB:
			if cl := reToolCallClose.FindStringIndex(cur); cl != nil {
				block := cur[:cl[1]]
				if looksLikeToolCallJSON(bPayload(cur, cl[0])) {
					f.set(cur[cl[1]:], filterNormal) // 伪调用：整块丢弃
					continue
				}
				out.WriteString(block) // 非工具调用载荷：原样放行
				f.set(cur[cl[1]:], filterNormal)
				continue
			}
			if len(cur) > dsmlDropCap {
				out.WriteString(cur)
				f.set("", filterNormal)
			}
		default: // filterNormal
			aLoc := reDSMLLead.FindStringIndex(cur)
			bLoc := indexToolCallAnchor(cur)
			start, mode := -1, filterNormal
			switch {
			case aLoc != nil && (bLoc == nil || aLoc[0] <= bLoc[0]):
				start, mode = aLoc[0], filterDropA
			case bLoc != nil:
				start, mode = bLoc[0], filterHoldB
			}
			if start < 0 {
				keep := holdbackLen(cur)
				out.WriteString(cur[:len(cur)-keep])
				f.set(cur[len(cur)-keep:], filterNormal)
				return out.String()
			}
			out.WriteString(cur[:start])
			f.set(cur[start:], mode)
			continue
		}
		return out.String()
	}
}

// bPayload 形态 B 块内的载荷（闭标签之前、开标签之后）。
func bPayload(block string, closeAt int) string {
	if open := reToolCallOpen.FindStringIndex(block); open != nil && open[1] <= closeAt {
		return block[open[1]:closeAt]
	}
	// 开标签不完整（跨帧边界等）：退化为第一个 '>' 之后。
	if gt := strings.IndexByte(block, '>'); gt >= 0 && gt < closeAt {
		return block[gt+1 : closeAt]
	}
	return block[:closeAt]
}

// bPayloadTail 未闭合（截断）形态 B 片段里的载荷：开标签之后的部分；连开标签
// 的 '>' 都还没到则返回空串（该片段无从判定为工具调用）。
func bPayloadTail(block string) string {
	if gt := strings.IndexByte(block, '>'); gt >= 0 {
		return block[gt+1:]
	}
	return ""
}

// Flush 流结束收尾：常态放行残余缓冲；吞弃态丢弃；挂起态按载荷形状决定。
func (f *dsmlStreamFilter) Flush() string {
	cur := f.buf.String()
	f.buf.Reset()
	mode := f.mode
	f.mode = filterNormal
	switch mode {
	case filterDropA:
		return ""
	case filterHoldB:
		if looksLikeToolCallJSONPrefix(bPayloadTail(cur)) {
			return ""
		}
		// 非工具调用载荷（如正文里对 <tool_call> 的语法引用）：原样放行，但
		// 残余里可能还有真正的锚要处理，按常态帧重新走一遍过滤。
		var out strings.Builder
		rest := cur
		for {
			aLoc := reDSMLLead.FindStringIndex(rest)
			bLoc := indexToolCallAnchor(rest)
			start := -1
			var fam int
			switch {
			case aLoc != nil && (bLoc == nil || aLoc[0] <= bLoc[0]):
				start, fam = aLoc[0], 1
			case bLoc != nil:
				start, fam = bLoc[0], 2
			}
			if start < 0 {
				// 末尾不完整锚点已不可能再长成标记（流已结束）：照常放行。
				out.WriteString(rest)
				return out.String()
			}
			out.WriteString(rest[:start])
			if fam == 2 {
				if cl := reToolCallClose.FindStringIndex(rest[start:]); cl != nil {
					block := rest[start : start+cl[1]]
					if !looksLikeToolCallJSON(bPayload(block, cl[0])) {
						out.WriteString(block)
					}
					rest = rest[start+cl[1]:]
					continue
				}
				if looksLikeToolCallJSONPrefix(bPayloadTail(rest[start:])) {
					return out.String()
				}
				out.WriteString(rest[start:])
				return out.String()
			}
			if m := reDSMLEnd.FindStringIndex(rest[start:]); m != nil {
				rest = rest[start+m[1]:]
				continue
			}
			return out.String()
		}
	}
	return cur
}
