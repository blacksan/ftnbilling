/* Apartment picker pop-up for <select data-apt-select>. External file: the admin CSP allows no inline script.
   The select stays in the form as the source of truth (name, value, the router filtering in app.js all keep
   working); this script hides it, shows a button with the chosen apartment, and opens a <dialog> that lists
   every apartment the select offers. Without JS the plain select is what people use. */
(function () {
  "use strict";
  if (typeof HTMLDialogElement === "undefined") return;
  var doc = document;
  var SEARCH_FROM = 8; // show the search box when there are more apartments than this
  var seq = 0;

  function el(tag, cls, text) { var e = doc.createElement(tag); if (cls) e.className = cls; if (text != null) e.textContent = text; return e; }

  function offered(sel) {
    return Array.prototype.filter.call(sel.options, function (o) { return o.value && !o.hidden && !o.disabled; });
  }
  function plural(n, one, many) { return n + " " + (n === 1 ? one : many); }
  function detail(o) {
    var bits = [];
    var subs = Number(o.getAttribute("data-subs") || 0);
    bits.push(plural(subs, "subscriber", "subscribers"));
    var routers = o.getAttribute("data-router-names");
    bits.push(routers ? "Router: " + routers : "No router yet");
    return bits.join(" · ");
  }

  function enhance(sel) {
    if (sel._aptPicker) return;
    sel._aptPicker = true;
    var id = "aptpick-" + (++seq);
    var wrap = sel.closest("label") || sel.parentNode;
    wrap.classList.add("aptpick");

    var btn = el("button", "aptpick-btn");
    btn.type = "button";
    btn.setAttribute("aria-haspopup", "dialog");
    var name = el("span", "aptpick-name"), sub = el("span", "aptpick-sub");
    btn.appendChild(name); btn.appendChild(sub);
    sel.parentNode.insertBefore(btn, sel);
    // Keep the select in the form but out of sight and out of the tab order.
    sel.classList.add("aptpick-native");
    sel.tabIndex = -1;
    sel.setAttribute("aria-hidden", "true");
    // Required but empty: the browser would focus the invisible select. Show the problem on the picker instead.
    var err = el("span", "aptpick-err", "Choose an apartment."); err.id = id + "-err"; err.hidden = true; err.setAttribute("role", "alert");
    sel.parentNode.insertBefore(err, sel.nextSibling);
    function clearErr() { err.hidden = true; btn.removeAttribute("aria-invalid"); btn.removeAttribute("aria-describedby"); }
    sel.addEventListener("invalid", function (e) {
      e.preventDefault();
      err.hidden = false;
      btn.setAttribute("aria-invalid", "true");
      btn.setAttribute("aria-describedby", err.id);
      btn.focus();
    });
    sel.addEventListener("change", clearErr);

    function placeholder() {
      var first = sel.options[0];
      return first && !first.value ? first.textContent : "Choose an apartment";
    }
    function sync() {
      var o = sel.options[sel.selectedIndex];
      if (o && o.value) {
        name.textContent = o.textContent;
        sub.textContent = o.getAttribute("data-address") || "";
        btn.classList.remove("empty");
      } else {
        name.textContent = placeholder();
        sub.textContent = "";
        btn.classList.add("empty");
      }
      sub.hidden = !sub.textContent;
    }
    sync();
    sel.addEventListener("change", sync);
    sel.addEventListener("ftn:aptsync", sync);
    if (sel.form) sel.form.addEventListener("reset", function () { setTimeout(sync, 0); });

    // The dialog is built when first opened and rebuilt each time, because other code narrows the options.
    var dlg = null, q = null, list = null, empty = null;
    function build() {
      dlg = el("dialog", "modal aptpick-dlg");
      dlg.id = id;
      dlg.setAttribute("aria-labelledby", id + "-t");
      var card = el("div", "mcard");
      var head = el("header");
      var h = el("h2", null, "Choose an apartment"); h.id = id + "-t";
      var x = el("button", "ibtn"); x.type = "button"; x.setAttribute("data-close", ""); x.setAttribute("aria-label", "Close");
      x.innerHTML = '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>';
      head.appendChild(h); head.appendChild(x);
      card.appendChild(head);
      q = el("input", "aptpick-q"); q.type = "search"; q.autocomplete = "off";
      q.placeholder = "Search by name, address or router"; q.setAttribute("aria-label", "Search apartments");
      q.setAttribute("aria-controls", id + "-list");
      card.appendChild(q);
      list = el("ul", "aptpick-list"); list.id = id + "-list";
      card.appendChild(list);
      empty = el("p", "muted aptpick-empty", "No apartment matches."); empty.hidden = true; empty.setAttribute("role", "status");
      card.appendChild(empty);
      dlg.appendChild(card);
      doc.body.appendChild(dlg);
      q.addEventListener("input", filter);
      // Enter in the search box picks the only visible row.
      q.addEventListener("keydown", function (e) {
        if (e.key === "Enter") {
          e.preventDefault();
          var rows = visibleRows();
          if (rows.length === 1) rows[0].click();
        }
      });
      dlg.addEventListener("close", function () { btn.focus(); });
    }
    function visibleRows() {
      return Array.prototype.filter.call(list.querySelectorAll(".aptrow"), function (b) { return !b.parentNode.hidden; });
    }
    function filter() {
      var t = q.value.toLowerCase().trim();
      Array.prototype.forEach.call(list.children, function (li) {
        li.hidden = !!t && li.getAttribute("data-text").indexOf(t) < 0;
      });
      empty.hidden = visibleRows().length > 0;
    }
    function fill() {
      list.textContent = "";
      var opts = offered(sel);
      opts.forEach(function (o) {
        var li = el("li");
        var b = el("button", "aptrow"); b.type = "button";
        var selected = o.value === sel.value;
        if (selected) { b.classList.add("on"); b.setAttribute("aria-current", "true"); }
        var addr = o.getAttribute("data-address") || "";
        b.appendChild(el("b", null, o.textContent));
        if (addr) b.appendChild(el("span", "aptrow-addr", addr));
        b.appendChild(el("span", "aptrow-meta", detail(o)));
        li.setAttribute("data-text", (o.textContent + " " + addr + " " + (o.getAttribute("data-router-names") || "")).toLowerCase());
        b.addEventListener("click", function () {
          sel.value = o.value;
          sel.dispatchEvent(new Event("change", { bubbles: true }));
          dlg.close();
        });
        li.appendChild(b);
        list.appendChild(li);
      });
      q.hidden = opts.length <= SEARCH_FROM;
      q.value = "";
      empty.textContent = opts.length ? "No apartment matches." : "No apartments to choose from.";
      empty.hidden = opts.length > 0;
    }
    btn.addEventListener("click", function () {
      if (!dlg) build();
      fill();
      if (!dlg.open) dlg.showModal();
      var target = !q.hidden ? q : (list.querySelector(".aptrow.on") || list.querySelector(".aptrow") || dlg.querySelector("[data-close]"));
      target.focus();
      var on = list.querySelector(".aptrow.on");
      if (on && on.scrollIntoView) on.scrollIntoView({ block: "nearest" });
    });
  }

  Array.prototype.forEach.call(doc.querySelectorAll("select[data-apt-select]"), enhance);
})();
