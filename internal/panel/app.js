/* 小浣熊网关控制台前端
 *
 * 纯原生 JS（无框架、无构建）：面板要跟二进制一起走，多一个构建步骤就多一个
 * 交付环节出错的机会。所有网络请求都经 api()，401 统一回登录框。
 *
 * 状态都放在 state 里，渲染函数只读 state → DOM，不做双向绑定。数据量小
 * （几十个账号、几百条日志），整表重绘比 diff 更简单也更不容易出 bug。
 */
(function () {
  "use strict";

  // 支持 ?key=xxx 直接带密钥打开（省去每次输入；也方便把「去看配置」这类
  // 链接发给同事——链路仍走 HTTPS，密钥不出现在日志里）。
  const urlKey = new URLSearchParams(location.search).get("key") || "";
  if (urlKey) {
    try { localStorage.setItem("xhx_key", urlKey); } catch (e) { /* 隐私模式忽略 */ }
  }
  const state = {
    key: urlKey || localStorage.getItem("xhx_key") || "",
    tab: "accounts",
    overview: null,
    models: null,
    requests: null,
    usage: null,
    logs: [],
    config: null,
    autoRefresh: null,
    scan: [],
    sms: { verificationID: "" },
    modelsFilter: { q: "", freeOnly: false },
    configView: "form",
    qr: { id: null, timer: null },
    device: { id: null, timer: null },
    // 账号表的筛选/排序/分页。全是内存态：数据源仍是 overview 快照，
    // 切筛选不发请求，也不改 10 秒轮询节奏。
    accView: { q: "", status: "all", sort: "default", size: 50, page: 1 },
  };

  // 账号表的 keyed 行缓存：uid → { el, sig }。轮询时按签名做增量更新，
  // 避免整表 innerHTML 重建（会丢滚动位置、hover tooltip、选中文本）。
  const accRows = new Map();

  // $ 是 querySelector 的薄封装（选择器必须带 # / . / [）。
  // 裸 id 会被当成标签名、静默返回 null——这类手误只会表现成「页面某块空白」，
  // 排查成本很高（踩过：$("models-count") 让模型页整页空白）。所以找不到时
  // 顺手检查同名的 id 是否存在，存在就明确告警。
  const $ = (sel, root) => {
    const el = (root || document).querySelector(sel);
    if (el === null && /^[A-Za-z][\w-]*$/.test(sel) && document.getElementById(sel)) {
      console.warn(`[panel] 选择器 "${sel}" 少了 "#"，实际元素是 #${sel}`);
    }
    return el;
  };
  const $$ = (sel, root) => Array.from((root || document).querySelectorAll(sel));

  // ---------------------------------------------------------------------
  // 主题：跟随系统 → 浅色 → 深色（点一次循环一档）

  const THEME_KEY = "xhx_theme";
  const THEME_ORDER = ["auto", "light", "dark"];
  const THEME_LABEL = { auto: "跟随系统", light: "浅色", dark: "深色" };

  function themePref() {
    const saved = localStorage.getItem(THEME_KEY);
    return THEME_ORDER.indexOf(saved) >= 0 ? saved : "auto";
  }

  function effTheme() {
    const t = themePref();
    if (t !== "auto") return t;
    return matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
  }

  // 只切 <html data-theme>，颜色全交给 CSS 令牌。
  // 图标同步换：浅色显示太阳（代表当前外观），深色显示月亮。
  function applyTheme() {
    const eff = effTheme();
    document.documentElement.dataset.theme = eff;
    const ico = $("#icoTheme");
    if (ico) {
      ico.innerHTML = eff === "light"
        ? '<circle cx="8" cy="8" r="3"/><path d="M8 1.2v1.9M8 12.9v1.9M1.2 8h1.9M12.9 8h1.9M3.2 3.2l1.4 1.4M11.4 11.4l1.4 1.4M12.8 3.2l-1.4 1.4M4.6 11.4l-1.4 1.4"/>'
        : '<path d="M13.2 9.6A5.6 5.6 0 0 1 6.4 2.8a5.6 5.6 0 1 0 6.8 6.8z"/>';
    }
    const btn = $("#btnTheme");
    if (btn) btn.title = "主题：" + THEME_LABEL[themePref()] + "（点击切换）";
  }

  // ---------------------------------------------------------------------
  // 手机端抽屉（桌面端由 CSS 保证 .nav 常驻、.nav-btn 不显示）

  function setNav(open) {
    const drawer = $("#navDrawer");
    if (!drawer) return;
    drawer.classList.toggle("open", open);
    const veil = $("#navVeil");
    if (veil) veil.classList.toggle("on", open);
    const btn = $("#navBtn");
    if (btn) btn.setAttribute("aria-expanded", open ? "true" : "false");
  }

  // 立刻应用一次：脚本在 </body> 前执行，此时 DOM 已就绪，
  // 不会出现「先白一下再变深色」的闪烁。
  applyTheme();

  // ---------------------------------------------------------------------
  // 基础工具

  function fmtNum(n) {
    if (n === null || n === undefined || Number.isNaN(n)) return "-";
    if (typeof n !== "number") return String(n);
    return n.toLocaleString("zh-CN");
  }

  function fmtDur(ms) {
    if (!ms || ms < 0) return "-";
    if (ms < 1000) return ms + "ms";
    if (ms < 60000) return (ms / 1000).toFixed(1) + "s";
    return Math.floor(ms / 60000) + "m" + Math.round((ms % 60000) / 1000) + "s";
  }

  function fmtTime(v) {
    if (!v) return "-";
    const d = typeof v === "string" ? new Date(v) : new Date(v * 1000);
    if (Number.isNaN(d.getTime())) return "-";
    const p = (x) => String(x).padStart(2, "0");
    const now = new Date();
    const sameDay = d.toDateString() === now.toDateString();
    const time = p(d.getHours()) + ":" + p(d.getMinutes()) + ":" + p(d.getSeconds());
    return sameDay ? time : p(d.getMonth() + 1) + "-" + p(d.getDate()) + " " + time;
  }

  function fmtAgo(ts) {
    if (!ts) return "-";
    const sec = Math.round(Date.now() / 1000 - ts);
    if (sec < 60) return sec + " 秒前";
    if (sec < 3600) return Math.round(sec / 60) + " 分钟前";
    if (sec < 86400) return Math.round(sec / 3600) + " 小时前";
    return Math.round(sec / 86400) + " 天前";
  }

  function esc(s) {
    return String(s === null || s === undefined ? "" : s)
      .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;").replace(/'/g, "&#39;");
  }

  function toast(msg, kind) {
    // 极简 toast：顶部中间浮一条，2.6 秒后消失。
    // 样式全在 style.css（#toast.ok/.warn/.bad）：以前写内联，深色主题下是白字压色块。
    let el = $("#toast");
    if (!el) {
      el = document.createElement("div");
      el.id = "toast";
      document.body.appendChild(el);
    }
    el.className = kind === "bad" ? "bad" : kind === "warn" ? "warn" : "ok";
    el.textContent = msg;
    el.style.opacity = "1";
    clearTimeout(el._t);
    el._t = setTimeout(() => { el.style.opacity = "0"; }, 2600);
  }

  // ---------------------------------------------------------------------
  // API

  // GET 的两条防护，都是为「10 秒轮询 + 公网抖动」这个组合：
  //   1) 超时。裸 fetch 在链路半死时会挂几分钟，既不报错也不返回，界面永远停在
  //      旧值上，也没有任何提示。超时必须按方法分档：POST 里有批量签到/保活，
  //      实测要一两分钟，给 8 秒会把「还在正常跑」误报成超时（服务端还在真扣积分）。
  //   2) 同路径防重入。上一轮没回来就跳过本轮（**不排队**）：排队只会让数据更旧，
  //      而乱序返回会让旧响应盖掉新响应。
  // 两条都只对 GET 生效：POST 是用户动作，跳过等于「点了没反应」。
  const GET_TIMEOUT_MS = 8000;
  const POST_TIMEOUT_MS = 300000;
  const apiInflight = new Set();

  async function api(path, opts) {
    opts = opts || {};
    const method = String(opts.method || "GET").toUpperCase();
    const isGet = method === "GET";
    if (isGet && apiInflight.has(path)) return null; // 上一轮还没回来：本轮直接跳过
    if (isGet) apiInflight.add(path);

    const headers = Object.assign({}, opts.headers || {});
    if (state.key) headers["Authorization"] = "Bearer " + state.key;
    if (opts.body && typeof opts.body !== "string") {
      headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(opts.body);
    }

    const timeout = isGet ? GET_TIMEOUT_MS : POST_TIMEOUT_MS;
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), timeout);
    let res;
    try {
      res = await fetch(path, Object.assign({}, opts, { headers, signal: ctl.signal }));
    } catch (e) {
      if (e && e.name === "AbortError") {
        throw new Error("请求超时（" + Math.round(timeout / 1000) + " 秒）：" + path);
      }
      throw e;
    } finally {
      clearTimeout(timer);
      if (isGet) apiInflight.delete(path);
    }

    if (res.status === 401) {
      showLogin("密钥无效，请重新输入");
      throw new Error("unauthorized");
    }
    const text = await res.text();
    let data = null;
    try { data = text ? JSON.parse(text) : null; } catch (e) { data = { raw: text }; }
    if (!res.ok) {
      const msg = (data && (data.error || (data.error && data.error.message))) || res.statusText;
      throw new Error(typeof msg === "string" ? msg : JSON.stringify(msg));
    }
    return data;
  }

  // ---------------------------------------------------------------------
  // 登录

  function showLogin(msg) {
    $("#app").hidden = true;
    $("#modal-login").hidden = false;
    $("#login-msg").textContent = msg || "";
    setTimeout(() => $("#login-key").focus(), 30);
  }

  function showApp() {
    $("#modal-login").hidden = true;
    $("#app").hidden = false;
  }

  async function tryEnter(key) {
    state.key = key || "";
    localStorage.setItem("xhx_key", state.key);
    try {
      await api("/panel/api/overview");
      showApp();
      await refreshAll();
      const initial = location.hash.slice(1);
      if (initial) await activateTab(initial, false);
      // ?import=sms / ?import=client 直接打开「添加账号」的对应页
      // （分享「去加个号」的链接用；也让无头截图能验证弹层里的内容）。
      const params = new URLSearchParams(location.search);
      const wantImport = params.get("import");
      if (wantImport) {
        // 顺手支持 ?phone=：把手机号填好，用户只需要过一次人机验证
        // （人机验证必须真人点，代码里没法代劳）。
        const phone = params.get("phone");
        if (phone) $("#sms-phone").value = phone;
        const inviter = params.get("inviter");
        if (inviter) $("#sms-inviter").value = inviter;
        openModal("modal-import");
        switchImportMode(wantImport);
      }
      return true;
    } catch (e) {
      if (e.message !== "unauthorized") showLogin(e.message);
      return false;
    }
  }

  // ---------------------------------------------------------------------
  // 数据加载

  // 每个 loadXxx 都先判 null：api() 在「同路径上一轮还没回来」时会直接返回 null
  // 表示本轮跳过。不判的话要么把 null 赋进 state（后续 render 崩），要么白刷一遍。
  async function loadOverview() {
    const data = await api("/panel/api/overview");
    if (!data) return;
    state.overview = data;
    renderCards();
    renderAccounts();
    renderScheduler();
    $("#ver").textContent = data.version || "dev";
  }

  async function loadModels() {
    const data = await api("/panel/api/models");
    if (!data) return;
    state.models = data;
    renderModels();
  }

  async function loadRequests() {
    const data = await api("/panel/api/requests?limit=100");
    if (!data) return;
    state.requests = data;
    renderRequests();
  }

  async function loadUsage() {
    const data = await api("/panel/api/usage");
    if (!data) return;
    state.usage = data;
    renderUsage();
  }

  async function loadLogs() {
    const data = await api("/panel/api/logs?limit=300");
    if (!data) return;
    state.logs = data.lines || [];
    renderLogs();
  }

  async function loadConfig() {
    const data = await api("/panel/api/config");
    if (!data) return;
    state.config = data;
    $("#config-path").textContent = data.path || "";
    $("#config-editor").value = JSON.stringify(data.config, null, 2);
    renderConfigForm();
  }

  async function refreshAll() {
    try {
      await loadOverview();
      if (state.tab === "models") await loadModels();
      if (state.tab === "requests") await loadRequests();
      if (state.tab === "usage") await loadUsage();
      if (state.tab === "logs") await loadLogs();
      if (state.tab === "config" && !state.config) await loadConfig();
    } catch (e) {
      if (e.message !== "unauthorized") toast(e.message, "bad");
    }
  }

  // ---------------------------------------------------------------------
  // 渲染：账号

  function accountState(a) {
    const now = Date.now() / 1000;
    if (a.disabled) return { text: "已禁用", cls: "bad" };
    if (a.paused) return { text: "已暂停", cls: "warn" };
    if (a.breaker_until > now) return { text: "熔断中", cls: "bad", tip: a.cooldown_note };
    if (a.cooldown_until > now) return { text: "冷却 · " + coolKind(a.cooldown_kind), cls: "warn", tip: a.cooldown_note };
    if (a.in_flight > 0) return { text: "使用中", cls: "blue" };
    return { text: "空闲", cls: "ok" };
  }

  function coolKind(kind) {
    return { plan_limit: "额度", soft_rate: "限流", session_dead: "登录态", gateway: "网关", busy: "排队", not_found: "404" }[kind] || "冷却";
  }

  // 状态归类：筛选、异常条、排序分组三处必须用同一个口径，
  // 否则会出现「条上数了 3 个冷却、点进去只有 2 行」这种自相矛盾。
  // 优先级与 accountState() 一致：禁用 > 暂停 > 熔断/冷却 > 可用。
  function accStatus(a) {
    const now = Date.now() / 1000;
    if (a.disabled) return "off";
    if (a.paused) return "paused";
    if (a.breaker_until > now || a.cooldown_until > now) return "cool";
    return "ok";
  }

  function accMatch(a, v) {
    if (v.status !== "all") {
      if (v.status === "issue") {
        if (accStatus(a) === "ok") return false;
      } else if (accStatus(a) !== v.status) {
        return false;
      }
    }
    if (!v.q) return true;
    const q = v.q.toLowerCase();
    return String(a.nickname || "").toLowerCase().includes(q) ||
      String(a.uid || "").toLowerCase().includes(q);
  }

  // 排序分组：异常优先。0 禁用 / 1 冷却 / 2 暂停 / 3 正常，组内按导入顺序（列表原序）。
  function accRank(a) {
    const s = accStatus(a);
    return s === "off" ? 0 : s === "cool" ? 1 : s === "paused" ? 2 : 3;
  }

  function accSortList(list, sort) {
    const arr = list.slice();
    if (sort === "default" || !sort) {
      // 稳定排序靠「只比 rank」+ slice 保持原序（现代 V8 的 sort 本身稳定，
      // 但显式依赖它比依赖实现细节更安全）。
      return arr.sort((a, b) => accRank(a) - accRank(b));
    }
    const byNum = (f) => (a, b) => (f(b) || 0) - (f(a) || 0);
    if (sort === "credits_desc") return arr.sort(byNum((x) => x.credits));
    if (sort === "credits_asc") return arr.sort((a, b) => (a.credits || 0) - (b.credits || 0));
    if (sort === "fails_desc") return arr.sort(byNum((x) => (x.total || 0) - (x.success || 0)));
    if (sort === "success_desc") return arr.sort(byNum((x) => x.success));
    if (sort === "last_used") return arr.sort(byNum((x) => x.last_used));
    if (sort === "name") {
      return arr.sort((a, b) => String(a.nickname || a.uid).localeCompare(String(b.nickname || b.uid), "zh-CN"));
    }
    return arr;
  }

  function renderCards() {
    const o = state.overview;
    if (!o) return;
    const c = o.counts || {};
    // 侧栏状态灯：有账号且有可用号 = 运行中；有号但全冷却 = 降级；没号 = 待配置。
    const pulse = $("#navPulse");
    const navState = $("#navState");
    if (pulse && navState) {
      if (!c.total) {
        pulse.className = "pulse warn";
        navState.textContent = "待配置";
      } else if (!c.healthy) {
        pulse.className = "pulse bad";
        navState.textContent = "全部冷却";
      } else {
        pulse.className = "pulse";
        navState.textContent = "运行中";
      }
    }
    const r = o.requests || {};
    const successRate = r.total ? ((r.success / r.total) * 100).toFixed(1) + "%" : "-";
    const cards = [
      { label: "账号总数", value: c.total || 0, foot: `可用 ${c.healthy || 0} · 冷却 ${c.cooling || 0} · 禁用 ${c.disabled || 0}` },
      { label: "在途请求", value: c.in_flight || 0, foot: `RPM ${r.rpm ? r.rpm.toFixed(1) : "-"}`, cls: "accent" },
      { label: "请求总数", value: fmtNum(r.total || 0), foot: `失败 ${fmtNum(r.failure || 0)}` },
      { label: "成功率", value: successRate, foot: `P50 ${fmtDur(r.latency_p50_ms)} · P95 ${fmtDur(r.latency_p95_ms)}`, cls: "ok" },
      // Token 总量只统计上游给了 usage 的请求；未记账的条数必须一起露出来，
      // 否则「上游没回 usage」会被读成「这个池子特别省 token」。
      { label: "Token 总量", value: fmtNum(r.tokens_total || 0),
        foot: r.unmetered ? `输入 + 输出 · ${fmtNum(r.unmetered)} 条未记账` : "输入 + 输出" },
      { label: "运行时长", value: fmtDur((o.uptime_s || 0) * 1000), foot: "自进程启动" },
    ];
    $("#cards").innerHTML = cards.map((card) => `
      <div class="card ${card.cls || ""}">
        <div class="label">${esc(card.label)}</div>
        <div class="value">${esc(card.value)}</div>
        <div class="foot">${esc(card.foot || "")}</div>
      </div>`).join("");
    // 顶栏副标题跟着账号概览走：一眼看到「几个能用、几个在途」。
    const meta = $("#subMeta");
    if (meta) {
      meta.textContent = c.total
        ? `可用 ${c.healthy || 0}/${c.total} · 在途 ${c.in_flight || 0} · 成功率 ${successRate}`
        : "账号池为空";
    }
  }

  // ── 账号表：行签名 + 增量更新 ─────────────────────────────────────────
  // 原来每 10 秒一次 innerHTML 重建整表。号一多后果很具体：滚动位置回弹、
  // hover 的 title 消失、正在看的行跳到别处、选中文本被清空。
  // 现在：首次全量建表；之后签名相同就一个字节都不写，不同才重建那一行；
  // 新 uid 建行、消失的删行、顺序变化用 insertBefore 移动（保留节点身份）。
  // 行内按钮走 tbody 的事件委托 + data-uid，所以重建单行不会丢事件绑定。
  function accRowSig(a, maxCred) {
    const st = accountState(a);
    // fmtAgo 的文本随时间变。把「距今多少分钟」做成桶放进签名：
    // 完全不进签名 → 「3 分钟前」会冻住；按秒进签名 → 每轮都全表重建。
    const agoBucket = a.last_used ? Math.floor((Date.now() / 1000 - a.last_used) / 60) : -1;
    return [
      a.uid, a.nickname, a.credits, maxCred, a.checked_in === todayStr() ? 1 : 0,
      st.text, st.cls, st.tip || "", a.cooldown_note || "", a.last_error || "",
      a.in_flight || 0, a.total || 0, a.success || 0, a.avg_latency_ms || 0,
      agoBucket, a.has_refresh ? 1 : 0, a.disabled ? 1 : 0, a.paused ? 1 : 0,
      a.device_bound || "",
    ].join("\u0001");
  }

  function accRowHtml(a, maxCred) {
    const now = Date.now() / 1000;
    const st = accountState(a);
    const total = a.total || 0;
    const rate = total ? ((a.success / total) * 100).toFixed(0) + "%" : "-";
    const signed = a.checked_in === todayStr();
    // 未绑设备的号上游根本不发每日积分：显示「无资格」而不是灰着的「未签」，
    // 否则用户会一直点签到、一直没反应，误以为是网关坏了。
    const checkin = signed ? `<span class="tag ok">已签</span>`
      : a.device_bound === "unbound"
        ? `<span class="tag warn" title="上游只给已绑定可信设备的账号发每日积分；需在官方客户端登录一次完成绑定">未绑设备</span>`
        : `<span class="tag">未签</span>`;
    // 积分条：剩余 / 池内最高。表达的是「这个号还剩多少额度」的绝对量，
    // 基准取全池最大值、不跟筛选走——否则同一个号切筛选前后百分比会变，
    // 「筛到只剩低分号时显示满格」会被误读成额度健康。
    const pct = Math.min(100, Math.round(((a.credits || 0) / maxCred) * 100));
    const pctCls = pct < 15 ? "bad" : pct < 40 ? "warn" : "";
    const actions = [];
    actions.push(`<button class="btn mini" data-act="checkin" data-uid="${esc(a.uid)}">签到</button>`);
    actions.push(`<button class="btn mini" data-act="balance" data-uid="${esc(a.uid)}">余额</button>`);
    if (a.disabled) {
      actions.push(`<button class="btn mini" data-act="enable" data-uid="${esc(a.uid)}">启用</button>`);
    } else if (a.paused) {
      actions.push(`<button class="btn mini" data-act="resume" data-uid="${esc(a.uid)}">恢复</button>`);
    } else {
      if (a.cooldown_until > now || a.breaker_until > now) {
        actions.push(`<button class="btn mini" data-act="revive" data-uid="${esc(a.uid)}">解冻</button>`);
      }
      actions.push(`<button class="btn mini" data-act="pause" data-uid="${esc(a.uid)}">暂停</button>`);
      actions.push(`<button class="btn mini" data-act="disable" data-uid="${esc(a.uid)}">禁用</button>`);
    }
    actions.push(`<button class="btn mini danger" data-act="remove" data-uid="${esc(a.uid)}">移除</button>`);
    // data-label 是手机端「卡片化」用的：≤760px 时表头隐藏、每个 td 前面
    // 用 ::before 打出字段名，否则一屏数字看不出哪列是哪列。
    return `<tr data-uid="${esc(a.uid)}">
      <td class="who" data-label="账号">
        <div class="acct-name">${esc(a.nickname || a.uid)}</div>
        <div class="uid">${esc(a.uid)}${a.has_refresh ? "" : " · 无续期令牌"}${a.device_bound === "unbound" ? " · 未绑设备" : ""}</div>
      </td>
      <td class="cred-cell" data-label="积分" title="剩余 ${esc(fmtNum(a.credits))}（池内最高 ${esc(fmtNum(maxCred))}）">
        <div class="n">${fmtNum(a.credits)}</div>
        <div class="bar"><i class="${pctCls}" style="width:${pct}%"></i></div>
      </td>
      <td data-label="签到">${checkin}</td>
      <td data-label="状态" title="${esc(st.tip || "")}"><span class="tag ${st.cls}">${esc(st.text)}</span>
        ${a.cooldown_note && (a.cooldown_until > now) ? `<div class="uid">${esc(a.cooldown_note)}</div>` : ""}
        ${a.last_error && !(a.cooldown_until > now) ? `<div class="uid" title="${esc(a.last_error)}">${esc(a.last_error.slice(0, 40))}</div>` : ""}
      </td>
      <td class="num" data-label="在途">${a.in_flight || 0}</td>
      <td class="num" data-label="请求">${fmtNum(total)}<span class="muted"> · ${fmtAgo(a.last_used)}</span></td>
      <td class="num" data-label="成功率">${rate}</td>
      <td class="num" data-label="延迟">${fmtDur(a.avg_latency_ms)}</td>
      <td class="acts" data-label="操作"><div class="rowacts">${actions.join("")}</div></td>
    </tr>`;
  }

  // <template> 解析成节点：不执行脚本、不加载资源，
  // 也不像往活 DOM 里插字符串那样触发整表重排。
  function trFromHtml(html) {
    const tpl = document.createElement("template");
    tpl.innerHTML = html;
    return tpl.content.firstElementChild;
  }

  const ACC_STATUS = [
    { k: "all", label: "全部" },
    { k: "issue", label: "异常" },
    { k: "ok", label: "可用" },
    { k: "cool", label: "冷却" },
    { k: "paused", label: "暂停" },
    { k: "off", label: "禁用" },
  ];

  function renderAccChips(all) {
    const box = $("#accStatusChips");
    if (!box) return;
    const n = { all: all.length, issue: 0, ok: 0, cool: 0, paused: 0, off: 0 };
    all.forEach((a) => {
      const s = accStatus(a);
      n[s]++;
      if (s !== "ok") n.issue++;
    });
    const v = state.accView;
    // 签名没变就不写 DOM：10 秒轮询里绝大多数轮次计数都不变，
    // 白写一次 innerHTML 等于让整条工具栏重排一次。
    const sig = ACC_STATUS.map((s) => s.k + n[s.k]).join("|") + v.status;
    if (box.dataset.sig === sig) return;
    box.dataset.sig = sig;
    box.innerHTML = ACC_STATUS.map((s) => {
      const tone = s.k === "issue" && n.issue ? " bad" : (s.k === "cool" && n.cool) ? " warn" : "";
      return `<button class="chip${v.status === s.k ? " on" : ""}${tone}" data-status="${s.k}" type="button">` +
        `${s.label}<span class="n">${n[s.k]}</span></button>`;
    }).join("");
  }

  // 异常汇总条：全池健康时整条 hidden，0 高 0 边距，不占位。
  // 留一条常驻空条既浪费高度，又会被读成「装饰」——异常真出现时反而不显眼。
  function renderAlertBar(all) {
    const bar = $("#alertBar");
    if (!bar) return;
    const n = { off: 0, cool: 0, paused: 0 };
    all.forEach((a) => {
      const s = accStatus(a);
      if (n[s] !== undefined) n[s]++;
    });
    const parts = [];
    if (n.off) parts.push(n.off + " 个已禁用");
    if (n.cool) parts.push(n.cool + " 个冷却 / 熔断");
    if (n.paused) parts.push(n.paused + " 个已暂停");
    if (!parts.length) {
      bar.hidden = true;
      return;
    }
    bar.hidden = false;
    const txt = $("#alertText");
    if (txt) txt.textContent = "需要关注：" + parts.join(" · ");
    const dot = $("#alertDot");
    if (dot) dot.className = "pulse " + (n.off || n.cool ? "bad" : "warn");
    const chips = $("#alertChips");
    if (chips) {
      chips.innerHTML = [
        n.off ? `<button class="chip on bad" data-status="off" type="button">查看禁用 ${n.off}</button>` : "",
        n.cool ? `<button class="chip on warn" data-status="cool" type="button">查看冷却 ${n.cool}</button>` : "",
        n.paused ? `<button class="chip on warn" data-status="paused" type="button">查看暂停 ${n.paused}</button>` : "",
      ].join("");
    }
  }

  function renderAccPager(total, pages, start, shown) {
    const info = $("#accPageInfo");
    const num = $("#accPageNum");
    const prev = $("#accPrev");
    const next = $("#accNext");
    if (info) {
      info.textContent = total ? `第 ${start + 1}–${start + shown} 条 · 共 ${total} 条` : "共 0 条";
    }
    if (num) num.textContent = pages > 1 ? `${state.accView.page} / ${pages}` : "";
    if (prev) prev.disabled = state.accView.page <= 1;
    if (next) next.disabled = state.accView.page >= pages;
  }

  function renderAccounts() {
    const tbody = $("#accounts-table tbody");
    if (!tbody) return;
    const list = (state.overview && state.overview.accounts) || [];
    const v = state.accView;

    if (!list.length) {
      accRows.clear();
      tbody.innerHTML = `<tr><td colspan="9"><div class="empty">
        <div class="big">账号池是空的</div>
        点右上角「添加账号」导入凭证，或用「客户端目录」「手机号」登录。</div></td></tr>`;
      renderAccChips(list);
      renderAlertBar(list);
      renderAccPager(0, 1, 0, 0);
      return;
    }

    const maxCred = Math.max(1, ...list.map((a) => a.credits || 0));
    // ① 过滤 → ② 排序 → ③ 分页，全在内存里完成。
    // 顺序不能换：先分页再排序会让「第 2 页」的含义随排序变，翻页时内容乱跳。
    const filtered = list.filter((a) => accMatch(a, v));
    const sorted = accSortList(filtered, v.sort);
    const total = sorted.length;
    const size = v.size > 0 ? v.size : Math.max(1, total);
    const pages = Math.max(1, Math.ceil(total / size));
    // 页码夹紧：筛选后总数变少时，停在原来第 5 页会看到空表（最容易被当成 bug）。
    if (v.page > pages) v.page = pages;
    if (v.page < 1) v.page = 1;
    const start = (v.page - 1) * size;
    const rows = sorted.slice(start, start + size);

    renderAccChips(list);
    renderAlertBar(list);
    renderAccPager(total, pages, start, rows.length);

    if (!rows.length) {
      accRows.clear();
      // 两种空态要分开：「池子空」和「筛选没命中」的下一步动作完全不同。
      tbody.innerHTML = `<tr><td colspan="9"><div class="empty">
        <div class="big">没有匹配的账号</div>
        当前筛选下 ${list.length} 个号里没有命中的。
        <button class="btn mini" id="accClearFilter" type="button">清除筛选</button></div></td></tr>`;
      return;
    }

    const items = rows.map((a) => ({
      key: a.uid,
      sig: accRowSig(a, maxCred),
      html: () => accRowHtml(a, maxCred), // 惰性求值：签名没变的行连字符串都不拼
    }));

    // 首次（或刚从空态回来）：一次 innerHTML 建全表，比逐行 append 快得多。
    if (!accRows.size) {
      tbody.innerHTML = items.map((it) => it.html()).join("");
      items.forEach((it, i) => accRows.set(it.key, { el: tbody.children[i], sig: it.sig }));
      return;
    }

    // ① 删掉本轮不该存在的行
    const seen = new Set(items.map((it) => it.key));
    for (const [key, row] of accRows) {
      if (seen.has(key)) continue;
      row.el.remove();
      accRows.delete(key);
    }
    // ② 一个游标走完新顺序：对得上就不动，对不上就 insertBefore 拽过来。
    //    处理过的行总在游标之前，所以游标之后剩下的必然是这轮不该存在的节点。
    let cursor = tbody.firstElementChild;
    for (const it of items) {
      let row = accRows.get(it.key);
      const atCursor = !!row && row.el === cursor;
      if (!row) {
        row = { el: trFromHtml(it.html()), sig: it.sig };
        accRows.set(it.key, row);
      } else if (row.sig !== it.sig) {
        const fresh = trFromHtml(it.html());
        row.el.replaceWith(fresh);
        row.el = fresh;
        row.sig = it.sig;
      }
      if (atCursor) cursor = row.el.nextElementSibling;
      else tbody.insertBefore(row.el, cursor);
    }
    while (cursor) {
      const next = cursor.nextElementSibling;
      cursor.remove();
      cursor = next;
    }
  }

  function todayStr() {
    const d = new Date();
    const p = (x) => String(x).padStart(2, "0");
    return d.getFullYear() + "-" + p(d.getMonth() + 1) + "-" + p(d.getDate());
  }

  function renderScheduler() {
    const o = state.overview;
    const box = $("#scheduler-body");
    if (!o || !o.scheduler) {
      box.innerHTML = `<div class="kv"><span class="k">调度器</span><span class="v">未启用</span></div>`;
      return;
    }
    const cfg = o.scheduler.config || {};
    const results = o.scheduler.results || {};
    const rows = [
      ["签到时点", (cfg.CheckinHours || []).map((h) => h + ":00").join(" / ") + (cfg.CheckinEnabled ? "" : "（已关闭）")],
      // 说清楚「签到」到底做什么：它不是发分入口（grant 恒幂等空操作），
      // 而是观测当天有没有 daily_grant 到账。不写这句会被当成「点了就该加分」。
      ["签到做什么", "观测当日 daily_grant 是否到账（发放由上游按天触发，时点不固定）"],
      ["保活时点", ((cfg.KeepaliveHours || []).length >= 24 ? "每小时" : (cfg.KeepaliveHours || []).map((h) => h + ":00").join(" / ")) + (cfg.KeepaliveEnabled ? "" : "（已关闭）")],
      ["余额刷新", (cfg.BalanceRefreshMinutes || 0) + " 分钟一次"],
      ["含已禁用账号", cfg.IncludeDisabled ? "是" : "否"],
      ["新号静默期", (cfg.NewAccountGraceDays || 0) + " 天"],
    ];
    const resultRows = Object.keys(results).map((job) => {
      const r = results[job];
      const name = { checkin: "签到", keepalive: "保活", balance: "余额" }[job] || job;
      return [name + " 最近一次", `${r.ok || 0} 成功 / ${r.skipped || 0} 跳过 / ${r.failed || 0} 失败 · ${fmtTime(r.ended_at)}`];
    });
    box.innerHTML = rows.concat(resultRows).map(([k, v]) =>
      `<div class="kv"><span class="k">${esc(k)}</span><span class="v">${esc(v)}</span></div>`).join("") +
      (o.session ? `<div class="kv"><span class="k">会话粘性</span><span class="v">绑定 ${o.session.bindings || 0} · 命中率 ${((o.session.hit_rate || 0) * 100).toFixed(0)}%</span></div>` : "");
  }

  // ---------------------------------------------------------------------
  // 渲染：模型

  // 模型卡片：一屏看清「能不能用、贵不贵、多大上下文」。
  //
  // 原来是七列表格，ID 和名称各占一列、上下文是 1000000 这种原始数字，
  // 扫一眼分不出哪个免费、哪个上下文大。改成卡片 + 人读的数值格式。
  // 单价换算：实测（2026-10-26）x1 模型约 0.5 分/千 token，且每请求有 2 分下限。
  // 上游目录只给倍率不给单价，所以这里用「实测拟合的基准 × 目录倍率」估，
  // 界面上必须标「≈」，别让用户当账单。
  const RATE_X1_PER_KTOKEN = 0.5;

  function multiplierOf(m) {
    const n = parseFloat(String(m.credits || "").replace("x", ""));
    return Number.isFinite(n) ? n : null;
  }

  function estCost(m) {
    const free = m.billing_status === "limited_free" || m.credits === "x0";
    if (free) return '<span>单价 <b>免费</b></span>';
    const mult = multiplierOf(m);
    if (mult == null) return "";
    const rate = mult * RATE_X1_PER_KTOKEN;
    return `<span>≈ <b>${rate >= 0.1 ? rate.toFixed(2) : rate.toFixed(3)}</b> 分/千token</span>`;
  }

  function fmtCount(n) {
    if (!n) return "—";
    if (n >= 10000) {
      const wan = n / 10000;
      return (wan >= 100 ? Math.round(wan) : Math.round(wan * 10) / 10) + " 万";
    }
    return String(n);
  }

  function modelCard(m) {
    const free = m.billing_status === "limited_free" || m.credits === "x0";
    const tags = [];
    // 「免费」已经在右上角的价格位显示，标签行里不再重复一次。
    if (m.default) tags.push('<span class="tag ok">默认</span>');
    if (m.reasoning) tags.push('<span class="tag">思考</span>');
    if (m.vision) tags.push('<span class="tag">视觉</span>');
    if (m.category) tags.push('<span class="tag">' + esc(m.category) + '</span>');
    // 上游 visible=false：官网模型选择器里看不到，但**可调用**（默认模型就在这类里）。
    if (m.unlisted) tags.push('<span class="tag warn" title="上游目录未在官网选择器里展示，但实测可调用">上游未列出</span>');
    if (m.hidden) tags.push('<span class="tag bad">已拉黑</span>');
    const price = free
      ? '<span class="pill ok">免费</span>' + (m.base_credits ? ' <s>' + esc(m.base_credits) + '</s>' : '')
      : (m.credits ? esc(m.credits) : '');

    // 别名是网关侧的概念，上游目录里没有——不显示出来，用户根本不知道
    // 自己还能用 gpt-4o / o3 这类名字调这个模型。
    const aliasChips = (m.aliases || []).map((a) =>
      `<button class="tag alias" data-copy="${esc(a)}" title="点击复制别名">${esc(a)}</button>`).join("");

    // 上游的 name 其实是一句「适合做什么」的描述，不是名字；ID 才是要填进请求的。
    // 所以标题用 ID（等宽、可选中），描述放第二行——原来的表格里这两列正好反了。
    return `<div class="model-card${free ? " free" : ""}${m.hidden ? " off" : ""}">
      <div class="mc-head">
        <button class="mc-title" data-copy="${esc(m.id)}" title="点击复制模型 ID">${esc(m.id)}</button>
        <div class="mc-price">${price}</div>
      </div>
      ${m.name ? `<div class="mc-desc" title="${esc(m.name)}">${esc(m.name)}</div>` : ""}
      <div class="mc-tags">${tags.join("") || '<span class="tag">标准</span>'}</div>
      ${aliasChips ? `<div class="mc-aliases"><span class="hint">别名</span>${aliasChips}</div>` : ""}
      ${m.free_note ? `<div class="mc-note">${esc(m.free_note)}</div>` : ""}
      <div class="mc-stats">
        <span>上下文 <b>${fmtCount(m.context_length)}</b></span>
        <span>输出 <b>${fmtCount(m.max_output_tokens)}</b></span>
        ${m.recent_ms ? `<span title="最近成功对话请求的中位耗时（实时统计）">实测 <b class="${m.recent_ms >= 60000 ? "lat-slow" : m.recent_ms >= 25000 ? "lat-warn" : ""}">${fmtDur(m.recent_ms)}</b></span>` : ""}
        ${m.recent_failures ? `<span title="最近窗口内该模型的失败/超时次数">失败 <b class="lat-slow">${m.recent_failures}</b></span>` : ""}
        ${estCost(m)}
      </div>
    </div>`;
  }

  // groupOrphans 把「别名 → 目标」按目标聚成 {目标: [别名]}。
  function groupOrphans(orphans) {
    const byTarget = {};
    orphans.forEach((o) => { (byTarget[o.target] = byTarget[o.target] || []).push(o.alias); });
    return byTarget;
  }

  function renderModels() {
    const d = state.models;
    if (!d) return;
    const all = d.models || [];
    const f = state.modelsFilter;
    const q = (f.q || "").trim().toLowerCase();
    const list = all.filter((m) => {
      if (f.freeOnly && !(m.billing_status === "limited_free" || m.credits === "x0")) return false;
      if (!q) return true;
      // 别名也参与搜索：用户往往记着「我平时用 gpt-4o」，而不是上游模型 id。
      return (m.id || "").toLowerCase().includes(q)
        || (m.name || "").toLowerCase().includes(q)
        || (m.aliases || []).some((a) => a.toLowerCase().includes(q));
    });
    // 免费优先，然后按倍率从便宜到贵，最后按名字——这样「该用哪个」一眼可见。
    const priceOf = (m) => {
      const raw = (m.credits || "").replace("x", "");
      const n = parseFloat(raw);
      return Number.isFinite(n) ? n : 999;
    };
    // 同价按 id 排：上游的 name 是中文描述，按它排出来的顺序对用户不可预测。
    list.sort((a, b) => priceOf(a) - priceOf(b) || a.id.localeCompare(b.id));

    let meta = d.refreshed_at
      ? `目录更新于 ${fmtTime(d.refreshed_at)}　·　按倍率从低到高排列　·　点模型 ID 或别名可复制`
      : "尚未拉取过目录（当前可能是兜底清单）";
    if (d.default_model) meta += `　·　默认模型 ${d.default_model}`;
    $("#models-meta").textContent = meta;
    $("#models-billing").textContent =
      "计费口径（实测）：每请求最低 2 分；x1 约 0.5 分/千 token，倍率线性缩放。"
      + "卡片上的单价是按目录倍率推算的估值，实际以积分流水为准。"
      + "　生图：POST /v1/images/generations（模型 raccoon-image-gen，实测不扣分、单张 20-200s，4K 档 136-148s）。"
      + "实测上游生图接口忽略 model 字段：gpt-image-1/2/2.5、dall-e-2/3 只是客户端别名，"
      + "走的还是同一条通道、同一个模型；真正生效的是 size 与 quality"
      + "（省略 size 时上游给 1254×1254 而不是 1024，quality 默认 low）。"
      + "尺寸有两条上限——最长边 ≤3840、总像素 ≤3840×2160（方形最多 2880×2880），超了网关自动等比压，"
      + "并在响应头 X-XHX-Image-Size 标注实际提交的尺寸。"
      + "单账号并发上限 10（与对话共用），超出会 429，网关会自动等 2 秒原地重试。";

    const grid = $("#models-grid");
    let html = list.map(modelCard).join("");

    // 目录外的别名目标：上游目录里没有、但实测仍可调用（如 raccoon-chat-ml-5-5）。
    // 不列出来，用户看到「gpt-4o 映射到一个列表里不存在的模型」只会以为配错了。
    // 它们也要跟着搜索过滤，否则「搜个不存在的词还冒出一张卡」很奇怪。
    const orphans = (d.orphans || []).filter((o) => {
      if (f.freeOnly) return false; // 目录外没有倍率信息，不能声称免费
      if (!q) return true;
      return o.alias.toLowerCase().includes(q) || o.target.toLowerCase().includes(q);
    });
    if (orphans.length) {
      const byTarget = groupOrphans(orphans);
      html += Object.entries(byTarget).map(([target, aliases]) => `
        <div class="model-card ghost">
          <div class="mc-head">
            <button class="mc-title" data-copy="${esc(target)}" title="点击复制模型 ID">${esc(target)}</button>
            <div class="mc-price"><span class="tag warn">目录外</span></div>
          </div>
          <div class="mc-desc">上游目录里没有条目，但别名指向它——实测可调用。
            倍率未在目录中公布；同规模请求实测与 <code>raccoon-8c4485</code>（x1）扣分相同，按 x1 估算。</div>
          <div class="mc-aliases"><span class="hint">别名</span>${aliases.sort().map((a) =>
            `<button class="tag alias" data-copy="${esc(a)}">${esc(a)}</button>`).join("")}</div>
        </div>`).join("");
    }
    // 生图通道：上游 model_catalog 里没有它（只有对话模型），但确实能出图。
    // 不列出来，用户只会以为「这里不能生图」。
    const imageModels = d.image_models || [];
    const imgMatch = !q || "生图".includes(q) || imageModels.some((m) => m.toLowerCase().includes(q));
    if (imageModels.length && imgMatch && !f.freeOnly) {
      const [main, ...aliases] = imageModels;
      html += `
        <div class="model-card">
          <div class="mc-head">
            <button class="mc-title" data-copy="${esc(main)}" title="点击复制模型 ID">${esc(main)}</button>
            <div class="mc-price"><span class="tag ok">生图</span></div>
          </div>
          <div class="mc-desc">文生图 / 图生图通道：<code>POST /v1/images/generations</code>，
            默认返回 <code>url</code>（不传 response_format 就是 url；要 base64 传 <code>b64_json</code>）。
            实测 20-200s，不扣积分（不保证长期）。
            上游只有这一个模型组——<b>model 字段不参与路由</b>，真正生效的是 size 与 quality。</div>
          ${aliases.length ? `<div class="mc-aliases"><span class="hint">客户端别名</span>${aliases.map((a) =>
            `<button class="tag alias" data-copy="${esc(a)}">${esc(a)}</button>`).join("")}</div>` : ""}
        </div>`;
    }
    // 统计：前半段始终描述「目录里有什么」，筛选时再补一个「筛出几个」——
    // 否则筛完还是那串数字，看不出过滤生效没有。
    const freeCount = all.filter((m) => m.billing_status === "limited_free" || m.credits === "x0").length;
    const aliasCount = all.reduce((n, m) => n + (m.aliases || []).length, 0) + ((d.orphans || []).length);
    const unlistedCount = all.filter((m) => m.unlisted).length;
    const filtering = !!(q || f.freeOnly);
    $("#models-count").textContent = `${all.length} 个 · ${freeCount} 个免费`
      + (aliasCount ? ` · ${aliasCount} 个别名` : "")
      + (unlistedCount ? ` · ${unlistedCount} 个上游未列出` : "")
      + (filtering ? ` · 筛出 ${list.length + Object.keys(groupOrphans(orphans)).length} 个` : "");

    if (!html) {
      grid.innerHTML = `<p class="hint" style="padding:18px 0">${all.length ? "没有匹配的模型" : "目录为空（上游不可达？点「从上游刷新」重试）"}</p>`;
      return;
    }
    grid.innerHTML = html;

    grid.querySelectorAll("[data-copy]").forEach((el) => {
      el.addEventListener("click", async () => {
        const text = el.dataset.copy;
        try {
          await navigator.clipboard.writeText(text);
          toast("已复制 " + text, "ok");
        } catch (e) {
          prompt("复制这个模型名：", text);
        }
      });
    });
  }

  // ---------------------------------------------------------------------
  // 配置：表单 ⇄ JSON
  //
  // 表单是「按 schema 生成」的，不是手写 60 个 input——加字段只改这张表。
  // 保存时只提交表单里出现的键（服务端 merge），所以没被表单覆盖的高级键
  // 不会因为「保存一下」就丢掉。

  const CONFIG_SCHEMA = [
    {
      title: "基础",
      fields: [
        { path: "listen", label: "监听地址", hint: "如 0.0.0.0:7867", restart: true },
        { path: "api_key", label: "API Key", type: "password", hint: "空 = 不鉴权（仅限本机/私网）" },
        { path: "default_model", label: "默认模型", hint: "请求未带 model 时使用" },
        { path: "auth_dir", label: "账号目录", restart: true },
        { path: "state_file", label: "状态文件", restart: true },
      ],
    },
    {
      title: "账号池",
      fields: [
        { path: "pool.strategy", label: "选号策略", type: "select", options: [
          ["round_robin", "轮转（默认）"], ["least_in_flight", "最少在途"], ["priority", "优先级"], ["weighted", "加权"],
        ] },
        { path: "pool.max_in_flight", label: "单号在途上限", type: "number", hint: "上游单号硬上限 5（第 6 个起立即 429，跨模型共享）；别调高" },
        { path: "pool.queue_wait", label: "名额等待", hint: "全部号都满时最多等多久，如 2s；0 = 不等" },
        { path: "pool.breaker_threshold", label: "熔断阈值", type: "number", hint: "连续失败几次触发熔断" },
        { path: "pool.breaker_cooldown", label: "熔断冷却", hint: "如 10m" },
        { path: "pool.degrade_threshold", label: "降权阈值", type: "number" },
        { path: "pool.degrade_cooldown", label: "降权冷却", hint: "如 10m" },
      ],
    },
    {
      title: "冷却",
      fields: [
        { path: "cooldown.plan_credit", label: "额度不足冷却", hint: "如 12h；0 = 到次日 04:00" },
        { path: "cooldown.soft_rate", label: "限流冷却基数", hint: "如 60s，连续触发指数退避" },
        { path: "cooldown.soft_rate_max", label: "限流冷却封顶", hint: "如 2h" },
      ],
    },
    {
      title: "定时任务",
      fields: [
        { path: "schedule.checkin_enabled", label: "开启签到", type: "bool" },
        { path: "schedule.keepalive_enabled", label: "开启保活", type: "bool" },
        { path: "schedule.balance_refresh_enabled", label: "定时刷余额", type: "bool" },
        { path: "schedule.checkin_hours", label: "签到小时", type: "list", hint: "逗号分隔，如 9" },
        { path: "schedule.keepalive_hours", label: "保活小时", type: "list", hint: "默认 0-23 每小时检查一次；间隔必须小于 6h 续期窗口，收窄会漏号" },
        { path: "schedule.concurrency", label: "任务并发", type: "number" },
        { path: "schedule.balance_refresh_minutes", label: "刷余额间隔（分钟）", type: "number" },
        { path: "schedule.new_account_grace_days", label: "新号静默期（天）", type: "number" },
      ],
    },
    {
      title: "会话粘性",
      fields: [
        { path: "session_sticky.enabled", label: "启用", type: "bool" },
        { path: "session_sticky.ttl", label: "绑定有效期", restart: true, hint: "如 30m" },
        { path: "session_sticky.gc_interval", label: "清理间隔", restart: true, hint: "如 5m" },
        { path: "session_sticky.prefix_affinity", label: "前缀也粘性", type: "bool", hint: "同 system 的请求钉在一个号（多号池会退化）" },
      ],
    },
    {
      title: "服务",
      fields: [
        { path: "server.read_timeout", label: "请求读超时", restart: true, hint: "如 300s" },
        { path: "server.min_output_tokens", label: "max_tokens 下限", type: "number", hint: "思考 token 算在额度里，太小会正文回空（实测要 2000+）；负数 = 不抬升" },
      ],
    },
    {
      title: "上游",
      fields: [
        { path: "upstream.timeout_seconds", label: "短请求超时（秒）", type: "number", restart: true },
        { path: "upstream.header_timeout_seconds", label: "等响应头超时（秒）", type: "number", restart: true },
        { path: "upstream.idle_timeout_seconds", label: "流内空闲上限（秒）", type: "number", restart: true },
        { path: "upstream.user_agent", label: "User-Agent", restart: true, hint: "空 = 内置" },
        { path: "upstream.client_version", label: "客户端版本", restart: true, hint: "空 = 内置" },
        { path: "upstream.llm_base", label: "LLM 基址", restart: true, hint: "空 = 官方" },
        { path: "upstream.auth_base", label: "认证基址", restart: true, hint: "空 = 官方" },
        { path: "upstream.points_base", label: "积分基址", restart: true, hint: "空 = 官方" },
        { path: "upstream.origin", label: "站点 Origin", restart: true, hint: "空 = 官方" },
        { path: "upstream.proxy.enabled", label: "按账号走代理", type: "bool", hint: "每个账号固定一个出口，避免同 IP 批量查积分被风控" },
        { path: "upstream.proxy.template", label: "代理模板", wide: true, hint: "socks5h://user-{account}:pass@host:port；{account} 换成 uid" },
        { path: "upstream.proxy.scope", label: "代理作用域", hint: "points = 只积分域走代理（推荐）；all = 全部走" },
        { path: "upstream.proxy.egress_ttl_minutes", label: "出口 IP 缓存（分钟）", type: "number" },
        { path: "upstream.proxy.egress_check_url", label: "出口探测地址", hint: "空 = 内置兜底列表" },
      ],
    },
    {
      title: "登录与导入",
      fields: [
        { path: "login.enabled", label: "允许浏览器授权", type: "bool" },
        { path: "login.app_name", label: "授权页客户端名" },
        { path: "login.callback_base", label: "回调基址", hint: "本机留空；线上填 https://你的域名" },
        { path: "import.client_dirs", label: "额外扫描目录", type: "lines", wide: true, hint: "每行一个路径（文件或目录），用于「客户端目录」扫描" },
      ],
    },
    {
      title: "请求归档",
      fields: [
        { path: "logging.request_archive_enabled", label: "记录请求", type: "bool" },
        { path: "logging.request_retention_days", label: "保留天数", type: "number" },
        { path: "logging.request_archive_max_mb", label: "上限（MB）", type: "number" },
        { path: "logging.request_client_info", label: "记录客户端信息", type: "bool" },
      ],
    },
    {
      title: "生图",
      fields: [
        // 这一组的默认值都在代码里，配置里没有对应键时表单必须显示「默认的样子」，
        // 否则用户什么都没改、一保存就把生图关掉了（undefined 当 false 是最常见的坑）。
        { path: "images.enabled", label: "开启生图通道", type: "bool", defaultTrue: true, restart: true, hint: "默认开；关掉后 /v1/images/generations 直接拒绝" },
        { path: "images.timeout_seconds", label: "单张超时（秒）", type: "number", restart: true, hint: "实测 49-267s（267s 是 4096x4096 压到 2880 那次），默认 600" },
        // 没有「上游图片模型」：上游一条通道只有一个模型组、model 被忽略，
        // 生图只按端点路由（客户端填什么都出同一张模型的图，见 /v1/models 的别名）。
        {
          path: "images.aliases", label: "图片模型别名", type: "lines", wide: true, omitIfEmpty: true,
          hint: "一行一个，默认 gpt-image-1 / gpt-image-1-mini / gpt-image-2 / gpt-image-2.5 / dall-e-2 / dall-e-3。"
            + "实测上游生图忽略 model，别名只是让客户端能选中；留空 = 用默认表，要彻底禁用请到 JSON 视图写 []",
        },
        {
          path: "images.response_format", label: "默认返回形态", type: "select", restart: true,
          options: [["url", "url（只回链接，默认）"], ["b64_json", "b64_json（只回 base64）"], ["both", "both（链接 + base64）"]],
          hint: "客户端没传 response_format（或传 null/None）时用这个；显式传了以客户端为准",
        },
        {
          path: "images.return_url", label: "提供可访问 url", type: "bool", defaultTrue: true, restart: true,
          hint: "默认开：图落盘/传对象存储并提供 /v1/images/<id>。关掉后只能回 base64，点名要 url 会 400",
        },
        { path: "images.public_base", label: "url 对外基址", restart: true, hint: "线上填 https://你的域名；空 = 按请求 Host 推导" },
        { path: "images.dir", label: "图片落盘目录", restart: true, hint: "空 = state 同目录 images/" },
        {
          path: "images.max_total_mb", label: "图片目录上限（MB）", type: "number", restart: true, omitIfEmpty: true,
          hint: "默认 2048，0 = 不限制。超限按修改时间删最旧的（旧链接会 404）",
        },
      ],
    },
    {
      title: "模型映射",
      fields: [
        { path: "models.hidden", label: "隐藏模型", type: "lines", wide: true, hint: "每行一个模型 id：不列出、点名也拒绝" },
        { path: "models.include_upstream_hidden", label: "列出上游未展示的模型", type: "bool", defaultTrue: true, restart: true, hint: "上游 visible=false 的模型仍可调用（默认模型就在其中）；关掉则 /v1/models 只列它官网展示的" },
        { path: "aliases", label: "别名（通用名=上游 id）", type: "map", wide: true, hint: "每行一条，如 gpt-4o=raccoon-chat-ml-5-5" },
      ],
    },
  ];

  // 字段定义索引：collectConfigForm 需要知道某个 path 的 omitIfEmpty 之类的元信息。
  const CONFIG_FIELDS = (() => {
    const m = {};
    CONFIG_SCHEMA.forEach((sec) => sec.fields.forEach((f) => { m[f.path] = f; }));
    return m;
  })();

  function getPath(obj, path) {
    return path.split(".").reduce((o, k) => (o == null ? undefined : o[k]), obj);
  }

  function setPath(obj, path, value) {
    const keys = path.split(".");
    let cur = obj;
    for (let i = 0; i < keys.length - 1; i++) {
      if (typeof cur[keys[i]] !== "object" || cur[keys[i]] === null) cur[keys[i]] = {};
      cur = cur[keys[i]];
    }
    cur[keys[keys.length - 1]] = value;
  }

  function fieldHTML(f, value) {
    const id = "cfg-" + f.path.replace(/\./g, "-");
    const label = `<label class="cfg-label" for="${id}">${esc(f.label)}${f.restart ? ' <span class="tag warn">需重启</span>' : ""}${f.hint ? `<code>${esc(f.path)}</code>` : ""}</label>`;
    const hint = f.hint ? `<span class="hint">${esc(f.hint)}</span>` : "";
    const wide = f.wide ? " wide" : "";
    if (f.type === "bool") {
      // 配置里没这个键时，用 schema 里的默认值决定勾选状态：
      // 一律按 false 渲染会让「默认开」的开关一保存就被关掉。
      const checked = value == null ? !!f.defaultTrue : !!value;
      return `<div class="cfg-row${wide}">${label}<div class="cfg-bool"><input type="checkbox" id="${id}" data-path="${f.path}" data-type="bool" ${checked ? "checked" : ""}></div>${hint}</div>`;
    }
    if (f.type === "select") {
      const opts = f.options.map(([v, t]) => `<option value="${v}" ${v === value ? "selected" : ""}>${esc(t)}</option>`).join("");
      return `<div class="cfg-row${wide}">${label}<select id="${id}" data-path="${f.path}" data-type="text">${opts}</select>${hint}</div>`;
    }
    if (f.type === "number") {
      return `<div class="cfg-row${wide}">${label}<input type="number" id="${id}" data-path="${f.path}" data-type="number" value="${value == null ? "" : esc(String(value))}">${hint}</div>`;
    }
    if (f.type === "list") {
      return `<div class="cfg-row${wide}">${label}<input type="text" id="${id}" data-path="${f.path}" data-type="list" value="${esc((value || []).join(","))}">${hint}</div>`;
    }
    if (f.type === "lines") {
      return `<div class="cfg-row${wide}">${label}<textarea id="${id}" data-path="${f.path}" data-type="lines" spellcheck="false">${esc((value || []).join("\n"))}</textarea>${hint}</div>`;
    }
    if (f.type === "map") {
      const rows = Object.entries(value || {}).map(([k, v]) => k + "=" + v).join("\n");
      return `<div class="cfg-row${wide}">${label}<textarea id="${id}" data-path="${f.path}" data-type="map" spellcheck="false">${esc(rows)}</textarea>${hint}</div>`;
    }
    return `<div class="cfg-row${wide}">${label}<input type="${f.type === "password" ? "password" : "text"}" id="${id}" data-path="${f.path}" data-type="text" value="${value == null ? "" : esc(String(value))}">${hint}</div>`;
  }

  function renderConfigForm() {
    const cfg = (state.config && state.config.config) || {};
    $("#config-form").innerHTML = CONFIG_SCHEMA.map((sec) => `
      <div class="cfg-section">
        <h3>${esc(sec.title)}</h3>
        <div class="cfg-grid">${sec.fields.map((f) => fieldHTML(f, getPath(cfg, f.path))).join("")}</div>
      </div>`).join("");
  }

  // collectConfigForm 只收集表单里出现的键——服务端会 merge，没列出来的保持原样。
  function collectConfigForm() {
    const out = {};
    $$("#config-form [data-path]").forEach((el) => {
      const path = el.dataset.path;
      const type = el.dataset.type;
      let v;
      if (type === "bool") v = el.checked;
      else if (type === "number") {
        // omitIfEmpty 的数字字段：留空表示「用默认值」，不能当成 0 下发。
        // max_total_mb 的 0 是「不限制」，空值当 0 会静默取消上限。
        if (el.value === "" && CONFIG_FIELDS[path] && CONFIG_FIELDS[path].omitIfEmpty) return;
        v = el.value === "" ? 0 : Number(el.value);
      }
      else if (type === "list") {
        v = el.value.split(",").map((x) => x.trim()).filter(Boolean).map((x) => (isNaN(Number(x)) ? x : Number(x)));
      } else if (type === "lines") {
        v = el.value.split("\n").map((x) => x.trim()).filter(Boolean);
      } else if (type === "map") {
        v = {};
        el.value.split("\n").forEach((line) => {
          const i = line.indexOf("=");
          if (i > 0) v[line.slice(0, i).trim()] = line.slice(i + 1).trim();
        });
      } else v = el.value;
      // omitIfEmpty：留空时**不下发**这个键，让服务端 merge 后保留原有值/默认值。
      // 否则空数组会把「默认别名表」硬覆盖成 []，等于静默关掉功能。
      if ((type === "list" || type === "lines") && Array.isArray(v) && v.length === 0
        && CONFIG_FIELDS[path] && CONFIG_FIELDS[path].omitIfEmpty) {
        return;
      }
      setPath(out, path, v);
    });
    return out;
  }

  function deepMerge(base, patch) {
    const out = JSON.parse(JSON.stringify(base || {}));
    for (const [k, v] of Object.entries(patch || {})) {
      if (v && typeof v === "object" && !Array.isArray(v) && out[k] && typeof out[k] === "object" && !Array.isArray(out[k])) {
        out[k] = deepMerge(out[k], v);
      } else {
        out[k] = v;
      }
    }
    return out;
  }

  function switchConfigView(view) {
    const editor = $("#config-editor");
    if (view === state.configView) return;
    if (view === "json") {
      // 表单 → JSON：把表单值合进当前配置，方便在 JSON 里接着改高级键。
      const merged = deepMerge((state.config && state.config.config) || {}, collectConfigForm());
      editor.value = JSON.stringify(merged, null, 2);
      editor.hidden = false;
      $("#config-form").hidden = true;
    } else {
      let parsed;
      try {
        parsed = JSON.parse(editor.value);
      } catch (e) {
        toast("JSON 语法错误，先修好再切回表单", "bad");
        return;
      }
      state.config = Object.assign({}, state.config, { config: parsed });
      renderConfigForm();
      editor.hidden = true;
      $("#config-form").hidden = false;
    }
    state.configView = view;
    $$("#config-view .seg-btn").forEach((b) => b.classList.toggle("active", b.dataset.view === view));
  }

  $("#config-view").addEventListener("click", (e) => {
    const btn = e.target.closest(".seg-btn");
    if (btn) switchConfigView(btn.dataset.view);
  });

  // ---------------------------------------------------------------------
  // 渲染：请求

  // C2PA 签名者标签：同一个模型名在上游会在 Azure/OpenAI 之间漂移
  // （raccoon-image-gen 是 LiteLLM model group）。面板上直接暴露「这次落到谁」，
  // 否则只能事后扫图猜分布。空值代表这次响应没有凭证，与「未知」不是一回事。
  function c2paTag(s) {
    if (!s) return `<span class="muted" title="这次响应没有 C2PA 溯源凭证">—</span>`;
    if (s === "mixed") return `<span class="tag warn" title="同一次响应里多张图来自不同后端">mixed</span>`;
    const short = s.includes("OpenAI") ? "OpenAI" : s.includes("Microsoft") ? "Azure" : s;
    return `<span class="tag ${short === "OpenAI" ? "blue" : ""}" title="${esc(s)}">${esc(short)}</span>`;
  }

  function renderRequests() {
    const d = state.requests;
    if (!d) return;
    const m = d.metrics || {};
    const cards = [
      { label: "请求总数", value: fmtNum(m.total || 0) },
      { label: "成功", value: fmtNum(m.success || 0), cls: "ok" },
      { label: "失败", value: fmtNum(m.failure || 0), cls: m.failure ? "bad" : "" },
      { label: "P50", value: fmtDur(m.latency_p50_ms) },
      { label: "P95", value: fmtDur(m.latency_p95_ms) },
      { label: "在途", value: m.in_flight || 0, cls: "accent" },
    ];
    $("#req-cards").innerHTML = cards.map((c) => `
      <div class="card ${c.cls || ""}"><div class="label">${esc(c.label)}</div>
      <div class="value">${esc(c.value)}</div></div>`).join("");

    const tbody = $("#requests-table tbody");
    const events = d.events || [];
    if (!events.length) {
      tbody.innerHTML = `<tr><td colspan="10" class="muted" style="padding:22px;text-align:center">还没有请求记录</td></tr>`;
      return;
    }
    tbody.innerHTML = events.map((e) => {
      const statusCls = e.status >= 400 || e.error_kind ? "bad" : "ok";
      const proto = { chat: "OpenAI", messages: "Anthropic", responses: "Responses", images: "生图" }[e.protocol] || e.protocol || "-";
      // 上游没回 usage 时三个 token 都是 0，但那是「没记账」不是「没消耗」。
      // 显示 0 会被读成后者，还会把总量和均值一起拉低，所以单独标成 ?。
      const tokens = e.usage_missing
        ? `<span class="muted" title="上游未返回 usage，这一条的用量未知">?</span>`
        : fmtNum(e.total_tokens || e.total || 0);
      return `<tr>
        <td class="nowrap">${fmtTime(e.time)}</td>
        <td><span class="tag blue">${esc(proto)}</span></td>
        <td class="mono">${esc(e.model || "-")}</td>
        <td>${esc(e.account || e.uid || "-")}</td>
        <td><span class="tag ${statusCls}">${e.status}</span></td>
        <td class="num">${fmtDur(e.duration_ms)}</td>
        <td class="num">${fmtDur(e.ttfb_ms)}</td>
        <td class="num">${tokens}</td>
        <td>${c2paTag(e.c2pa_signer)}</td>
        <td>${e.error_kind ? `<span class="err-text" title="${esc(e.error || "")}">${esc(e.error_kind)}</span>` : (e.stream ? `<span class="tag">流式</span>` : "")}</td>
      </tr>`;
    }).join("");
  }

  // ---------------------------------------------------------------------
  // 渲染：用量

  function renderUsage() {
    const d = state.usage || {};
    const life = d.lifetime || {};
    const kv = [
      ["请求总数", fmtNum(life.requests || 0)],
      ["失败", fmtNum(life.failures || 0)],
      ["输入 tokens", fmtNum(life.prompt_tokens || 0)],
      ["输出 tokens", fmtNum(life.completion_tokens || 0)],
      ["总 tokens", fmtNum(life.total_tokens || 0)],
    ];
    $("#usage-total").innerHTML = kv.map(([k, v]) =>
      `<div class="kv"><span class="k">${esc(k)}</span><span class="v">${esc(v)}</span></div>`).join("");

    const accounts = d.accounts || [];
    $("#usage-accounts tbody").innerHTML = accounts.length ? accounts.map((a) => `<tr>
      <td>${esc(a.account || a.uid)}</td>
      <td class="num">${fmtNum(a.requests)}</td>
      <td class="num">${fmtNum(a.failures)}</td>
      <td class="num">${fmtNum(a.prompt_tokens)}</td>
      <td class="num">${fmtNum(a.completion_tokens)}</td>
      <td class="nowrap">${fmtTime(a.last_used)}</td>
    </tr>`).join("") : `<tr><td colspan="6" class="muted" style="padding:18px;text-align:center">暂无数据</td></tr>`;

    const days = (d.days || []).slice().reverse();
    $("#usage-days tbody").innerHTML = days.length ? days.map((x) => `<tr>
      <td class="mono">${esc(x.date)}</td>
      <td class="num">${fmtNum(x.requests)}</td>
      <td class="num">${fmtNum(x.failures)}</td>
      <td class="num">${fmtNum(x.prompt_tokens)}</td>
      <td class="num">${fmtNum(x.completion_tokens)}</td>
    </tr>`).join("") : `<tr><td colspan="5" class="muted" style="padding:18px;text-align:center">暂无数据</td></tr>`;
  }

  // ---------------------------------------------------------------------
  // 渲染：日志

  function renderLogs() {
    const box = $("#logbox");
    const atBottom = box.scrollTop + box.clientHeight >= box.scrollHeight - 40;
    box.innerHTML = state.logs.map((l) =>
      `<div><span class="lt">${esc(l.time)}</span>${esc(l.text)}</div>`).join("");
    if (atBottom) box.scrollTop = box.scrollHeight;
  }

  // ---------------------------------------------------------------------
  // 事件：标签页

  // 顶栏标题与副标题：切页时换掉，side nav 只给一个词，顶栏说清「现在在哪」。
  const TAB_TITLES = {
    accounts: ["账号池", "账号健康度、积分与调度"],
    models: ["模型", "上游目录、别名与单价"],
    requests: ["请求", "最近 100 条请求与耗时"],
    points: ["积分", "发放制额度：每日 300 与三笔一次性奖励"],
    usage: ["用量", "按账号 / 按日累计"],
    logs: ["运行日志", "进程实时输出"],
    config: ["配置", "热改字段与 JSON 全量编辑"],
  };

  // activateTab 切换标签并加载该页数据。#models 这类 hash 可直接定位（方便
  // 分享「去看配置」这种链接，也让无头截图能逐页验证）。
  async function activateTab(tab, updateHash) {
    const btn = $(`.tab[data-tab="${tab}"]`);
    if (!btn) tab = "accounts";
    state.tab = tab;
    $$(".tab").forEach((t) => t.classList.toggle("active", t.dataset.tab === tab));
    $$(".pane").forEach((p) => p.classList.toggle("active", p.id === "pane-" + tab));
    const meta = TAB_TITLES[tab] || ["控制台", ""];
    const ttl = $("#ttl");
    if (ttl) ttl.textContent = meta[0];
    const sub = $("#subMeta");
    // 账号页的副标题由 renderCards 写实时数据；其他页写静态说明。
    if (sub && tab !== "accounts") sub.textContent = meta[1];
    if (updateHash && location.hash.slice(1) !== tab) {
      history.replaceState(null, "", "#" + tab);
    }
    setNav(false); // 手机端：点完导航就收起抽屉
    try {
      if (tab === "models") await loadModels();
      if (tab === "requests") await loadRequests();
      if (tab === "points") await loadPoints();
      if (tab === "usage") await loadUsage();
      if (tab === "logs") await loadLogs();
      if (tab === "config") await loadConfig();
      if (tab === "accounts") await loadOverview();
    } catch (e2) {
      console.error("[panel] tab=" + tab + " failed:", e2 && e2.stack || e2);
      if (e2.message !== "unauthorized") toast(e2.message, "bad");
    }
  }

  $("#tabs").addEventListener("click", (e) => {
    const btn = e.target.closest(".tab");
    if (!btn) return;
    e.preventDefault();
    activateTab(btn.dataset.tab, true);
  });

  window.addEventListener("hashchange", () => {
    const tab = location.hash.slice(1);
    if (tab && tab !== state.tab) activateTab(tab, false);
  });

  // ---------------------------------------------------------------------
  // 事件：账号表工具栏（搜索 / 筛选 / 排序 / 分页）
  //
  // 全部只改 state.accView 再重绘，不发请求。改任何一项都把页码重置到 1：
  // 否则在「第 3 页」改筛选会落到一个可能已经不存在的位置。

  function setAccFilter(patch) {
    Object.assign(state.accView, patch);
    if (!("page" in patch)) state.accView.page = 1;
    renderAccounts();
  }

  let accQTimer = null;
  $("#accQ").addEventListener("input", (e) => {
    const val = e.target.value;
    const box = $("#accSearch");
    if (box) box.classList.toggle("has-q", !!val);
    // 90ms 防抖：账号列表只有几十行，但每敲一个字符重绘一次没必要。
    clearTimeout(accQTimer);
    accQTimer = setTimeout(() => setAccFilter({ q: val.trim() }), 90);
  });

  $("#accQClear").addEventListener("click", () => {
    const q = $("#accQ");
    q.value = "";
    q.dispatchEvent(new Event("input"));
    q.focus();
  });

  function onStatusChip(e) {
    const chip = e.target.closest("[data-status]");
    if (!chip) return;
    setAccFilter({ status: chip.dataset.status });
  }
  $("#accStatusChips").addEventListener("click", onStatusChip);
  // 异常汇总条里的「查看禁用 N」也是同一套 data-status，直接复用。
  $("#alertChips").addEventListener("click", onStatusChip);

  $("#accSort").addEventListener("change", (e) => setAccFilter({ sort: e.target.value }));
  $("#accSize").addEventListener("change", (e) => setAccFilter({ size: Number(e.target.value) || 0 }));
  $("#accPrev").addEventListener("click", () => {
    if (state.accView.page > 1) { state.accView.page--; renderAccounts(); }
  });
  $("#accNext").addEventListener("click", () => {
    state.accView.page++;
    renderAccounts();
  });

  // 「清除筛选」按钮长在空态文案里（动态生成），只能走文档级委托。
  document.addEventListener("click", (e) => {
    if (!e.target.closest("#accClearFilter")) return;
    const q = $("#accQ");
    if (q) q.value = "";
    const box = $("#accSearch");
    if (box) box.classList.remove("has-q");
    setAccFilter({ q: "", status: "all" });
  });

  // ---------------------------------------------------------------------
  // 事件：主题 / 手机端抽屉

  $("#btnTheme").addEventListener("click", () => {
    const next = THEME_ORDER[(THEME_ORDER.indexOf(themePref()) + 1) % THEME_ORDER.length];
    localStorage.setItem(THEME_KEY, next);
    applyTheme();
    toast("主题：" + THEME_LABEL[next], "ok");
  });

  // 跟随系统时，系统切换（macOS 日落变深色）要立刻生效；手动选定则忽略。
  const mqLight = matchMedia("(prefers-color-scheme: light)");
  if (mqLight.addEventListener) {
    mqLight.addEventListener("change", () => {
      if (themePref() === "auto") applyTheme();
    });
  }

  $("#navBtn").addEventListener("click", () => {
    const drawer = $("#navDrawer");
    setNav(!(drawer && drawer.classList.contains("open")));
  });
  $("#navVeil").addEventListener("click", () => setNav(false));
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") setNav(false);
  });

  // ---------------------------------------------------------------------
  // 事件：账号操作

  $("#accounts-table").addEventListener("click", async (e) => {
    const btn = e.target.closest("button[data-act]");
    if (!btn) return;
    const uid = btn.dataset.uid;
    const act = btn.dataset.act;
    if (act === "remove" && !confirm(`确定移除账号 ${uid}？凭证会备份到 auths/removed/`)) return;
    btn.disabled = true;
    try {
      if (act === "checkin") {
        const r = await api(`/panel/api/accounts/${encodeURIComponent(uid)}/checkin`, { method: "POST" });
        // 三档而不是两档：unbound / pending 是「上游的状态」，不是这次操作失败。
        // 全用绿色会显得「签到成功」；全用红色又会让人来查网关。
        const kind = r.status === "failed" ? "bad" : (r.status === "ok" || r.status === "already") ? "ok" : "warn";
        toast(`签到：${r.status} — ${r.message}`, kind);
      } else if (act === "balance") {
        const r = await api(`/panel/api/accounts/${encodeURIComponent(uid)}/balance`, { method: "POST" });
        toast(`余额 ${fmtNum(r.remain)} 分（每日 ${fmtNum(r.daily)} / 奖励 ${fmtNum(r.reward)}）`, "ok");
      } else if (act === "remove") {
        await api(`/panel/api/accounts/${encodeURIComponent(uid)}/remove`, { method: "POST" });
        toast("已移除", "ok");
      } else {
        await api(`/panel/api/accounts/${encodeURIComponent(uid)}/${act}`, { method: "POST" });
        toast("已执行", "ok");
      }
      await loadOverview();
    } catch (err) {
      if (err.message !== "unauthorized") toast(err.message, "bad");
    } finally {
      btn.disabled = false;
    }
  });

  // 批量任务
  $$("[data-action]").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const action = btn.dataset.action;
      const names = { checkin_all: "全部签到", keepalive_all: "保活续期", balance_all: "刷新余额", bind_device_all: "补绑可信设备" };
      if (!confirm(`执行「${names[action] || action}」？账号多时可能需要一两分钟。`)) return;
      btn.disabled = true;
      const old = btn.textContent;
      btn.textContent = "执行中…";
      try {
        // 补绑设备的返回是 {ok, results[]}，没有 skipped/failed 计数，
        // 单独走一条分支，免得把「跳过 0 / 失败 0」当成什么都没做。
        if (action === "bind_device_all") {
          const r = await api("/panel/api/accounts/bind-device-all", { method: "POST" });
          const list = r.results || [];
          const skipped = list.filter((x) => x.skipped).length;
          const failed = list.filter((x) => x.error).length;
          // 「此前已绑定」也计入成功：上游侧本来就是 bound，
          // 这次只是把本地状态补上，不该让用户以为失败了。
          const already = list.filter((x) => x.note).length;
          const extra = already ? `（${already} 个此前已绑定，已同步本地状态）` : "";
          toast(`补绑完成：成功 ${r.ok || 0} / 已绑跳过 ${skipped} / 失败 ${failed}${extra}`, failed ? "warn" : "ok");
          if (failed) console.warn("补绑失败详情:", list.filter((x) => x.error));
          await loadOverview();
          return;
        }
        const r = await api(`/panel/api/${action}`, { method: "POST" });
        toast(`完成：成功 ${r.ok || 0} / 跳过 ${r.skipped || 0} / 失败 ${r.failed || 0}`, r.failed ? "warn" : "ok");
        await loadOverview();
      } catch (e) {
        if (e.message !== "unauthorized") toast(e.message, "bad");
      } finally {
        btn.disabled = false;
        btn.textContent = old;
      }
    });
  });

  // ---------------------------------------------------------------------
  // 事件：模型 / 用量 / 配置

  $("#btn-models-refresh").addEventListener("click", async (e) => {
    const btn = e.currentTarget;
    btn.disabled = true;
    btn.textContent = "刷新中…";
    try {
      await api("/panel/api/models/refresh", { method: "POST" });
      await loadModels();
      toast("目录已刷新", "ok");
    } catch (err) {
      if (err.message !== "unauthorized") toast(err.message, "bad");
    } finally {
      btn.disabled = false;
      btn.textContent = "从上游刷新";
    }
  });

  $("#btn-usage-reset").addEventListener("click", async () => {
    if (!confirm("重置用量统计？历史归档不受影响。")) return;
    try {
      await api("/panel/api/usage/reset", { method: "POST" });
      await loadUsage();
      toast("已重置", "ok");
    } catch (e) {
      toast(e.message, "bad");
    }
  });

  $("#btn-config-reload").addEventListener("click", async () => {
    try { await loadConfig(); toast("已重新读取", "ok"); } catch (e) { toast(e.message, "bad"); }
  });

  $("#btn-config-save").addEventListener("click", async () => {
    let payload;
    // JSON 视图里编辑的是整份配置，删掉的行必须真的删掉 ⇒ 以提交内容为准；
    // 表单视图只提交它管的键 ⇒ 走合并，别把没用表单覆盖的键洗掉。
    let url = "/panel/api/config";
    if (state.configView === "json") {
      try {
        payload = JSON.parse($("#config-editor").value);
      } catch (e) {
        $("#config-msg").textContent = "JSON 语法错误：" + e.message;
        return;
      }
      url += "?mode=replace";
    } else {
      payload = collectConfigForm();
    }
    const btn = $("#btn-config-save");
    btn.disabled = true;
    try {
      const r = await api(url, { method: "POST", body: payload });
      const need = (r.restart_required || []).filter(Boolean);
      $("#config-msg").textContent = need.length
        ? "已保存。以下字段需要重启进程才生效：" + need.join("、")
        : "已保存并热生效。";
      toast("配置已保存", "ok");
      // 重新拉一次：服务端补过默认值的字段要在表单里体现出来。
      await loadConfig();
      await loadOverview();
    } catch (e) {
      $("#config-msg").textContent = e.message;
      toast(e.message, "bad");
    } finally {
      btn.disabled = false;
    }
  });

  $("#models-search").addEventListener("input", (e) => {
    state.modelsFilter.q = e.target.value;
    renderModels();
  });
  $("#models-free-only").addEventListener("change", (e) => {
    state.modelsFilter.freeOnly = e.target.checked;
    renderModels();
  });

  // ---------------------------------------------------------------------
  // 事件：导入弹层

  function openModal(id) { $("#" + id).hidden = false; }
  function closeModal(id) {
    $("#" + id).hidden = true;
    if (id === "modal-import") stopLoginTimers();
  }

  $$("[data-close]").forEach((btn) =>
    btn.addEventListener("click", () => closeModal(btn.dataset.close)));

  $$(".modal").forEach((m) =>
    m.addEventListener("click", (e) => { if (e.target === m && m.id === "modal-import") closeModal(m.id); }));

  $("#btn-open-import").addEventListener("click", () => {
    openModal("modal-import");
    switchImportMode("paste");
  });

  function switchImportMode(mode) {
    $$("#import-tabs .mtab").forEach((b) => b.classList.toggle("active", b.dataset.mode === mode));
    $$("#modal-import .mode").forEach((m) => m.classList.toggle("active", m.id === "mode-" + mode));
    if (mode === "qr") startQr();
    if (mode === "device") renderCallbackBase();
    if (mode === "client") scanClient();
    if (mode === "sms") prepareSms();
  }

  // ---------------------------------------------------------------- 积分来源
  //
  // 上游没有「做任务换积分」这套（官网定价页：免费版 300 积分/日，0 点重置），
  // 能自动化的只有两笔一次性奖励的领取，以及每日到账的核对。
  async function loadPoints() {
    const tbody = $("#points-table tbody");
    tbody.innerHTML = `<tr><td colspan="10" class="hint">读取中…</td></tr>`;
    try {
      const r = await api("/panel/api/points");
      const rows = r.accounts || [];
      if (!rows.length) {
        tbody.innerHTML = `<tr><td colspan="10" class="hint">还没有账号</td></tr>`;
        return;
      }
      tbody.innerHTML = rows.map((row) => {
        if (!row.audit) {
          return `<tr><td>${esc(row.nickname || row.uid)}</td><td colspan="9" class="hint bad">${esc(row.error || "读取失败")}</td></tr>`;
        }
        const a = row.audit;
        const src = (key) => (a.sources || []).find((s) => s.key === key) || {};
        const daily = src("daily");
        const newUser = src("new_user");
        const desktop = src("desktop_login");
        const mobile = src("mobile_login");
        const dailyOK = !!(daily.claimed && (a.daily > 0 || (daily.last_at || "").slice(0, 10) === new Date().toISOString().slice(0, 10)));
        const expire = a.expire ? new Date(a.expire * 1000).toLocaleString("zh-CN") : "不过期";
        const canClaim = !desktop.claimed || !mobile.claimed;
        const burn = a.spent_7d ? `<span class="pill bad">-${a.spent_7d}</span>` : '<span class="hint">—</span>';
        const left = a.days_left < 0
          ? '<span class="hint">不消耗</span>'
          : `<b>${Math.floor(a.days_left)}</b> <span class="hint">天</span>`;
        return `<tr>
          <td>${esc(row.nickname || row.uid)}<div class="hint">#${esc(row.uid)}</div></td>
          <td><b>${a.remain}</b></td>
          <td>${burn}${a.spent_today ? `<div class="hint">今日 -${a.spent_today}</div>` : ""}</td>
          <td>${left}</td>
          <td>${dailyOK ? '<span class="pill ok">已到账</span>' : '<span class="pill warn">未到账</span>'}
            <div class="hint">${esc(a.note || "")}</div></td>
          <td>${newUser.claimed ? `<span class="pill ok">+${newUser.points}</span>` : '<span class="pill warn">未领</span>'}</td>
          <td>${desktop.claimed ? `<span class="pill ok">+${desktop.points}</span>` : '<span class="pill warn">可补领</span>'}</td>
          <td>${mobile.claimed ? `<span class="pill ok">+${mobile.points}</span>` : '<span class="pill warn">可补领</span>'}</td>
          <td class="hint">${esc(expire)}</td>
          <td>${canClaim ? `<button class="btn" data-claim="${esc(row.uid)}">补领</button>` : ""}</td>
        </tr>`;
      }).join("");
      $("#points-note").textContent = "「补领」调的是上游的一次性奖励接口，幂等：领过会返回 granted=false，不会重复加分。";
      tbody.querySelectorAll("[data-claim]").forEach((btn) => {
        btn.addEventListener("click", async () => {
          const uid = btn.dataset.claim;
          btn.disabled = true;
          btn.textContent = "补领中…";
          try {
            const res = await api(`/panel/api/accounts/${encodeURIComponent(uid)}/points/claim`, { method: "POST" });
            const got = (res.desktop_granted ? 3000 : 0) + (res.mobile_granted ? 3000 : 0);
            toast(got ? `补领成功 +${got}` : "没有可补领的奖励（已领过）", got ? "ok" : "warn");
            await loadPoints();
            await loadOverview();
          } catch (err) {
            toast(err.message, "bad");
            btn.disabled = false;
            btn.textContent = "补领";
          }
        });
      });
    } catch (e) {
      if (e.message !== "unauthorized") tbody.innerHTML = `<tr><td colspan="10" class="hint bad">${esc(e.message)}</td></tr>`;
    }
  }

  $("#btn-points-refresh").addEventListener("click", loadPoints);

  // ---------------------------------------------------------------- 客户端目录
  //
  // 桌面端把票明文写在 Application Support 里，直接读出来比手工挖 token 靠谱。
  // 扫描是只读的；导入前会拿票打一次 user_info（幂等 GET），既验活也把
  // IAM 主体（pu-…）换成 office 域数字 uid——不换的话同一个号会出现两行。
  async function scanClient() {
    const status = $("#client-status");
    const list = $("#client-list");
    status.textContent = "扫描中…";
    list.innerHTML = "";
    $("#btn-client-import").disabled = true;
    try {
      const r = await api("/panel/api/accounts/scan");
      state.scan = (r.candidates || []).filter((c) => !c.error && c.uid);
      renderScanList(r);
      $("#btn-client-import").disabled = state.scan.length === 0;
      status.textContent = state.scan.length
        ? `找到 ${state.scan.length} 个可用凭据`
        : `没找到可用凭据（已查 ${(r.locations || []).length} 个位置）`;
    } catch (e) {
      if (e.message !== "unauthorized") toast(e.message, "bad");
      status.textContent = "扫描失败：" + e.message;
    }
  }

  function renderScanList(r) {
    const list = $("#client-list");
    const usable = (r.candidates || []).filter((c) => !c.error && c.uid);
    const broken = (r.candidates || []).filter((c) => c.error || !c.uid);
    $("#client-empty").hidden = usable.length + broken.length > 0;
    const parts = [];
    for (const c of usable) {
      const exp = c.expires_at ? new Date(c.expires_at).toLocaleDateString("zh-CN") : "未知";
      const tags = [];
      if (c.already_imported) tags.push("已在池（会更新票）");
      if (c.note) tags.push(c.note);
      parts.push(`
        <label class="scan-item picked">
          <input type="checkbox" class="scan-pick" value="${esc(c.path)}" checked>
          <div class="body">
            <div class="who">${esc(c.nickname || c.uid)} <span class="hint">#${esc(c.uid)}</span></div>
            <div class="path" title="${esc(c.path)}">${esc(c.path)}</div>
            <div class="meta">${c.phone ? "手机 " + esc(c.phone) + " · " : ""}到期 ${esc(exp)}${tags.length ? " · " + esc(tags.join(" · ")) : ""}</div>
          </div>
        </label>`);
    }
    for (const c of broken) {
      parts.push(`
        <div class="scan-item">
          <div class="body">
            <div class="who bad">读不出来</div>
            <div class="path" title="${esc(c.path)}">${esc(c.path)}</div>
            <div class="meta bad">${esc(c.error || c.verify_error || "未知错误")}</div>
          </div>
        </div>`);
    }
    if (!usable.length && !broken.length) {
      const paths = (r.locations || []).map((l) => (l.exists ? "✓ " : "· ") + l.path).join("\n");
      parts.push(`<div class="scan-item"><div class="body">
        <div class="meta">已检查以下位置（✓ = 文件存在）：</div>
        <div class="path" style="white-space:pre-wrap;white-space:pre-line">${esc(paths)}</div>
      </div></div>`);
    }
    list.innerHTML = parts.join("");
    list.querySelectorAll(".scan-pick").forEach((el) => {
      el.addEventListener("change", () => {
        el.closest(".scan-item").classList.toggle("picked", el.checked);
      });
    });
  }

  $("#btn-client-scan").addEventListener("click", scanClient);

  $("#btn-client-import").addEventListener("click", async (e) => {
    const picked = Array.from(document.querySelectorAll(".scan-pick:checked")).map((el) => el.value);
    if (!picked.length) { toast("先勾选要导入的凭据", "warn"); return; }
    const btn = e.currentTarget;
    btn.disabled = true;
    btn.textContent = "导入中…";
    try {
      const r = await api("/panel/api/accounts/scan/import", { method: "POST", body: { paths: picked } });
      const ok = (r.results || []).filter((x) => x.ok);
      const failed = (r.results || []).filter((x) => !x.ok);
      if (ok.length) toast(`已导入 ${ok.length} 个账号`, "ok");
      if (failed.length) toast(`失败 ${failed.length} 个：${failed[0].error}`, "bad");
      await loadOverview();
      if (ok.length) closeModal("modal-import");
      else scanClient();
    } catch (err) {
      if (err.message !== "unauthorized") toast(err.message, "bad");
    } finally {
      btn.disabled = false;
      btn.textContent = "导入选中";
    }
  });

  // ---------------------------------------------------------------- 手机号验证码
  //
  // 上游 /send_sms 强制要求阿里云验证码票据（实测假票 → 100006）。所以前端要
  // 真跑一次控件：initAliyunCaptcha 绑到「发送验证码」按钮上，点按钮弹验证框，
  // 回调里拿到 captchaVerifyParam 直接发给我们后端。
  //
  // 面板域名不在上游白名单时控件会初始化失败（服务器部署常见）——那时退化成
  // 「手动填票据」：在网页版 F12 里抄一次 captcha_param。
  let smsCaptchaState = "idle"; // idle | loading | ready | failed

  async function prepareSms() {
    if (smsCaptchaState !== "idle") return;
    smsCaptchaState = "loading";
    const status = $("#sms-status");
    status.textContent = "正在加载人机验证…";
    try {
      const cfg = await api("/panel/api/login/sms/config");
      window.AliyunCaptchaConfig = { region: cfg.region, prefix: cfg.prefix };
      await loadScript("https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js", 12000);
      window.initAliyunCaptcha({
        SceneId: cfg.scene,
        mode: "popup",
        element: "#captcha-element",
        button: "#btn-sms-send",
        language: "cn",
        slideStyle: { width: 360, height: 40 },
        getInstance(inst) { if (inst) smsCaptchaState = "ready"; },
        onError() { smsCaptchaFailed("人机验证加载失败"); },
        onClose(reason) { if (reason && reason !== "userDismiss") smsCaptchaFailed("人机验证被关闭"); },
        async captchaVerifyCallback(param) {
          try {
            await sendSmsWith(param);
            return { captchaResult: true, bizResult: true };
          } catch (err) {
            smsCaptchaFailed(err.message);
            return { captchaResult: false, bizResult: false };
          }
        },
      });
      status.textContent = "点「发送验证码」会弹出人机验证";
    } catch (e) {
      smsCaptchaFailed(e.message || "加载失败");
    }
  }

  function smsCaptchaFailed(msg) {
    smsCaptchaState = "failed";
    const status = $("#sms-status");
    status.textContent = "人机验证不可用：" + msg + "。可以展开下面的「手动填票据」，或改用「浏览器授权」。";
    const manual = $("#sms-manual");
    if (manual) manual.open = true;
  }

  function loadScript(src, timeoutMs) {
    return new Promise((resolve, reject) => {
      const existing = document.querySelector(`script[src="${src}"]`);
      if (existing && window.initAliyunCaptcha) { resolve(); return; }
      const el = document.createElement("script");
      const timer = setTimeout(() => { el.remove(); reject(new Error("加载超时")); }, timeoutMs || 15000);
      el.src = src;
      el.async = true;
      el.onload = () => { clearTimeout(timer); resolve(); };
      el.onerror = () => { clearTimeout(timer); reject(new Error("脚本加载失败（域名可能不在白名单）")); };
      document.head.appendChild(el);
    });
  }

  async function sendSmsWith(captchaParam) {
    const phone = $("#sms-phone").value.trim();
    if (!phone) throw new Error("先填手机号");
    if (!captchaParam) throw new Error("没拿到人机验证票据");
    // 邀请人要在发码时就带上：上游两条登录通道的验证码不通用，
    // 带归因就必须从发码起就走桌面端通道，否则验码必然失败。
    const r = await api("/panel/api/login/sms/send", {
      method: "POST",
      body: {
        phone,
        captcha_param: captchaParam,
        inviter_user_id: ($("#sms-inviter").value || "").trim(),
        // 默认 iam：这条通道已验证可用。可信设备不靠通道创建，
        // 导入后由「补绑设备」显式调 /devices_current_bind 完成。
        login_via: (($("#sms-channel") && $("#sms-channel").value) || "iam"),
      },
    });
    // IAM 通道发码会返回 verification_id，登录时必须带回去。
    state.sms.verificationID = r.verification_id || "";
    $("#sms-status").textContent = "验证码已发送，请查收短信";
    $("#sms-code").focus();
    toast("验证码已发送", "ok");
  }

  $("#btn-sms-send-manual").addEventListener("click", async (e) => {
    const btn = e.currentTarget;
    btn.disabled = true;
    try {
      await sendSmsWith($("#sms-captcha-manual").value.trim());
    } catch (err) {
      $("#sms-status").textContent = err.message;
      toast(err.message, "bad");
    } finally {
      btn.disabled = false;
    }
  });

  $("#btn-sms-login").addEventListener("click", async (e) => {
    const phone = $("#sms-phone").value.trim();
    const code = $("#sms-code").value.trim();
    if (!phone || !code) { toast("填手机号和验证码", "warn"); return; }
    const btn = e.currentTarget;
    btn.disabled = true;
    btn.textContent = "登录中…";
    try {
      const r = await api("/panel/api/login/sms/verify", {
        method: "POST",
        body: {
          phone, code,
          verification_id: state.sms.verificationID || "",
          inviter_user_id: ($("#sms-inviter").value || "").trim(),
          login_via: (($("#sms-channel") && $("#sms-channel").value) || "iam"),
        },
      });
      toast(`${r.updated ? "已更新" : "已导入"} ${r.account.nickname || r.account.uid}`, "ok");
      $("#sms-code").value = "";
      closeModal("modal-import");
      await loadOverview();
    } catch (err) {
      if (err.message !== "unauthorized") {
        $("#sms-status").textContent = err.message;
        toast(err.message, "bad");
      }
    } finally {
      btn.disabled = false;
      btn.textContent = "登录";
    }
  });

  // renderCallbackBase 展示当前生效的回调基址；为空说明既没配 login.callback_base、
  // 监听地址也不是环回，浏览器授权这条路走不通（提示改用粘贴）。
  function renderCallbackBase() {
    const base = (state.overview && state.overview.callback_base) || "";
    const code = $("#callback-base");
    const warn = $("#device-warn");
    if (base) {
      code.textContent = base + "/auth/callback";
      warn.hidden = true;
    } else {
      code.textContent = "（未配置）";
      warn.hidden = false;
      warn.textContent = "当前无法自动回跳：请在 config.json 里设置 login.callback_base " +
        "（例如 https://xhx2api.1task.cn），或直接用「粘贴凭证」。";
    }
  }

  $("#import-tabs").addEventListener("click", (e) => {
    const btn = e.target.closest(".mtab");
    if (btn) switchImportMode(btn.dataset.mode);
  });

  $("#btn-import-submit").addEventListener("click", async (e) => {
    const content = $("#import-content").value.trim();
    if (!content) { toast("请先粘贴内容", "warn"); return; }
    const btn = e.currentTarget;
    btn.disabled = true;
    btn.textContent = "导入中…";
    try {
      const r = await api("/panel/api/accounts/import", { method: "POST", body: { content } });
      toast(`已导入 ${r.nickname || r.uid}`, "ok");
      $("#import-content").value = "";
      closeModal("modal-import");
      await loadOverview();
    } catch (err) {
      if (err.message !== "unauthorized") toast(err.message, "bad");
    } finally {
      btn.disabled = false;
      btn.textContent = "导入";
    }
  });

  // 扫码
  async function startQr() {
    stopQrTimer();
    const status = $("#qr-status");
    status.textContent = "正在生成二维码…";
    status.className = "qr-status";
    try {
      const r = await api("/panel/api/login/qr/start", { method: "POST" });
      state.qr.id = r.id;
      const img = $("#qr-image");
      img.src = `/panel/api/login/qr/png?id=${encodeURIComponent(r.id)}&key=${encodeURIComponent(state.key)}`;
      img.hidden = false;
      status.textContent = "等待扫码（微信扫一扫）";
      state.qr.timer = setInterval(() => pollQr(r.id), 2000);
    } catch (e) {
      status.textContent = e.message;
      status.className = "qr-status bad";
    }
  }

  async function pollQr(id) {
    try {
      const r = await api(`/panel/api/login/qr/poll?id=${encodeURIComponent(id)}`);
      const status = $("#qr-status");
      status.textContent = r.status || "";
      if (r.done) {
        stopQrTimer();
        status.textContent = `已入池：${r.nickname || r.uid}`;
        status.className = "qr-status ok";
        toast("扫码登录成功", "ok");
        await loadOverview();
        setTimeout(() => closeModal("modal-import"), 1200);
      }
    } catch (e) {
      stopQrTimer();
      const status = $("#qr-status");
      status.textContent = e.message;
      status.className = "qr-status bad";
    }
  }

  function stopQrTimer() {
    if (state.qr.timer) { clearInterval(state.qr.timer); state.qr.timer = null; }
  }

  $("#btn-qr-start").addEventListener("click", startQr);

  // 浏览器授权
  $("#btn-device-start").addEventListener("click", async () => {
    const status = $("#device-status");
    status.textContent = "正在创建会话…";
    try {
      const r = await api("/panel/api/login/device/start", { method: "POST" });
      state.device.id = r.id;
      state.device.url = r.url;
      // 浏览器可能拦截弹窗；拦了也不影响——用户可以用「复制授权链接」。
      const win = window.open(r.url, "_blank", "noopener");
      status.textContent = win ? "已打开授权页，等待完成…" : "弹窗被拦截，请点「复制授权链接」手动打开";
      state.device.timer = setInterval(() => pollDevice(r.id), 2000);
    } catch (e) {
      status.textContent = e.message;
    }
  });

  $("#btn-device-copy").addEventListener("click", async () => {
    const url = state.device.url;
    if (!url) { toast("先点「打开授权页」生成链接", "warn"); return; }
    try {
      await navigator.clipboard.writeText(url);
      toast("授权链接已复制", "ok");
    } catch (e) {
      // 非 HTTPS / 无剪贴板权限：退化成提示用户手动选。
      prompt("复制这条授权链接：", url);
    }
  });

  async function pollDevice(id) {
    try {
      const r = await api(`/panel/api/login/device/poll?id=${encodeURIComponent(id)}`);
      const status = $("#device-status");
      status.textContent = r.status || "";
      if (r.done) {
        stopDeviceTimer();
        status.textContent = `已入池：${r.nickname || r.uid}`;
        toast("授权登录成功", "ok");
        await loadOverview();
        setTimeout(() => closeModal("modal-import"), 1200);
      }
    } catch (e) {
      stopDeviceTimer();
      $("#device-status").textContent = e.message;
    }
  }

  function stopDeviceTimer() {
    if (state.device.timer) { clearInterval(state.device.timer); state.device.timer = null; }
  }

  function stopLoginTimers() { stopQrTimer(); stopDeviceTimer(); }

  // ---------------------------------------------------------------------
  // 事件：顶栏与登录

  $("#btn-refresh").addEventListener("click", refreshAll);

  $("#btn-logout").addEventListener("click", () => {
    state.key = "";
    localStorage.removeItem("xhx_key");
    showLogin("");
  });

  $("#btn-login").addEventListener("click", async () => {
    const key = $("#login-key").value.trim();
    const entered = await tryEnter(key);
    if (!entered && !key) showLogin("密钥不正确");
  });

  $("#login-key").addEventListener("keydown", (e) => {
    if (e.key === "Enter") $("#btn-login").click();
  });

  // 日志自动刷新
  $("#logs-auto").addEventListener("change", () => {
    if ($("#logs-auto").checked) startLogTimer(); else stopLogTimer();
  });

  function startLogTimer() {
    stopLogTimer();
    state.autoRefresh = setInterval(() => {
      if (state.tab === "logs") loadLogs().catch(() => {});
    }, 3000);
  }
  function stopLogTimer() {
    if (state.autoRefresh) { clearInterval(state.autoRefresh); state.autoRefresh = null; }
  }

  // 账号页每 10 秒刷新一次（看冷却倒计时与在途）
  setInterval(() => {
    if (state.tab === "accounts" && !$("#app").hidden && document.visibilityState === "visible") {
      loadOverview().catch(() => {});
    }
  }, 10000);

  // ---------------------------------------------------------------------
  // 启动

  async function boot() {
    // 空密钥也试一次：配置里 api_key 为空的面板应当直接进入。
    //
    // 整个流程必须兜住异常：以前 showLogin 只挂在 `!ok` 分支上，任何一个
    // 未预期的抛错（接口返回非 JSON、元素缺失）都会让 boot 静默 reject，
    // 页面停在 #app hidden 的状态 —— 用户看到的就是纯白（比报错难查得多）。
    try {
      const ok = await tryEnter(state.key);
      if (!ok) showLogin(state.key ? "密钥无效，请重新输入" : "");
    } catch (e) {
      showLogin("进入失败：" + ((e && e.message) || "未知错误"));
    }
    try {
      startLogTimer();
    } catch (e) {
      console.warn("[panel] 日志定时器启动失败", e);
    }
  }

  boot();
})();
