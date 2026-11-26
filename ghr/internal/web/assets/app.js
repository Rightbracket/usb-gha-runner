"use strict";
(function () {
  const $ = (s, el = document) => el.querySelector(s);
  const esc = (v) => String(v ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  const gb = (v) => (v ?? 0).toFixed(1) + " GB";
  const dur = (s) => {
    s = Math.max(0, Math.round(s || 0));
    const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
    if (d) return `${d}d ${h}h`;
    if (h) return `${h}h ${m}m`;
    if (m) return `${m}m ${s % 60}s`;
    return `${s}s`;
  };
  const ago = (t) => (t ? dur((Date.now() - new Date(t).getTime()) / 1000) + " ago" : "");
  const dl = (el, rows) => { el.innerHTML = rows.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${v}</dd>`).join(""); };
  const rowsOr = (tbody, rows, cols, empty) => {
    tbody.innerHTML = rows.length ? rows.join("") : `<tr><td colspan="${cols}" class="empty">${esc(empty)}</td></tr>`;
  };

  // Confirm dialogs for destructive buttons (inline handlers are blocked by CSP).
  document.addEventListener("submit", (e) => {
    const msg = e.target.dataset && e.target.dataset.confirm;
    if (msg && !window.confirm(msg)) e.preventDefault();
  });

  // Config page: show only the fields for the chosen credential type.
  const form = $("#cfg-form");
  if (form) {
    const sync = () => {
      const t = (form.querySelector("input[name=auth_type]:checked") || {}).value || "app";
      form.querySelectorAll("[data-auth]").forEach((el) => el.classList.toggle("hidden", el.dataset.auth !== t));
    };
    form.addEventListener("change", sync);
    sync();
  }

  // Status page: refresh every 5 seconds.
  const page = $("[data-page=status]");
  if (page) {
    const render = (st) => {
      $("#target").textContent = st.target || "";
      $("#not-configured").classList.toggle("hidden", !!st.configured);
      rowsOr($("#slots tbody"), (st.slots || []).map((s) => `<tr>
        <td>${s.slot}</td><td>${esc(s.runner_name)}</td>
        <td><span class="state s-${esc(s.state)}">${esc(s.state)}</span>${s.last_error ? `<div class="muted small">${esc(s.last_error)}</div>` : ""}</td>
        <td>${esc(s.job || "")}</td><td>${esc(ago(s.since))}</td>
        <td>${(s.cpus || 0).toFixed(1)} CPU / ${((s.memory_mb || 0) / 1024).toFixed(1)} GB</td></tr>`), 6,
        st.configured ? "Starting worker slots…" : "No slots running.");
      rowsOr($("#history tbody"), (st.history || []).map((h) => `<tr>
        <td>${esc(ago(h.time))}</td><td>${esc(h.runner_name)}</td><td>${esc(h.job)}</td>
        <td><span class="state s-${esc(h.result)}">${esc(h.result)}</span></td><td>${esc(dur(h.seconds))}</td></tr>`), 5, "No jobs yet.");

      const m = st.machine || {};
      dl($("#machine"), [
        ["Uptime", esc(dur(m.uptime_sec))],
        ["CPU", `${m.cpus} threads, load ${(m.load1 || 0).toFixed(2)}`],
        ["Memory", `${((m.mem_total_mb - m.mem_avail_mb) / 1024).toFixed(1)} of ${(m.mem_total_mb / 1024).toFixed(1)} GB used`],
        ["Temperature", m.temp_c ? `${m.temp_c.toFixed(0)} °C` : "n/a"],
        ["IP", esc((m.ips || []).join(", ") || "none")],
        ["GitHub", m.github_ok ? `<span class="pill ok">reachable</span>` : `<span class="pill bad">unreachable</span> <span class="muted small">${esc(m.github_error)}</span>`],
      ]);
      const k = st.stick || {};
      const pct = k.data_used_pct || 0;
      dl($("#stick"), [
        ["/data", `${gb(k.data_total_gb - k.data_free_gb)} of ${gb(k.data_total_gb)} used<div class="meter ${pct >= 80 ? "hot" : ""}"><span></span></div>`],
        ["Builder cache", gb(k.cache_gb)],
        ["Auto-prune", "nightly, when over 80% full"],
      ]);
      const bar = $("#stick .meter span");
      if (bar) bar.style.width = Math.min(100, pct).toFixed(0) + "%"; // CSSOM, allowed by the CSP
      const b = st.builders || {};
      const ok = (v) => v === "active" ? `<span class="pill ok">running</span>` : `<span class="pill bad">${esc(v)}</span>`;
      dl($("#builders"), [
        ["Docker", ok(b.docker)],
        ["Sysbox", ok(b.sysbox)],
        ["Podman / Buildah", b.podman ? "installed" : "missing"],
        ["QEMU", esc((b.qemu || []).join(", ") || "none registered")],
        ["SSH", esc(st.ssh === "active" ? "on" : "off")],
      ]);
      const disks = (st.disks || []).filter((d) => !d.boot_stick);
      const boot = (st.disks || []).find((d) => d.boot_stick);
      const pill = $("#disks-ok");
      pill.className = "pill " + (st.disks_ok ? "ok" : "bad");
      pill.textContent = st.disks_ok ? "All host disks protected" : "Check host disks";
      const drow = (d) => `<tr><td>${esc(d.name)}${d.boot_stick ? ' <span class="muted">(boot stick)</span>' : ""}</td><td>${esc(d.model)}</td><td>${gb(d.size_gb)}</td>
        <td>${d.boot_stick ? "n/a" : d.read_only ? "Yes" : "<b>No</b>"}</td>
        <td>${d.boot_stick ? "stick" : (d.mounted || []).length ? "<b>" + esc(d.mounted.join(", ")) + "</b>" : "No"}</td>
        <td>${esc([...(d.holders || []), d.swap ? "swap" : ""].filter(Boolean).join(", ") || "-")}</td></tr>`;
      rowsOr($("#disks tbody"), [...(boot ? [drow(boot)] : []), ...disks.map(drow)], 6, "No disks found.");
      $("#updated").textContent = "Updated " + new Date().toLocaleTimeString();
    };
    const tick = async () => {
      try {
        const r = await fetch("/api/status", { credentials: "same-origin" });
        if (r.status === 401) { location.href = "/login"; return; }
        const st = await r.json();
        if (st.error) throw new Error(st.error);
        render(st);
      } catch (e) {
        $("#updated").textContent = "Can't reach the runner service: " + e.message;
      }
    };
    tick();
    setInterval(tick, 5000);
  }

  // Logs page: server-sent events from journalctl.
  const log = $("#log");
  if (log) {
    let es;
    const open = (unit) => {
      if (es) es.close();
      log.textContent = "";
      es = new EventSource("/api/logs?unit=" + encodeURIComponent(unit));
      es.onmessage = (e) => {
        const stick = log.scrollTop + log.clientHeight >= log.scrollHeight - 20;
        log.append(e.data + "\n");
        if (log.childNodes.length > 4000) log.removeChild(log.firstChild);
        if (stick) log.scrollTop = log.scrollHeight;
      };
    };
    $("#log-tabs").addEventListener("click", (e) => {
      const u = e.target.dataset && e.target.dataset.unit;
      if (!u) return;
      document.querySelectorAll("#log-tabs button").forEach((b) => b.classList.toggle("on", b === e.target));
      open(u);
    });
    open("runner");
  }
})();
