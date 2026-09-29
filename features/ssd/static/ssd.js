// CATMonitor SSD 监控 SPA — 原生 JS + Canvas，零依赖、零点击交互。
// 布局：概览（overview cards）→ 盘组（大框 = 逻辑盘：使用率 + 框内三张 IO
// 曲线；内嵌小框 = 物理 SSD：完整 SMART 表平铺；直连盘自成一体，曲线 +
// SMART 全在框内）。前端轮询 /api/ssd，曲线 60 点滚动缓冲。
(function () {
  'use strict';

  var HISTORY_POINTS = 60;
  var COLOR_READ = '#3b82f6';
  var COLOR_WRITE = '#f59e0b';
  // Per-physical-disk palette for the temperature chart series.
  var PALETTE = ['#3b82f6', '#f59e0b', '#10b981', '#ef4444',
                 '#8b5cf6', '#06b6d4', '#ec4899', '#84cc16'];
  // Hourly bar chart colors: volume bars share the IO chart read/write
  // colors; IO-count bars use lighter hues; the cumulative line is purple.
  var COLOR_IOS_READ = '#06b6d4';
  var COLOR_IOS_WRITE = '#eab308';
  var COLOR_CUMUL = '#8b5cf6';

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
      statCard('SSD 物理盘', ov.ssd_count, '容量 ' + fmtGB(ov.ssd_total_capacity_gb)) +
      statCard('HDD 物理盘', ov.hdd_count, '容量 ' + fmtGB(ov.hdd_total_capacity_gb)) +
      statCard('平均使用率', fmtPct(ov.avg_space_usage), '按有文件系统的实体统计') +
      statCard('健康状态', health, healthSub.join(' · ')) +
      statCard('最高磨损 (SSD)', ov.max_wear_percent > 0 ? fmtPct(ov.max_wear_percent) : 'N/A', 'SSD 寿命已消耗百分比');
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
    var ssdHTML = '', hddHTML = '';
    var ssdPD = 0, hddPD = 0, hasFrames = false;
    groups.forEach(function (g) {
      var isHDD = groupMedia(g) === 'hdd';
      if (isHDD) hddPD += (g.members || []).length;
      else ssdPD += (g.members || []).length;
      var html;
      if (g.logical) {
        hasFrames = true;
        html = groupFrame(g);
      } else {
        html = (g.members || []).map(directFrame).join('');
      }
      if (isHDD) hddHTML += html; else ssdHTML += html;
    });
    $('diskCount').textContent = (ssdPD + hddPD) + ' 块物理盘（SSD ' + ssdPD + ' · HDD ' + hddPD + '）';
    $('topologyHint').textContent = hasFrames
      ? '大框 = 逻辑盘（使用率、IO 曲线与窗口统计在这一层）· 内嵌小框 = 物理盘（温度曲线与 SMART 平铺）'
      : '';
    var out = '';
    if (ssdHTML) out += '<h3 class="sub-title">SSD 盘组</h3>' + ssdHTML;
    if (hddHTML) out += '<h3 class="sub-title">HDD 盘组</h3>' + hddHTML;
    $('diskGrid').innerHTML = out;

    // Feed rolling buffers and draw every frame's charts: three IO charts
    // from the curve source, the per-member temperature chart, and the 24h
    // hourly bar chart.
    groups.forEach(function (g) {
      var curveDev = null, io = null, members = [], hourly = null;
      if (g.logical) {
        curveDev = g.logical.device;
        io = g.logical.io;
        members = g.members || [];
        hourly = g.logical.hourly;
      } else if (g.members && g.members.length) {
        curveDev = g.members[0].device;
        io = g.members[0].io;
        members = g.members;
        hourly = g.members[0].hourly;
      }
      if (!curveDev) return;
      drawChartsFor(curveDev, io);
      drawTempChart(curveDev, members);
      drawHourlyChart(curveDev, hourly);
    });
  }

  // groupMedia picks the section a group belongs to: the logical volume's
  // media when wrapped, else the first member's.
  function groupMedia(g) {
    if (g.logical && g.logical.media) return g.logical.media;
    if (g.members && g.members.length) return g.members[0].media || 'ssd';
    return 'ssd';
  }

  // groupFrame renders a RAID logical volume frame: header (usage) + charts
  // + window stats + nested physical member cards.
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
      (lv.raid_level ? '<span class="badge badge-raid">' + esc(lv.raid_level) + '</span>' : '') +
      '  <span class="group-model">' + esc(lv.model || '') + '</span>' +
      '  <div class="group-usage">' + usageBar(usage) + '</div>' +
      '  <span class="group-cap">' + fmtGB(lv.capacity_gb) + '</span>' +
      '</div>' +
      chartsHTML(lv.device, lv.io, g.members || []) +
      hourlyChartCards(lv.device, lv.hourly) +
      windowStatsHTML(lv.window_stats) +
      '<div class="group-members">' + members + '</div>' + noMembers +
      '</div>';
  }

  // directFrame renders a standalone physical (direct-attach) disk as its
  // own frame: usage header + its own charts + window stats + its full
  // SMART card.
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
      chartsHTML(d.device, d.io, [d]) +
      hourlyChartCards(d.device, d.hourly) +
      windowStatsHTML(d.window_stats) +
      '<div class="group-members">' + physicalCard(d) + '</div>' +
      '</div>';
  }

  function usageClass(u) {
    if (u == null || isNaN(u)) return 'accent';
    if (u >= 90) return 'err';
    if (u >= 75) return 'warn';
    return 'ok';
  }

  function usageBar(pct) {
    var cls = usageClass(pct);
    var w = pct != null && !isNaN(pct) ? Math.max(0, Math.min(100, pct)) : 0;
    var text = pct != null && !isNaN(pct) ? fmtPct(pct) : '使用率 N/A';
    return '<div><div class="disk-row"><span class="k">使用率</span><span>' + esc(text) + '</span></div>' +
      '<div class="bar ' + cls + '"><i style="width:' + w + '%"></i></div></div>';
  }

  // chartsHTML builds the four canvas cards for one curve source (logical
  // volume or direct disk): three IO charts (per curve source) + the
  // temperature chart (one line per member physical disk). Legends carry
  // CURRENT values.
  function chartsHTML(curveDevice, io, members) {
    var id = safeID(curveDevice);
    if (!io) io = {};
    return '<div class="group-charts">' +
      chartCard('读写吞吐 (MB/s)', 'tp', id, io.read_throughput_mb_s, io.write_throughput_mb_s, 'MB/s') +
      chartCard('读写 IOPS (次/s)', 'io', id, io.read_iops, io.write_iops, '次/s') +
      chartCard('读写延迟 (ms)', 'lat', id, io.read_latency_ms, io.write_latency_ms, 'ms') +
      tempChartCard(curveDevice, members) +
      '</div>';
  }

  function chartCard(title, kind, id, readVal, writeVal, unit) {
    return '<div class="chart-card">' +
      '<div class="chart-head"><span>' + esc(title) + '</span>' +
      '<span class="legend">' +
      legendItem('读', COLOR_READ, readVal, unit) +
      legendItem('写', COLOR_WRITE, writeVal, unit) +
      '</span></div>' +
      '<canvas id="chart-' + kind + '-' + id + '"></canvas>' +
      '</div>';
  }

  // tempChartCard: one line per member physical disk, legend = device name
  // + current temperature + °C.
  function tempChartCard(curveDevice, members) {
    var legend = (members || []).map(function (m, i) {
      var color = PALETTE[i % PALETTE.length];
      var t = (m.smart && m.smart.temperature != null && !isNaN(m.smart.temperature))
        ? fmtNum(m.smart.temperature) : '-';
      return '<span><i style="background:' + color + '"></i>' + esc(m.device) +
        ' <b class="lg-val" style="color:' + color + '">' + t + '</b> <span class="lg-unit">°C</span></span>';
    }).join('');
    return '<div class="chart-card">' +
      '<div class="chart-head"><span>温度 (°C)</span><span class="legend">' + legend + '</span></div>' +
      '<canvas id="chart-temp-' + safeID(curveDevice) + '"></canvas>' +
      '</div>';
  }

  // legendItem renders "读 12.5 MB/s": the value colored like its series
  // (bold), the unit in small muted gray. Missing data shows a dash.
  function legendItem(name, color, val, unit) {
    var v = (val == null || isNaN(val)) ? '-' : fmtNum(val);
    return '<span><i style="background:' + color + '"></i>' + name +
      ' <b class="lg-val" style="color:' + color + '">' + esc(v) + '</b>' +
      ' <span class="lg-unit">' + esc(unit) + '</span></span>';
  }

  // hourlyChartCards renders TWO full-width 24h hourly bar charts: data
  // volume (GB) and IO counts (次). Each has its own single Y axis, 2 bars
  // per clock hour, and a purple cumulative line (read+write) rising over
  // the bars. Legends carry the last FULL bucket's value plus the
  // cumulative total, so the numbers are readable without the chart.
  function hourlyChartCards(curveDevice, hourly) {
    if (!hourly || !hourly.length) return '';
    var last = lastFullBucket(hourly);
    var cumGB = 0, cumIOS = 0;
    hourly.forEach(function (b) {
      cumGB += (b.read_gb || 0) + (b.write_gb || 0);
      cumIOS += (b.read_ios || 0) + (b.write_ios || 0);
    });
    var volLegend =
      legendItem('读量', COLOR_READ, last ? last.read_gb : null, 'GB') +
      legendItem('写量', COLOR_WRITE, last ? last.write_gb : null, 'GB') +
      legendItem('累计读写', COLOR_CUMUL, cumGB, 'GB');
    var iosLegend =
      legendItem('读次', COLOR_IOS_READ, last ? last.read_ios : null, '次') +
      legendItem('写次', COLOR_IOS_WRITE, last ? last.write_ios : null, '次') +
      legendItem('累计读写次', COLOR_CUMUL, cumIOS, '次');
    var id = safeID(curveDevice);
    return '<div class="chart-card chart-card-wide">' +
      '<div class="chart-head"><span>近 24 小时逐时数据量 (GB)（每柱 = 该整点起 1 小时）</span>' +
      '<span class="legend">' + volLegend + '</span></div>' +
      '<canvas id="chart-hvol-' + id + '" class="canvas-hourly"></canvas>' +
      '</div>' +
      '<div class="chart-card chart-card-wide">' +
      '<div class="chart-head"><span>近 24 小时逐时 IO 次数 (次)（每柱 = 该整点起 1 小时）</span>' +
      '<span class="legend">' + iosLegend + '</span></div>' +
      '<canvas id="chart-hios-' + id + '" class="canvas-hourly"></canvas>' +
      '</div>';
  }

  // lastFullBucket returns the newest non-partial bucket; falls back to the
  // newest bucket when everything is partial (fresh start).
  function lastFullBucket(hourly) {
    for (var i = hourly.length - 1; i >= 0; i--) {
      if (!hourly[i].partial) return hourly[i];
    }
    return hourly.length ? hourly[hourly.length - 1] : null;
  }

  // drawHourlyChart paints both hourly canvases (volume + counts).
  function drawHourlyChart(curveDevice, hourly) {
    if (!hourly || !hourly.length) return;
    var id = safeID(curveDevice);
    drawHourlyBars($('chart-hvol-' + id), hourly, 'volume');
    drawHourlyBars($('chart-hios-' + id), hourly, 'counts');
  }

  // drawHourlyBars renders one hourly bar chart in the given mode:
  // 'volume' (读/写 GB bars + cumulative GB line) or 'counts' (读/写 IO bars
  // + cumulative count line). Single Y axis; its max covers the cumulative
  // total so bars sit low and the line rises to the top. 24 slots
  // right-aligned, partial (current hour) bucket at reduced opacity,
  // hour-range x labels every step slots.
  function drawHourlyBars(canvas, hourly, mode) {
    if (!canvas) return;
    var dpr = window.devicePixelRatio || 1;
    var w = canvas.clientWidth || 900;
    var hpx = canvas.clientHeight || 200;
    canvas.width = w * dpr;
    canvas.height = hpx * dpr;
    var ctx = canvas.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, hpx);

    var padL = 48, padR = 10, padT = 12, padB = 26;
    var plotW = w - padL - padR;
    var plotH = hpx - padT - padB;
    var gridColor = cssVar('--grid-line') || '#eee';
    var axisColor = cssVar('--axis-text') || '#999';

    var SLOTS = 24;
    var slotW = plotW / SLOTS;
    var buckets = hourly.slice(-SLOTS);
    var offset = SLOTS - buckets.length; // right-align: uncovered past stays blank

    // Mode selects the two bar series and the cumulative sum.
    var readOf = mode === 'volume' ? function (b) { return b.read_gb || 0; }
                                   : function (b) { return b.read_ios || 0; };
    var writeOf = mode === 'volume' ? function (b) { return b.write_gb || 0; }
                                    : function (b) { return b.write_ios || 0; };
    var colorA = mode === 'volume' ? COLOR_READ : COLOR_IOS_READ;
    var colorB = mode === 'volume' ? COLOR_WRITE : COLOR_IOS_WRITE;

    // Scale: the axis max covers the cumulative total (so the line reaches
    // the top while bars stay below) and the largest single bucket.
    var cum = 0, cums = [];
    var maxV = 0;
    buckets.forEach(function (b) {
      var a = readOf(b), bv = writeOf(b);
      cum += a + bv;
      cums.push(cum);
      maxV = Math.max(maxV, a, bv);
    });
    var axisMax = Math.max(cum, maxV, 0.0001) * 1.08;

    // Horizontal gridlines + single left axis labels.
    ctx.font = '11px sans-serif';
    for (var i = 0; i <= 4; i++) {
      var gy = padT + plotH - plotH * i / 4;
      ctx.strokeStyle = gridColor;
      ctx.beginPath();
      ctx.moveTo(padL, gy);
      ctx.lineTo(w - padR, gy);
      ctx.stroke();
      ctx.fillStyle = axisColor;
      ctx.textAlign = 'right';
      ctx.fillText(fmtNum(axisMax * i / 4), padL - 6, gy);
    }

    // Label step: 2 hours when the plot is wide, 3 otherwise.
    var step = plotW >= 800 ? 2 : 3;
    ctx.textAlign = 'center';
    for (var s = 0; s < SLOTS; s++) {
      if (s % step !== 0) continue;
      var bi = s - offset; // bucket index in this slot
      if (bi < 0 || bi >= buckets.length) continue;
      var x0 = padL + s * slotW;
      // Light vertical gridline at the labeled hour boundary.
      ctx.strokeStyle = gridColor;
      ctx.beginPath();
      ctx.moveTo(x0, padT);
      ctx.lineTo(x0, padT + plotH);
      ctx.stroke();
      ctx.fillStyle = axisColor;
      ctx.fillText(hourRangeLabel(buckets[bi]), x0 + slotW / 2, padT + plotH + 14);
    }

    // Bars: 2 per slot (读/写 in the chart's unit).
    var barW = slotW / 3;
    buckets.forEach(function (b, bIdx) {
      var s = offset + bIdx;
      var x0 = padL + s * slotW + (slotW - 2 * barW) / 2;
      ctx.globalAlpha = b.partial ? 0.45 : 1;
      drawBar(ctx, x0, padT + plotH, barW, readOf(b) / axisMax, plotH, colorA);
      drawBar(ctx, x0 + barW, padT + plotH, barW, writeOf(b) / axisMax, plotH, colorB);
      ctx.globalAlpha = 1;
    });

    // Cumulative line (read+write in the chart's unit) across bucket right
    // edges, starting from the first bucket's left edge at zero.
    ctx.strokeStyle = COLOR_CUMUL;
    ctx.lineWidth = 2;
    ctx.beginPath();
    buckets.forEach(function (b, bIdx) {
      var xEnd = padL + (offset + bIdx + 1) * slotW;
      var y = padT + plotH - plotH * (cums[bIdx] / axisMax);
      if (bIdx === 0) {
        var xStart = padL + (offset + bIdx) * slotW;
        ctx.moveTo(xStart, padT + plotH);
        ctx.lineTo(xEnd, y);
      } else {
        ctx.lineTo(xEnd, y);
      }
    });
    ctx.stroke();
  }

  function drawBar(ctx, x, baseY, bw, frac, plotH, color) {
    if (frac <= 0) return;
    var h = Math.max(1, plotH * Math.min(frac, 1));
    ctx.fillStyle = color;
    ctx.fillRect(x, baseY - h, Math.max(bw - 0.5, 0.5), h);
  }

  // hourRangeLabel turns "10:00" into "10:00–11:00" (partial buckets read
  // "10:00–至今").
  function hourRangeLabel(b) {
    if (b.partial) return b.hour + '–至今';
    var parts = b.hour.split(':');
    var h = (parseInt(parts[0], 10) + 1) % 24;
    var hh = (h < 10 ? '0' : '') + h;
    return b.hour + '–' + hh + ':' + (parts[1] || '00');
  }

  // windowStatsHTML renders the 1/6/12/24h traffic/IO totals as a compact
  // table. Windows still accumulating show a footnote.
  function windowStatsHTML(stats) {
    if (!stats || !stats['1h']) return '';
    var labels = ['1h', '6h', '12h', '24h'];
    var fullMinutes = { '1h': 60, '6h': 360, '12h': 720, '24h': 1440 };
    var partial = false;
    labels.forEach(function (l) {
      if (!stats[l] || stats[l].covered_minutes < fullMinutes[l]) partial = true;
    });
    function row(name, fmt) {
      return '<tr><td>' + name + '</td>' + labels.map(function (l) {
        var st = stats[l];
        return '<td>' + (st ? fmt(st) : 'N/A') + '</td>';
      }).join('') + '</tr>';
    }
    return '<div class="window-stats">' +
      '<div class="window-title">窗口统计（读写总量）</div>' +
      '<table class="window-table">' +
      '<tr><th></th>' + labels.map(function (l) { return '<th>近' + l + '</th>'; }).join('') + '</tr>' +
      row('读数据量', function (st) { return fmtGB(st.read_gb); }) +
      row('写数据量', function (st) { return fmtGB(st.write_gb); }) +
      row('读 IO 次数', function (st) { return fmtNum(st.read_ios); }) +
      row('写 IO 次数', function (st) { return fmtNum(st.write_ios); }) +
      '</table>' +
      (partial ? '<div class="hint">* 部分窗口仍在积累中，数值为已积累时段的总量</div>' : '') +
      '</div>';
  }

  // physicalCard renders one physical disk with the FULL SMART table laid out
  // flat — no click interaction. SSD/HDD badge distinguishes media.
  function physicalCard(d) {
    var h = healthOf(d);
    var mediaBadge = d.media === 'hdd' ? '<span class="badge badge-hdd">HDD</span>' : '<span class="badge badge-ssd">SSD</span>';
    return '<div class="disk-card disk-card-flat">' +
      '<div class="disk-head">' +
      '  <span class="health-dot ' + h + '" title="' + (h === 'ok' ? '健康' : h === 'err' ? '异常' : '无 SMART 数据') + '"></span>' +
      '  <span class="disk-device">' + esc(d.device) + '</span>' + mediaBadge +
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

  // drawTempChart feeds each member's temperature rolling buffer (60s
  // smartctl cache → step-like curve) and draws the per-disk lines. Missing
  // temperatures push null points so series stay aligned; the renderer
  // breaks the line on null.
  function drawTempChart(curveDevice, members) {
    var canvas = $('chart-temp-' + safeID(curveDevice));
    if (!canvas) return;
    var series = [];
    (members || []).forEach(function (m, i) {
      var key = 'temp:' + m.device;
      var h = state.history[key];
      if (!h) {
        h = [];
        state.history[key] = h;
      }
      var t = (m.smart && m.smart.temperature != null && !isNaN(m.smart.temperature))
        ? m.smart.temperature : null;
      push(h, t);
      series.push({ color: PALETTE[i % PALETTE.length], data: h });
    });
    drawChart(canvas, series);
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
      s.data.forEach(function (v) { if (v != null && v > max) max = v; });
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
      var penDown = false;
      for (var j = 0; j < s.data.length; j++) {
        var v = s.data[j];
        if (v == null) { penDown = false; continue; } // break the line on gaps
        var x = x0 + step * j;
        var yy = padT + plotH - plotH * (v / nice);
        if (!penDown) { ctx.moveTo(x, yy); penDown = true; } else { ctx.lineTo(x, yy); }
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
