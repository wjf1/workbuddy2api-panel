'use strict';
/* logic_test.js 面板纯函数层的断言集（零依赖，直接用 node 跑）。
 *
 *   node internal/panel/logic_test.js
 *
 * 为什么用 node 原生 assert 而不是引入测试框架：仓库没有 npm 依赖，也不想为几个
 * 格式化函数引入 node_modules 与锁文件。node 内置 assert + 退出码就够 CI 用了
 * （internal/panel/logic_js_test.go 会执行本文件并按退出码判定）。
 *
 * 断言的取舍：只钉**对外契约**（数字怎么显示、边界怎么归类、非法输入怎么兜底），
 * 不钉实现细节；如果某个边界看起来"奇怪"但确实是线上现状，注释里写明为什么保留，
 * 免得下一个人当成 bug 顺手改掉。
 */
const assert = require('assert');
const L = require('./logic.js');

let n = 0;
function eq(actual, expected, what) {
  assert.strictEqual(
    actual, expected,
    what + '：期望 ' + JSON.stringify(expected) + '，实际 ' + JSON.stringify(actual),
  );
  n++;
}

/* ── esc：文本与属性双安全 ─────────────────────────────────────────── */
eq(L.esc(null), '', 'esc(null) 应为空串（拼 HTML 时不出现 "null"）');
eq(L.esc(undefined), '', 'esc(undefined) 应为空串');
eq(L.esc(0), '0', 'esc(0) 应保留 0（不能被 falsy 判定吃掉）');
eq(L.esc('<img onerror=alert(1)>'), '&lt;img onerror=alert(1)&gt;', 'esc 必须转义尖括号');
eq(L.esc('" onmouseover="alert(1)'), '&quot; onmouseover=&quot;alert(1)', 'esc 必须转义双引号（属性闭合注入）');
eq(L.esc("' onfocus='alert(1)"), '&#39; onfocus=&#39;alert(1)', 'esc 必须转义单引号');
eq(L.esc('&lt;'), '&amp;lt;', 'esc 先转 & 再转别的：已转义的实体要再转一层，防二次解码');

/* ── dur：秒 → 时/分/秒 ────────────────────────────────────────────── */
eq(L.dur(0), '0秒', 'dur(0)');
eq(L.dur(59), '59秒', 'dur(59) 不足一分钟只报秒');
eq(L.dur(60), '1分00秒', 'dur(60) 秒位补零');
eq(L.dur(3599), '59分59秒', 'dur(3599) 差一秒到一小时');
eq(L.dur(3600), '1时00分', 'dur(3600) 进入小时档后秒被舍去');
eq(L.dur(3661), '1时01分', 'dur(3661) 分位补零');
eq(L.dur(-5), '0秒', 'dur(负数) 夹到 0（倒计时不会出现负时长）');
eq(L.dur(0.6), '1秒', 'dur 四舍五入到整秒');

/* ── fmtTok：token 数简写 ─────────────────────────────────────────── */
eq(L.fmtTok(0), '0', 'fmtTok(0)');
eq(L.fmtTok(999), '999', 'fmtTok(999) 不进位');
eq(L.fmtTok(1000), '1.0k', 'fmtTok(1000) 千位保留一位小数，单位是小写 k（沿用旧界面口径）');
eq(L.fmtTok(1500), '1.5k', 'fmtTok(1500)');
eq(L.fmtTok(1e6), '1.00M', 'fmtTok(1e6) 百万位两位小数、单位大写 M');
eq(L.fmtTok(1e9), '1.00B', 'fmtTok(1e9) 十亿位单位大写 B');
eq(L.fmtTok(null), '0', 'fmtTok(null) 兜底 0');

/* ── fmtMs / fmtRate ─────────────────────────────────────────────── */
eq(L.fmtMs(0), '—', 'fmtMs(0) 显示破折号：0ms 表示"没有数据"，不是"零耗时"');
eq(L.fmtMs(250), '250ms', 'fmtMs(250)');
eq(L.fmtMs(1500), '1.50s', 'fmtMs(1500) 超过一秒转秒');
eq(L.fmtRate(0), '—', 'fmtRate(0) 无速率');
eq(L.fmtRate(12.34), '12.3 tok/s', 'fmtRate 保留一位小数');

