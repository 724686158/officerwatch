/* officerwatch 前端 —— 无框架、无构建、一个文件。
 *
 * 为什么不用框架：这个页面只有 6 个视图、一张主表，引入 React/Vite 会带来
 * node_modules、构建产物、版本漂移三件套；而它要嵌进 Go 二进制里用 go:embed 分发。
 * 前身的教训不是"别用框架"，是"1700 行无结构"。
 *
 * 约定：
 *   - 所有请求走 api()，自动带 BASE（页面挂在 /officerwatch/ 子路径下）
 *   - 所有 DOM 走模板字符串 + esc()，不接受未转义的用户数据
 *   - 状态都在 state 里，渲染函数只读 state
 */
(() => {
  'use strict';

  // ───────────────────────────── 基础设施

  function basePath() {
    let p = location.pathname;
    if (!p.endsWith('/')) {
      const seg = p.split('/').pop() || '';
      p = seg.includes('.') ? p.slice(0, p.lastIndexOf('/') + 1) : p + '/';
    }
    return p;
  }
  const BASE = basePath();

  async function api(path, opts = {}) {
    const res = await fetch(BASE + path, {
      headers: { 'Content-Type': 'application/json' },
      ...opts,
    });
    if (res.status === 401) {
      // 会话过期：回登录页（带一个回跳参数，登录后能回到原来那页）
      location.href = BASE + 'login';
      throw new Error('未登录');
    }
    const text = await res.text();
    let data = null;
    try { data = text ? JSON.parse(text) : null; } catch { data = { error: text }; }
    if (!res.ok) throw new Error((data && data.error) || ('HTTP ' + res.status));
    return data;
  }

  const esc = (s) => String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');

  const $ = (sel, root = document) => root.querySelector(sel);

  function fmtISK(v) {
    if (!v) return '0';
    if (v >= 1e9) return (v / 1e9).toFixed(2) + 'B';
    if (v >= 1e6) return (v / 1e6).toFixed(1) + 'M';
    if (v >= 1e3) return (v / 1e3).toFixed(0) + 'K';
    return String(Math.round(v));
  }

  function fmtInt(v) { return (v || 0).toLocaleString('en-US'); }

  function fmtTime(iso) {
    if (!iso) return '—';
    const d = new Date(iso);
    if (isNaN(d)) return '—';
    const p = (n) => String(n).padStart(2, '0');
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
  }

  function timeAgo(iso) {
    if (!iso) return '—';
    const d = new Date(iso);
    if (isNaN(d)) return '—';
    const s = (Date.now() - d.getTime()) / 1000;
    if (s < 60) return Math.round(s) + ' 秒前';
    if (s < 3600) return Math.round(s / 60) + ' 分钟前';
    if (s < 86400) return Math.round(s / 3600) + ' 小时前';
    return Math.round(s / 86400) + ' 天前';
  }

  const ROLE_LABEL = { npc_kill: '官员击杀', officer_down: '官员被爆' };
  const CLASS_ZH = { battleship: '战列', cruiser: '巡洋', frigate: '护卫' };
  const STATUS_TONE = {
    sent: ['good', '已推送'], filtered: ['', '已过滤'], failed: ['bad', '失败'],
    no_channel: ['warn', '无通道'], legacy: ['', '历史'], pending: ['warn', '待发送'],
  };

  // 舰船图：走自家代理（带缓存），取不到就隐藏，保留文字信息
  function shipImg(typeId, size, kind = 'render', cls = '') {
    if (!typeId) return '';
    // render 拿不到就退回 icon，再失败才隐藏（保留文字信息）
    const onerr = kind === 'render'
      ? "this.dataset.f='1';this.src=this.src.replace('kind=render','kind=icon')"
      : "this.style.visibility='hidden'";
    return `<img class="${cls}" src="${BASE}api/ship/${typeId}?size=${size}&kind=${kind}"
      alt="" loading="lazy" onerror="${onerr.replace(/"/g, '&quot;')}">`;
  }

  function chip(tone, text) {
    return `<span class="chip ${tone || ''}">${esc(text)}</span>`;
  }

  function statusChip(k) {
    const [tone, label] = STATUS_TONE[k.notifyStatus] || ['', k.notifyStatus || '—'];
    const title = k.notifyError ? ` title="${esc(k.notifyError)}"` : '';
    return `<span class="chip ${tone}"${title}>${esc(label)}</span>`;
  }

  function empty(text, icon = '🩸') {
    return `<div class="empty"><div class="big">${icon}</div>${esc(text)}</div>`;
  }

  let toastTimer = null;
  function toast(msg, kind = '') {
    const t = $('#toast');
    t.textContent = msg;
    t.className = 'toast ' + kind;
    t.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { t.hidden = true; }, 3200);
  }

  // ───────────────────────────── 主题

  function applyTheme(mode) {
    const dark = mode === 'dark' || (mode === 'auto' && matchMedia('(prefers-color-scheme: dark)').matches);
    if (mode === 'auto') document.documentElement.removeAttribute('data-theme');
    else document.documentElement.setAttribute('data-theme', dark ? 'dark' : 'light');
    localStorage.setItem('ow-theme', mode);
  }

  function cycleTheme() {
    const cur = localStorage.getItem('ow-theme') || 'auto';
    const next = cur === 'auto' ? 'light' : cur === 'light' ? 'dark' : 'auto';
    applyTheme(next);
    toast('主题：' + ({ auto: '跟随系统', light: '浅色', dark: '深色' })[next]);
  }

  // ───────────────────────────── 状态

  const state = {
    tab: 'overview',
    overview: null,
    kills: { items: [], total: 0, limit: 50, offset: 0 },
    filters: { faction: '', role: '', minValue: '', q: '', sort: 'time' },
    officers: [],
    officersSyncedAt: '',
    stats: null,
    settings: null,
    channels: null,
    status: null,
  };

  // ───────────────────────────── 概览

  function kpiCard(label, value, note, tone) {
    return `<div class="kpi ${tone || ''}">
      <div class="label">${esc(label)}</div>
      <div class="v">${value}</div>
      <div class="note">${esc(note || '')}</div>
    </div>`;
  }

  function barChart(days) {
    const total = days.reduce((a, d) => a + d.kills, 0);
    if (!total) {
      return `<div class="empty" style="padding:28px 16px">
        <div class="big">📉</div>
        这个区间没有新击杀${state.overview && state.overview.overview.total ? '（历史记录在更早的时间）' : '，采集器正在等'}</div>`;
    }
    const max = Math.max(1, ...days.map(d => d.kills));
    const cols = days.map((d) => {
      const h = d.kills ? Math.max(3, Math.round((d.kills / max) * 100)) : 0;
      const label = d.date.slice(5);
      const tip = `${d.date}：${d.kills} 次 · ${fmtISK(d.value)} ISK`;
      return `<div class="col" title="${esc(tip)}">
        <div class="bar ${d.kills ? '' : 'zero'}" style="height:${h}%"></div>
        <div class="x">${esc(label)}</div>
      </div>`;
    }).join('');
    return `<div class="chart">${cols}</div>`;
  }

  async function renderOverview() {
    const d = await api('api/overview');
    state.overview = d;
    const o = d.overview || {};
    const factions = (d.factions || []);
    const maxF = Math.max(1, ...factions.map(f => f.kills));

    const rows = (d.recent || []).map((k) => `
      <tr class="clickable" data-kill="${k.killmailId}">
        <td class="time">${esc(fmtTime(k.killmailTime))}</td>
        <td><span class="shipCell">${shipImg(k.officerTypeId, 32, 'render', 'shipIcon')}<span class="strong">${esc(k.officerName)}<span class="sub">${esc([k.officerZh, k.faction].filter(Boolean).join(' · '))}</span></span></span></td>
        <td>${chip('', ROLE_LABEL[k.role] || k.role)}</td>
        <td><span class="shipCell">${shipImg(k.victimShipTypeId, 32, 'render', 'shipIcon')}<span>${esc(k.victimShipName || ('type ' + k.victimShipTypeId))}<span class="sub">${esc(k.victimName || '')}</span></span></span></td>
        <td>${esc(k.systemName || '—')}<span class="sub" title="${esc(k.locationName || k.regionName || '')}">${esc(k.locationName || k.regionName || '')}</span></td>
        <td class="num mono">${fmtISK(k.totalValue)}</td>
        <td>${statusChip(k)}</td>
      </tr>`).join('');

    $('#tab-overview').innerHTML = `
      <div class="page-head">
        <div>
          <h1>概览</h1>
          <p>全服官员击杀雷达 · 数据源 ${esc(d.source === 'zkb' ? 'zKillboard 轮询' : d.source === 'both' ? 'R2Z2 + zKillboard' : 'zKillboard R2Z2 序列流')}</p>
        </div>
      </div>

      <div class="kpis">
        ${kpiCard('今日', fmtInt(o.today), '击杀次数')}
        ${kpiCard('近 7 天', fmtInt(o.last7d), '击杀次数')}
        ${kpiCard('累计', fmtInt(o.total), '入库总数')}
        ${kpiCard('今日损失', fmtISK(o.valueToday), 'ISK', 'accent')}
        ${kpiCard('单笔最大', fmtISK(o.biggest), 'ISK', 'good')}
        ${kpiCard('监控中', fmtInt(o.officersOn) + ' / ' + fmtInt(o.officersAll), '官员 / 名录', 'accent')}
      </div>

      <div class="grid-2">
        <div class="card">
          <header><h2>最近 14 天</h2><span class="hint">柱高 = 当日击杀数</span></header>
          <div class="body">${barChart(d.daily || [])}</div>
        </div>
        <div class="card">
          <header><h2>阵营分布</h2><span class="hint">累计</span></header>
          <div class="body">
            ${factions.length ? `<div class="bars">${factions.map(f => `
              <div class="row">
                <span>${esc(f.faction || '未知')}</span>
                <span class="track"><span class="fill" style="width:${Math.round(f.kills / maxF * 100)}%"></span></span>
                <span class="val">${fmtInt(f.kills)}</span>
              </div>`).join('')}</div>` : empty('还没有数据')}
          </div>
        </div>
      </div>

      <div class="card" style="margin-top:16px">
        <header><h2>最近入库</h2><a class="hint" href="#kills">查看全部 →</a></header>
        ${rows ? `<div class="tbl-wrap"><table class="tbl">
          <thead><tr><th>时间</th><th>官员</th><th>类型</th><th class="hide-sm">受害者座舰</th><th>地点</th><th class="num">价值</th><th>状态</th></tr></thead>
          <tbody>${rows}</tbody></table></div>` : empty('还没有命中记录 —— 采集器在等第一位官员出手')}
      </div>`;
  }

  // ───────────────────────────── 击杀列表

  async function renderKills() {
    const f = state.filters;
    const q = new URLSearchParams();
    q.set('limit', state.kills.limit);
    q.set('offset', state.kills.offset);
    if (f.faction) q.set('faction', f.faction);
    if (f.role) q.set('role', f.role);
    if (f.minValue) q.set('minValue', f.minValue);
    if (f.q) q.set('q', f.q);
    if (f.sort) q.set('sort', f.sort);
    const d = await api('api/kills?' + q.toString());
    state.kills.items = d.items || [];
    state.kills.total = d.total || 0;

    const factions = [...new Set(state.officers.map(o => o.faction))].sort();
    const rows = state.kills.items.map((k) => `
      <tr class="clickable" data-kill="${k.killmailId}">
        <td class="time">${esc(fmtTime(k.killmailTime))}</td>
        <td><span class="shipCell">${shipImg(k.officerTypeId, 32, 'render', 'shipIcon')}<span class="strong">${esc(k.officerName)}<span class="sub">${esc([k.officerZh, k.faction].filter(Boolean).join(' · '))}</span></span></span></td>
        <td>${chip('', ROLE_LABEL[k.role] || k.role)}</td>
        <td>${esc(k.victimName || (k.victimCharId ? String(k.victimCharId) : '—'))}<span class="sub"><span class="shipCell">${shipImg(k.victimShipTypeId, 24, 'render', 'shipIcon')}<span>${esc(k.victimShipName || '')}</span></span></span></td>
        <td>${esc(k.systemName || '—')}<span class="sub" title="${esc(k.locationName || k.regionName || '')}">${esc(k.locationName || k.regionName || '')}</span></td>
        <td class="num mono strong">${fmtISK(k.totalValue)}</td>
        <td>${statusChip(k)}</td>
        <td class="num"><a href="${esc(k.zkbUrl)}" target="_blank" rel="noopener" onclick="event.stopPropagation()">zkb</a></td>
      </tr>`).join('');

    const total = state.kills.total;
    const from = total ? state.kills.offset + 1 : 0;
    const to = Math.min(state.kills.offset + state.kills.limit, total);

    $('#tab-kills').innerHTML = `
      <div class="page-head">
        <div>
          <h1>击杀记录</h1>
          <p>共 ${fmtInt(total)} 条 · 当前 ${from}–${to}</p>
        </div>
        <div class="actions">
          <button class="btn" data-action="import">＋ KillID 导入</button>
        </div>
      </div>

      <div class="filters">
        <select data-filter="faction">
          <option value="">全部阵营</option>
          ${factions.map(x => `<option value="${esc(x)}" ${f.faction === x ? 'selected' : ''}>${esc(x)}</option>`).join('')}
        </select>
        <select data-filter="role">
          <option value="">全部类型</option>
          <option value="npc_kill" ${f.role === 'npc_kill' ? 'selected' : ''}>官员击杀</option>
          <option value="officer_down" ${f.role === 'officer_down' ? 'selected' : ''}>官员被爆</option>
        </select>
        <input type="number" data-filter="minValue" placeholder="最低价值 ISK" value="${esc(f.minValue)}" />
        <input type="text" class="grow" data-filter="q" placeholder="搜星系 / 星域 / 受害者 ID / 座舰" value="${esc(f.q)}" />
        <select data-filter="sort">
          <option value="time" ${f.sort === 'time' ? 'selected' : ''}>按时间</option>
          <option value="value" ${f.sort === 'value' ? 'selected' : ''}>按价值</option>
        </select>
        <button class="btn" data-action="resetFilters">重置</button>
      </div>

      <div class="card">
        ${rows ? `<div class="tbl-wrap"><table class="tbl">
          <thead><tr><th>时间</th><th>官员</th><th>类型</th><th class="hide-sm">受害者</th><th>地点</th><th class="num">价值</th><th>状态</th><th class="num">链接</th></tr></thead>
          <tbody>${rows}</tbody></table></div>` : empty('没有符合条件的记录')}
        <div class="form-foot" style="padding:10px 16px;margin:0">
          <button class="btn small" data-action="page" data-off="${state.kills.offset - state.kills.limit}" ${state.kills.offset <= 0 ? 'disabled' : ''}>上一页</button>
          <button class="btn small" data-action="page" data-off="${state.kills.offset + state.kills.limit}" ${to >= total ? 'disabled' : ''}>下一页</button>
          <span class="spacer"></span>
          <span class="hint" style="color:var(--faint)">点任意一行看详情</span>
        </div>
      </div>`;
  }

  // ───────────────────────────── 图鉴

  async function renderOfficers() {
    const d = await api('api/officers');
    state.officers = d.items || [];
    state.officersSyncedAt = d.syncedAt || '';

    const byFaction = {};
    for (const o of state.officers) {
      (byFaction[o.faction || '未知'] ||= []).push(o);
    }
    const onCount = state.officers.filter(o => o.enabled).length;

    const groups = Object.keys(byFaction).sort().map((faction) => {
      const items = byFaction[faction].sort((a, b) => b.killCount - a.killCount || a.name.localeCompare(b.name));
      const factionZh = (byFaction[faction][0] || {}).factionZh || '';
      return `<div class="faction-head">
          <h3>${esc(faction)}${factionZh ? ` <span class="zh">${esc(factionZh)}</span>` : ''}</h3>
          ${chip('accent', items.length + ' 位')}
          <span class="line"></span>
        </div>
        <div class="officer-grid">
          ${items.map(o => `
            <div class="officer ${o.enabled ? '' : 'off'}">
              ${shipImg(o.typeId, 64, 'render', 'thumb')}
              <div class="info">
                <div class="name">${esc(o.name)}</div>
                <div class="zh"><span class="zhname">${esc(o.zh || '—')}</span>${o.shipClassZh ? `<span class="cls">${esc(o.shipClassZh)}</span>` : ''}</div>
                <div class="meta" title="${esc([o.groupName, o.lastSeen ? '最近 ' + fmtTime(o.lastSeen) : ''].filter(Boolean).join(' · '))}">船型 ${o.typeId}${o.characterId ? ` · 官员 #${o.characterId}` : ''}</div>
                <div class="meta" title="${esc([o.groupName, o.lastSeen ? '最近 ' + fmtTime(o.lastSeen) : ''].filter(Boolean).join(' · '))}">${o.killCount
                  ? `${fmtInt(o.killCount)} 次 · 最高 ${fmtISK(o.maxValue)}`
                  : (o.enabled ? '已监控，还没观测到' : '未开启监控')}</div>
              </div>
              <label class="switch" title="${o.enabled ? '已开启监控' : '已关闭'}">
                <input type="checkbox" ${o.enabled ? 'checked' : ''} data-action="toggleOfficer" data-id="${o.typeId}">
                <span></span>
              </label>
            </div>`).join('')}
        </div>`;
    }).join('');

    $('#tab-officers').innerHTML = `
      <div class="page-head">
        <div>
          <h1>官员图鉴</h1>
          <p>${state.officers.length} 位官员 · 监控中 ${onCount} 位 · 名录同步 ${state.officersSyncedAt ? timeAgo(state.officersSyncedAt) : '从未'}</p>
        </div>
        <div class="actions">
          <button class="btn" data-action="syncCatalog">同步名录</button>
          <button class="btn ghost" data-action="deepScan" title="扫完 Entity 分类下 415 个船组，找新官员">深度重扫</button>
        </div>
      </div>
      <div class="card" style="margin-bottom:8px">
        <div class="body tight" style="padding:12px 16px;color:var(--dim);font-size:var(--fs-sm)">
          名录来自 ESI 的 <b>Entity</b> 分类里名字含 “Officer” 的船组 —— 与客户端同源，不需要手工维护 ID。
          开关只影响“是否入库并推送”，不影响数据采集。
        </div>
      </div>
      ${groups || empty('名录是空的，点右上角“同步名录”')}`;
  }

  // ───────────────────────────── 统计

  async function renderStats() {
    const d = await api('api/stats?days=30');
    state.stats = d;
    const days = d.daily || [];
    const totalKills = days.reduce((a, b) => a + b.kills, 0);
    const totalValue = days.reduce((a, b) => a + b.value, 0);
    const maxO = Math.max(1, ...(d.topOfficers || []).map(o => o.killCount));

    $('#tab-stats').innerHTML = `
      <div class="page-head">
        <div><h1>统计</h1><p>近 30 天 ${fmtInt(totalKills)} 次 · 合计损失 ${fmtISK(totalValue)} ISK</p></div>
      </div>
      <div class="card">
        <header><h2>每日击杀（30 天）</h2><span class="hint">悬停看当日损失</span></header>
        <div class="body">${barChart(days)}</div>
      </div>
      <div class="grid-2" style="margin-top:16px">
        <div class="card">
          <header><h2>击杀最多的官员</h2><span class="hint">Top 10</span></header>
          <div class="body">
            ${(d.topOfficers || []).length ? `<div class="bars">${d.topOfficers.map(o => `
              <div class="row">
                <span title="${esc(o.name)}" style="overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(o.name)}</span>
                <span class="track"><span class="fill" style="width:${Math.round(o.killCount / maxO * 100)}%"></span></span>
                <span class="val">${fmtInt(o.killCount)}</span>
              </div>`).join('')}</div>` : empty('暂无数据')}
          </div>
        </div>
        <div class="card">
          <header><h2>阵营累计</h2></header>
          <div class="body">
            ${(d.factions || []).length ? `<div class="bars">${d.factions.map(f => `
              <div class="row">
                <span>${esc(f.faction || '未知')}</span>
                <span class="track"><span class="fill" style="width:${Math.round(f.kills / Math.max(1, ...d.factions.map(x => x.kills)) * 100)}%"></span></span>
                <span class="val">${fmtISK(f.value)}</span>
              </div>`).join('')}</div>` : empty('暂无数据')}
          </div>
        </div>
      </div>`;
  }

  // ───────────────────────────── 设置

  async function renderSettings() {
    const d = await api('api/settings');
    state.settings = d.settings;
    state.channels = d.channels;
    if (!state.officers.length) {
      try { state.officers = (await api('api/officers')).items || []; } catch { /* 忽略 */ }
    }
    const s = state.settings || {};
    const ch = state.channels || {};
    const factions = [...new Set(state.officers.map(o => o.faction))].filter(Boolean).sort();
    const sel = new Set(s.factions || []);

    const chanRow = (name, configured, detail) => `
      <div class="row" style="display:flex;align-items:center;gap:10px;padding:6px 0">
        ${chip(configured ? 'good' : '', configured ? '已配置' : '未配置')}
        <b>${esc(name)}</b>
        <span style="color:var(--dim)">${esc(detail || '')}</span>
      </div>`;

    $('#tab-settings').innerHTML = `
      <div class="page-head">
        <div><h1>设置</h1><p>推送口径与通知通道。凭据在服务器上的 config.json（0600），不进浏览器。</p></div>
      </div>

      <div class="card">
        <header><h2>推送口径</h2></header>
        <div class="body">
          <div class="form-grid">
            <label class="form-row">
              <span>监控角色</span>
              <select id="setRole">
                <option value="npc_kill" ${s.role === 'npc_kill' ? 'selected' : ''}>只报“官员打死玩家”</option>
                <option value="officer_down" ${s.role === 'officer_down' ? 'selected' : ''}>只报“玩家打死官员”</option>
                <option value="both" ${s.role === 'both' ? 'selected' : ''}>两者都报</option>
              </select>
              <span class="hint">官员被爆通常更值钱（掉官员装备）</span>
            </label>
            <label class="form-row">
              <span>最低价值（ISK）</span>
              <input type="number" id="setMinValue" min="0" step="1000000" value="${Number(s.minValue || 0)}">
              <span class="hint">0 = 不限；低于此值只入库不推送</span>
            </label>
            <label class="form-row">
              <span>每小时最多推送</span>
              <input type="number" id="setMaxPerHour" min="0" max="500" value="${Number(s.maxPerHour || 0)}">
              <span class="hint">0 = 不限；防止官员战区刷屏</span>
            </label>
            <label class="form-row">
              <span>家星系（算跳数）</span>
              <input type="text" id="setHome" placeholder="例如 Jita" value="${esc(s.homeSystem || '')}">
              <span class="hint">抢官员用：邮件会写"距 Jita N 跳"；留空则不显示</span>
            </label>
            <label class="form-row">
              <span>首次回填（小时）</span>
              <input type="number" id="setBackfill" min="0" max="24" value="${Number(s.backfillHours || 0)}">
              <span class="hint">仅冷启动且无游标时生效，上限 24（R2Z2 只留 24 小时）</span>
            </label>
          </div>

          <div class="form-row" style="margin-top:16px">
            <span>阵营白名单（不选 = 全部）</span>
            <div class="chips" id="factionChips">
              ${factions.map(f => `<span class="chip ${sel.has(f) ? 'on' : ''}" data-faction="${esc(f)}">${esc(f)}</span>`).join('') || '<span class="hint">先同步名录</span>'}
            </div>
          </div>

          <div class="form-grid" style="margin-top:16px">
            <div class="form-row">
              <span>静默时段</span>
              <div style="display:flex;align-items:center;gap:8px">
                <label class="switch"><input type="checkbox" id="setQuiet" ${s.quietEnabled ? 'checked' : ''}><span></span></label>
                <input type="number" id="setQuietStart" min="0" max="23" style="width:70px" value="${Number(s.quietStart ?? 2)}">
                <span style="color:var(--dim)">点 →</span>
                <input type="number" id="setQuietEnd" min="0" max="23" style="width:70px" value="${Number(s.quietEnd ?? 9)}">
                <span style="color:var(--dim)">点</span>
              </div>
              <span class="hint">静默期内只入库，不推送</span>
            </div>
            <div class="form-row">
              <span>通知开关</span>
              <div style="display:flex;align-items:center;gap:16px">
                <label class="field"><label class="switch"><input type="checkbox" id="setNotify" ${s.notifyEnabled ? 'checked' : ''}><span></span></label> 总开关</label>
                ${ch.onebot && ch.onebot.configured
                  ? `<label class="field"><label class="switch"><input type="checkbox" id="setAtAll" ${s.atAll ? 'checked' : ''}><span></span></label> 群里 @全体</label>`
                  : `<span class="hint" style="color:var(--faint)">（@全体需要 OneBot 机器人，当前未配置，该项不生效）</span>`}
              </div>
              <span class="hint">@全体只在 QQ 群里有效</span>
            </div>
          </div>

          <div class="form-row" style="margin-top:18px">
            <span>收件人（每条告警都会发给他们）</span>
            <div class="chips" id="rcptChips">
              ${(s.recipients || []).map(e => `<span class="chip on" data-email="${esc(e)}">${esc(e)} <b data-action="delRcpt" style="cursor:pointer;opacity:.6;margin-left:2px">✕</b></span>`).join('') || '<span class="hint">还没有收件人</span>'}
            </div>
            <div style="display:flex;gap:8px;margin-top:8px;max-width:520px">
              <input type="text" id="newRcpt" class="grow" placeholder="someone@example.com（可一次粘贴多个，逗号/分号分隔）">
              <button class="btn" data-action="addRcpt">添加</button>
            </div>
            <span class="hint">最多 20 个；改完点“保存设置”生效。留空则用 config.json 的 smtp.to</span>
          </div>

          <div class="form-foot">
            <button class="btn primary" data-action="saveSettings">保存设置</button>
            <button class="btn" data-action="testNotify">发测试消息</button>
            <span class="spacer"></span>
            <span class="hint" style="color:var(--faint)">改完立即生效，不用重启</span>
          </div>
        </div>
      </div>

      <div class="card" style="margin-top:16px">
        <header><h2>通知通道</h2><span class="hint">config.json</span></header>
        <div class="body">
          ${chanRow('邮件（主通道）', ch.smtp && ch.smtp.configured, ch.smtp && ch.smtp.from ? '发件 ' + ch.smtp.from : '')}
          ${chanRow('Webhook', ch.webhook && ch.webhook.configured, 'JSON POST')}
          ${chanRow('OneBot（QQ 群，可选）', ch.onebot && ch.onebot.configured, ch.onebot && ch.onebot.groupId ? '群 ' + ch.onebot.groupId : '需要自建机器人服务端')}
        </div>
      </div>`;
  }

  // ───────────────────────────── 状态

  async function renderStatus() {
    const [d, p] = await Promise.all([
      api('api/status'),
      api('api/patrol?limit=16').catch(() => ({ items: [], latest: [] })),
    ]);
    state.status = d;
    updateLive();
    const st = d.stats || {};
    const db = d.db || {};
    const cat = d.catalog || {};
    const up = d.uptimeSec || 0;
    const upStr = up > 86400 ? `${Math.floor(up / 86400)}天 ${Math.floor(up % 86400 / 3600)}小时`
      : up > 3600 ? `${Math.floor(up / 3600)}小时 ${Math.floor(up % 3600 / 60)}分`
      : `${Math.floor(up / 60)}分`;

    const ev = (d.events || []).map(e => `
      <li>
        <span class="t">${esc(fmtTime(e.ts))}</span>
        <span class="lvl">${chip(e.level === 'error' ? 'bad' : e.level === 'warn' ? 'warn' : '', e.level)}</span>
        <span class="msg">${esc(e.message)}</span>
      </li>`).join('');

    const idleSec = d.idleSec ?? -1;
    const stalled = idleSec > 300 || (d.consecFails || 0) >= 3;
    const idleStr = idleSec < 0 ? '尚未开始' : (idleSec < 90 ? idleSec + ' 秒前' : idleSec < 5400 ? Math.round(idleSec / 60) + ' 分钟前' : Math.round(idleSec / 3600) + ' 小时前');

    $('#tab-status').innerHTML = `
      ${stalled ? `<div class="warn" style="margin:0 0 14px;padding:12px 16px;border-radius:10px;background:var(--warn-soft);border:1px solid var(--warn);color:var(--warn)">
        ⚠️ 采集可能已停摆：上次成功拉取在 ${esc(idleStr)}${d.consecFails ? `，连续失败 ${d.consecFails} 次` : ''}。
        服务进程还活着，但拿不到 zKillboard 的序列流 —— 检查服务器出网 / DNS。
      </div>` : ''}
      <div class="page-head">
        <div><h1>运行状态</h1><p>版本 ${esc(d.version)} · 已运行 ${esc(upStr)} · 数据源 ${esc(d.source)}</p></div>
        <div class="actions"><button class="btn" data-action="refresh">刷新</button></div>
      </div>

      <div class="grid-2">
        <div class="card">
          <header><h2>采集器</h2><span class="hint">${esc(st.lastFetch ? timeAgo(st.lastFetch) : '未开始')}</span></header>
          <div class="body">
            <dl class="kv">
              <dt>模式</dt><dd>${esc(d.source)}</dd>
              <dt>已读取</dt><dd>${fmtInt(st.fetched)}</dd>
              <dt>命中官员</dt><dd>${fmtInt(st.matched)}</dd>
              <dt>新入库</dt><dd>${fmtInt(st.stored)}</dd>
              <dt>已推送</dt><dd>${fmtInt(st.notified)}</dd>
              <dt>被口径过滤</dt><dd>${fmtInt(st.skipped)}</dd>
              <dt>错误</dt><dd>${fmtInt(st.errors)}</dd>
              <dt>序列游标</dt><dd class="mono">${esc(st.lastSequence || '—')}</dd>
              <dt>落后</dt><dd>${d.lag ? d.lag + ' 条' : '实时'}${d.lag && d.lag > 500 ? ' <span class="chip warn">积压</span>' : ''}</dd>
              <dt>上次成功</dt><dd>${esc(idleStr)}</dd>
              <dt>连续失败</dt><dd style="color:${(d.consecFails || 0) >= 3 ? 'var(--bad)' : 'inherit'}">${d.consecFails || 0}</dd>
              <dt>最近错误</dt><dd style="color:var(--bad)">${esc(st.lastError || '—')}</dd>
            </dl>
          </div>
        </div>
        <div class="card">
          <header><h2>数据库与名录</h2></header>
          <div class="body">
            <dl class="kv">
              <dt>库大小</dt><dd>${(db.sizeBytes / 1048576).toFixed(2)} MB</dd>
              <dt>击杀行数</dt><dd>${fmtInt(db.killmailRows)}</dd>
              <dt>名录</dt><dd>${fmtInt(cat.count)} 位</dd>
              <dt>名录同步</dt><dd>${esc(cat.syncedAt ? timeAgo(cat.syncedAt) : '从未')}</dd>
              <dt>通道</dt><dd>${(d.channels || []).map(x => chip('good', x)).join(' ') || chip('', '无')}</dd>
            </dl>
          </div>
        </div>
      </div>

      <div class="card" style="margin-top:16px">
        <header>
          <h2>巡检</h2>
          <span class="hint">每 5 分钟一轮 · 连续 2 轮失败才发告警</span>
        </header>
        <div class="body">
          ${(() => {
            const runs = p.items || [];
            const latest = p.latest || [];
            if (!runs.length) return empty('还没跑过巡检', '🩺');
            const dots = runs.slice().reverse().map(x => {
              const t = fmtTime(x.ts);
              return `<span class="patrol-dot ${x.ok ? '' : 'bad'}" title="${esc(t + ' · ' + x.summary)}"></span>`;
            }).join('');
            const rows = latest.map(c => `
              <div class="patrol-row">
                <span class="patrol-mark ${c.status}">${c.status === 'ok' ? '✓' : c.status === 'warn' ? '!' : '✕'}</span>
                <span class="patrol-label">${esc(c.label)}</span>
                <span class="patrol-detail">${esc(c.detail || '')}</span>
              </div>`).join('');
            const okCount = runs.filter(x => x.ok).length;
            return `
              <div style="display:flex;align-items:center;gap:14px;flex-wrap:wrap;margin-bottom:12px">
                <span class="patrol-dots">${dots}</span>
                <span class="hint" style="color:var(--faint)">最近 ${runs.length} 轮，正常 ${okCount} 轮</span>
                <span class="hint" style="color:var(--faint)">上一轮 ${timeAgo(runs[0].ts)}</span>
              </div>
              <div class="patrol-list">${rows}</div>`;
          })()}
        </div>
      </div>

      <div class="card" style="margin-top:16px">
        <header><h2>事件</h2><span class="hint">最近 40 条</span></header>
        <div class="body">
          ${ev ? `<ul class="events">${ev}</ul>` : empty('暂无事件', '🗒️')}
        </div>
      </div>`;
  }

  // refreshStatusLight 只拉状态（轻量），保证每个页面右上角的心跳都是真的
  async function refreshStatusLight() {
    try {
      state.status = await api('api/status');
      updateLive();
    } catch { /* 忽略：灯不亮不影响页面 */ }
  }

  function updateLive() {
    const el = $('#live');
    const st = state.status && state.status.stats;
    if (!st || !st.lastFetch) { el.className = 'live'; el.querySelector('span').textContent = '待启动'; return; }
    const age = (Date.now() - new Date(st.lastFetch).getTime()) / 1000;
    if (age < 120) { el.className = 'live ok'; el.querySelector('span').textContent = '运行中'; }
    else if (age < 900) { el.className = 'live warn'; el.querySelector('span').textContent = '迟缓 ' + Math.round(age / 60) + 'm'; }
    else { el.className = 'live bad'; el.querySelector('span').textContent = '停摆'; }
  }

  // ───────────────────────────── 抽屉

  async function openKill(id) {
    const d = await api('api/kills/' + id);
    const k = d.kill;
    $('#drawerTitle').textContent = k.officerName + ' · ' + fmtISK(k.totalValue) + ' ISK';
    $('#drawerBody').innerHTML = `
      ${shipImg(k.victimShipTypeId, 128, 'render', 'shipBig')}
      <dl class="kv">
        <dt>KillID</dt><dd class="mono">${k.killmailId}</dd>
        <dt>官员</dt><dd>${esc(k.officerName)}${k.officerZh ? ' · ' + esc(k.officerZh) : ''}
          ${chip('accent', k.faction || '—')}${chip('', CLASS_ZH[k.officerClass] || '')}
          <span style="color:var(--faint);font-size:12px">船型 ${k.officerTypeId}</span></dd>
        <dt>类型</dt><dd>${esc(ROLE_LABEL[k.role] || k.role)}</dd>
        <dt>时间</dt><dd>${esc(fmtTime(k.killmailTime))}</dd>
        <dt>地点</dt><dd>${esc(k.systemName || '—')}${k.regionName ? ' · ' + esc(k.regionName) : ''}${k.systemSec ? ` · 安全 ${k.systemSec.toFixed(2)}` : ''}${k.jumps ? ` · <b>距家 ${k.jumps} 跳</b>` : ''}</dd>
        <dt>星体</dt><dd>${esc(k.locationName || '—')}</dd>
        <dt>总价值</dt><dd class="mono">${fmtISK(k.totalValue)} ISK</dd>
        <dt>掉落 / 损毁</dt><dd class="mono">${k.droppedValue || k.destroyedValue ? fmtISK(k.droppedValue) + ' / ' + fmtISK(k.destroyedValue) : '—'}</dd>
        <dt>受害者</dt><dd>${esc(k.victimName || (k.victimCharId ? '角色 ' + k.victimCharId : 'NPC / 未知'))}${k.victimCorpName ? ' · ' + esc(k.victimCorpName) : ''}${k.victimAllianceName ? ' · ' + esc(k.victimAllianceName) : ''}<br><span style="color:var(--faint)">${esc(k.victimShipName || ('type ' + k.victimShipTypeId))}</span></dd>
        <dt>历史排名</dt><dd>${k.officerKillRank ? `${k.officerKillRank} / ${k.officerKillTotal}（按损失价值）` : '—'}</dd>
        <dt>攻击者</dt><dd>${k.attackerCount ? fmtInt(k.attackerCount) + ' 位' : '—（历史数据未记录）'}</dd>
        <dt>通知</dt><dd>${statusChip(k)} ${k.notifiedAt ? esc(fmtTime(k.notifiedAt)) : ''}</dd>
      </dl>
      <div style="display:flex;gap:10px;margin:14px 0">
        <a class="btn" href="${esc(k.zkbUrl)}" target="_blank" rel="noopener">在 zKillboard 打开 ↗</a>
        <button class="btn" data-action="renotify" data-id="${k.killmailId}">重发通知</button>
      </div>
      <details class="raw"><summary>原始数据（R2Z2 / ESI）</summary>
        <pre>${esc(JSON.stringify(d.raw, null, 2))}</pre>
      </details>`;
    $('#drawer').hidden = false;
  }

  function closeDrawer() { $('#drawer').hidden = true; }

  // ───────────────────────────── 交互

  const renderers = {
    overview: renderOverview, kills: renderKills, officers: renderOfficers,
    stats: renderStats, settings: renderSettings, status: renderStatus,
  };

  async function show(tab, force) {
    if (!renderers[tab]) tab = 'overview';
    const changed = state.tab !== tab;
    state.tab = tab;
    document.querySelectorAll('.tab').forEach((el) => el.classList.toggle('on', el.id === 'tab-' + tab));
    document.querySelectorAll('#nav a').forEach((a) => a.classList.toggle('on', a.dataset.tab === tab));
    refreshStatusLight();
    try {
      await renderers[tab]();
    } catch (err) {
      toast('加载失败：' + err.message, 'err');
      $('.tab.on').innerHTML = empty('加载失败：' + err.message, '⚠️');
    }
    if (changed && location.hash !== '#' + tab) history.replaceState(null, '', '#' + tab);
  }

  function collectSettings() {
    const factions = [...document.querySelectorAll('#factionChips .chip.on')].map(x => x.dataset.faction);
    return {
      minValue: Number($('#setMinValue').value || 0),
      role: $('#setRole').value,
      factions,
      quietEnabled: $('#setQuiet').checked,
      quietStart: Number($('#setQuietStart').value || 0),
      quietEnd: Number($('#setQuietEnd').value || 0),
      maxPerHour: Number($('#setMaxPerHour').value || 0),
      atAll: $('#setAtAll').checked,
      notifyEnabled: $('#setNotify').checked,
      backfillHours: Number($('#setBackfill').value || 0),
      homeSystem: ($('#setHome').value || '').trim(),
      recipients: [...document.querySelectorAll('#rcptChips .chip')].map(x => x.dataset.email).filter(Boolean),
    };
  }

  async function doAction(el) {
    const a = el.dataset.action;
    switch (a) {
      case 'closeDrawer': return closeDrawer();
      case 'refresh': return show(state.tab, true);
      case 'page': {
        state.kills.offset = Math.max(0, Number(el.dataset.off || 0));
        return renderKills();
      }
      case 'resetFilters':
        state.filters = { faction: '', role: '', minValue: '', q: '', sort: 'time' };
        state.kills.offset = 0;
        return renderKills();
      case 'import': {
        const v = prompt('输入 KillID（会从 zKillboard + ESI 拉取并入库）');
        if (!v) return;
        el.disabled = true;
        try {
          const r = await api('api/kills/import', { method: 'POST', body: JSON.stringify({ killId: Number(v) }) });
          toast(r.message || '已处理', r.ok ? 'ok' : 'err');
          show(state.tab, true);
        } catch (e) { toast(e.message, 'err'); }
        finally { el.disabled = false; }
        return;
      }
      case 'renotify': {
        const id = el.dataset.id;
        el.disabled = true;
        try {
          await api('api/kills/' + id + '/renotify', { method: 'POST' });
          toast('已重发', 'ok');
        } catch (e) { toast(e.message, 'err'); }
        finally { el.disabled = false; }
        return;
      }
      case 'addRcpt': {
        const input = $('#newRcpt');
        const raw = (input.value || '').trim();
        if (!raw) return;
        const box = $('#rcptChips');
        if (box.querySelector('.hint')) box.innerHTML = '';
        const parts = raw.split(/[,;\s]+/).filter(Boolean);
        for (const p of parts) {
          if ([...box.querySelectorAll('.chip')].some(x => x.dataset.email.toLowerCase() === p.toLowerCase())) continue;
          const el = document.createElement('span');
          el.className = 'chip on';
          el.dataset.email = p;
          el.innerHTML = `${esc(p)} <b data-action="delRcpt" style="cursor:pointer;opacity:.6;margin-left:2px">✕</b>`;
          box.appendChild(el);
        }
        input.value = '';
        return;
      }
      case 'delRcpt': {
        const chipEl = el.closest('.chip');
        if (chipEl) chipEl.remove();
        if (!$('#rcptChips').children.length) $('#rcptChips').innerHTML = '<span class="hint">还没有收件人</span>';
        return;
      }
      case 'saveSettings': {
        el.disabled = true;
        try {
          await api('api/settings', { method: 'PUT', body: JSON.stringify(collectSettings()) });
          toast('已保存', 'ok');
        } catch (e) { toast(e.message, 'err'); }
        finally { el.disabled = false; }
        return;
      }
      case 'testNotify': {
        el.disabled = true;
        try {
          const r = await api('api/test-notify', { method: 'POST' });
          const parts = Object.entries(r.result || {}).map(([k, v]) => `${k}: ${v}`);
          toast(parts.join(' · ') || '没有通道', 'ok');
        } catch (e) { toast(e.message, 'err'); }
        finally { el.disabled = false; }
        return;
      }
      case 'syncCatalog':
      case 'deepScan': {
        el.disabled = true;
        try {
          const r = await api('api/catalog/sync', { method: 'POST', body: JSON.stringify({ deep: a === 'deepScan' }) });
          toast(`名录已同步，新增 ${r.added} 条`, 'ok');
          show(state.tab, true);
        } catch (e) { toast(e.message, 'err'); }
        finally { el.disabled = false; }
        return;
      }
    }
  }

  document.addEventListener('click', async (ev) => {
    const row = ev.target.closest('[data-kill]');
    if (row && !ev.target.closest('a')) { openKill(row.dataset.kill).catch(e => toast(e.message, 'err')); return; }
    const act = ev.target.closest('[data-action]');
    if (act) { doAction(act).catch(e => toast(e.message, 'err')); return; }
    const chipEl = ev.target.closest('#factionChips .chip');
    if (chipEl) { chipEl.classList.toggle('on'); }
  });

  document.addEventListener('change', async (ev) => {
    const t = ev.target;
    if (t.dataset.filter) {
      state.filters[t.dataset.filter] = t.value;
      state.kills.offset = 0;
      renderKills().catch(e => toast(e.message, 'err'));
      return;
    }
    if (t.dataset.action === 'toggleOfficer') {
      const id = t.dataset.id;
      t.disabled = true;
      try {
        await api('api/officers/' + id + '/toggle', { method: 'POST', body: JSON.stringify({ enabled: t.checked }) });
        t.closest('.officer').classList.toggle('off', !t.checked);
        toast(t.checked ? '已开启监控' : '已关闭监控', 'ok');
      } catch (e) { t.checked = !t.checked; toast(e.message, 'err'); }
      finally { t.disabled = false; }
    }
  });

  let searchTimer = null;
  document.addEventListener('keydown', (ev) => {
    if (ev.target && ev.target.id === 'newRcpt' && ev.key === 'Enter') {
      ev.preventDefault();
      const btn = document.querySelector('[data-action="addRcpt"]');
      if (btn) doAction(btn).catch(e => toast(e.message, 'err'));
    }
  });

  document.addEventListener('input', (ev) => {
    const t = ev.target;
    if (t.dataset.filter === 'q') {
      clearTimeout(searchTimer);
      searchTimer = setTimeout(() => {
        state.filters.q = t.value;
        state.kills.offset = 0;
        renderKills().catch(e => toast(e.message, 'err'));
      }, 320);
    }
  });

  $('#nav').addEventListener('click', (ev) => {
    const a = ev.target.closest('a[data-tab]');
    if (!a) return;
    ev.preventDefault();
    show(a.dataset.tab);
  });

  $('#themeBtn').addEventListener('click', cycleTheme);
  $('#refreshBtn').addEventListener('click', () => show(state.tab, true));

  window.addEventListener('hashchange', () => show((location.hash || '#overview').slice(1)));
  window.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape') closeDrawer();
  });

  // ───────────────────────────── 启动

  applyTheme(localStorage.getItem('ow-theme') || 'auto');
  matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
    if ((localStorage.getItem('ow-theme') || 'auto') === 'auto') applyTheme('auto');
  });

  show((location.hash || '#overview').slice(1));

  // 心跳：45 秒刷一次状态灯；概览/状态页自动刷新，其它页不打扰用户操作。
  setInterval(async () => {
    try {
      state.status = await api('api/status');
      updateLive();
      if (state.tab === 'overview' || state.tab === 'status') renderers[state.tab]();
    } catch { /* 忽略 */ }
  }, 45000);
})();
