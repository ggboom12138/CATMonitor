// CATMonitor SSD 监控 SPA — 原生 JS + Canvas，零依赖、零点击交互。
// 布局：概览（overview cards）→ 盘组（大框 = 逻辑盘：使用率 + 框内三张 IO
// 曲线；内嵌小框 = 物理 SSD：完整 SMART 表平铺；直连盘自成一体，曲线 +
// SMART 全在框内）。前端轮询 /api/ssd，曲线 60 点滚动缓冲。
(function () {
  'use strict';

  var HISTORY_POINTS = 60;
  var COLOR_READ = '#3b82f6';
  var COLOR_WRITE = '#f59e0b';

  var state = {
    intervalSec: 3,
    timer: null,
    sessionID: null,
    history: {},   // curveDevice -> {readMB:[], writeMB:[], readIOPS:[], writeIOPS:[], readLat:[], writeLat:[]}
  };

  // ---------- helpers ----------
  function $(id) { return document.getElementById(id); }

  function cssVar(name) {
    return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  }

  // safeID turns a device name ("sdb", "megaraid,0") into an HTML-safe id
  // fragment for per-group canvas elements.
  function safeID(s) {
    return String(s).replace(/[^a-zA-Z0-9_-]/g, '_');
  }

  function fmtNum(v) {
    if (v == null || isNaN(v)) return 'N/A';
    var abs = Math.abs(v);
    if (abs >= 1e9) return (v / 1e9).toFixed(1) + 'G';
    if (abs >= 1e6) return (v / 1e6).toFixed(1) + 'M';
    if (abs >= 1e4) return (v / 1e3).toFixed(1) + 'K';
    return String(Math.round(v * 100) / 100);
  }

  function fmtGB(v) {
    if (v == null || isNaN(v)) return 'N/A';
    if (Math.abs(v) >= 1000) return (v / 1000).toFixed(2) + ' TB';
    return Math.round(v * 100) / 100 + ' GB';
  }

  function fmtHours(h) {
    if (h == null || isNaN(h)) return 'N/A';
    if (h >= 8760) return Math.round(h) + ' h（约 ' + (h / 8760).toFixed(1) + ' 年）';
    return Math.round(h) + ' h';
  }

  function fmtTemp(t) {
    if (t == null || isNaN(t)) return 'N/A';
    return Math.round(t) + ' °C';
  }

  function fmtPct(v) {
    if (v == null || isNaN(v)) return 'N/A';
    return (Math.round(v * 100) / 100) + ' %';
  }

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  function push(arr, v) {
    arr.push(v);
    if (arr.length > HISTORY_POINTS) arr.shift();
  }

  function showBanner(kind, text) {
    var b = $('banner');
    if (!text) { b.className = 'banner hidden'; b.textContent = ''; return; }
    b.className = 'banner ' + kind;
    b.textContent = text;
  }

  // ---------- data fetch ----------
  function fetchAndRender() {
    fetch('/api/ssd', { cache: 'no-store' })
      .then(function (r) {
        if (!r.ok) throw new Error('HTTP ' + r.status);
        return r.json();
      })
      .then(function (data) { render(data); })
      .catch(function (err) {
        showBanner('err', '获取数据失败：' + err.message + '（daemon 未启动或 snapshot 未就绪？）');
      });
  }

  // ---------- render ----------
  function render(data) {
    showBanner(null);
    $('updateTime').textContent = data.timestamp || '--';

    if (data.session_id !== state.sessionID) {
      // daemon restarted: reset rolling buffers
      state.sessionID = data.session_id;
      state.history = {};
    }

    renderOverview(data.overview || {});
    renderGroups(data.groups || []);
  }

  function renderOverview(ov) {
    var health;
    if (ov.failed_count > 0) {
      health = '<span class="err">' + ov.failed_count + ' 异常</span>';
    } else if (ov.healthy_count > 0) {
      health = '<span class="ok">' + ov.healthy_count + ' 正常</span>';
    } else {
      health = '<span class="na">无 SMART</span>';
    }
    var healthSub = [];
    if (ov.healthy_count) healthSub.push('<span class="ok">' + ov.healthy_count + ' 正常</span>');
    if (ov.failed_count) healthSub.push('<span class="err">' + ov.failed_count + ' 异常</span>');
    if (ov.no_smart_count) healthSub.push('<span class="na">' + ov.no_smart_count + ' 无 SMART</span>');
    $('overview').innerHTML =
      statCard('SSD 物理盘', ov.ssd_count, '') +
      statCard('物理总容量', fmtGB(ov.total_capacity_gb), '') +
      statCard('平均使用率', fmtPct(ov.avg_space_usage), '按有文件系统的实体统计') +
      statCard('健康状态', health, healthSub.join(' · ')) +
      statCard('最高磨损', ov.max_wear_percent > 0 ? fmtPct(ov.max_wear_percent) : 'N/A', '寿命已消耗百分比');
  }

  function statCard(label, value, sub) {
    return '<div class="stat-card"><div class="stat-label">' + esc(label) + '</div>' +
      '<div class="stat-value">' + value + '</div>' +
      (sub ? '<div class="stat-sub">' + sub + '</div>' : '') +
      '</div>';
  }

  function healthOf(d) {
    if (!d.smart || d.smart.passed == null) return 'na';
    return d.smart.passed === 1 ? 'ok' : 'err';
  }

  // ---------- groups ----------
  function renderGroups(groups) {
    var pdCount = 0;
    var frames = '';
    var standalone = '';
    groups.forEach(function (g) {
      pdCount += (g.members || []).length;
      if (g.logical) {
        frames += groupFrame(g);
      } else {
        (g.members || []).forEach(function (m) { standalone += directFrame(m); });
      }
    });
    $('diskCount').textContent = pdCount + ' 块物理盘';
    $('topologyHint').textContent = frames
      ? '大框 = RAID 逻辑盘（使用率与 IO 曲线在这一层）· 内嵌小框 = 物理 SSD（完整 SMART 平铺）'
      : '';
    $('diskGrid').innerHTML = frames + standalone;

    // Feed rolling buffers and draw every frame's charts.
    groups.forEach(function (g) {
      if (g.logical) {
        drawChartsFor(g.logical.device, g.logical.io);
      } else {
        (g.members || []).forEach(function (m) { drawChartsFor(m.device, m.io); });
      }
    });
  }

  // groupFrame renders a RAID logical volume frame: header (usage) + charts
  // + nested physical member cards.
  function groupFrame(g) {
    var lv = g.logical;
    var usage = lv.space ? lv.space.usage_percent : null;
    var members = (g.members || []).map(physicalCard).join('');
    var noMembers = (!g.members || g.members.length === 0)
      ? '<div class="hint">未能推断成员物理盘（容量无法唯一匹配）</div>' : '';
    return '<div class="group-frame">' +
      '<div class="group-head">' +
      '  <span class="group-device">' + esc(lv.device) + '</span>' +
      '  <span class="badge accent">逻辑盘</span>' +
      '  <span class="group-model">' + esc(lv.model || '') + '</span>' +
      '  <div class="group-usage">' + usageBar(usage) + '</div>' +
      '  <span class="group-cap">' + fmtGB(lv.capacity_gb) + '</span>' +
      '</div>' +
      chartsHTML(lv.device) +
      '<div class="group-members">' + members + '</div>' + noMembers +
      '</div>';
  }

  // directFrame renders a standalone physical (direct-attach) disk as its
  // own frame: usage header + its own charts + its full SMART card.
  function directFrame(d) {
    var usage = d.space ? d.space.usage_percent : null;
    return '<div class="group-frame">' +
      '<div class="group-head">' +
      '  <span class="group-device">' + esc(d.device) + '</span>' +
      '  <span class="badge">直连盘</span>' +
      '  <span class="group-model">' + esc(d.model || '') + '</span>' +
      '  <div class="group-usage">' + usageBar(usage) + '</div>' +
      '  <span class="group-cap">' + fmtGB(d.capacity_gb) + '</span>' +
      '</div>' +
      chartsHTML(d.device) +
      '<div class="group-members">' + physicalCard(d) + '</div>' +
      '</div>';
  }

  function usageBar(pct) {
    var cls = usageClass(pct);
    var w = pct != null && !isNaN(pct) ? Math.max(0, Math.min(100, pct)) : 0;
    var text = pct != null && !isNaN(pct) ? fmtPct(pct) : '使用率 N/A';
    return '<div><div class="disk-row"><span class="k">使用率</span><span>' + esc(text) + '</span></div>' +
      '<div class="bar ' + cls + '"><i style="width:' + w + '%"></i></div></div>';
  }

  // chartsHTML builds the three canvas cards for one curve source (logical
  // volume or direct disk).
  function chartsHTML(curveDevice) {
    var id = safeID(curveDevice);
    return '<div class="group-charts">' +
      chartCard('读写吞吐 (MB/s)', 'tp', id) +
      chartCard('读写 IOPS (次/s)', 'io', id) +
      chartCard('读写延迟 (ms)', 'lat', id) +
      '</div>';
  }

  function chartCard(title, kind, id) {
    return '<div class="chart-card">' +
      '<div class="chart-head"><span>' + esc(title) + '</span>' +
      '<span class="legend"><span><i style="background:' + COLOR_READ + '"></i>读</span>' +
      '<span><i style="background:' + COLOR_WRITE + '"></i>写</span></span></div>' +
      '<canvas id="chart-' + kind + '-' + id + '"></canvas>' +
      '</div>';
  }

  // physicalCard renders one physical SSD with the FULL SMART table laid out
  // flat — no click interaction.
  function physicalCard(d) {
    var h = healthOf(d);
    return '<div class="disk-card disk-card-flat">' +
      '<div class="disk-head">' +
      '  <span class="health-dot ' + h + '" title="' + (h === 'ok' ? '健康' : h === 'err' ? '异常' : '无 SMART 数据') + '"></span>' +
      '  <span class="disk-device">' + esc(d.device) + '</span>' +
      '  <span style="margin-left:auto;color:var(--muted);font-size:12px">' + fmtGB(d.capacity_gb) + '</span>' +
      '</div>' +
      '<div class="disk-model">' + esc(d.model || '未知型号') +
      (d.interface ? ' · ' + esc(d.interface) : '') +
      (d.serial ? ' · SN ' + esc(d.serial) : '') + '</div>' +
      '<table class="smart-table">' + smartRowsHTML(d.smart || {}) + '</table>' +
      '</div>';
  }

  // ---------- SMART table ----------
  var SMART_ROWS = [
    ['健康状态', function (s) {
      if (s.passed == null) return td('N/A', 'na');
      return s.passed === 1 ? td('PASSED', 'ok') : td('FAILED', 'err');
    }],
    ['温度', function (s) { return tdVal(s.temperature, fmtTemp, s.temperature > 65 ? 'warn' : ''); }],
    ['磨损百分比', function (s) { return tdVal(s.wear_percent, fmtPct, s.wear_percent >= 80 ? 'err' : s.wear_percent >= 60 ? 'warn' : ''); }],
    ['可用备件', function (s) { return tdVal(s.available_spare, fmtPct, s.available_spare != null && s.available_spare < 10 ? 'warn' : ''); }],
    ['累计写入量 (TBW)', function (s) { return tdVal(s.data_written_gb, fmtGB); }],
    ['累计读取量', function (s) { return tdVal(s.data_read_gb, fmtGB); }],
    ['累计通电时长', function (s) { return tdVal(s.power_on_hours, fmtHours); }],
    ['通电次数', function (s) { return tdVal(s.power_cycles, function (v) { return Math.round(v) + ' 次'; }); }],
    ['介质错误数', function (s) { return tdVal(s.media_errors, function (v) { return Math.round(v) + ' 次'; }, s.media_errors > 0 ? 'err' : ''); }],
    ['重映射扇区', function (s) { return tdVal(s.reallocated_sectors, function (v) { return Math.round(v) + ' 个'; }, s.reallocated_sectors > 0 ? 'warn' : ''); }],
    ['意外断电次数', function (s) { return tdVal(s.unsafe_shutdowns, function (v) { return Math.round(v) + ' 次'; }); }],
  ];

  function td(text, cls) { return '<td class="' + (cls || '') + '">' + esc(text) + '</td>'; }
  function tdVal(v, fmt, cls) {
    if (v == null || isNaN(v)) return td('N/A', 'na');
    return td(fmt(v), cls);
  }

  function smartRowsHTML(s) {
    var html = '';
    SMART_ROWS.forEach(function (row) {
      html += '<tr><td>' + row[0] + '</td>' + row[1](s) + '</tr>';
    });
    return html;
  }

  // ---------- charts ----------
  // drawChartsFor feeds the rolling buffer of one curve source and redraws
  // its three canvases.
  function drawChartsFor(curveDevice, io) {
    var h = state.history[curveDevice];
    if (!h) {
      h = { readMB: [], writeMB: [], readIOPS: [], writeIOPS: [], readLat: [], writeLat: [] };
      state.history[curveDevice] = h;
    }
    var id = safeID(curveDevice);
    if (!io) io = {};
    push(h.readMB, io.read_throughput_mb_s || 0);
    push(h.writeMB, io.write_throughput_mb_s || 0);
    push(h.readIOPS, io.read_iops || 0);
    push(h.writeIOPS, io.write_iops || 0);
    push(h.readLat, io.read_latency_ms || 0);
    push(h.writeLat, io.write_latency_ms || 0);
    drawChart($('chart-tp-' + id), [
      { color: COLOR_READ, data: h.readMB },
      { color: COLOR_WRITE, data: h.writeMB },
    ]);
    drawChart($('chart-io-' + id), [
      { color: COLOR_READ, data: h.readIOPS },
      { color: COLOR_WRITE, data: h.writeIOPS },
    ]);
    drawChart($('chart-lat-' + id), [
      { color: COLOR_READ, data: h.readLat },
      { color: COLOR_WRITE, data: h.writeLat },
    ]);
  }

  function drawChart(canvas, series) {
    if (!canvas) return;
    var dpr = window.devicePixelRatio || 1;
    var w = canvas.clientWidth || 300;
    var hpx = canvas.clientHeight || 160;
    canvas.width = w * dpr;
    canvas.height = hpx * dpr;
    var ctx = canvas.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, hpx);

    var padL = 44, padR = 8, padT = 6, padB = 16;
    var plotW = w - padL - padR;
    var plotH = hpx - padT - padB;

    var gridColor = cssVar('--grid-line') || '#eee';
    var axisColor = cssVar('--axis-text') || '#999';

    var max = 0;
    series.forEach(function (s) {
      s.data.forEach(function (v) { if (v > max) max = v; });
    });
    if (max <= 0) max = 1;
    var nice = niceCeil(max);

    ctx.font = '11px sans-serif';
    ctx.textAlign = 'right';
    ctx.textBaseline = 'middle';
    for (var i = 0; i <= 4; i++) {
      var y = padT + plotH - plotH * i / 4;
      ctx.strokeStyle = gridColor;
      ctx.beginPath();
      ctx.moveTo(padL, y);
      ctx.lineTo(w - padR, y);
      ctx.stroke();
      ctx.fillStyle = axisColor;
      ctx.fillText(fmtNum(nice * i / 4), padL - 6, y);
    }

    series.forEach(function (s) {
      if (!s.data.length) return;
      var step = plotW / (HISTORY_POINTS - 1);
      var x0 = padL + plotW - step * (s.data.length - 1);
      ctx.strokeStyle = s.color;
      ctx.lineWidth = 1.6;
      ctx.beginPath();
      for (var j = 0; j < s.data.length; j++) {
        var x = x0 + step * j;
        var yy = padT + plotH - plotH * (s.data[j] / nice);
        if (j === 0) ctx.moveTo(x, yy); else ctx.lineTo(x, yy);
      }
      ctx.stroke();
    });
  }

  function niceCeil(v) {
    var exp = Math.floor(Math.log10(v));
    var base = Math.pow(10, exp);
    var n = v / base;
    var nice;
    if (n <= 1) nice = 1;
    else if (n <= 2) nice = 2;
    else if (n <= 5) nice = 5;
    else nice = 10;
    return nice * base;
  }

  // ---------- controls ----------
  function restartTimer() {
    if (state.timer) clearInterval(state.timer);
    state.timer = setInterval(fetchAndRender, state.intervalSec * 1000);
  }

  function initTheme() {
    var btn = $('themeBtn');
    var current = document.documentElement.getAttribute('data-theme');
    btn.textContent = current === 'dark' ? '☀️' : '🌙';
    btn.addEventListener('click', function () {
      var next = document.documentElement.getAttribute('data-theme') === 'dark' ? 'light' : 'dark';
      document.documentElement.setAttribute('data-theme', next);
      try { localStorage.setItem('ssd-theme', next); } catch (e) {}
      btn.textContent = next === 'dark' ? '☀️' : '🌙';
      fetchAndRender(); // redraw charts with new theme colors
    });
  }

  function init() {
    initTheme();
    $('applyBtn').addEventListener('click', function () {
      var v = parseInt($('intervalInput').value, 10);
      if (v >= 1 && v <= 60) {
        state.intervalSec = v;
        restartTimer();
      }
    });
    $('refreshBtn').addEventListener('click', fetchAndRender);
    window.addEventListener('resize', fetchAndRender);
    fetchAndRender();
    restartTimer();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
