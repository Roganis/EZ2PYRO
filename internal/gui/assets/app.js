// Link Doctor GUI: polls /api/state once a second and drives the host and
// test through the local API.
(function () {
  "use strict";
  var token = new URLSearchParams(location.search).get("t") || "";
  var $ = function (id) { return document.getElementById(id); };
  var state = null;
  var stallsSeen = 0, lastProgressLen = 0;

  function store(k, v) { try { localStorage.setItem("ld." + k, v); } catch (e) {} }
  function load(k, d) { try { var v = localStorage.getItem("ld." + k); return v === null ? d : v; } catch (e) { return d; } }

  function api(method, path, body) {
    return fetch(path, {
      method: method,
      headers: { "X-Token": token, "Content-Type": "application/json" },
      body: body ? JSON.stringify(body) : undefined,
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) {
        if (!r.ok) throw new Error(j.error || ("HTTP " + r.status));
        return j;
      });
    });
  }

  function text(el, s) { el.textContent = s == null ? "" : s; }
  function fmt(n, d) { return (n == null || isNaN(n)) ? "—" : Number(n).toFixed(d == null ? 0 : d); }
  function clock(s) { s = Math.max(0, Math.round(s)); var m = Math.floor(s / 60); return m + ":" + String(s % 60).padStart(2, "0"); }
  var modeNames = { ramp: "Best bitrate", soak: "Stutter hunt", live: "Live monitor" };

  // ---- tabs ----
  function showTab(name) {
    ["test", "host", "results"].forEach(function (t) {
      $("tab-" + t).setAttribute("aria-selected", t === name ? "true" : "false");
      $("panel-" + t).hidden = t !== name;
    });
    store("tab", name);
    if (name === "results") loadRuns();
  }
  document.querySelectorAll(".tabs button").forEach(function (b) {
    b.addEventListener("click", function () { showTab(b.dataset.tab); });
  });

  // ---- test form ----
  $("peer").value = load("peer", "");
  $("bitrate").value = load("bitrate", "150");
  $("duration").value = load("duration", "30");
  var savedMode = load("mode", "ramp");
  document.querySelectorAll("input[name=mode]").forEach(function (r) { r.checked = r.value === savedMode; });

  function mode() { var r = document.querySelector("input[name=mode]:checked"); return r ? r.value : "ramp"; }
  function syncForm() {
    var m = mode();
    $("opt-bitrate").hidden = m === "ramp";
    $("opt-duration").hidden = m !== "soak";
  }
  document.querySelectorAll("input[name=mode]").forEach(function (r) { r.addEventListener("change", syncForm); });
  syncForm();

  $("start").addEventListener("click", function () {
    var p = {
      peer: $("peer").value.trim(),
      mode: mode(),
      bitrate_mbps: Number($("bitrate").value) || 150,
      duration_min: Number($("duration").value) || 30,
      fps: Number($("fps").value) || 60,
      budget_ms: Number($("budget").value) || 0,
      pkt_size: Number($("pkt").value) || 1200,
      freebox: $("freebox").checked,
    };
    store("peer", p.peer); store("mode", p.mode); store("bitrate", String(p.bitrate_mbps)); store("duration", String(p.duration_min));
    $("form-error").hidden = true;
    $("start").disabled = true;
    stallsSeen = 0; lastProgressLen = 0;
    api("POST", "/api/test/start", p).then(poll).catch(function (e) {
      $("form-error").hidden = false; text($("form-error"), e.message);
    }).finally(function () { $("start").disabled = false; });
  });
  $("stop").addEventListener("click", function () { $("stop").disabled = true; api("POST", "/api/test/stop").then(poll); });
  function reset() { api("POST", "/api/test/reset").then(poll); }
  $("again").addEventListener("click", reset);
  $("err-back").addEventListener("click", reset);

  // ---- host ----
  $("host-toggle").addEventListener("click", function () {
    var running = state && state.host.running;
    $("host-toggle").disabled = true;
    api("POST", running ? "/api/host/stop" : "/api/host/start").catch(function (e) {
      $("host-error").hidden = false; text($("host-error"), e.message);
    }).then(poll).finally(function () { $("host-toggle").disabled = false; });
  });

  // ---- rendering ----
  function renderHost(h) {
    $("host-dot").className = "dot" + (h.running ? " good" : "");
    text($("host-status"), h.running ? "Hosting — waiting for the Deck" : "Not hosting");
    if (h.running && h.log.some(function (l) { return l.indexOf("sending session") >= 0; }) &&
        !/controller disconnected|stopped hosting/.test(h.log[h.log.length - 1] || "")) {
      text($("host-status"), "Hosting — test running");
    }
    $("host-toggle").textContent = h.running ? "Stop hosting" : "Start hosting";
    $("host-toggle").className = h.running ? "big" : "primary big";
    $("host-addrs").hidden = !h.running;
    var list = $("addr-list");
    var addrs = h.addrs || [];
    if (list.dataset.v !== addrs.join(",")) {
      list.dataset.v = addrs.join(",");
      list.innerHTML = "";
      addrs.forEach(function (a) { var d = document.createElement("div"); d.className = "addr"; d.textContent = a; list.appendChild(d); });
      if (!addrs.length) {
        var d = document.createElement("div"); d.className = "muted";
        d.textContent = "No home-network address found. Is this PC connected to your router?";
        list.appendChild(d);
      }
    }
    if (h.error) { $("host-error").hidden = false; text($("host-error"), h.error); }
    else if (h.running) $("host-error").hidden = true;
    setLog($("host-log"), h.log);
  }

  function setLog(pre, lines) {
    var s = (lines || []).join("\n");
    if (pre.textContent !== s) {
      var atBottom = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 4;
      pre.textContent = s;
      if (atBottom) pre.scrollTop = pre.scrollHeight;
    }
  }

  function renderTest(t) {
    var st = t.state;
    $("test-form").hidden = st !== "idle";
    $("test-run").hidden = !(st === "running" || st === "stopping");
    $("test-done").hidden = st !== "done";
    $("test-error").hidden = st !== "error";
    if (st === "running" || st === "stopping") renderRunning(t);
    if (st === "done") renderDone(t);
    if (st === "error") { text($("err-text"), t.error); setLog($("err-log"), t.log); }
  }

  function renderRunning(t) {
    var p = t.params, prog = t.progress || [];
    var last = prog[prog.length - 1];
    text($("run-label"), t.state === "stopping" ? "Stopping — writing the report…" : "Running");
    var title = modeNames[p.mode] + " against " + p.peer;
    if (p.mode !== "ramp") title += " at " + fmt(p.bitrate_mbps) + " Mbit/s";
    text($("run-title"), title);
    $("stop").disabled = t.state === "stopping";
    $("stop").textContent = p.mode === "live" ? "Stop" : "Stop and write report";

    var elapsed = (Date.now() - t.started_unix_ms) / 1000;
    var bar = $("bar-wrap");
    if (p.mode === "soak") {
      var total = p.duration_min * 60;
      bar.className = "bar";
      $("bar").style.width = Math.min(100, 100 * elapsed / total) + "%";
      text($("run-time"), clock(elapsed) + " of " + clock(total) + " — you can stop early; the report is still written.");
    } else {
      bar.className = "bar indeterminate";
      $("bar").style.width = "";
      text($("run-time"), clock(elapsed) + (p.mode === "ramp" ? " — usually 2–4 minutes" : " — press Stop when done"));
    }

    // Count stalls seen live (runs of stall seconds).
    for (var i = lastProgressLen; i < prog.length; i++) {
      if (prog[i].stall && !(i > 0 && prog[i - 1].stall)) stallsSeen++;
    }
    lastProgressLen = prog.length;
    var lateTotal = 0, framesTotal = 0;
    prog.forEach(function (x) { lateTotal += x.late; framesTotal += x.frames; });

    if (last) {
      text($("t-mbps"), fmt(last.mbps));
      text($("t-target"), "Mbit/s · target " + fmt(last.bitrate_mbps));
      text($("t-late"), last.late + " / " + last.frames);
      $("t-late").parentNode.classList.toggle("bad", last.late > 0);
      text($("t-late-total"), framesTotal ? fmt(100 * lateTotal / framesTotal, 2) + "% so far" : "");
      text($("t-loss"), fmt(last.loss_pct, 2) + "%");
      text($("t-p99"), fmt(last.p99_delay_ms, 1) + " ms");
      text($("t-rtt"), fmt(last.rtt_ms, 1) + " ms");
    }
    text($("t-stalls"), stallsSeen);
    $("t-stalls").parentNode.classList.toggle("bad", stallsSeen > 0);
    drawChart(prog);

    var steps = t.steps || [];
    $("steps-wrap").hidden = p.mode !== "ramp" || steps.length === 0;
    var tb = $("steps");
    if (tb.dataset.n !== String(steps.length)) {
      tb.dataset.n = String(steps.length);
      tb.innerHTML = "";
      steps.forEach(function (s) { tb.appendChild(stepRow(s)); });
    }
    setLog($("test-log"), t.log);
  }

  function stepRow(s) {
    var tr = document.createElement("tr");
    [fmt(s.bitrate_mbps) + " Mbit/s", fmt(s.recv_mbps) + " Mbit/s", fmt(s.late_pct, 2) + "%", fmt(s.loss_pct, 2) + "%"].forEach(function (v) {
      var td = document.createElement("td"); td.className = "num"; td.textContent = v; tr.appendChild(td);
    });
    var td = document.createElement("td");
    td.innerHTML = '<span class="status-dot"><span class="dot ' + (s.clean ? "good" : "bad") + '"></span></span>';
    td.firstChild.appendChild(document.createTextNode(s.clean ? "clean" : "stutters"));
    tr.appendChild(td);
    return tr;
  }

  function cssVar(n) { return getComputedStyle(document.documentElement).getPropertyValue(n).trim(); }

  // Received throughput (line), target (dashed) and stall seconds (red ticks).
  function drawChart(prog) {
    var c = $("chart"), dpr = window.devicePixelRatio || 1;
    var w = c.clientWidth, h = 150;
    if (c.width !== Math.round(w * dpr)) { c.width = Math.round(w * dpr); c.height = Math.round(h * dpr); }
    var ctx = c.getContext("2d");
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, h);
    var pts = prog.slice(-300);
    var padL = 44, padB = 18, padT = 6, cw = w - padL - 6, ch = h - padB - padT;
    var maxY = 10;
    pts.forEach(function (p) { maxY = Math.max(maxY, p.mbps, p.bitrate_mbps); });
    maxY *= 1.15;
    var n = Math.max(pts.length, 60);
    var x = function (i) { return padL + cw * i / (n - 1); };
    var y = function (v) { return padT + ch * (1 - v / maxY); };
    ctx.font = "11px system-ui, sans-serif";
    ctx.fillStyle = cssVar("--muted");
    ctx.strokeStyle = cssVar("--grid");
    ctx.lineWidth = 1;
    for (var g = 0; g <= 2; g++) {
      var gv = maxY * g / 2 / 1.15, gy = y(gv);
      ctx.beginPath(); ctx.moveTo(padL, gy); ctx.lineTo(w - 6, gy); ctx.stroke();
      ctx.fillText(Math.round(gv), 4, gy + 4);
    }
    if (pts.length) ctx.fillText("last " + clock(pts.length) , padL, h - 4);
    ctx.fillStyle = cssVar("--critical");
    pts.forEach(function (p, i) { if (p.stall) ctx.fillRect(x(i) - 1.5, padT + ch - 12, 3, 12); });
    function line(get, color, dash) {
      ctx.beginPath(); ctx.setLineDash(dash || []); ctx.strokeStyle = color; ctx.lineWidth = 2;
      pts.forEach(function (p, i) { var yy = y(get(p)); if (i) ctx.lineTo(x(i), yy); else ctx.moveTo(x(i), yy); });
      ctx.stroke(); ctx.setLineDash([]);
    }
    if (pts.length > 1) {
      line(function (p) { return p.bitrate_mbps; }, cssVar("--muted"), [5, 4]);
      line(function (p) { return p.mbps; }, cssVar("--accent"));
    }
  }

  function renderDone(t) {
    var r = t.result, v = r.verdicts && r.verdicts[0];
    var box = $("verdict");
    if (box.dataset.dir !== r.dir) {
      box.dataset.dir = r.dir;
      box.innerHTML = "";
      var add = function (tag, cls, s) { var e = document.createElement(tag); if (cls) e.className = cls; e.textContent = s; box.appendChild(e); return e; };
      add("div", "label", modeNames[t.params.mode] + " finished" + (t.error ? " early" : ""));
      if (v) {
        add("div", "cause", v.cause);
        add("div", "", v.evidence);
        var fx = add("div", "fix", ""); var b = document.createElement("b"); b.textContent = "What to do: "; fx.appendChild(b); fx.appendChild(document.createTextNode(v.fix));
      }
      if (r.ramp && r.ramp.recommended_mbps) {
        var rec = add("div", "fix", "");
        var rb = document.createElement("b"); rb.textContent = "Set Pyrowave to about " + fmt(r.ramp.recommended_mbps) + " Mbit/s";
        rec.appendChild(rb); rec.appendChild(document.createTextNode(" (70% of the highest clean step, " + fmt(r.ramp.highest_clean_mbps) + " Mbit/s)."));
      }
      if (r.verdicts && r.verdicts.length > 1) {
        var ul = document.createElement("ul");
        r.verdicts.slice(1, 4).forEach(function (o) { var li = document.createElement("li"); li.textContent = "Also: " + o.cause + " — " + o.evidence; ul.appendChild(li); });
        box.appendChild(ul);
      }
      var s = r.summary, tiles = $("done-tiles");
      tiles.innerHTML = "";
      [["Late frames", fmt(s.late_pct, 2) + "%", s.late_frames + " of " + s.frames],
       ["Stalls", String(s.stalls), s.stalls ? "longest " + fmt(s.longest_stall_ms) + " ms" : "none"],
       ["Packet loss", fmt(s.loss_pct, 2) + "%", s.packets_lost + " packets"],
       ["Frame delay p99", fmt(s.p99_delay_ms, 1) + " ms", "p50 " + fmt(s.p50_delay_ms, 1) + " ms"],
       ["Throughput", fmt(s.mean_mbps), "Mbit/s · RTT " + fmt(s.median_rtt_ms, 1) + " ms"]].forEach(function (x) {
        var d = document.createElement("div"); d.className = "tile";
        ["k", "v", "n"].forEach(function (c, i) { var e = document.createElement("div"); e.className = c; e.textContent = x[i]; d.appendChild(e); });
        tiles.appendChild(d);
      });
      var warn = $("done-warn"); warn.innerHTML = "";
      (r.warnings || []).concat(t.error ? [t.error] : []).forEach(function (w) {
        var d = document.createElement("div"); d.className = "alert"; d.textContent = w; warn.appendChild(d);
      });
    }
    var a = $("open-report");
    a.hidden = !r.report_url;
    if (r.report_url) a.href = r.report_url;
  }

  // ---- results ----
  var selected = [];
  function loadRuns() {
    api("GET", "/api/runs").then(function (runs) {
      var tb = $("runs"); tb.innerHTML = "";
      $("runs-empty").hidden = runs.length > 0;
      selected = selected.filter(function (n) { return runs.some(function (r) { return r.name === n; }); });
      runs.forEach(function (r) {
        var tr = document.createElement("tr");
        var td0 = document.createElement("td"); var cb = document.createElement("input"); cb.type = "checkbox";
        cb.checked = selected.indexOf(r.name) >= 0;
        cb.setAttribute("aria-label", "select " + r.name);
        cb.addEventListener("change", function () {
          if (cb.checked) selected.push(r.name); else selected = selected.filter(function (n) { return n !== r.name; });
          if (selected.length > 2) { selected.shift(); loadRuns(); }
          $("compare").disabled = selected.length !== 2;
        });
        td0.appendChild(cb); tr.appendChild(td0);
        var when = r.started ? new Date(r.started).toLocaleString() : r.name;
        var what = (modeNames[r.mode] || r.mode) + (r.mode === "ramp" ? "" : " · " + fmt(r.bitrate_mbps) + " Mbit/s");
        if (r.mode === "ramp" && r.summary.recommended_mbps) what += " → " + fmt(r.summary.recommended_mbps) + " Mbit/s";
        [when, what, fmt(r.summary.late_pct, 2) + "%", String(r.summary.stalls), r.top ? r.top.cause : "—"].forEach(function (v, i) {
          var td = document.createElement("td"); td.textContent = v; if (i === 2 || i === 3) td.className = "num"; tr.appendChild(td);
        });
        var tdl = document.createElement("td");
        var a = document.createElement("a"); a.className = "button"; a.textContent = "Open"; a.target = "_blank"; a.rel = "noopener";
        a.href = "/runs/" + encodeURIComponent(r.name) + "/report.html";
        tdl.appendChild(a); tr.appendChild(tdl);
        tb.appendChild(tr);
      });
      $("compare").disabled = selected.length !== 2;
    });
  }
  $("compare").addEventListener("click", function () {
    // Older run as A.
    var rows = selected.slice().sort();
    var win = window.open("about:blank", "_blank");
    api("POST", "/api/compare", { a: rows[0], b: rows[1] }).then(function (j) {
      if (win) win.location = j.url; else location.href = j.url;
    }).catch(function (e) { if (win) win.close(); alert(e.message); });
  });

  // ---- polling ----
  var prevTestState = null;
  function poll() {
    return api("GET", "/api/state").then(function (s) {
      state = s;
      text($("whoami"), s.hostname + " · " + s.os + " · " + s.version);
      text($("outdir"), "Saved in " + s.out_dir);
      renderHost(s.host);
      renderTest(s.test);
      if (prevTestState && prevTestState !== "done" && s.test.state === "done") loadRuns();
      prevTestState = s.test.state;
    }).catch(function (e) {
      text($("whoami"), "Connection to linkdoctor lost — is it still running? (" + e.message + ")");
    });
  }
  function loop() { poll().finally(function () { setTimeout(loop, document.hidden ? 3000 : 1000); }); }

  showTab(load("tab", navigator.platform.indexOf("Win") === 0 ? "host" : "test"));
  window.addEventListener("resize", function () { if (state) drawChart(state.test.progress || []); });
  loop();
})();
