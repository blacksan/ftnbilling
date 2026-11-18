/* Fast Tech Net admin: small vanilla script. No framework, no network except
   this site. Every handler the pages used to carry inline lives here, so the
   admin CSP can refuse inline script. */
"use strict";
(function () {
  var doc = document, root = doc.documentElement;
  function $(s, r) { return (r || doc).querySelector(s); }
  function $$(s, r) { return Array.prototype.slice.call((r || doc).querySelectorAll(s)); }
  function el(tag, cls, text) { var e = doc.createElement(tag); if (cls) e.className = cls; if (text != null) e.textContent = text; return e; }
  function csrf() { var i = $("input[name=_csrf]"); return i ? i.value : ""; }
  function enc(v) { return encodeURIComponent(v); }
  function getJSON(url) {
    return fetch(url, { credentials: "same-origin", headers: { Accept: "application/json" } }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) { j.__status = r.status; return j; });
    });
  }
  function postForm(url, data) {
    var body = new URLSearchParams();
    for (var k in data) if (data[k] != null) body.append(k, data[k]);
    body.set("_csrf", csrf());
    return fetch(url, { method: "POST", credentials: "same-origin", body: body, headers: { Accept: "application/json", "Content-Type": "application/x-www-form-urlencoded" } })
      .then(function (r) { return r.json().catch(function () { return {}; }).then(function (j) { j.__status = r.status; return j; }); });
  }

  /* ---- theme: system, light, dark ------------------------------------------ */
  var MODES = ["system", "light", "dark"], LABEL = { system: "System", light: "Light", dark: "Dark" };
  function themeNow() { var t = root.getAttribute("data-theme"); return t === "light" || t === "dark" ? t : "system"; }
  function themeSet(m) {
    if (m === "system") { root.removeAttribute("data-theme"); try { localStorage.removeItem("ftn-theme"); } catch (e) {} }
    else { root.setAttribute("data-theme", m); try { localStorage.setItem("ftn-theme", m); } catch (e) {} }
    themeLabel();
  }
  function themeLabel() {
    var b = $("[data-theme-btn]"); if (!b) return;
    var n = MODES[(MODES.indexOf(themeNow()) + 1) % 3];
    b.setAttribute("aria-label", "Theme: " + LABEL[themeNow()] + ". Switch to " + LABEL[n]);
    b.title = "Theme: " + LABEL[themeNow()];
  }
  doc.addEventListener("click", function (e) {
    var b = e.target.closest("[data-theme-btn]");
    if (b) { themeSet(MODES[(MODES.indexOf(themeNow()) + 1) % 3]); toast("Theme: " + LABEL[themeNow()]); }
  });
  themeLabel();

  /* ---- toasts ---------------------------------------------------------------- */
  function toast(msg, bad) {
    var box = $("#toasts"); if (!box) return;
    var t = el("div", "toast" + (bad ? " bad" : "")); t.appendChild(el("span", "", msg));
    var x = el("button", "", "×"); x.type = "button"; x.setAttribute("aria-label", "Dismiss"); x.onclick = function () { t.remove(); };
    t.appendChild(x); box.appendChild(t);
    setTimeout(function () { t.remove(); }, 5000);
  }
  var flash = $("[data-flash]");
  if (flash) {
    toast(flash.textContent, flash.classList.contains("bad"));
    flash.hidden = true;
    try {
      var u = new URL(location.href);
      if (u.searchParams.has("msg")) { u.searchParams.delete("msg"); history.replaceState(null, "", u.pathname + u.search + u.hash); }
    } catch (e) {}
  }

  /* ---- sidebar ---------------------------------------------------------------- */
  function navOpen(on) { doc.body.classList.toggle("nav-open", on); }
  doc.addEventListener("click", function (e) {
    if (e.target.closest("[data-nav-open]")) navOpen(true);
    else if (e.target.closest("[data-nav-close]") || e.target.closest(".side a.nav")) navOpen(false);
  });
  var saved = {};
  try { saved = JSON.parse(localStorage.getItem("ftn-nav") || "{}") || {}; } catch (e) {}
  $$(".side details[data-g]").forEach(function (d) {
    var g = d.getAttribute("data-g");
    if (saved[g] === false && !d.querySelector("[aria-current=page]")) d.open = false;
    d.addEventListener("toggle", function () { saved[g] = d.open; try { localStorage.setItem("ftn-nav", JSON.stringify(saved)); } catch (e) {} });
  });
  var rs = $("#router-switch");
  if (rs) rs.addEventListener("change", function () { location.href = "/admin/site/select?site=" + enc(rs.value) + "&next=" + enc(rs.getAttribute("data-next") || "/admin"); });
  $$("select[data-goto]").forEach(function (s) { s.addEventListener("change", function () { location.href = s.value; }); });

  if (location.search.indexOf("add=1") >= 0) { var ar = $("#addrouter"); if (ar) setTimeout(function () { openDlg(ar); }, 50); }

  /* ---- dialogs ------------------------------------------------------------------ */
  function openDlg(d) {
    if (!d) return;
    if (!d.open) d.showModal();
    var f = d.querySelector("[autofocus],input:not([type=hidden]):not([type=radio]),select,textarea");
    if (f) setTimeout(function () { f.focus(); }, 30);
    d.dispatchEvent(new CustomEvent("ftn:open"));
  }
  doc.addEventListener("click", function (e) {
    var o = e.target.closest("[data-open]");
    if (o) {
      e.preventDefault();
      var dlg = $("#" + o.getAttribute("data-open"));
      var pre = o.getAttribute("data-mac"); // "Create customer" on a New devices row
      if (dlg && pre) {
        var f = $("form", dlg);
        $("input[name=mac]", f).value = pre;
        var rsel = $("[data-router-select]", f); if (rsel && o.getAttribute("data-site")) rsel.value = o.getAttribute("data-site");
        var fl = o.getAttribute("data-flat"); if (fl) $("input[name=flat]", f).value = fl;
        step(f, "form"); devsFor(f);
      }
      openDlg(dlg); return;
    }
    var c = e.target.closest("[data-close]");
    if (c) { var d = c.closest("dialog"); if (d) d.close(); return; }
    if (e.target.tagName === "DIALOG" && e.target.open) e.target.close();
  });

  // Pages that need a dialog open on load (a form error, or ?add=1) carry
  // <span hidden data-autoopen="dialog-id" data-mac data-flat>.
  $$("[data-autoopen]").forEach(function (n) {
    n.setAttribute("data-open", n.getAttribute("data-autoopen"));
    setTimeout(function () { n.click(); }, 60);
  });
  doc.addEventListener("click", function (e) { if (e.target.closest("[data-print]")) window.print(); });

  /* A native confirm for the simple "are you sure" buttons and forms. */
  doc.addEventListener("submit", function (e) {
    var m = e.target.getAttribute && e.target.getAttribute("data-confirm");
    if (m && !window.confirm(m)) e.preventDefault();
  }, true);

  /* A dialog that shows what a fix will do and asks for a yes. */
  function ask(title, text, yes) {
    return new Promise(function (resolve) {
      var d = el("dialog", "modal"), f = el("div", "mcard");
      var h = el("header"); h.appendChild(el("h2", "", title)); f.appendChild(h);
      f.appendChild(el("p", "", text));
      var ft = el("footer"), n = el("button", "btn ghost", "Cancel"), y = el("button", "btn", yes || "Confirm");
      n.type = y.type = "button"; ft.appendChild(n); ft.appendChild(y); f.appendChild(ft); d.appendChild(f); doc.body.appendChild(d);
      function done(v) { d.close(); d.remove(); resolve(v); }
      n.onclick = function () { done(false); }; y.onclick = function () { done(true); };
      d.addEventListener("cancel", function () { done(false); });
      d.showModal(); y.focus();
    });
  }

  /* ---- copy buttons ------------------------------------------------------------------ */
  doc.addEventListener("click", function (e) {
    var b = e.target.closest("[data-copy],[data-copy-from]"); if (!b) return;
    var text = b.getAttribute("data-copy");
    if (text == null) { var src = $(b.getAttribute("data-copy-from")); text = src ? (src.value != null ? src.value : src.textContent) : ""; }
    function ok() { var o = b.textContent; b.textContent = "Copied"; toast("Copied to the clipboard"); setTimeout(function () { b.textContent = o; }, 1600); }
    if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(text).then(ok, function () { fallback(); });
    else fallback();
    function fallback() {
      var t = el("textarea"); t.value = text; t.style.position = "fixed"; t.style.opacity = "0"; doc.body.appendChild(t); t.select();
      try { doc.execCommand("copy"); ok(); } catch (x) { toast("Select the text and copy it by hand", true); } t.remove();
    }
  });
  $$("textarea[readonly],input[readonly]").forEach(function (t) { t.addEventListener("focus", function () { t.select(); }); });

  /* ---- command palette ------------------------------------------------------------------ */
  var cmd = $("#cmdk"), cq = $("#cmdk-q"), cl = $("#cmdk-list"), cTimer = 0, cSeq = 0, cItems = [], cSel = 0;
  function navEntries() {
    return $$(".side a.nav").map(function (a) { return { title: a.firstChild && a.lastChild ? a.textContent.replace(/\d+$/, "").trim() : a.textContent.trim(), href: a.getAttribute("href") }; });
  }
  function cRender(groups) {
    cl.textContent = ""; cItems = [];
    var any = false;
    groups.forEach(function (g) {
      if (!g.items || !g.items.length) return;
      any = true;
      var h = el("li", "grp", g.name); h.setAttribute("role", "presentation"); cl.appendChild(h);
      g.items.forEach(function (it) {
        var li = el("li"); li.setAttribute("role", "presentation");
        var a = el("a"); a.href = it.href; a.setAttribute("role", "option"); a.appendChild(el("b", "", it.title));
        if (it.sub) a.appendChild(el("span", "", it.sub));
        li.appendChild(a); cl.appendChild(li); cItems.push(a);
      });
    });
    if (!any) cl.appendChild(el("li", "none", cq.value ? "Nothing found. Try a flat, phone number, MAC or receipt code." : "Type to search."));
    cSel = 0; cMark();
  }
  function cMark() { cItems.forEach(function (a, i) { a.setAttribute("aria-selected", i === cSel ? "true" : "false"); if (i === cSel) a.scrollIntoView({ block: "nearest" }); }); }
  function cRun() {
    var q = cq.value.trim(), seq = ++cSeq;
    var nav = navEntries().filter(function (n) { return !q || n.title.toLowerCase().indexOf(q.toLowerCase()) >= 0; });
    var groups = [{ name: "Go to", items: nav.slice(0, q ? 5 : 12) }];
    cRender(groups);
    if (q.length < 2) return;
    getJSON("/admin/search.json?q=" + enc(q)).then(function (j) {
      if (seq !== cSeq || !j.groups) return;
      cRender(j.groups.concat(groups));
    });
  }
  function cOpen() { if (!cmd) return; cq.value = ""; cRun(); openDlg(cmd); }
  if (cmd) {
    cq.addEventListener("input", function () { clearTimeout(cTimer); cTimer = setTimeout(cRun, 160); });
    cq.addEventListener("keydown", function (e) {
      if (e.key === "ArrowDown") { e.preventDefault(); cSel = Math.min(cItems.length - 1, cSel + 1); cMark(); }
      else if (e.key === "ArrowUp") { e.preventDefault(); cSel = Math.max(0, cSel - 1); cMark(); }
      else if (e.key === "Enter" && cItems[cSel]) { e.preventDefault(); location.href = cItems[cSel].href; }
    });
  }
  doc.addEventListener("click", function (e) { if (e.target.closest("[data-open-search]")) cOpen(); });
  doc.addEventListener("keydown", function (e) {
    var typing = /^(INPUT|TEXTAREA|SELECT)$/.test((e.target.tagName || "")) || e.target.isContentEditable;
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") { e.preventDefault(); cOpen(); }
    else if (e.key === "/" && !typing && !e.metaKey && !e.ctrlKey) { e.preventDefault(); cOpen(); }
  });
  var kb = $(".kbtn kbd"); if (kb && /Mac|iPhone|iPad/.test(navigator.platform)) kb.textContent = "⌘K";

  /* ---- Add customer ------------------------------------------------------------------------- */
  var macRe = /^[0-9A-F]{12}$/;
  function fmtMAC(v) {
    var h = v.toUpperCase().replace(/[^0-9A-F]/g, "");
    return macRe.test(h) ? h.match(/.{2}/g).join(":") : v.toUpperCase();
  }
  $$("input[name=mac]").forEach(function (i) {
    i.addEventListener("blur", function () { i.value = fmtMAC(i.value.trim()); });
    i.addEventListener("paste", function () { setTimeout(function () { i.value = fmtMAC(i.value.trim()); filterDevs(i.form); }, 0); });
    i.addEventListener("input", function () { filterDevs(i.form); });
  });
  function devsFor(form) {
    var sel = $("[data-router-select]", form), box = $("[data-devlist]", form), hint = $("[data-mac-hint]", form);
    if (!sel || !box) return;
    box.textContent = ""; form._devs = [];
    if (!sel.value) { if (hint) hint.textContent = "Choose a router to see the devices it can see."; return; }
    if (hint) hint.textContent = "Looking at what this router sees…";
    getJSON("/admin/routers/devices.json?router=" + enc(sel.value)).then(function (j) {
      form._devs = j.items || [];
      if (hint) hint.textContent = form._devs.length ? "Pick a device this router can see, or paste the MAC." : (j.error ? "Could not read the router. Paste the MAC instead." : "No new devices right now. Plug the ONT in, or paste its MAC.");
      filterDevs(form);
    }).catch(function () { if (hint) hint.textContent = "Could not read the router. Paste the MAC instead."; });
  }
  function filterDevs(form) {
    var box = $("[data-devlist]", form); if (!box) return;
    var q = ($("input[name=mac]", form).value || "").toUpperCase().replace(/[^0-9A-Z]/g, "");
    box.textContent = "";
    (form._devs || []).filter(function (d) { return !q || d.mac.replace(/:/g, "").indexOf(q) >= 0 || (d.host_name || "").toUpperCase().indexOf(q) >= 0; }).slice(0, 12).forEach(function (d) {
      var b = el("button"); b.type = "button";
      b.appendChild(el("b", "", d.mac));
      b.appendChild(el("span", "", [d.vendor || "unknown vendor", d.host_name, d.ip, d.online ? "online" : "seen " + (d.seen || "")].filter(Boolean).join(" · ")));
      b.onclick = function () { $("input[name=mac]", form).value = d.mac; box.textContent = ""; };
      box.appendChild(b);
    });
  }
  $$("[data-router-select]").forEach(function (s) { s.addEventListener("change", function () { devsFor(s.form); }); });
  var ac = $("#addcustomer");
  if (ac) ac.addEventListener("ftn:open", function () { var f = $("form", ac); if (f && !f._devs) devsFor(f); });

  function step(form, name) { $$("[data-step]", form).forEach(function (n) { n.hidden = n.getAttribute("data-step") !== name; }); }
  function provRow(box, id, text, state) {
    var r = $("#" + id, box);
    if (!r) { r = el("div", "row2"); r.id = id; r.appendChild(el("span")); r.appendChild(el("span", "t")); r.appendChild(el("span", "pill")); box.appendChild(r); }
    var ic = r.firstChild; ic.className = state === "work" ? "spin" : ""; ic.textContent = state === "ok" ? "✓" : state === "wait" ? "•" : "";
    $(".t", r).textContent = text;
    var p = $(".pill", r); p.className = "pill " + (state === "ok" ? "active" : state === "wait" ? "queued" : state === "bad" ? "failed" : "paused");
    p.textContent = state === "ok" ? "done" : state === "wait" ? "queued" : state === "bad" ? "failed" : "working";
  }
  function pollAccount(form, id) {
    var box = $(".prov", form), tries = 0;
    function tick() {
      getJSON("/admin/accounts/status.json?id=" + enc(id)).then(function (j) {
        tries++;
        if (j.text) provRow(box, "pv-router", j.text, j.state);
        if (j.done || tries > 40) {
          if (!j.done && j.state !== "bad") provRow(box, "pv-router", "Queued: applies automatically when the router answers", "wait");
          return;
        }
        setTimeout(tick, tries < 6 ? 1000 : 2500);
      }).catch(function () { setTimeout(tick, 3000); });
    }
    tick();
  }
  function ajaxForm(form, kind) {
    form.addEventListener("submit", function (e) {
      e.preventDefault();
      var old = $(".ajax-err", form); if (old) old.remove();
      var btn = $("button[type=submit]", form); if (btn) btn.disabled = true;
      var body = new URLSearchParams(new FormData(form));
      fetch(form.action, { method: "POST", credentials: "same-origin", body: body, headers: { Accept: "application/json", "Content-Type": "application/x-www-form-urlencoded" } })
        .then(function (r) { return r.json().catch(function () { return { error: "The server sent something unexpected." }; }); })
        .then(function (j) {
          if (btn) btn.disabled = false;
          if (!j.ok) {
            var er = el("div", "flash bad ajax-err", j.error || "That did not work."); er.setAttribute("role", "alert");
            var first = $("[data-step=form]", form); first.insertBefore(er, first.firstChild); return;
          }
          if (kind === "customer") {
            step(form, "status");
            var box = $(".prov", form); box.textContent = "";
            provRow(box, "pv-acct", "Account " + (j.flat || "") + " created (" + (j.code || "") + ")", "ok");
            var rt = j.router || {};
            provRow(box, "pv-router", rt.text || "Sending it to the router…", rt.state || "work");
            if (j.warning) provRow(box, "pv-warn", j.warning, "bad");
            var v = $("[data-view-account]", form); if (v && j.url) v.href = j.url;
            if (!rt.done) pollAccount(form, j.id);
          } else {
            step(form, "status");
            $("[data-join-cmd]", form).value = j.command || ""; $("[data-join-cmd2]", form).value = j.command_nocheck || "";
            var vr = $("[data-view-router]", form); if (vr && j.url) vr.href = j.url;
            if (!j.command) { $("[data-join-text]", form).textContent = j.note || "Router created. Open it to make the join command."; $("[data-join-spin]", form).hidden = true; }
            else pollJoin(form, j.id);
          }
        }).catch(function () { if (btn) btn.disabled = false; toast("Could not reach the server", true); });
    });
  }
  $$("form[data-ajax=customer]").forEach(function (f) {
    ajaxForm(f, "customer");
    var again = $("[data-again]", f);
    if (again) again.addEventListener("click", function () { f.reset(); step(f, "form"); f._devs = null; devsFor(f); });
  });
  $$("form[data-ajax=router]").forEach(function (f) { ajaxForm(f, "router"); });

  /* ---- join progress (Add router, router page) ------------------------------------------------ */
  function joinText(r) {
    if (r.rest_ok && (r.joined || r.token === "used")) return ["Connected. The server is setting the router up.", true];
    if (r.handshake) return ["Tunnel is up. Waiting for the router's API…", false];
    if (r.token === "used") return ["The router ran the line. Waiting for the tunnel…", false];
    if (r.refused_ua) return ["The line was fetched by something that is not a RouterOS router. Paste it in the router's terminal.", false];
    if (r.token === "expired") return ["The line expired. Make a new one.", false];
    return ["Waiting for the router to run the line", false];
  }
  function pollJoin(scope, id) {
    var txt = $("[data-join-text]", scope), spin = $("[data-join-spin]", scope), n = 0;
    if (!txt) return;
    (function tick() {
      getJSON("/admin/routers/status.json?id=" + enc(id)).then(function (r) {
        var t = joinText(r); txt.textContent = t[0];
        if (t[1]) { if (spin) { spin.className = ""; spin.textContent = "✓"; } return; }
        if (++n < 400) setTimeout(tick, 3000);
      }).catch(function () { setTimeout(tick, 5000); });
    })();
  }
  $$("[data-join-poll]").forEach(function (b) { pollJoin(b, b.getAttribute("data-join-poll")); });

  /* ---- Live now ------------------------------------------------------------------------------------- */
  function bps(v) {
    if (!v || v < 1) return "0";
    if (v >= 1e9) return (v / 1e9).toFixed(1) + " Gbps";
    if (v >= 1e6) return (v / 1e6).toFixed(v >= 1e7 ? 0 : 1) + " Mbps";
    return Math.round(v / 1e3) + " kbps";
  }
  var live = $("[data-live]");
  if (live) {
    var tab = "all", last = null;
    var tbody = $("tbody", live);
    var draw = function () {
      if (!last) return;
      var rows = (last.rows || []).filter(function (r) { return tab === "all" || (tab === "flats" && r.kind === "flat") || (tab === "unregistered" && r.kind !== "flat"); });
      tbody.textContent = "";
      rows.forEach(function (r) {
        var tr = el("tr");
        function td(label, content, cls) { var c = el("td", cls || ""); c.setAttribute("data-label", label); if (content instanceof Node) c.appendChild(content); else c.textContent = content; tr.appendChild(c); return c; }
        var who = el("td", "who"), a = el("a"); a.href = r.href || "#"; a.appendChild(el("b", "", r.flat || r.mac)); who.appendChild(a); who.appendChild(el("span", "", r.tenant || (r.kind === "flat" ? "" : "not registered"))); tr.appendChild(who);
        td("IP", r.ip || "", "mono"); td("MAC", r.mac || "", "mono");
        td("Down", bps(r.down_bps), "n"); td("Up", bps(r.up_bps), "n"); td("Plan", r.cap_bps ? bps(r.cap_bps) : "", "n");
        td("Router", r.router || "");
        tbody.appendChild(tr);
      });
      $("[data-live-empty]", live).hidden = rows.length > 0;
      $(".tw", live).hidden = rows.length === 0;
      var set = function (k, v) { var n = $("[data-stat=" + k + "]", live); if (n) n.textContent = v; };
      set("online", String(last.online || 0)); set("down", bps(last.down_bps)); set("up", bps(last.up_bps)); set("busiest", last.busiest || "None");
      var cnts = { all: (last.rows || []).length, flats: last.flats || 0, unregistered: last.unregistered || 0 };
      $$("[data-livetab]", live).forEach(function (b) { $(".n", b).textContent = cnts[b.getAttribute("data-livetab")]; });
      var st = $("[data-live-at]", live); if (st) st.textContent = "Updated " + new Date().toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
    };
    var load = function () { getJSON(live.getAttribute("data-live")).then(function (j) { if (j.rows) { last = j; draw(); } }).catch(function () {}); };
    $$("[data-livetab]", live).forEach(function (b) {
      b.addEventListener("click", function () { tab = b.getAttribute("data-livetab"); $$("[data-livetab]", live).forEach(function (x) { x.setAttribute("aria-selected", x === b ? "true" : "false"); }); draw(); });
    });
    load(); setInterval(function () { if (!doc.hidden) load(); }, 10000);
  }

  /* ---- Router setup tab: checklist, fixes, job stepper --------------------------------------------- */
  var setup = $("[data-setup]");
  if (setup) {
    var rid = setup.getAttribute("data-setup"), list = $("[data-checks]", setup), sum = $("[data-checks-sum]", setup);
    var ORDER = { fail: 0, warn: 1, unknown: 2, ok: 3, info: 4 }, MARK = { ok: "✓", warn: "!", fail: "×", unknown: "?", info: "i" };
    var renderChecks = function (j) {
      list.textContent = "";
      if (!j.items) { list.appendChild(el("li", "ph", j.error ? "The router could not be read: " + j.error : "Could not load the checklist.")); sum.textContent = ""; return; }
      var s = j.summary || {};
      sum.textContent = (s.fail ? s.fail + " to fix, " : "") + (s.warn ? s.warn + " to look at, " : "") + (s.ok || 0) + " fine" + (j.router && j.router.reachable === false ? ". The router is not reachable right now." : "");
      j.items.slice().sort(function (a, b) { return (ORDER[a.status] - ORDER[b.status]) || 0; }).forEach(function (it) {
        var li = el("li", "check s-" + it.status);
        li.appendChild(el("span", "st", MARK[it.status] || "?")); li.lastChild.setAttribute("aria-label", it.status);
        var body = el("div"); body.appendChild(el("h4", "", it.title));
        if (it.detail) body.appendChild(el("p", "", it.detail));
        if (it.evidence && it.evidence.length) {
          var dt = el("details"); dt.appendChild(el("summary", "muted", "What the router said"));
          var ul = el("ul", "ev"); it.evidence.slice(0, 12).forEach(function (x) { ul.appendChild(el("li", "", x)); }); dt.appendChild(ul); body.appendChild(dt);
        }
        var fx = it.fix || {};
        if (fx.kind === "manual" && fx.command) {
          var c = el("code", "cmd", fx.command); body.appendChild(c);
          var cp = el("button", "btn ghost sm", "Copy command"); cp.type = "button"; cp.setAttribute("data-copy", fx.command); cp.style.marginTop = "6px"; body.appendChild(cp);
        }
        li.appendChild(body);
        if ((fx.kind === "auto" || fx.kind === "confirm") && it.status !== "ok" && it.status !== "info") {
          var wrap = el("div", "fix"), b = el("button", "btn sm", fx.label || "Fix"); b.type = "button";
          b.onclick = function () {
            var go = fx.kind === "confirm" ? ask(fx.label || "Confirm", fx.confirm_text || "Go ahead with this change?", fx.label || "Confirm") : Promise.resolve(true);
            go.then(function (yes) {
              if (!yes) return; b.disabled = true; b.textContent = "Working…";
              postForm("/admin/routers/fix", { id: rid, check: it.id, confirm: fx.kind === "confirm" ? "1" : "" }).then(function (r) {
                if (r.ok) toast((r.applied && r.applied.length ? r.applied.join(". ") : "Fixed") + "."); else toast(r.error || "That fix did not work.", true);
                loadChecks(true);
              });
            });
          };
          wrap.appendChild(b); li.appendChild(wrap);
        }
        list.appendChild(li);
      });
    };
    var loadChecks = function (fresh) {
      sum.textContent = "Reading the router…";
      getJSON("/admin/routers/checks.json?id=" + enc(rid) + (fresh ? "&fresh=1" : "")).then(renderChecks).catch(function () { renderChecks({}); });
    };
    var rc = $("[data-recheck]", setup); if (rc) rc.addEventListener("click", function () { loadChecks(true); });
    loadChecks(false);

    // The provisioning job (W5c): /admin/routers/setup.json draws the stepper, the next action
    // and the two buttons the job allows (Start migration with the router name typed, Roll back
    // now). There is no confirm button: the server keeps or rolls back a migration on evidence.
    var jobBox = $("[data-job]", setup), rname = setup.getAttribute("data-name") || "", isOwner = setup.hasAttribute("data-owner");
    var STATE_TEXT = { fixing: "Fixing\u2026", checking: "Checking the router\u2026", staging: "Getting ready\u2026", building: "Building the network\u2026",
      arming: "Arming the safety net\u2026", switching: "Switching customers over\u2026", verifying: "Checking that every flat is back online\u2026",
      rolling_back: "Rolling back\u2026", live: "Live", rolled_back: "Rolled back to how it was", waiting_router: "Waiting for the router to join" };
    var ROLLBACK_STATES = { arming: 1, switching: 1, verifying: 1 };
    var MARKS = { ok: "\u2713", failed: "\u00d7", skipped: "\u2013", running: "", pending: "" };
    var postJob = function (path, data, okMsg) {
      return postForm("/admin/routers/setup/" + path, data).then(function (r) {
        if (r.__status >= 200 && r.__status < 300) { if (okMsg) toast(okMsg); } else toast(r.error || "That did not work.", true);
        loadJob(true);
      });
    };
    var loadJob = function (force) {
      if (!force && jobBox.contains(doc.activeElement) && doc.activeElement.tagName === "INPUT") return; // do not wipe what is being typed
      getJSON("/admin/routers/setup.json?id=" + enc(rid)).then(function (j) {
        if (j.__status !== 200 || (!j.state && !j.steps)) return;
        var typed = $("[data-migrate-name]", jobBox), keep = $("[data-keep-unknown]", jobBox);
        var typedV = typed ? typed.value : "", keepV = keep ? keep.checked : false;
        jobBox.textContent = "";
        var na = j.next_action || {}, text = na.text || STATE_TEXT[j.state] || "";
        if (j.state === "live") text = "Live";
        if (text) {
          var tone = j.state === "live" ? "ok" : (j.state === "failed" || j.state === "rolled_back" || na.kind === "fix") ? "bad" : "";
          var nx = el("div", "next-action" + (tone ? " " + tone : "")); nx.setAttribute("role", "status"); nx.setAttribute("data-next-kind", na.kind || "none");
          nx.appendChild(el("b", "", text));
          if (j.last_error && j.last_error !== text) nx.appendChild(el("span", "", j.last_error));
          jobBox.appendChild(nx);
        }
        var meta = [];
        if (j.state) meta.push("State: " + String(j.state).replace(/_/g, " "));
        if (j.seconds_left > 0) meta.push("rolls back by itself in " + (j.seconds_left >= 90 ? Math.ceil(j.seconds_left / 60) + " min" : j.seconds_left + " s") + " if a flat does not come back");
        var ev = j.evidence || {};
        if (ev.flats_required > 0) meta.push(ev.flats_bound + " of " + ev.flats_required + " flats back on the new network");
        if (meta.length) jobBox.appendChild(el("p", "muted", meta.join(". ") + "."));
        var steps = j.steps || [];
        if (steps.length) {
          var ol = el("ol", "steps");
          steps.forEach(function (st) {
            var cls = st.status === "ok" ? "done" : st.status === "running" ? "now" : st.status === "failed" ? "fail" : "";
            var li = el("li", cls); li.setAttribute("data-step-key", st.key || "");
            li.appendChild(el("span", "dot", st.status === "pending" || st.status === "running" ? String(st.n) : (MARKS[st.status] || String(st.n))));
            var d = el("div"); d.appendChild(el("b", "", st.title || st.key || "")); if (st.detail) d.appendChild(el("span", "", st.detail)); li.appendChild(d); ol.appendChild(li);
          });
          jobBox.appendChild(ol);
        }
        var act = el("div", "row"); act.style.marginTop = "12px";
        if (!isOwner) { if (j.state === "ready" || na.kind === "migrate" || na.kind === "retry") jobBox.appendChild(el("p", "muted", "Only an owner can start or roll back a set-up.")); return; }
        if (na.kind === "retry" && j.join && j.join.state !== "waiting" && j.state !== "waiting_router") {
          var rn = el("button", "btn", "Run set-up"); rn.type = "button"; rn.onclick = function () { rn.disabled = true; postJob("run", { id: rid }); }; act.appendChild(rn);
        }
        if (na.kind === "migrate" || j.state === "ready") {
          var box = el("div", "migrate");
          box.appendChild(el("p", "", "Type the router name to start the migration. The server keeps it if every flat comes back, and rolls it back if not."));
          var inp = el("input"); inp.setAttribute("data-migrate-name", ""); inp.placeholder = rname; inp.autocomplete = "off"; inp.setAttribute("aria-label", "Router name"); inp.value = typedV;
          var m = el("button", "btn", "Start migration"); m.type = "button"; m.disabled = inp.value.trim() !== rname;
          inp.addEventListener("input", function () { m.disabled = inp.value.trim() !== rname; });
          m.onclick = function () { m.disabled = true; postJob("migrate", { id: rid, confirm_name: inp.value.trim(), keep_unknown: kp.checked ? "1" : "" }, "Migration started."); };
          var lb = el("label", "ck"), kp = el("input"); kp.type = "checkbox"; kp.setAttribute("data-keep-unknown", ""); kp.checked = keepV; lb.appendChild(kp); lb.appendChild(doc.createTextNode(" Keep devices the server does not know"));
          var rowm = el("div", "row"); rowm.appendChild(inp); rowm.appendChild(m); box.appendChild(rowm); if (j.live_devices > 0) box.appendChild(lb);
          jobBox.appendChild(box);
        }
        if (ROLLBACK_STATES[j.state]) {
          var rb = el("button", "btn danger", "Roll back now"); rb.type = "button";
          rb.onclick = function () { ask("Roll back now?", "The router goes back to how it was before the switch within about 30 seconds.", "Roll back").then(function (y) { if (y) postJob("rollback", { id: rid }, "Rolling back."); }); };
          act.appendChild(rb);
        }
        if (act.childNodes.length) jobBox.appendChild(act);
      }).catch(function () {});
    };
    if (jobBox) { loadJob(true); setInterval(function () { if (!doc.hidden) loadJob(false); }, 4000); }
  }

  /* ---- router page: live tabs refresh in place ------------------------------------------------------ */
  // <div id="rt-live" data-refresh="10"> is fetched again from the same address and swapped in, unless
  // somebody is typing in it. Typing in the clients search reloads the list from the server (debounced).
  var rtLive = $("#rt-live[data-refresh]");
  if (rtLive) {
    var rtBusy = false;
    var rtSwap = function (url) {
      if (rtBusy) return; rtBusy = true;
      fetch(url, { credentials: "same-origin", headers: { Accept: "text/html" } }).then(function (r) { return r.text(); }).then(function (t) {
        var nd = new DOMParser().parseFromString(t, "text/html").getElementById("rt-live"), cur = $("#rt-live");
        if (nd && cur) {
          var q = $("[data-clients-q]", cur);
          if (q) { // keep the search form (and what is typed in it); swap its counts and everything below
            var cf = q.form, nf = $("form", nd), cs = $(".muted", cf), ns = nf && $(".muted", nf);
            if (cs && ns) cs.textContent = ns.textContent;
            while (cf.nextSibling) cf.parentNode.removeChild(cf.nextSibling);
            var after = nf ? nf.nextSibling : null;
            while (after) { var nx = after.nextSibling; cf.parentNode.appendChild(doc.importNode(after, true)); after = nx; }
          } else cur.innerHTML = nd.innerHTML;
        }
      }).catch(function () {}).then(function () { rtBusy = false; });
    };
    var rtUrl = function () { var u = new URL(location.href), q = $("[data-clients-q]"); if (q) u.searchParams.set("q", q.value.trim()); return u.pathname + u.search; };
    setInterval(function () { if (!doc.hidden && !doc.querySelector("dialog[open]")) rtSwap(rtUrl()); }, Number(rtLive.getAttribute("data-refresh")) * 1000);
    var cq = $("[data-clients-q]"), ct = null;
    if (cq) {
      var cform = cq.form;
      cform.addEventListener("submit", function (e) { e.preventDefault(); rtSwap(rtUrl()); });
      cq.addEventListener("input", function () { clearTimeout(ct); ct = setTimeout(function () { rtBusy = false; rtSwap(rtUrl()); }, 300); });
    }
  }

  // M-Pesa settings: poll the KES 1 test prompt until it is no longer pending.
  var stk = $("[data-stk]");
  if (stk) {
    var stkN = 0, pollStk = function () {
      getJSON("/admin/settings/mpesa/stk-test?id=" + enc(stk.getAttribute("data-stk"))).then(function (j) {
        $("[data-stk-status]", stk).textContent = j.status || "";
        $("[data-stk-detail]", stk).textContent = j.detail || (j.receipt ? "receipt " + j.receipt : "");
        if (j.status === "pending" && ++stkN < 15) setTimeout(pollStk, 8000);
      }).catch(function () {});
    };
    pollStk();
  }

  /* ---- small page helpers ----------------------------------------------------------------------------- */
  // Record payment: choosing the amount picks the plan with that price.
  var amt = $("[data-autopkg]");
  if (amt) {
    var pkgSel = $("#" + amt.getAttribute("data-autopkg"));
    amt.addEventListener("input", function () {
      for (var i = 0; i < pkgSel.options.length; i++) if (pkgSel.options[i].getAttribute("data-price") === amt.value) { pkgSel.selectedIndex = i; return; }
      pkgSel.selectedIndex = 0;
    });
  }
  // Table filter box: hides rows that do not contain the text.
  $$("input[data-filter]").forEach(function (inp) {
    var tb = $(inp.getAttribute("data-filter"));
    inp.addEventListener("input", function () {
      var q = inp.value.toLowerCase();
      $$("tbody tr", tb).forEach(function (tr) { tr.hidden = q && tr.textContent.toLowerCase().indexOf(q) < 0; });
    });
  });
  // Auto refresh a section: <div data-reload="30"> reloads the page every N seconds when visible.
  var rl = $("[data-reload]");
  if (rl) setInterval(function () { if (!doc.hidden && !doc.querySelector("dialog[open]")) location.reload(); }, Number(rl.getAttribute("data-reload")) * 1000);
})();
