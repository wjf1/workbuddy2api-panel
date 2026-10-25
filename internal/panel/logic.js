'use strict';
/* logic.js 面板前端的纯函数层（不碰 DOM、不读全局状态）。
 *
 * 为什么单独一份：
 *   app.js 3500 行里混着「渲染/事件」和「格式化/解析」两类代码，后者本来是可以用
 *   断言测的，但因为和 UI 代码同文件，测试只能依赖浏览器/DOM，于是长期只有
 *   `node --check` 的语法校验——`fmtTok(1e6)` 到底该出 "1.00M" 还是 "1000.0k"
 *   这类回归没人拦得住。抽出来后 internal/panel/logic_test.js 用 node 直接跑断言。
 *
 * 双环境导出：
 *   - 浏览器：index.html 先加载本文件，挂到 window.WBLogic，app.js 顶部解构取用
 *   - node：module.exports（logic_test.js 用 require）
 *
 * 约束：本文件必须是**纯函数**——不碰 Date.now() 之外的全局状态，且时间相关的
 * 函数把「现在」作为可选入参传入（ago(iso, nowMs)），否则测试只能断言"大约"。
 */
(function (root) {
  /* esc 文本/属性双安全转义。不能只用 div.innerHTML（它转义 <>& 但不转义引号），
     否则字符串拼进 HTML 属性（如 title="uid: ..."）时引号可闭合属性并注入事件处理器。
     显式替换 5 个字符：& < > " '（& 必须最先，避免二次转义）。 */
  function esc(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  /* ago 相对时间。nowMs 显式传入是为了可测：省略时才取当前时刻。 */
  function ago(iso, nowMs) {
    if (!iso || String(iso).startsWith('0001-')) return '—';
    const now = nowMs === undefined ? Date.now() : nowMs;
    const s = (now - new Date(iso).getTime()) / 1000;
    if (!Number.isFinite(s)) return '—';
    if (s < 0) return '刚刚';
    if (s < 60) return Math.floor(s) + ' 秒前';
    if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
    if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
    return Math.floor(s / 86400) + ' 天前';
  }

  /* dur 时长：<1 分钟只报秒，<1 小时报分秒，否则报时分（秒被舍去）。 */
  function dur(sec) {
    sec = Math.max(0, Math.round(sec));
    const h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
    return h ? h + '时' + String(m).padStart(2, '0') + '分' : m ? m + '分' + String(s).padStart(2, '0') + '秒' : s + '秒';
  }

  /* parseAPITime 解析后端时间串；Go 零值时间（0001-）与非法串统一返回 0，
     让调用方用一个判据处理"没有值"。 */
  function parseAPITime(value) {
    const text = String(value || '');
    if (!text || text.startsWith('0001-')) return 0;
    const ms = Date.parse(text);
    return Number.isFinite(ms) ? ms : 0;
  }

  /* fmtLocalDateTime 本地时区 "YYYY-MM-DD HH:mm"（浏览器本地，不是 UTC）。 */
  function fmtLocalDateTime(ms) {
    const d = new Date(ms);
    const p = n => String(n).padStart(2, '0');
    return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' +
      p(d.getHours()) + ':' + p(d.getMinutes());
  }

  /* fmtK 计数简写：>=1000 转 K（四舍五入到整数 K）。 */
  function fmtK(n) { n = Number(n || 0); return n >= 1000 ? Math.round(n / 1000) + 'K' : String(n); }

  function fmtBytes(bytes) {
    const n = Number(bytes || 0);
    if (n < 1024) return n + ' B';
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
    return (n / 1024 / 1024).toFixed(1) + ' MB';
  }

  /* fmtTok token 数简写：B/M/k 三档，注意 k 是小写、B/M 是大写（沿用旧界面口径）。 */
  function fmtTok(n) {
    n = Number(n || 0);
    if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
    if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
    if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
    return String(n);
  }

  /* fmtMs 毫秒：0/空 显示破折号（"没有数据"而不是 "0ms"），>=1s 转秒。 */
  function fmtMs(ms) {
    ms = Number(ms || 0);
    if (!ms) return '—';
    if (ms >= 1000) return (ms / 1000).toFixed(2) + 's';
    return Math.round(ms) + 'ms';
  }

  function fmtRate(r) { return r ? Number(r).toFixed(1) + ' tok/s' : '—'; }

  /* trimFixed 去掉定点数尾部的 0 与小数点（"1.50" → "1.5"，"2.00" → "2"）。 */
  function trimFixed(s) {
    if (!String(s).includes('.')) return String(s);
    return String(s).replace(/0+$/, '').replace(/\.$/, '');
  }

  function fmtCredit(n) {
    const v = Number(n || 0);
    if (!Number.isFinite(v)) return '—';
    return trimFixed(v.toFixed(2));
  }

  /* fmtCreditRatio 每百万 token 的单价；样本数或 token 数为 0 时不给数字
     （否则会显示成 0.0000/1M，看起来像"免费"）。 */
  function fmtCreditRatio(v, samples, tokens) {
    if (!samples || !tokens) return '—';
    const n = Number(v || 0);
    if (!Number.isFinite(n)) return '—';
    return trimFixed(n.toFixed(4)) + ' / 1M';
  }

  function fmtModelRate(rate) {
    const s = String(rate || '').trim();
    return s ? 'x' + s : '—';
  }

  const api = {
    esc, ago, dur, parseAPITime, fmtLocalDateTime,
    fmtK, fmtBytes, fmtTok, fmtMs, fmtRate, trimFixed,
    fmtCredit, fmtCreditRatio, fmtModelRate,
  };

  root.WBLogic = api;
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
})(typeof globalThis !== 'undefined' ? globalThis : this);