/* ── trimFixed / fmtCredit / fmtCreditRatio ──────────────────────── */
eq(L.trimFixed('1.50'), '1.5', 'trimFixed 去尾零');
eq(L.trimFixed('2.00'), '2', 'trimFixed 全零则连小数点一起去掉');
eq(L.trimFixed('3'), '3', 'trimFixed 无小数点时原样返回');
eq(L.trimFixed('0.0000'), '0', 'trimFixed 只留一个 0，不能变成空串');
eq(L.fmtCredit(0), '0', 'fmtCredit(0)');
eq(L.fmtCredit(1.005), '1', 'fmtCredit 走 toFixed(2) 再裁尾零 → "1"');
eq(L.fmtCredit('abc'), '—', 'fmtCredit(非数字) 显示破折号');
eq(L.fmtCredit(Infinity), '—', 'fmtCredit(Infinity) 显示破折号（JSON 里出现 Infinity 宁可空着）');
eq(L.fmtCreditRatio(2.5, 3, 1000), '2.5 / 1M', 'fmtCreditRatio 每百万 token 单价');
eq(L.fmtCreditRatio(2.5, 0, 1000), '—', 'fmtCreditRatio 无样本时不给数字（否则像"免费"）');
eq(L.fmtCreditRatio(2.5, 3, 0), '—', 'fmtCreditRatio 无 token 数时不给数字');

/* ── fmtModelRate / fmtK / fmtBytes ──────────────────────────────── */
eq(L.fmtModelRate(''), '—', 'fmtModelRate 空值');
eq(L.fmtModelRate(' 0.5 '), 'x0.5', 'fmtModelRate 加 x 前缀并去空格');
eq(L.fmtK(999), '999', 'fmtK(999)');
eq(L.fmtK(1000), '1K', 'fmtK(1000)');
eq(L.fmtK(1500), '2K', 'fmtK 四舍五入到整数 K（1.5→2）');
eq(L.fmtBytes(512), '512 B', 'fmtBytes 小于 1KB 用字节');
eq(L.fmtBytes(2048), '2.0 KB', 'fmtBytes 千字节一位小数');
eq(L.fmtBytes(1.5 * 1024 * 1024), '1.5 MB', 'fmtBytes 兆字节一位小数');

/* ── parseAPITime：Go 零值时间与非法输入统一成 0 ─────────────────── */
eq(L.parseAPITime(''), 0, 'parseAPITime("") = 0');
eq(L.parseAPITime(null), 0, 'parseAPITime(null) = 0');
eq(L.parseAPITime('0001-01-01T00:00:00Z'), 0, 'Go 零值时间必须视为"没有值"');
eq(L.parseAPITime('not-a-date'), 0, '非法串不能返回 NaN（会让算术全线变 NaN）');
eq(L.parseAPITime('2026-10-25T10:00:00Z'), Date.parse('2026-10-25T10:00:00Z'), '正常时间按 Date.parse 解析');

/* ── fmtLocalDateTime：本地时区 YYYY-MM-DD HH:mm ─────────────────── */
const localMs = new Date(2026, 9, 25, 8, 5).getTime(); // 用本地分量构造，断言与运行机时区无关
eq(L.fmtLocalDateTime(localMs), '2026-10-25 08:05', 'fmtLocalDateTime 补零到分钟，不出现秒');

/* ── ago：相对时间（"现在"由入参固定，测试才能确定性）────────────── */
const NOW = Date.parse('2026-10-25T12:00:00Z');
const sec = s => new Date(NOW - s * 1000).toISOString();
eq(L.ago('', NOW), '—', 'ago 空值');
eq(L.ago('0001-01-01T00:00:00Z', NOW), '—', 'ago Go 零值时间');
eq(L.ago('not-a-date', NOW), '—', 'ago 非法时间不能吐 "NaN 秒前"');
eq(L.ago(sec(-30), NOW), '刚刚', 'ago 未来时间（时钟漂移）显示"刚刚"而不是负数');
eq(L.ago(sec(30), NOW), '30 秒前', 'ago 秒级');
eq(L.ago(sec(90), NOW), '1 分钟前', 'ago 分钟级向下取整');
eq(L.ago(sec(7200), NOW), '2 小时前', 'ago 小时级');
eq(L.ago(sec(3 * 86400), NOW), '3 天前', 'ago 天级');

console.log('logic_test.js: ' + n + ' assertions passed');
