// dump.go 上游原始报文旁路落盘（诊断用，默认关闭）。
//
// 背景（issue #149）：客户端看到「带 tools 的 agentic 请求返回 stop + 零 tool_calls」，
// 但 request-logs 只有元数据（设计如此：不写正文），dsml 修复命中日志又在 stderr，
// 于是没有人能回答「上游到底发过 tool_calls 没有」。本文件给运维一个显式开关：
// Client.DumpDir（WB2A_DUMP_UPSTREAM）非空时，每次 chat 请求落一个自包含文件——
// 出站请求体（我们真正发给上游的东西）+ 上游原始 SSE 字节，不经任何改写。
//
// 只有显式配置目录才落盘：正文含用户提示词，不能默认开。单个文件封顶 8 MB
// （只停写不掐流，响应照常透传给客户端），写盘失败只记一行日志、绝不影响请求。
//
// 诊断用，故**不做热加载**：流式读取中切换目录会引入与 reader 生命周期的竞争，
// 收益远小于风险——改配置后重启生效。
package upstream

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// dumpLimit 单文件封顶：请求体 + 响应体合计预算。
// 定 8 MB 的理由：一次长 SSE 流（思考过程 + 长回答）通常在 1-3 MB，8 MB 足以
// 覆盖「有没有发 tool_calls」这类判断，又不至于把磁盘写满；超限后只停写不掐流，
// 客户端侧体验完全不受影响。
const dumpLimit = 8 << 20

// dumpRequestMax 请求体片段上限默认值：足够看清 tools / messages 结构，又不至于
// 吃掉整个配额。Client.DumpRequestMax 可覆盖（长会话把 tools 排在几百条 messages
// 之后，默认值常在中途截断）。
const dumpRequestMax = 1 << 18

// dumpUpstream 在 DumpDir 为空时原样返回 rc（零开销、零行为变化）。
//
// 有意让所有失败分支都回落 rc 本身：诊断开关不能变成新的故障点，目录建不出来、
// 文件打不开都只是「没有 dump」，请求必须照常进行。
func (c *Client) dumpUpstream(rc io.ReadCloser, prepared []byte, a *auth.Auth) io.ReadCloser {
	if c.DumpDir == "" {
		return rc
	}
	if err := os.MkdirAll(c.DumpDir, 0o700); err != nil {
		log.Printf("WARN: [upstream] dump dir %s: %v", c.DumpDir, err)
		return rc
	}
	f, err := os.OpenFile(filepath.Join(c.DumpDir, dumpName(a)), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		log.Printf("WARN: [upstream] dump open: %v", err)
		return rc
	}
	// DumpRequestMax<=0 = 用默认上限（0 是「未配置」而非「不写请求体」——
	// 请求体是这套 dump 的主要目的，不该能配成 0 把主证据关掉）。
	reqMax := c.DumpRequestMax
	if reqMax <= 0 {
		reqMax = dumpRequestMax
	}
	head := prepared
	if len(head) > reqMax {
		head = head[:reqMax]
	}
	// 分段标记写在同一个文件里：排障时不必再去别处对齐时间戳。
	f.Write([]byte("=== request ===\n"))
	f.Write(head)
	f.Write([]byte("\n=== response ===\n"))
	return &dumpReader{rc: rc, f: f, left: dumpLimit - int64(len(head))}
}

// dumpName 用「时间-域-账号前 8 位」拼文件名：可排序、不落完整 UID。
// 用 O_EXCL 打开，同毫秒撞名时后续那次直接失败（记 WARN 后不 dump），
// 而不是覆盖前一份证据。
// dumpRetentionDays dump 文件保留窗口：dump 内容含完整用户提示词与上游响应
// （敏感数据），无限期留存既是磁盘增长点也是留存风险。启动时与每日一次清理。
const dumpRetentionDays = 7

// CleanDumpDir 清理超过保留窗口的 dump 文件（启动时与后台每日各一次）。
// 文件名带毫秒时间戳前缀（dumpName），按前缀字典序即时间序——解析失败/无时间戳
// 前缀的文件不动（可能是用户手放的对照样本）。
func (c *Client) CleanDumpDir() {
	if c.DumpDir == "" {
		return
	}
	entries, err := os.ReadDir(c.DumpDir)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -dumpRetentionDays).Format("20060102-150405")
	removed := 0
	for _, e := range entries {
		name := e.Name()
		if len(name) < 15 || name[:15] >= cutoff {
			continue // 无时间戳前缀（<15 字符）或仍在窗口内
		}
		if os.Remove(filepath.Join(c.DumpDir, name)) == nil {
			removed++
		}
	}
	if removed > 0 {
		log.Printf("[upstream] dump 保留清理：删除 %d 个超过 %d 天的文件（%s）", removed, dumpRetentionDays, c.DumpDir)
	}
}

// StartDumpCleaner 后台每日清理（随 ctx 退出）。
func (c *Client) StartDumpCleaner(ctx context.Context) {
	if c.DumpDir == "" {
		return
	}
	c.CleanDumpDir() // 启动即清一次
	go func() {
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.CleanDumpDir()
			}
		}
	}()
}

func dumpName(a *auth.Auth) string {
	tag := "unknown"
	if a != nil {
		tag = a.Realm()
		if uid := a.UID; uid != "" {
			if len(uid) > 8 {
				uid = uid[:8]
			}
			tag += "-" + uid
		}
	}
	return time.Now().Format("20060102-150405.000") + "-" + tag + ".log"
}

// dumpReader 边透传边写盘；配额用尽后只透传。Close 关闭文件。
//
// 关键点：Read 永远返回底层读到的全部 n 字节，写盘只是旁路拷贝——配额耗尽
// 只影响文件内容，绝不影响 SSE 流的推进。
type dumpReader struct {
	rc   io.ReadCloser
	f    *os.File
	left int64
	once sync.Once
}

// Read 透传底层字节，并把仍在配额内的前缀旁路写入文件。
// 写盘错误刻意忽略：这是诊断旁路，报错只会污染日志，且每块都失败会刷屏。
func (d *dumpReader) Read(p []byte) (int, error) {
	n, err := d.rc.Read(p)
	if n > 0 && d.left > 0 {
		w := p[:n]
		if int64(len(w)) > d.left {
			w = w[:d.left]
		}
		d.f.Write(w)
		d.left -= int64(len(w))
	}
	return n, err
}

// Close 先关底流（释放上游连接，优先级更高），文件关闭只做一次（防重复 Close）。
func (d *dumpReader) Close() error {
	err := d.rc.Close()
	d.once.Do(func() { d.f.Close() })
	return err
}
