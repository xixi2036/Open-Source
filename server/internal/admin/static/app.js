// Hush 管理后台：原生 JS 单页应用，无构建步骤、无第三方依赖。
// 所有用户可控的字符串（昵称、签名、动态、评论、举报详情……）一律通过 textContent 写入 DOM，不使用 innerHTML。
(function () {
  'use strict';

  // ---------- 基础工具 ----------
  const SVG_NS = 'http://www.w3.org/2000/svg';
  const state = { admin: null, csrf: '', version: '', timer: null };
  const RANK = { viewer: 1, moderator: 2, owner: 3 };
  const ROLE_NAME = { owner: '所有者', moderator: '审核员', viewer: '只读' };
  const can = (role) => state.admin && RANK[state.admin.role] >= RANK[role];

  function h(tag, props, ...children) {
    const el = document.createElement(tag);
    applyProps(el, props);
    append(el, children);
    return el;
  }
  function s(tag, attrs, ...children) {
    const el = document.createElementNS(SVG_NS, tag);
    for (const [k, v] of Object.entries(attrs || {})) el.setAttribute(k, v);
    append(el, children);
    return el;
  }
  function applyProps(el, props) {
    for (const [k, v] of Object.entries(props || {})) {
      if (v == null || v === false) continue;
      if (k === 'class') el.className = v;
      else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
      else if (k === 'value') el.value = v;
      else if (k === 'checked' || k === 'disabled' || k === 'selected' || k === 'required') el[k] = !!v;
      else el.setAttribute(k, v === true ? '' : v);
    }
  }
  function append(el, children) {
    for (const c of children.flat(Infinity)) {
      if (c == null || c === false) continue;
      el.append(c.nodeType ? c : document.createTextNode(String(c)));
    }
  }
  // fill 用 children 替换 el 的全部内容（与 h 一样会展开数组、跳过 null / false）。
  function fill(el, ...children) {
    el.textContent = '';
    append(el, children);
  }
  const $app = () => document.getElementById('app');

  const nf = new Intl.NumberFormat('zh-CN');
  const fmtNum = (n) => (n == null ? '—' : nf.format(n));
  function fmtBytes(n) {
    if (n == null) return '—';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let i = 0, v = Number(n);
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return (i === 0 ? String(v) : v.toFixed(v >= 100 ? 0 : v >= 10 ? 1 : 2)) + ' ' + units[i];
  }
  const dtf = new Intl.DateTimeFormat('zh-CN', { year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false });
  const fmtTime = (ms) => (ms ? dtf.format(new Date(ms)) : '—'); // 本地时区
  function fmtDuration(sec) {
    sec = Math.max(0, Math.floor(sec));
    const d = Math.floor(sec / 86400), hh = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
    if (d) return `${d} 天 ${hh} 小时`;
    if (hh) return `${hh} 小时 ${m} 分`;
    if (m) return `${m} 分 ${sec % 60} 秒`;
    return `${sec} 秒`;
  }
  const fmtMs = (v) => (v == null ? '—' : v >= 100 ? v.toFixed(0) : v >= 10 ? v.toFixed(1) : v.toFixed(2));
  const shortId = (id) => (id && id.length > 10 ? id.slice(0, 8) + '…' : id || '—');

  function toast(msg, isError) {
    const el = h('div', { class: 'toast' + (isError ? ' error' : '') }, msg);
    document.getElementById('toasts').append(el);
    setTimeout(() => el.remove(), isError ? 6000 : 3500);
  }

  // ---------- 接口 ----------
  class ApiError extends Error {
    constructor(status, code, message) { super(message); this.status = status; this.code = code; }
  }
  async function api(method, path, body) {
    const opts = { method, credentials: 'same-origin', headers: {} };
    if (method !== 'GET') opts.headers['X-CSRF-Token'] = state.csrf;
    if (body !== undefined) { opts.headers['Content-Type'] = 'application/json'; opts.body = JSON.stringify(body); }
    let resp;
    try { resp = await fetch(path, opts); } catch (e) { throw new ApiError(0, 'network', '无法连接服务器'); }
    let data = null;
    const text = await resp.text();
    if (text) { try { data = JSON.parse(text); } catch (e) { data = null; } }
    if (!resp.ok) {
      if (resp.status === 401 && path !== '/admin/api/login' && state.admin) { state.admin = null; renderLogin('登录已过期，请重新登录'); }
      throw new ApiError(resp.status, data && data.error, (data && data.message) || `请求失败（${resp.status}）`);
    }
    return data;
  }
  // act 执行一次写操作并给出提示条；成功返回结果，失败返回 undefined。
  async function act(okMsg, fn) {
    try { const r = await fn(); if (okMsg) toast(okMsg); return r === undefined ? true : r; }
    catch (e) { toast(e.message, true); return undefined; }
  }
  const qs = (params) => {
    const u = new URLSearchParams();
    for (const [k, v] of Object.entries(params)) if (v !== '' && v != null && v !== false) u.set(k, v);
    const str = u.toString();
    return str ? '?' + str : '';
  };

  // ---------- 弹窗 ----------
  // dialog({title, message, fields, confirmText, danger, requireText}) → Promise<values | null>
  function dialog(o) {
    return new Promise((resolve) => {
      const inputs = {};
      const fields = (o.fields || []).map((f, i) => {
        const id = 'dlg-' + i;
        let input;
        if (f.type === 'select') input = h('select', { id }, f.options.map(([v, t]) => h('option', { value: v, selected: v === f.value }, t)));
        else if (f.type === 'textarea') input = h('textarea', { id, placeholder: f.placeholder, maxlength: f.maxlength });
        else if (f.type === 'checkbox') input = h('input', { id, type: 'checkbox', checked: f.value });
        else input = h('input', { id, type: f.type || 'text', placeholder: f.placeholder, autocomplete: f.autocomplete || 'off', required: f.required, minlength: f.minlength, maxlength: f.maxlength });
        if (f.value != null && f.type !== 'select' && f.type !== 'checkbox') input.value = f.value;
        inputs[f.name] = input;
        if (f.type === 'checkbox') return h('div', { class: 'field' }, h('label', { class: 'check', for: id }, input, f.label));
        return h('div', { class: 'field' }, h('label', { for: id }, f.label), input, f.hint && h('div', { class: 'hint' }, f.hint));
      });
      const okBtn = h('button', { class: 'btn ' + (o.danger ? 'danger' : 'primary'), type: 'submit' }, o.confirmText || '确定');
      const dlg = h('dialog', { 'aria-labelledby': 'dlg-title' },
        h('form', { method: 'dialog', onsubmit: (ev) => {
          ev.preventDefault();
          const values = {};
          for (const [k, el] of Object.entries(inputs)) values[k] = el.type === 'checkbox' ? el.checked : el.value;
          if (o.requireText && values.confirm !== o.requireText) { toast('输入的内容与要求不一致', true); return; }
          done(values);
        } },
          h('h2', { id: 'dlg-title' }, o.title),
          o.message && h('p', { class: 'msg' }, o.message),
          fields,
          h('div', { class: 'btn-row' }, h('button', { class: 'btn', type: 'button', onclick: () => done(null) }, '取消'), okBtn)));
      function done(v) { dlg.close(); dlg.remove(); resolve(v); }
      dlg.addEventListener('cancel', (ev) => { ev.preventDefault(); done(null); });
      document.body.append(dlg);
      dlg.showModal();
    });
  }
  const confirmDanger = (title, message, confirmText) => dialog({ title, message, danger: true, confirmText: confirmText || '确认' });

  // ---------- 通用组件 ----------
  function table(cols, rows, emptyText) {
    if (!rows || rows.length === 0) return h('div', { class: 'empty' }, emptyText || '暂无数据');
    return h('div', { class: 'table-wrap' }, h('table', null,
      h('thead', null, h('tr', null, cols.map((c) => h('th', { class: c.right ? 'right' : null, scope: 'col' }, c.title)))),
      h('tbody', null, rows.map((r) => h('tr', null, cols.map((c) => h('td', { class: [c.right ? 'right num' : '', c.cls || ''].join(' ').trim() || null }, c.render(r))))))));
  }
  function pager(res, onPage) {
    const pages = Math.max(1, Math.ceil(res.total / res.size));
    return h('div', { class: 'pager' },
      h('span', null, `共 ${fmtNum(res.total)} 条`), h('span', { class: 'spacer' }),
      h('button', { class: 'btn sm', disabled: res.page <= 1, onclick: () => onPage(res.page - 1) }, '上一页'),
      h('span', { class: 'num' }, `${res.page} / ${pages}`),
      h('button', { class: 'btn sm', disabled: res.page >= pages, onclick: () => onPage(res.page + 1) }, '下一页'));
  }
  const badge = (text, kind) => h('span', { class: 'badge ' + (kind || '') }, text);
  const stat = (label, value, sub) => h('div', { class: 'card stat' }, h('div', { class: 'label' }, label), h('div', { class: 'value' }, value), sub && h('div', { class: 'sub' }, sub));
  const section = (title, actions, ...body) => h('section', { class: 'section' }, h('div', { class: 'section-head' }, h('h2', null, title), actions), body);
  const idCell = (id) => h('code', { title: id }, shortId(id));
  const userLink = (id, name) => h('a', { href: '#/users/' + encodeURIComponent(id) }, name || shortId(id));
  function field(label, input, hint) {
    const id = input.id || (input.id = 'f-' + Math.random().toString(36).slice(2, 9));
    return h('div', { class: 'field' }, h('label', { for: id }, label), input, hint && h('div', { class: 'hint' }, hint));
  }
  const loading = () => h('div', { class: 'empty' }, '加载中…');

  // ---------- 图表（内嵌 SVG） ----------
  // niceMax 返回不小于 v 的「好看」的纵轴上限：4 等分后每格都是整数。
  function niceMax(v) {
    const step = Math.max(1, v / 4);
    const p = Math.pow(10, Math.floor(Math.log10(step)));
    for (const m of [1, 2, 3, 4, 5, 6, 8, 10]) if (step <= m * p) return 4 * m * p;
    return 40 * p;
  }
  function chartFrame(W, H, pad, max, label) {
    const svg = s('svg', { class: 'chart', viewBox: `0 0 ${W} ${H}`, role: 'img', 'aria-label': label });
    for (let i = 0; i <= 4; i++) {
      const y = pad.t + (H - pad.t - pad.b) * i / 4;
      svg.append(s('line', { class: 'grid-line', x1: pad.l, x2: W - pad.r, y1: y, y2: y }));
      svg.append(s('text', { class: 'axis', x: pad.l - 6, y: y + 4, 'text-anchor': 'end' }, fmtNum(max * (4 - i) / 4)));
    }
    return svg;
  }
  function lineChart(points, label) {
    const W = 720, H = 240, pad = { l: 44, r: 12, t: 12, b: 26 };
    const max = niceMax(Math.max(1, ...points.map((p) => Math.max(p.a, p.b))));
    const svg = chartFrame(W, H, pad, max, label);
    const x = (i) => pad.l + (W - pad.l - pad.r) * (points.length === 1 ? 0.5 : i / (points.length - 1));
    const y = (v) => pad.t + (H - pad.t - pad.b) * (1 - v / max);
    const path = (key) => points.map((p, i) => `${i ? 'L' : 'M'}${x(i).toFixed(1)},${y(p[key]).toFixed(1)}`).join(' ');
    svg.append(s('path', { class: 'area-a', d: `${path('a')} L${x(points.length - 1)},${y(0)} L${x(0)},${y(0)} Z` }));
    svg.append(s('path', { class: 'line-b', d: path('b') }), s('path', { class: 'line-a', d: path('a') }));
    points.forEach((p, i) => {
      const last = i === points.length - 1;
      if (last || (i % 5 === 0 && points.length - 1 - i >= 3)) svg.append(s('text', { class: 'axis', x: x(i), y: H - 8, 'text-anchor': i === 0 ? 'start' : last ? 'end' : 'middle' }, p.label));
      const tip = `${p.full}：注册 ${fmtNum(p.a)}，活跃 ${fmtNum(p.b)}`;
      svg.append(s('circle', { class: 'dot-b', cx: x(i), cy: y(p.b), r: 2.5 }, s('title', null, tip)));
      svg.append(s('circle', { class: 'dot-a', cx: x(i), cy: y(p.a), r: 2.5 }, s('title', null, tip)));
    });
    return svg;
  }
  function barChart(points, label) {
    const W = 720, H = 240, pad = { l: 44, r: 12, t: 12, b: 26 };
    const max = niceMax(Math.max(1, ...points.map((p) => p.v)));
    const svg = chartFrame(W, H, pad, max, label);
    const bw = (W - pad.l - pad.r) / points.length;
    points.forEach((p, i) => {
      const bh = (H - pad.t - pad.b) * p.v / max;
      svg.append(s('rect', { class: 'bar', x: pad.l + i * bw + 3, y: H - pad.b - bh, width: Math.max(1, bw - 6), height: Math.max(bh, p.v ? 1.5 : 0), rx: 2 }, s('title', null, `${p.full}：${fmtNum(p.v)} 条`)));
      if (i % 4 === 3) svg.append(s('text', { class: 'axis', x: pad.l + i * bw + bw / 2, y: H - 8, 'text-anchor': 'middle' }, p.label));
    });
    return svg;
  }

  // ---------- 页面：概览 ----------
  async function pageDashboard(root) {
    const d = await api('GET', '/admin/api/dashboard');
    const t = d.totals;
    const REG = { open: '开放注册', invite: '仅邀请码', closed: '关闭注册' };
    fill(root, 
      h('div', { class: 'grid stats section' },
        stat('注册用户', fmtNum(t.users), t.users_disabled ? `其中已停用 ${fmtNum(t.users_disabled)}` : REG[d.registration] || ''),
        stat('今日新增', fmtNum(t.new_today)),
        stat('在线连接', fmtNum(d.online)),
        stat('今日活跃', fmtNum(t.active_today), '今日有过认证请求的用户'),
        stat('群组', fmtNum(t.groups)),
        stat('朋友圈动态', fmtNum(t.moments)),
        stat('离线队列', fmtNum(t.queue_items) + ' 条', fmtBytes(t.queue_bytes)),
        stat('附件', fmtNum(t.attachments) + ' 个', fmtBytes(t.attachment_bytes)),
        stat('待处理举报', fmtNum(t.pending_reports), t.pending_reports ? h('a', { href: '#/reports' }, '去处理') : null),
        stat('运行时长', fmtDuration(d.uptime_seconds), `版本 ${d.version} · ${d.go_version}`)),
      h('div', { class: 'grid two' },
        h('div', { class: 'card pad' },
          h('div', { class: 'section-head' }, h('h2', null, '最近 30 天注册与活跃'),
            h('div', { class: 'legend' }, h('span', null, h('i', { class: 'a' }), '注册'), h('span', null, h('i', { class: 'b' }), '活跃'))),
          lineChart(d.trend.map((p) => ({ a: p.registrations, b: p.active, label: p.day.slice(5), full: p.day })), '最近 30 天每天的注册数与活跃用户数')),
        h('div', { class: 'card pad' },
          h('div', { class: 'section-head' }, h('h2', null, '最近 24 小时每小时投递消息数')),
          barChart(d.hourly.map((p) => { const dt = new Date(p.ts); const hh = String(dt.getHours()).padStart(2, '0'); return { v: p.envelopes, label: hh + ':00', full: fmtTime(p.ts).slice(0, 13) + ' 时' }; }), '最近 24 小时每小时入队的加密信封数'),
          h('p', { class: 'muted small' }, '只统计加密信封的条数；聊天内容端到端加密，服务端与后台都无法查看。'))));
  }

  // ---------- 页面：用户 ----------
  async function pageUsers(root) {
    const f = { q: '', status: '', sort: 'created', page: 1 };
    const box = h('div', { class: 'card' }, loading());
    const q = h('input', { type: 'search', id: 'user-q', placeholder: '邮箱 / 昵称 / 用户 ID' });
    const status = h('select', { id: 'user-status' }, h('option', { value: '' }, '全部状态'), h('option', { value: 'active' }, '正常'), h('option', { value: 'disabled' }, '已停用'));
    const sort = h('select', { id: 'user-sort' }, h('option', { value: 'created' }, '按注册时间'), h('option', { value: 'active' }, '按最近活跃'));
    async function load() {
      const res = await api('GET', '/admin/api/users' + qs({ q: f.q, status: f.status, sort: f.sort, page: f.page, size: 20 }));
      fill(box, table([
        { title: '用户', render: (u) => h('div', null, userLink(u.id, u.name), h('div', { class: 'muted small' }, u.email)) },
        { title: 'ID', render: (u) => idCell(u.id) },
        { title: '平台 / 设备', render: (u) => [u.platform || '—', u.device_name ? ' · ' + u.device_name : ''].join('') },
        { title: '注册时间', render: (u) => fmtTime(u.created_at) },
        { title: '最近活跃', render: (u) => fmtTime(u.last_active_at) },
        { title: '状态', render: (u) => [u.disabled_at ? badge('已停用', 'danger') : badge('正常', 'ok'), ' ', u.online ? badge('在线', 'info') : null] },
        { title: '', cls: 'actions', render: (u) => h('a', { class: 'btn sm', href: '#/users/' + encodeURIComponent(u.id) }, '详情') },
      ], res.items, '没有符合条件的用户'), pager(res, (p) => { f.page = p; load().catch(showErr); }));
    }
    const search = (ev) => { ev.preventDefault(); f.q = q.value.trim(); f.status = status.value; f.sort = sort.value; f.page = 1; load().catch(showErr); };
    status.addEventListener('change', search); sort.addEventListener('change', search);
    fill(root, 
      h('form', { class: 'toolbar', onsubmit: search, role: 'search' },
        h('div', { class: 'field grow' }, h('label', { for: 'user-q' }, '搜索'), q),
        h('div', { class: 'field' }, h('label', { for: 'user-status' }, '状态'), status),
        h('div', { class: 'field' }, h('label', { for: 'user-sort' }, '排序'), sort),
        h('button', { class: 'btn primary', type: 'submit' }, '搜索')),
      box);
    await load();
  }

  async function pageUser(root, id) {
    const u = await api('GET', '/admin/api/users/' + encodeURIComponent(id));
    const reload = () => pageUser(root, id).catch(showErr);
    const base = '/admin/api/users/' + encodeURIComponent(id);
    const mod = can('moderator');
    const actions = h('div', { class: 'btn-row' },
      u.disabled_at
        ? h('button', { class: 'btn', disabled: !mod, onclick: async () => { if (await act('账号已启用', () => api('POST', base + '/enable'))) reload(); } }, '启用账号')
        : h('button', { class: 'btn danger-outline', disabled: !mod, onclick: async () => {
          const v = await dialog({ title: '停用账号', message: `停用后 ${u.email} 的登录令牌立即失效、无法再登录，在线连接会被断开。数据保留，可随时启用。`, danger: true, confirmText: '停用', fields: [{ name: 'reason', label: '原因（写入审计日志，可留空）', maxlength: 200 }] });
          if (v && await act('账号已停用', () => api('POST', base + '/disable', { reason: v.reason }))) reload();
        } }, '停用账号'),
      h('button', { class: 'btn', disabled: !mod, onclick: async () => {
        if (await confirmDanger('强制下线', '作废该用户的登录令牌并断开连接。密钥与数据保留，用户可以重新登录。', '强制下线') && await act('已强制下线', () => api('POST', base + '/logout'))) reload();
      } }, '强制下线'),
      h('button', { class: 'btn', disabled: !mod, onclick: async () => {
        const v = await dialog({ title: '重置资料', message: '清空违规的昵称、签名或头像，联系人会同步看到变化。', confirmText: '重置', danger: true,
          fields: [{ name: 'name', label: '昵称（改为「用户xxxxxx」）', type: 'checkbox', value: true }, { name: 'about', label: '个性签名', type: 'checkbox', value: true }, { name: 'avatar', label: '头像', type: 'checkbox', value: true }] });
        if (v && await act('资料已重置', () => api('POST', base + '/reset-profile', v))) reload();
      } }, '重置资料'),
      h('button', { class: 'btn danger', disabled: !mod, onclick: async () => {
        const v = await dialog({ title: '删除账号', danger: true, confirmText: '永久删除', requireText: u.email,
          message: `将永久删除该账号及其好友关系、群成员身份、朋友圈、离线队列和密钥，无法恢复。\n请输入该账号的邮箱 ${u.email} 以确认。`,
          fields: [{ name: 'confirm', label: '账号邮箱', autocomplete: 'off', required: true }] });
        if (v && await act('账号已删除', () => api('DELETE', base, { confirm_email: v.confirm }))) location.hash = '#/users';
      } }, '删除账号'));
    const kv = (pairs) => h('dl', { class: 'kv' }, pairs.map(([k, v]) => [h('dt', null, k), h('dd', null, v)]));
    fill(root, 
      h('p', { class: 'section' }, h('a', { href: '#/users' }, '← 返回用户列表')),
      section([u.name || '（无昵称）', ' ', u.disabled_at ? badge('已停用', 'danger') : badge('正常', 'ok'), ' ', u.online ? badge('在线', 'info') : badge('离线')], actions,
        h('div', { class: 'grid two' },
          h('div', { class: 'card pad' }, kv([
            ['用户 ID', h('code', null, u.id)], ['邮箱', u.email], ['昵称', u.name || '—'], ['个性签名', u.about || '—'],
            ['头像', u.avatar_id ? thumb(u.avatar_id) : '未设置'],
            ['注册时间', fmtTime(u.created_at)], ['最近活跃', fmtTime(u.last_active_at)],
            ['平台', u.platform || '—'], ['设备名', u.device_name || '—'], ['设备代', fmtNum(u.device_generation)],
            ['停用时间', u.disabled_at ? fmtTime(u.disabled_at) : '—']])),
          h('div', { class: 'card pad' }, kv([
            ['好友数', fmtNum(u.friends)], ['所在群', fmtNum(u.groups.length)],
            ['朋友圈条数', u.moments ? h('a', { href: '#/moments?author=' + encodeURIComponent(u.id) }, fmtNum(u.moments)) : '0'],
            ['队列积压', `${fmtNum(u.queue_items)} 条 · ${fmtBytes(u.queue_bytes)}`],
            ['附件用量', `${fmtNum(u.attachments)} 个 · ${fmtBytes(u.attachment_bytes)}`],
            ['有效令牌', fmtNum(u.sessions)], ['待处理举报', fmtNum(u.pending_reports)]]),
            h('p', { class: 'muted small' }, '队列只显示条数与字节数：消息内容端到端加密，后台没有查看入口。')))),
      section('所在群', null, h('div', { class: 'card' }, table([
        { title: '群名', render: (g) => h('a', { href: '#/groups/' + encodeURIComponent(g.id) }, g.name) },
        { title: '角色', render: (g) => GROUP_ROLE[g.role] || g.role },
        { title: '成员数', right: true, render: (g) => fmtNum(g.members) },
      ], u.groups, '未加入任何群'))));
  }
  const GROUP_ROLE = { owner: '群主', admin: '管理员', member: '成员' };

  function thumb(id) {
    const img = h('img', { src: '/admin/api/media/' + encodeURIComponent(id), alt: '图片', loading: 'lazy', width: 84, height: 84 });
    img.addEventListener('error', () => img.replaceWith(h('div', { class: 'ph' }, '无法预览')));
    return h('div', { class: 'thumbs' }, img);
  }

  // ---------- 页面：群组 ----------
  async function pageGroups(root) {
    const f = { q: '', page: 1 };
    const box = h('div', { class: 'card' }, loading());
    const q = h('input', { type: 'search', id: 'group-q', placeholder: '群名 / 群 ID' });
    async function load() {
      const res = await api('GET', '/admin/api/groups' + qs({ q: f.q, page: f.page, size: 20 }));
      fill(box, table([
        { title: '群名', render: (g) => h('a', { href: '#/groups/' + encodeURIComponent(g.id) }, g.name) },
        { title: 'ID', render: (g) => idCell(g.id) },
        { title: '群主', render: (g) => userLink(g.owner_id, g.owner_name) },
        { title: '成员', right: true, render: (g) => fmtNum(g.members) },
        { title: '消息数', right: true, render: (g) => fmtNum(g.messages) },
        { title: '创建时间', render: (g) => fmtTime(g.created_at) },
        { title: '最近消息', render: (g) => fmtTime(g.last_message_at) },
      ], res.items, '没有符合条件的群'), pager(res, (p) => { f.page = p; load().catch(showErr); }));
    }
    fill(root, 
      h('form', { class: 'toolbar', role: 'search', onsubmit: (ev) => { ev.preventDefault(); f.q = q.value.trim(); f.page = 1; load().catch(showErr); } },
        h('div', { class: 'field grow' }, h('label', { for: 'group-q' }, '搜索'), q), h('button', { class: 'btn primary', type: 'submit' }, '搜索')),
      box);
    await load();
  }

  async function pageGroup(root, id) {
    const { group: g, members } = await api('GET', '/admin/api/groups/' + encodeURIComponent(id));
    const base = '/admin/api/groups/' + encodeURIComponent(id);
    const reload = () => pageGroup(root, id).catch(showErr);
    const mod = can('moderator');
    fill(root, 
      h('p', { class: 'section' }, h('a', { href: '#/groups' }, '← 返回群组列表')),
      section(g.name, h('button', { class: 'btn danger', disabled: !mod, onclick: async () => {
        if (await confirmDanger('解散群', `将解散「${g.name}」，${members.length} 名成员都会被移出，无法恢复。`, '解散') && await act('群已解散', () => api('DELETE', base))) location.hash = '#/groups';
      } }, '解散群'),
        h('div', { class: 'card pad' }, h('dl', { class: 'kv' },
          h('dt', null, '群 ID'), h('dd', null, h('code', null, g.id)),
          h('dt', null, '群主'), h('dd', null, userLink(g.owner_id, g.owner_name)),
          h('dt', null, '创建时间'), h('dd', null, fmtTime(g.created_at)),
          h('dt', null, '成员数'), h('dd', null, fmtNum(g.members)),
          h('dt', null, '消息计数'), h('dd', null, `${fmtNum(g.messages)} 条（只有条数，内容端到端加密）`),
          h('dt', null, '最近消息'), h('dd', null, fmtTime(g.last_message_at)),
          h('dt', null, '群公告'), h('dd', null, g.announcement || '—'),
          h('dt', null, '群头像'), h('dd', null, g.avatar_id ? thumb(g.avatar_id) : '未设置')))),
      section('成员', null, h('div', { class: 'card' }, table([
        { title: '成员', render: (m) => h('div', null, userLink(m.user_id, m.name), h('div', { class: 'muted small' }, m.email)) },
        { title: '角色', render: (m) => badge(GROUP_ROLE[m.role] || m.role, m.role === 'owner' ? 'ok' : m.role === 'admin' ? 'info' : '') },
        { title: '加入时间', render: (m) => fmtTime(m.joined_at) },
        { title: '', cls: 'actions', render: (m) => h('div', { class: 'btn-row' },
          m.role !== 'owner' && h('button', { class: 'btn sm', disabled: !mod, onclick: async () => {
            if (await confirmDanger('转让群主', `把群主转让给 ${m.name || m.email}？原群主将降为管理员。`, '转让') && await act('群主已转让', () => api('POST', base + '/owner', { user_id: m.user_id }))) reload();
          } }, '设为群主'),
          h('button', { class: 'btn sm danger-outline', disabled: !mod, onclick: async () => {
            if (await confirmDanger('移除成员', `把 ${m.name || m.email} 移出该群？${m.role === 'owner' ? '群主会自动转让给其他成员。' : ''}`, '移除') && await act('成员已移除', () => api('DELETE', base + '/members/' + encodeURIComponent(m.user_id)))) {
              if (members.length === 1) location.hash = '#/groups'; else reload();
            }
          } }, '移除')) },
      ], members))));
  }

  // ---------- 页面：朋友圈审核 ----------
  async function pageMoments(root, _id, params) {
    const f = { author: params.get('author') || '', reported: false, order: 'reported', page: 1 };
    const box = h('div', { class: 'card' }, loading());
    const author = h('input', { type: 'text', id: 'moment-author', placeholder: '作者的用户 ID（可留空）', value: f.author });
    const reported = h('input', { type: 'checkbox', id: 'moment-reported' });
    const order = h('select', { id: 'moment-order' }, h('option', { value: 'reported' }, '被举报的优先'), h('option', { value: 'time' }, '按发布时间'));
    const mod = can('moderator');
    async function load() {
      const res = await api('GET', '/admin/api/moments' + qs({ author: f.author, reported: f.reported ? 1 : '', order: f.order, page: f.page, size: 10 }));
      if (!res.items.length) { fill(box, h('div', { class: 'empty' }, '没有符合条件的动态')); return; }
      fill(box, res.items.map((m) => {
        const commentsBox = h('div');
        async function loadComments() {
          const list = await api('GET', '/admin/api/moments/' + encodeURIComponent(m.id) + '/comments');
          fill(commentsBox, h('div', { class: 'comments' }, list.length ? list.map((c) => h('div', { class: 'comment' },
            userLink(c.author_id, c.author_name), h('span', { class: 'body' }, c.text), c.pending_reports ? badge('被举报', 'warn') : null,
            h('span', { class: 'muted small' }, fmtTime(c.created_at)),
            h('button', { class: 'btn sm danger-outline', disabled: !mod, onclick: async () => {
              if (await confirmDanger('删除评论', '删除这条评论？无法恢复。', '删除') && await act('评论已删除', () => api('DELETE', '/admin/api/comments/' + encodeURIComponent(c.id)))) loadComments().catch(showErr);
            } }, '删除'))) : h('span', { class: 'muted' }, '没有评论')));
        }
        return h('article', { class: 'moment' },
          h('div', { class: 'head' }, userLink(m.author_id, m.author_name), h('span', { class: 'muted small' }, m.author_email),
            h('span', { class: 'muted small' }, fmtTime(m.created_at)), m.vis_mode !== 'all' ? badge(m.vis_mode === 'include' ? '部分可见' : '部分不可见') : null,
            m.pending_reports ? badge(`被举报 ${m.pending_reports} 次`, 'warn') : null),
          m.text && h('div', { class: 'text' }, m.text),
          m.media.length ? h('div', { class: 'thumbs' }, m.media.map((x) => {
            const img = h('img', { src: '/admin/api/media/' + encodeURIComponent(x.attachment_id), alt: '动态图片', loading: 'lazy', width: 84, height: 84 });
            img.addEventListener('error', () => img.replaceWith(h('div', { class: 'ph' }, '无法预览')));
            return img;
          })) : null,
          h('div', { class: 'btn-row' },
            h('span', { class: 'muted small' }, `赞 ${fmtNum(m.likes)} · 评论 ${fmtNum(m.comments)}`),
            h('button', { class: 'btn sm', onclick: () => loadComments().catch(showErr) }, '查看评论'),
            h('button', { class: 'btn sm danger', disabled: !mod, onclick: async () => {
              if (await confirmDanger('删除动态', '删除这条动态及其评论、点赞和图片？无法恢复。', '删除') && await act('动态已删除', () => api('DELETE', '/admin/api/moments/' + encodeURIComponent(m.id)))) load().catch(showErr);
            } }, '删除动态')),
          commentsBox);
      }), pager(res, (p) => { f.page = p; load().catch(showErr); }));
    }
    const apply = (ev) => { ev.preventDefault(); f.author = author.value.trim(); f.reported = reported.checked; f.order = order.value; f.page = 1; load().catch(showErr); };
    reported.addEventListener('change', apply); order.addEventListener('change', apply);
    fill(root, 
      h('form', { class: 'toolbar', onsubmit: apply },
        h('div', { class: 'field grow' }, h('label', { for: 'moment-author' }, '作者'), author),
        h('div', { class: 'field' }, h('label', { for: 'moment-order' }, '排序'), order),
        h('label', { class: 'check', for: 'moment-reported' }, reported, '只看被举报'),
        h('button', { class: 'btn primary', type: 'submit' }, '筛选')),
      box);
    await load();
  }

  // ---------- 页面：举报 ----------
  async function pageReports(root) {
    const f = { status: 'pending', page: 1 };
    const box = h('div', { class: 'card' }, loading());
    const TYPE = { user: '用户', moment: '动态', comment: '评论', group: '群' };
    const ACTION = { ignore: '忽略', delete_content: '删除内容', disable_account: '停用账号' };
    const mod = can('moderator');
    async function resolve(r, action) {
      const tips = {
        ignore: '不做任何处理，仅把举报标记为已处理。',
        delete_content: { moment: '删除这条动态。', comment: '删除这条评论。', user: '清空该用户的昵称、签名和头像。', group: '解散这个群。' }[r.target_type],
        disable_account: `停用责任账号「${r.target.owner || shortId(r.target.owner_id)}」：令牌失效、无法登录、连接断开。`,
      };
      const v = await dialog({ title: '处理举报：' + ACTION[action], message: tips[action] + '\n针对同一对象的其他待处理举报会一并结案。', danger: action !== 'ignore', confirmText: ACTION[action],
        fields: [{ name: 'note', label: '处理备注（可留空）', type: 'textarea', maxlength: 500 }] });
      if (v && await act('举报已处理', () => api('POST', '/admin/api/reports/' + encodeURIComponent(r.id) + '/resolve', { action, note: v.note }))) load().catch(showErr);
    }
    function target(r) {
      const t = r.target;
      const link = r.target_type === 'user' ? userLink(r.target_id, t.summary)
        : r.target_type === 'group' ? h('a', { href: '#/groups/' + encodeURIComponent(r.target_id) }, t.summary || shortId(r.target_id))
          : h('span', null, t.summary || '（无文字）');
      return h('div', { class: 'clip' }, badge(TYPE[r.target_type] || r.target_type), ' ', t.exists ? link : h('span', { class: 'muted' }, '对象已不存在'),
        t.exists && t.owner_id && r.target_type !== 'user' ? h('div', { class: 'muted small' }, r.target_type === 'group' ? '群主：' : '作者：', userLink(t.owner_id, t.owner)) : null);
    }
    async function load() {
      const res = await api('GET', '/admin/api/reports' + qs({ status: f.status, page: f.page, size: 20 }));
      const cols = [
        { title: '举报时间', render: (r) => fmtTime(r.created_at) },
        { title: '举报人', render: (r) => userLink(r.reporter_id, r.reporter_name) },
        { title: '对象', render: target },
        { title: '理由', render: (r) => h('div', { class: 'clip' }, h('div', null, r.reason), r.detail && h('div', { class: 'muted small' }, r.detail)) },
      ];
      if (f.status === 'pending') cols.push({ title: '处理', cls: 'actions', render: (r) => h('div', { class: 'btn-row' },
        h('button', { class: 'btn sm', disabled: !mod, onclick: () => resolve(r, 'ignore') }, '忽略'),
        h('button', { class: 'btn sm danger-outline', disabled: !mod || !r.target.exists, onclick: () => resolve(r, 'delete_content') }, '删除内容'),
        h('button', { class: 'btn sm danger', disabled: !mod || !r.target.owner_id, onclick: () => resolve(r, 'disable_account') }, '停用账号')) });
      else cols.push({ title: '处理结果', render: (r) => h('div', { class: 'clip' }, badge(ACTION[r.action] || r.action, r.action === 'ignore' ? '' : 'danger'), ' ', h('span', { class: 'muted small' }, `${r.handled_by} · ${fmtTime(r.handled_at)}`), r.note && h('div', { class: 'small' }, r.note)) });
      fill(box, table(cols, res.items, f.status === 'pending' ? '没有待处理的举报' : '没有已处理的举报'), pager(res, (p) => { f.page = p; load().catch(showErr); }));
    }
    const tab = (value, text) => h('button', { type: 'button', role: 'tab', 'aria-selected': String(f.status === value), onclick: () => { f.status = value; f.page = 1; render(); } }, text);
    function render() {
      fill(root, h('div', { class: 'tabs', role: 'tablist' }, tab('pending', '待处理'), tab('resolved', '已处理')), box);
      load().catch(showErr);
    }
    render();
  }

  // ---------- 页面：注册控制（邀请码） ----------
  async function pageInvites(root) {
    const f = { page: 1 };
    const box = h('div', { class: 'card' }, loading());
    const modeBox = h('div');
    const mod = can('moderator');
    const count = h('input', { type: 'number', min: 1, max: 100, value: 1 });
    const maxUses = h('input', { type: 'number', min: 1, max: 100000, value: 1 });
    const expires = h('input', { type: 'datetime-local' });
    const note = h('input', { type: 'text', maxlength: 100, placeholder: '例如：内测第一批' });
    async function load() {
      const res = await api('GET', '/admin/api/invites' + qs({ page: f.page, size: 20 }));
      const MODE = { open: ['开放注册', 'ok', '任何人都可以注册，邀请码此时不起作用。'], invite: ['仅邀请码', 'info', '新用户注册时必须填写有效的邀请码，老用户登录不受影响。'], closed: ['关闭注册', 'danger', '不接受新用户注册，老用户登录不受影响。'] }[res.registration_mode];
      fill(modeBox, h('div', { class: 'card pad section' }, h('div', { class: 'btn-row' }, h('strong', null, '当前注册模式'), badge(MODE[0], MODE[1]), h('span', { class: 'muted' }, MODE[2]),
        can('owner') ? h('a', { class: 'btn sm', href: '#/settings' }, '修改注册模式与邮箱域名白名单') : h('span', { class: 'muted small' }, '（仅所有者可修改）'))));
      const now = Date.now();
      fill(box, table([
        { title: '邀请码', render: (i) => h('code', null, i.code) },
        { title: '已用 / 上限', right: true, render: (i) => `${fmtNum(i.used)} / ${fmtNum(i.max_uses)}` },
        { title: '过期时间', render: (i) => (i.expires_at ? fmtTime(i.expires_at) : '不过期') },
        { title: '状态', render: (i) => (i.revoked ? badge('已作废', 'danger') : i.expires_at && i.expires_at <= now ? badge('已过期', 'warn') : i.used >= i.max_uses ? badge('已用完') : badge('可用', 'ok')) },
        { title: '备注', render: (i) => h('div', { class: 'clip' }, i.note || '—') },
        { title: '创建', render: (i) => h('div', null, i.created_by, h('div', { class: 'muted small' }, fmtTime(i.created_at))) },
        { title: '', cls: 'actions', render: (i) => h('div', { class: 'btn-row' },
          h('button', { class: 'btn sm', disabled: !mod || !i.used, onclick: async () => {
            const uses = await act(null, () => api('GET', '/admin/api/invites/' + encodeURIComponent(i.code) + '/uses'));
            if (uses) dialog({ title: '使用记录', message: uses.map((u) => `${fmtTime(u.used_at)}  ${u.email}`).join('\n') || '还没有人使用', confirmText: '关闭' });
          } }, '使用记录'),
          h('button', { class: 'btn sm danger-outline', disabled: !mod || i.revoked, onclick: async () => {
            if (await confirmDanger('作废邀请码', `作废 ${i.code}？已经用它注册的账号不受影响。`, '作废') && await act('邀请码已作废', () => api('POST', '/admin/api/invites/' + encodeURIComponent(i.code) + '/revoke'))) load().catch(showErr);
          } }, '作废')) },
      ], res.items, '还没有邀请码'), pager(res, (p) => { f.page = p; load().catch(showErr); }));
    }
    fill(root, modeBox,
      section('生成邀请码', null, h('form', { class: 'card pad toolbar', onsubmit: async (ev) => {
        ev.preventDefault();
        const body = { count: Number(count.value) || 1, max_uses: Number(maxUses.value) || 1, note: note.value.trim() };
        if (expires.value) body.expires_at = new Date(expires.value).getTime();
        const made = await act(null, () => api('POST', '/admin/api/invites', body));
        if (made) { toast(`已生成 ${made.length} 个邀请码`); note.value = ''; f.page = 1; load().catch(showErr); }
      } },
        field('数量', count), field('每个可用次数', maxUses), field('过期时间（可留空）', expires), h('div', { class: 'field grow' }, h('label', { for: (note.id = 'invite-note') }, '备注'), note),
        h('button', { class: 'btn primary', type: 'submit', disabled: !mod }, '生成'))),
      section('邀请码列表', null, box));
    await load();
  }

  // ---------- 页面：系统公告 ----------
  async function pageAnnouncements(root) {
    const box = h('div', { class: 'card' }, loading());
    const mod = can('moderator');
    const title = h('input', { type: 'text', maxlength: 100, required: true });
    const body = h('textarea', { maxlength: 5000, required: true });
    const expires = h('input', { type: 'datetime-local' });
    async function load() {
      const list = await api('GET', '/admin/api/announcements');
      const now = Date.now();
      fill(box, table([
        { title: '发布时间', render: (a) => fmtTime(a.created_at) },
        { title: '公告', render: (a) => h('div', { class: 'clip' }, h('strong', null, a.title), h('div', { class: 'muted small' }, a.body.length > 160 ? a.body.slice(0, 160) + '…' : a.body)) },
        { title: '截止时间', render: (a) => (a.expires_at ? [fmtTime(a.expires_at), ' ', a.expires_at <= now ? badge('已过期', 'warn') : null] : '长期有效') },
        { title: '发布人', render: (a) => a.created_by || '—' },
        { title: '', cls: 'actions', render: (a) => h('button', { class: 'btn sm danger-outline', disabled: !mod, onclick: async () => {
          if (await confirmDanger('撤下公告', '撤下后客户端不再能从历史里拉到它；已经推送出去的通知不会收回。', '撤下') && await act('公告已撤下', () => api('DELETE', '/admin/api/announcements/' + encodeURIComponent(a.id)))) load().catch(showErr);
        } }, '撤下') },
      ], list, '还没有发布过公告'));
    }
    fill(root, 
      section('发布公告', null, h('form', { class: 'card pad', onsubmit: async (ev) => {
        ev.preventDefault();
        const payload = { title: title.value.trim(), body: body.value.trim() };
        if (expires.value) payload.expires_at = new Date(expires.value).getTime();
        if (!await dialog({ title: '发布公告', message: `「${payload.title}」将立即推送给全部用户，发布后无法修改。`, confirmText: '发布' })) return;
        const res = await act(null, () => api('POST', '/admin/api/announcements', payload));
        if (res) { toast(`公告已发布，推送给 ${fmtNum(res.recipients)} 个用户`); title.value = body.value = expires.value = ''; load().catch(showErr); }
      } },
        field('标题', title), field('正文', body), field('截止时间（可留空，到期后客户端不再显示）', expires),
        h('button', { class: 'btn primary', type: 'submit', disabled: !mod }, '发布给全部用户'))),
      section('历史公告', null, box));
    await load();
  }

  // ---------- 页面：系统设置 ----------
  async function pageSettings(root) {
    const owner = can('owner');
    function render(d) {
      const inputs = {};
      const over = new Set(d.overridden);
      const REG = { open: '开放注册', invite: '仅邀请码', closed: '关闭注册' };
      const groups = [];
      for (const fd of d.fields) {
        let g = groups.find((x) => x.name === fd.group);
        if (!g) groups.push(g = { name: fd.group, fields: [] });
        g.fields.push(fd);
      }
      const save = async (ev) => {
        ev.preventDefault();
        const values = {};
        for (const [k, el] of Object.entries(inputs)) if (el.value !== d.values[k]) values[k] = el.value;
        if (!Object.keys(values).length) { toast('没有修改'); return; }
        const res = await act('设置已保存并立即生效', () => api('PUT', '/admin/api/settings', { values }));
        if (res) render(res);
      };
      fill(root, 
        owner ? null : h('div', { class: 'banner info' }, '系统设置只有所有者可以修改，当前为只读。'),
        h('form', { class: 'settings', onsubmit: save }, groups.map((g) => section(g.name, null, h('div', { class: 'card pad' }, g.fields.map((fd) => {
          let input;
          if (fd.kind === 'enum') input = h('select', { disabled: !owner }, fd.enum.map((v) => h('option', { value: v, selected: v === d.values[fd.key] }, REG[v] || v)));
          else if (fd.kind === 'list') input = h('input', { type: 'text', value: d.values[fd.key], disabled: !owner, placeholder: '留空表示不限' });
          else input = h('input', { type: 'number', value: d.values[fd.key], min: fd.min, max: fd.max, disabled: !owner, required: true });
          inputs[fd.key] = input;
          const hint = h('span', null, fd.help || '');
          const human = () => (fd.kind === 'bytes' ? `≈ ${fmtBytes(Number(input.value))}` : fd.kind === 'hours' && Number(input.value) ? `≈ ${(Number(input.value) / 24).toFixed(1)} 天` : '');
          const humanEl = h('span', null, human());
          input.addEventListener('input', () => { humanEl.textContent = human(); });
          const dv = d.defaults[fd.key];
          const def = fd.kind === 'enum' ? REG[dv] : fd.kind === 'bytes' ? `${fmtNum(Number(dv))}（${fmtBytes(Number(dv))}）` : fd.kind === 'list' ? dv || '不限' : fmtNum(Number(dv));
          return field([fd.label, ' ', over.has(fd.key) ? badge('已覆盖启动参数', 'info') : null], input,
            [hint, fd.help ? h('br') : null, humanEl, humanEl.textContent ? ' · ' : '', `默认值：${def} `,
              over.has(fd.key) && owner ? h('button', { class: 'btn link', type: 'button', onclick: async () => {
                const res = await act('已恢复默认值', () => api('PUT', '/admin/api/settings', { reset: [fd.key] }));
                if (res) render(res);
              } }, '恢复默认') : null]);
        })))),
          h('div', { class: 'btn-row' }, h('button', { class: 'btn primary', type: 'submit', disabled: !owner }, '保存设置'),
            h('span', { class: 'muted small' }, '保存后立即生效，不需要重启；每次修改都会记入审计日志。'))));
    }
    render(await api('GET', '/admin/api/settings'));
  }

  // ---------- 页面：存储与维护 ----------
  async function pageStorage(root) {
    const owner = can('owner');
    async function load() {
      const d = await api('GET', '/admin/api/storage');
      fill(root, 
        h('div', { class: 'grid stats section' },
          stat('附件', fmtNum(d.attachments) + ' 个', fmtBytes(d.attachment_bytes)),
          stat('离线队列', fmtNum(d.queue_items) + ' 条', fmtBytes(d.queue_bytes)),
          stat('数据库文件', fmtBytes(d.db_bytes)),
          stat('备份', fmtNum(d.backups.length) + ' 份', `保留最近 ${d.backup_keep} 份`)),
        section('维护任务', h('div', { class: 'btn-row' },
          h('button', { class: 'btn', disabled: !owner, onclick: async () => {
            const r = await act(null, () => api('POST', '/admin/api/maintenance/cleanup'));
            if (r) { toast(`清理完成：过期队列项 ${fmtNum(r.queue_items)} 条，闲置附件 ${fmtNum(r.attachments)} 个，临时文件 ${fmtNum(r.temp_files)} 个`); load().catch(showErr); }
          } }, '立即清理'),
          h('button', { class: 'btn primary', disabled: !owner, onclick: async () => {
            const r = await act(null, () => api('POST', '/admin/api/backups'));
            if (r) { toast(`备份完成：${r.backup.name}（${fmtBytes(r.backup.size)}）`); load().catch(showErr); }
          } }, '创建备份')),
          h('p', { class: 'muted small' }, '清理：删除入队超过 30 天的队列项、90 天未访问且未被引用的附件、残留的临时文件（与每小时的后台任务相同）。备份：SQLite VACUUM INTO 在线备份到数据目录的 backups/，不含离线队列里尚未投递的消息。')),
        section('备份文件', null, h('div', { class: 'card' }, table([
          { title: '文件名', render: (b) => h('code', null, b.name) },
          { title: '大小', right: true, render: (b) => fmtBytes(b.size) },
          { title: '创建时间', render: (b) => fmtTime(b.created_at) },
          { title: '', cls: 'actions', render: (b) => (owner ? h('a', { class: 'btn sm', href: '/admin/api/backups/' + encodeURIComponent(b.name), download: b.name }, '下载') : null) },
        ], d.backups, '还没有备份')),
          h('p', { class: 'muted small' }, '备份含账号资料、关系、朋友圈与管理员密码哈希，请按最高敏感级别保管；每次下载都会记入审计日志。备份不含任何消息：离线队列里尚未投递的加密信封已被剔除，用备份恢复后这些消息不会再投递。')),
        section('附件占用排行', null, h('div', { class: 'card' }, table([
          { title: '上传者', render: (u) => (u.email ? h('div', null, userLink(u.user_id, u.name), h('div', { class: 'muted small' }, u.email)) : h('span', { class: 'muted' }, '账号已删除 ', idCell(u.user_id))) },
          { title: '文件数', right: true, render: (u) => fmtNum(u.files) },
          { title: '占用', right: true, render: (u) => fmtBytes(u.bytes) },
        ], d.top_uploaders, '还没有附件'))));
    }
    await load();
  }

  // ---------- 页面：监控 ----------
  async function pageMonitor(root) {
    let prev = null;
    async function tick() {
      const d = await api('GET', '/admin/api/monitor');
      const m = d.metrics;
      let dbWait = '—', dbSub = '累计 ' + fmtNum(d.db.wait_count) + ' 次';
      if (prev) {
        const dc = d.db.wait_count - prev.db.wait_count, dm = d.db.wait_ms - prev.db.wait_ms;
        dbWait = dc > 0 ? fmtMs(dm / dc) + ' ms' : '0 ms';
        dbSub = `最近 5 秒排队 ${fmtNum(Math.max(0, dc))} 次 · 平均等待`;
      }
      prev = d;
      fill(root, 
        h('div', { class: 'grid stats section' },
          stat('在线连接', fmtNum(d.online)),
          stat('每秒请求数', m.rps.toFixed(1), '最近 5 秒平均'),
          stat('数据库写队列等待', dbWait, dbSub),
          stat('队列积压', fmtNum(d.queue_items) + ' 条', fmtBytes(d.queue_bytes)),
          stat('Go 协程', fmtNum(d.runtime.goroutines)),
          stat('内存', fmtBytes(d.runtime.heap_alloc), `向系统申请 ${fmtBytes(d.runtime.sys)}`),
          stat('5xx / panic', `${fmtNum(m.errors_5xx)} / ${fmtNum(m.panics)}`, `累计请求 ${fmtNum(m.requests)}`),
          stat('运行时长', fmtDuration(m.uptime_seconds))),
        section('各接口耗时', h('span', { class: 'muted small' }, `每 5 秒刷新 · 分位数取最近约 5 分钟 · 更新于 ${fmtTime(d.ts).slice(11)}`), h('div', { class: 'card' }, table([
          { title: '接口', render: (r) => h('code', null, r.route) },
          { title: '累计请求', right: true, render: (r) => fmtNum(r.count) },
          { title: '5xx', right: true, render: (r) => (r.errors ? badge(fmtNum(r.errors), 'danger') : '0') },
          { title: '近 5 分钟', right: true, render: (r) => fmtNum(r.window_count) },
          { title: 'P50 (ms)', right: true, render: (r) => (r.window_count ? fmtMs(r.p50_ms) : '—') },
          { title: 'P95 (ms)', right: true, render: (r) => (r.window_count ? fmtMs(r.p95_ms) : '—') },
          { title: 'P99 (ms)', right: true, render: (r) => (r.window_count ? fmtMs(r.p99_ms) : '—') },
        ], m.routes, '还没有请求'))),
        section('最近的 5xx 与 panic', h('a', { class: 'btn sm', href: '/metrics', target: '_blank', rel: 'noopener' }, 'Prometheus 指标 /metrics'), h('div', { class: 'card' }, table([
          { title: '时间', render: (i) => fmtTime(i.ts) },
          { title: '类型', render: (i) => badge(i.kind, i.kind === 'panic' ? 'danger' : 'warn') },
          { title: '请求', render: (i) => h('code', null, `${i.method} ${i.path}`) },
          { title: '状态', right: true, render: (i) => i.status },
          { title: '详情', render: (i) => h('div', { class: 'clip small mono' }, (i.detail || '').split('\n')[0]) },
        ], m.recent, '没有 5xx 或 panic 记录'))));
    }
    await tick();
    state.timer = setInterval(() => { if (!document.hidden) tick().catch(() => {}); }, 5000);
  }

  // ---------- 页面：管理员 ----------
  async function pageAdmins(root) {
    const box = h('div', { class: 'card' }, loading());
    const roleOptions = () => Object.entries(ROLE_NAME).map(([v, t]) => [v, `${t}（${v}）`]);
    async function update(a, body, okMsg) {
      if (await act(okMsg, () => api('PUT', '/admin/api/admins/' + encodeURIComponent(a.id), body))) {
        if (a.id === state.admin.id) { toast('你修改了自己的账号，需要重新登录'); boot(); } else load().catch(showErr);
      }
    }
    async function load() {
      const list = await api('GET', '/admin/api/admins');
      fill(box, table([
        { title: '用户名', render: (a) => [h('strong', null, a.username), a.id === state.admin.id ? h('span', { class: 'muted small' }, '（当前）') : null] },
        { title: '角色', render: (a) => badge(ROLE_NAME[a.role] || a.role, a.role === 'owner' ? 'ok' : a.role === 'moderator' ? 'info' : '') },
        { title: '状态', render: (a) => (a.disabled ? badge('已停用', 'danger') : a.locked_until > Date.now() ? badge('已锁定', 'warn') : badge('正常', 'ok')) },
        { title: '创建时间', render: (a) => fmtTime(a.created_at) },
        { title: '最近登录', render: (a) => fmtTime(a.last_login_at) },
        { title: '', cls: 'actions', render: (a) => h('div', { class: 'btn-row' },
          h('button', { class: 'btn sm', onclick: async () => {
            const v = await dialog({ title: '修改角色：' + a.username, fields: [{ name: 'role', label: '角色', type: 'select', options: roleOptions(), value: a.role }], confirmText: '保存', message: '修改后该管理员需要重新登录。' });
            if (v && v.role !== a.role) update(a, { role: v.role }, '角色已修改');
          } }, '改角色'),
          h('button', { class: 'btn sm', onclick: async () => {
            const v = await dialog({ title: '重置密码：' + a.username, message: '重置后该管理员的全部会话失效。', confirmText: '重置', danger: true,
              fields: [{ name: 'password', label: '新密码（至少 12 位）', type: 'password', autocomplete: 'new-password', required: true, minlength: 12 }] });
            if (v) update(a, { password: v.password }, '密码已重置');
          } }, '重置密码'),
          h('button', { class: 'btn sm', onclick: async () => {
            if (a.disabled) update(a, { disabled: false }, '管理员已启用');
            else if (await confirmDanger('停用管理员', `停用 ${a.username}？其会话立即失效，无法再登录。`, '停用')) update(a, { disabled: true }, '管理员已停用');
          } }, a.disabled ? '启用' : '停用'),
          h('button', { class: 'btn sm danger-outline', disabled: a.id === state.admin.id, onclick: async () => {
            if (await confirmDanger('删除管理员', `永久删除管理员 ${a.username}？其操作记录仍保留在审计日志里。`, '删除') && await act('管理员已删除', () => api('DELETE', '/admin/api/admins/' + encodeURIComponent(a.id)))) load().catch(showErr);
          } }, '删除')) },
      ], list));
    }
    fill(root, 
      section('管理员', h('button', { class: 'btn primary', onclick: async () => {
        const v = await dialog({ title: '新建管理员', confirmText: '创建', fields: [
          { name: 'username', label: '用户名（3–32 位小写字母、数字、_ . -）', required: true, autocomplete: 'off' },
          { name: 'password', label: '密码（至少 12 位）', type: 'password', autocomplete: 'new-password', required: true, minlength: 12 },
          { name: 'role', label: '角色', type: 'select', options: roleOptions(), value: 'viewer' }] });
        if (v && await act('管理员已创建', () => api('POST', '/admin/api/admins', v))) load().catch(showErr);
      } }, '新建管理员'), box),
      h('div', { class: 'card pad' }, h('dl', { class: 'kv' },
        h('dt', null, '所有者'), h('dd', null, '全部权限，包括管理员管理、系统设置、备份与维护、审计日志。'),
        h('dt', null, '审核员'), h('dd', null, '用户与内容管理：停用 / 删除账号、群组、朋友圈、举报、邀请码、公告；不能改系统设置和管理员。'),
        h('dt', null, '只读'), h('dd', null, '只能查看，不能执行任何写操作。'))));
    await load();
  }

  // ---------- 页面：审计日志 ----------
  async function pageAudit(root) {
    const f = { admin: '', action: '', from: '', to: '', page: 1 };
    const box = h('div', { class: 'card' }, loading());
    const admin = h('input', { type: 'text', id: 'audit-admin', placeholder: '用户名' });
    const action = h('input', { type: 'text', id: 'audit-action', placeholder: '如 user. 或 user.disable', list: 'audit-actions' });
    const from = h('input', { type: 'date', id: 'audit-from' });
    const to = h('input', { type: 'date', id: 'audit-to' });
    const ACTIONS = ['auth.', 'user.', 'group.', 'moment.', 'comment.', 'report.', 'invite.', 'announcement.', 'settings.', 'backup.', 'maintenance.', 'admin.'];
    async function load() {
      const res = await api('GET', '/admin/api/audit' + qs({ admin: f.admin, action: f.action, page: f.page, size: 30,
        from: f.from ? new Date(f.from + 'T00:00:00').getTime() : '', to: f.to ? new Date(f.to + 'T23:59:59.999').getTime() : '' }));
      fill(box, table([
        { title: '时间', render: (a) => fmtTime(a.ts) },
        { title: '管理员', render: (a) => a.admin_name },
        { title: '动作', render: (a) => h('code', null, a.action) },
        { title: '对象', render: (a) => h('code', { title: a.target }, a.target.length > 30 ? a.target.slice(0, 28) + '…' : a.target || '—') },
        { title: '参数摘要', render: (a) => h('div', { class: 'clip small mono' }, a.detail || '—') },
        { title: '来源 IP', render: (a) => a.ip },
      ], res.items, '没有符合条件的记录'), pager(res, (p) => { f.page = p; load().catch(showErr); }));
    }
    fill(root, 
      h('form', { class: 'toolbar', role: 'search', onsubmit: (ev) => { ev.preventDefault(); f.admin = admin.value.trim().toLowerCase(); f.action = action.value.trim(); f.from = from.value; f.to = to.value; f.page = 1; load().catch(showErr); } },
        h('div', { class: 'field' }, h('label', { for: 'audit-admin' }, '管理员'), admin),
        h('div', { class: 'field' }, h('label', { for: 'audit-action' }, '动作'), action, h('datalist', { id: 'audit-actions' }, ACTIONS.map((a) => h('option', { value: a })))),
        h('div', { class: 'field' }, h('label', { for: 'audit-from' }, '起始日期'), from),
        h('div', { class: 'field' }, h('label', { for: 'audit-to' }, '结束日期'), to),
        h('button', { class: 'btn primary', type: 'submit' }, '查询')),
      h('p', { class: 'muted small section' }, '审计日志记录每一次写操作，只能追加，后台与数据库层面都不允许修改或删除。'),
      box);
    await load();
  }

  // ---------- 路由与框架 ----------
  const NAV = [
    { group: '总览' },
    { path: 'dashboard', title: '概览', page: pageDashboard },
    { path: 'monitor', title: '实时监控', page: pageMonitor },
    { group: '用户与内容' },
    { path: 'users', title: '用户', page: pageUsers, detail: pageUser, detailTitle: '用户详情' },
    { path: 'groups', title: '群组', page: pageGroups, detail: pageGroup, detailTitle: '群组详情' },
    { path: 'moments', title: '朋友圈审核', page: pageMoments },
    { path: 'reports', title: '举报', page: pageReports },
    { group: '运营' },
    { path: 'invites', title: '注册控制', page: pageInvites },
    { path: 'announcements', title: '系统公告', page: pageAnnouncements },
    { group: '系统' },
    { path: 'settings', title: '系统设置', page: pageSettings },
    { path: 'storage', title: '存储与维护', page: pageStorage },
    { path: 'admins', title: '管理员', page: pageAdmins, role: 'owner' },
    { path: 'audit', title: '审计日志', page: pageAudit, role: 'owner' },
  ];

  function showErr(e) {
    if (e && e.status === 401) return;
    toast((e && e.message) || '出错了', true);
  }

  function parseHash() {
    const raw = location.hash.replace(/^#\/?/, '') || 'dashboard';
    const [pathPart, query] = raw.split('?');
    const [path, id] = pathPart.split('/');
    return { path, id: id ? decodeURIComponent(id) : '', params: new URLSearchParams(query || '') };
  }

  async function route() {
    if (state.timer) { clearInterval(state.timer); state.timer = null; }
    if (!state.admin) return;
    const { path, id, params } = parseHash();
    const item = NAV.find((n) => n.path === path && (!n.role || can(n.role))) || NAV[1];
    document.querySelectorAll('.nav a').forEach((a) => {
      if (a.dataset.path === item.path) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
    });
    document.querySelector('.shell').classList.remove('nav-open');
    const title = id && item.detail ? item.detailTitle : item.title;
    document.getElementById('page-title').textContent = title;
    document.title = title + ' · Hush 管理后台';
    const root = document.getElementById('content');
    fill(root, loading());
    try {
      if (id && item.detail) await item.detail(root, id, params); else await item.page(root, id, params);
    } catch (e) {
      if (e && e.status === 401) return;
      fill(root, h('div', { class: 'card empty' }, (e && e.message) || '加载失败'));
    }
  }

  function applyTheme(theme) {
    if (theme === 'light' || theme === 'dark') document.documentElement.setAttribute('data-theme', theme);
    else document.documentElement.removeAttribute('data-theme');
  }
  function currentTheme() {
    const t = document.documentElement.getAttribute('data-theme');
    return t || (window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
  }
  function toggleTheme() {
    const next = currentTheme() === 'dark' ? 'light' : 'dark';
    applyTheme(next);
    try { localStorage.setItem('hush-admin-theme', next); } catch (e) { /* 隐私模式下忽略 */ }
  }

  function renderShell() {
    const a = state.admin;
    const nav = h('nav', { class: 'nav', 'aria-label': '主导航' }, NAV.filter((n) => !n.role || can(n.role)).map((n) =>
      (n.group ? h('div', { class: 'nav-group' }, n.group) : h('a', { href: '#/' + n.path, 'data-path': n.path }, n.title))));
    const shell = h('div', { class: 'shell' },
      h('aside', { class: 'sidebar' }, h('div', { class: 'brand' }, h('img', { src: '/static/favicon.svg', alt: '' }), 'Hush 管理后台'), nav,
        h('p', { class: 'muted small nav-group' }, '版本 ' + state.version)),
      h('div', { class: 'main' },
        h('header', { class: 'topbar' },
          h('button', { class: 'btn menu-btn', 'aria-label': '打开导航', onclick: () => shell.classList.toggle('nav-open') }, '☰'),
          h('h1', { id: 'page-title' }, ''),
          h('div', { class: 'who' }, h('span', { class: 'name' }, a.username), badge(ROLE_NAME[a.role] || a.role, a.role === 'owner' ? 'ok' : a.role === 'moderator' ? 'info' : '')),
          h('button', { class: 'btn sm', onclick: toggleTheme, 'aria-label': '切换浅色 / 深色' }, '浅色 / 深色'),
          h('button', { class: 'btn sm', onclick: changePassword }, '修改密码'),
          h('button', { class: 'btn sm', onclick: async () => { await act(null, () => api('POST', '/admin/api/logout')); state.admin = null; renderLogin(); } }, '退出')),
        h('main', { class: 'content', id: 'content' })));
    fill($app(), shell);
  }

  async function changePassword() {
    const v = await dialog({ title: '修改密码', message: '修改后需要重新登录。', confirmText: '修改', fields: [
      { name: 'old_password', label: '当前密码', type: 'password', autocomplete: 'current-password', required: true },
      { name: 'new_password', label: '新密码（至少 12 位）', type: 'password', autocomplete: 'new-password', required: true, minlength: 12 }] });
    if (v && await act('密码已修改，请重新登录', () => api('POST', '/admin/api/me/password', v))) { state.admin = null; renderLogin(); }
  }

  function renderLogin(msg) {
    if (state.timer) { clearInterval(state.timer); state.timer = null; }
    document.querySelectorAll('dialog').forEach((d) => d.remove());
    document.title = '登录 · Hush 管理后台';
    const user = h('input', { type: 'text', id: 'login-user', autocomplete: 'username', required: true, autocapitalize: 'none', spellcheck: 'false' });
    const pass = h('input', { type: 'password', id: 'login-pass', autocomplete: 'current-password', required: true });
    const err = h('div', { class: 'err', role: 'alert' }, msg || '');
    const btn = h('button', { class: 'btn primary', type: 'submit' }, '登录');
    fill($app(), h('div', { class: 'login' }, h('form', { class: 'card', onsubmit: async (ev) => {
      ev.preventDefault();
      btn.disabled = true; err.textContent = '';
      try {
        const res = await api('POST', '/admin/api/login', { username: user.value, password: pass.value });
        state.admin = res.admin; state.csrf = res.csrf; state.version = res.version;
        renderShell(); route();
      } catch (e) { err.textContent = e.message; pass.value = ''; pass.focus(); }
      btn.disabled = false;
    } },
      h('div', { class: 'brand' }, h('img', { src: '/static/favicon.svg', alt: '' }), 'Hush 管理后台'),
      h('h1', null, '登录'),
      h('p', { class: 'muted small field' }, '没有默认账号：首个管理员用 hush-server admin create 创建。'),
      h('div', { class: 'field' }, h('label', { for: 'login-user' }, '用户名'), user),
      h('div', { class: 'field' }, h('label', { for: 'login-pass' }, '密码'), pass),
      btn, err)));
    user.focus();
  }

  async function boot() {
    try {
      const me = await api('GET', '/admin/api/me');
      state.admin = me.admin; state.csrf = me.csrf; state.version = me.version;
      renderShell(); route();
    } catch (e) { state.admin = null; renderLogin(e.status === 401 ? '' : e.message); }
  }

  try { applyTheme(localStorage.getItem('hush-admin-theme')); } catch (e) { /* 忽略 */ }
  window.addEventListener('hashchange', route);
  boot();
})();
