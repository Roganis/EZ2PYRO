// linkdoctor report charts: aligned, zoom-synced uPlot lanes.
(function () {
  "use strict";
  var D = window.LD_DATA, CATS = window.LD_EVENT_CATS;
  var css = getComputedStyle(document.documentElement);
  function v(name) { return css.getPropertyValue(name).trim(); }
  var C = {
    s1: v("--series-1"), s2: v("--series-2"), s3: v("--series-3"), s4: v("--series-4"),
    s5: v("--series-5"), s6: v("--series-6"), s7: v("--series-7"),
    grid: v("--grid"), text: v("--text-secondary"), muted: v("--text-muted"),
    band: v("--stall-band"), critical: v("--critical"), surface: v("--surface-1"),
  };
  var catColors = [C.s2, C.s7, C.s5, C.s6];
  var AXIS_W = 104; // same y-axis width on every lane keeps the time axes aligned

  function fmtClock(s) {
    if (s == null) return "";
    var neg = s < 0; s = Math.abs(s);
    var m = Math.floor(s / 60), r = s - m * 60;
    return (neg ? "-" : "") + m + ":" + (r < 10 ? "0" : "") + r.toFixed(r % 1 ? 1 : 0);
  }
  var plots = [];
  var syncing = false;
  var readout = document.getElementById("readout");

  function stallBands(u) {
    var ctx = u.ctx, top = u.bbox.top, h = u.bbox.height;
    ctx.save();
    ctx.fillStyle = C.band;
    (D.stalls || []).forEach(function (s) {
      var x0 = u.valToPos(s[0], "x", true), x1 = u.valToPos(s[1], "x", true);
      if (x1 < u.bbox.left || x0 > u.bbox.left + u.bbox.width) return;
      ctx.fillRect(x0, top, Math.max(x1 - x0, 2), h);
    });
    ctx.restore();
  }

  function axes(yLabel, yFmt) {
    var font = "12px system-ui, sans-serif";
    return [
      { stroke: C.text, grid: { stroke: C.grid, width: 1 }, ticks: { stroke: C.grid }, font: font,
        values: function (u, vals) { return vals.map(fmtClock); } },
      { stroke: C.text, grid: { stroke: C.grid, width: 1 }, ticks: { show: false }, font: font, size: AXIS_W,
        label: yLabel, labelSize: 18, labelFont: font,
        values: yFmt ? function (u, vals) { return vals.map(yFmt); } : undefined },
    ];
  }

  function describe(u, idx) {
    if (idx == null) return "";
    var parts = ["t = " + fmtClock(u.data[0][idx])];
    for (var i = 1; i < u.series.length; i++) {
      var s = u.series[i], val = u.data[i][idx];
      if (!s.show || s.label == null || s.label === "") continue;
      if (val == null) continue;
      parts.push(s.label + " " + (s.fmt ? s.fmt(val) : val));
    }
    var t = u.data[0][idx];
    (D.stalls || []).forEach(function (st, k) {
      if (t >= st[0] - 0.05 && t <= st[1] + 0.05) parts.push("inside stall #" + (k + 1));
    });
    return parts.join(" · ");
  }

  function make(el, title, opts, data) {
    var width = el.clientWidth || 800;
    opts.width = width;
    opts.cursor = Object.assign({ y: false, sync: { key: "ld", setSeries: false }, drag: { x: true, y: false } }, opts.cursor || {});
    opts.legend = { show: false };
    opts.scales = Object.assign({ x: { time: false } }, opts.scales || {});
    opts.hooks = Object.assign({
      drawClear: [stallBands],
      setScale: [function (u, key) {
        if (key !== "x" || syncing) return;
        syncing = true;
        var mn = u.scales.x.min, mx = u.scales.x.max;
        plots.forEach(function (p) { if (p !== u) p.setScale("x", { min: mn, max: mx }); });
        syncing = false;
      }],
      setCursor: [function (u) { if (readout && u.cursor.idx != null && u.cursor.left >= 0) readout.textContent = describe(u, u.cursor.idx); }],
    }, opts.hooks || {});
    var p = new uPlot(opts, data, el);
    plots.push(p);
    return p;
  }

  var xmin = 0, xmax = 0;
  [D.delay, D.link, D.seconds].forEach(function (d) {
    if (d && d[0] && d[0].length) xmax = Math.max(xmax, d[0][d[0].length - 1]);
  });
  var xScale = { time: false, range: function (u, mn, mx) { return [mn, mx]; } };

  // Lane 1: p99 frame delay per 100 ms, with the late budget and incomplete frames.
  var el = document.getElementById("lane-delay");
  if (el && D.delay[0].length) {
    var ms = function (x) { return x.toFixed(1) + " ms"; };
    make(el, "delay", {
      height: 240,
      scales: { x: xScale, y: { range: function (u, mn, mx) { return [0, Math.max((mx || 0) * 1.1, D.budget * 1.5, 5)]; } } },
      axes: axes("ms"),
      series: [{}, { label: "p99 delay", stroke: C.s1, width: 2, fmt: ms, spanGaps: false, points: { show: false } },
               { label: "incomplete frames", show: true, stroke: "transparent", width: 0, points: { show: false } }],
      hooks: {
        draw: [function (u) {
          var ctx = u.ctx, y = u.valToPos(D.budget, "y", true);
          ctx.save();
          ctx.strokeStyle = C.muted; ctx.setLineDash([5, 4]); ctx.lineWidth = 1.5;
          ctx.beginPath(); ctx.moveTo(u.bbox.left, y); ctx.lineTo(u.bbox.left + u.bbox.width, y); ctx.stroke();
          ctx.setLineDash([]);
          ctx.fillStyle = C.critical;
          var base = u.bbox.top + u.bbox.height;
          for (var i = 0; i < u.data[0].length; i++) {
            if (u.data[2][i] > 0) {
              var x = u.valToPos(u.data[0][i], "x", true);
              if (x >= u.bbox.left && x <= u.bbox.left + u.bbox.width) ctx.fillRect(x - 1, base - 10, 3, 10);
            }
          }
          ctx.restore();
        }],
      },
    }, D.delay);
  }

  // Lane: delivered throughput per second (with the target bitrate).
  el = document.getElementById("lane-tput");
  if (el && D.seconds[0].length) {
    var target = D.seconds[0].map(function (t) {
      for (var i = 0; i < D.segments.length; i++) if (t >= D.segments[i][0] - 1 && t < D.segments[i][1]) return D.segments[i][2];
      return null;
    });
    var mb = function (x) { return x.toFixed(0) + " Mbit/s"; };
    make(el, "tput", {
      height: 150, scales: { x: xScale, y: { range: function (u, mn, mx) { return [0, Math.max(mx || 0, 10) * 1.1]; } } },
      axes: axes("Mbit/s"),
      series: [{}, { label: "delivered", stroke: C.s1, width: 2, fmt: mb, points: { show: false } },
               { label: "target", stroke: C.s2, width: 1.5, dash: [5, 4], fmt: mb, points: { show: false }, paths: uPlot.paths.stepped({ align: 1 }) }],
    }, [D.seconds[0], D.seconds[1], target]);
  }

  var L = D.link;
  if (L[0].length) {
    el = document.getElementById("lane-signal");
    make(el, "signal", {
      height: 130, scales: { x: xScale }, axes: axes("dBm"),
      series: [{}, { label: "signal", stroke: C.s1, width: 2, fmt: function (x) { return x + " dBm"; }, points: { show: false } }],
    }, [L[0], L[1]]);
    el = document.getElementById("lane-phy");
    var mbs = function (x) { return x.toFixed(0) + " Mbit/s"; };
    make(el, "phy", {
      height: 130, scales: { x: xScale, y: { range: function (u, mn, mx) { return [0, (mx || 100) * 1.1]; } } }, axes: axes("Mbit/s"),
      series: [{}, { label: "RX PHY", stroke: C.s1, width: 2, fmt: mbs, points: { show: false } },
               { label: "TX PHY", stroke: C.s2, width: 2, fmt: mbs, points: { show: false } }],
    }, [L[0], L[2], L[3]]);
    el = document.getElementById("lane-band");
    make(el, "band", {
      height: 90, scales: { x: xScale, y: { range: [1.5, 7] } },
      axes: axes("GHz", function (x) { return x === 2.4 || x === 5 || x === 6 ? String(x) : ""; }),
      series: [{}, { label: "band", stroke: C.s3, width: 2, fmt: function (x) { return x + " GHz"; }, points: { show: false }, paths: uPlot.paths.stepped({ align: 1 }) }],
    }, [L[0], L[4]]);
  }

  // Events lane: one row per category.
  el = document.getElementById("lane-events");
  if (el) {
    var ev = (D.events || []).slice().sort(function (a, b) { return a.t - b.t; });
    var xs = [xmin].concat(ev.map(function (e) { return e.t; })).concat([xmax]);
    var data = [xs];
    CATS.forEach(function (name, ci) {
      data.push([null].concat(ev.map(function (e) { return e.cat === ci ? ci + 1 : null; })).concat([null]));
    });
    var series = [{}];
    CATS.forEach(function (name, ci) {
      series.push({ label: name, stroke: catColors[ci], fill: catColors[ci], width: 0, paths: function () { return null; },
        points: { show: true, size: 9, stroke: C.surface, width: 1.5, fill: catColors[ci] }, fmt: function () {
          return "";
        } });
    });
    make(el, "events", {
      height: 50 + 30 * CATS.length, scales: { x: xScale, y: { range: [0.4, CATS.length + 0.6] } },
      axes: [axes("")[0], { stroke: C.text, grid: { show: false }, ticks: { show: false }, size: AXIS_W, label: " ", labelSize: 18, font: "12px system-ui, sans-serif",
        splits: function () { return CATS.map(function (_, i) { return i + 1; }); },
        values: function (u, vals) { return vals.map(function (x) { return CATS[x - 1] || ""; }); } }],
      series: series,
      hooks: { setCursor: [function (u) {
        var i = u.cursor.idx;
        if (i == null || i === 0 || i > ev.length || u.cursor.left < 0) return;
        readout.textContent = "t = " + fmtClock(ev[i - 1].t) + " · " + ev[i - 1].label;
      }] },
    }, data);
  }

  // Align every lane on the same full time range.
  syncing = true;
  plots.forEach(function (p) { p.setScale("x", { min: xmin, max: xmax }); });
  syncing = false;
  plots.forEach(function (p) {
    p.over.addEventListener("dblclick", function () {
      syncing = true;
      plots.forEach(function (q) { q.setScale("x", { min: xmin, max: xmax }); });
      syncing = false;
    });
  });

  var rs;
  window.addEventListener("resize", function () {
    clearTimeout(rs);
    rs = setTimeout(function () {
      plots.forEach(function (p) { p.setSize({ width: p.root.parentNode.clientWidth, height: p.height }); });
    }, 100);
  });
})();
