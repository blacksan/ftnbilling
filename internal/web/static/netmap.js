/* The Network page's Google map. External file: the admin CSP allows no inline script, and on this one page it also
   allows https://maps.googleapis.com. The loader below creates the Maps <script> itself (key + callback), so nothing
   is inline. Every name, address and serial comes from user-entered data: it is only ever put on the page with
   textContent and setAttribute on links we vet (safeHref), never as HTML. */
(function () {
  "use strict";
  var wrap = document.getElementById("netwrap");
  if (!wrap) return;

  var mapEl = document.getElementById("netmap");
  var loading = document.getElementById("netloading");
  var panel = document.getElementById("netpanel");
  var panelTitle = document.getElementById("netpanel-title");
  var panelBody = document.getElementById("netpanel-body");
  var panelClose = document.getElementById("netpanel-close");
  var hoverCard = document.getElementById("nethover");
  var key = wrap.getAttribute("data-key") || "";
  var jsonURL = wrap.getAttribute("data-json") || "/admin/network/map.json";
  var canHover = !!(window.matchMedia && window.matchMedia("(hover: hover) and (pointer: fine)").matches);
  var STATE_LABEL = { ok: "All good", partial: "Some offline", down: "Router down", unknown: "No router" };
  var RANK = { down: 3, partial: 2, ok: 1, unknown: 0 };
  var CLUSTER_PX = 44;
  var NO_CLUSTER_ZOOM = 18;

  // ---- small DOM helpers (text only) -------------------------------------------------------------
  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = String(text);
    return n;
  }
  // Only our own pages and Google Maps links become hrefs.
  function safeHref(u) {
    if (typeof u !== "string") return "#";
    if (u.charAt(0) === "/" && u.charAt(1) !== "/" && u.charAt(1) !== "\\") return u;
    if (u.indexOf("https://www.google.com/maps/") === 0) return u;
    return "#";
  }
  function link(text, href, cls, external) {
    var a = el("a", cls, text);
    a.setAttribute("href", safeHref(href));
    if (external) { a.setAttribute("target", "_blank"); a.setAttribute("rel", "noopener"); }
    return a;
  }
  function pill(state) { return el("span", "pill net-" + state, STATE_LABEL[state] || state); }
  function plural(n, one, many) { return n + " " + (n === 1 ? one : many); }

  // ---- the numbers on a summary (same wording as the list view and the Android app) ---------------
  function summaryRows(a) {
    var routers = a.routers.length ? a.routers.map(function (r) { return r.name + " (" + r.state + ")"; }).join(", ") : "none";
    var olts = a.olts.length ? a.olts.map(function (o) { return o.name + " " + o.onus_online + "/" + o.onus_total; }).join(", ") : "none";
    return [
      ["Address", a.address || "No address"],
      ["Houses", a.houses.active + " active of " + a.houses.total],
      ["Online now", String(a.houses.online)],
      ["Routers", routers],
      ["OLTs (ONUs online)", olts],
      ["ONUs online", a.onus_online + " of " + a.onus_total],
      ["Open alerts", String(a.open_alerts)]
    ];
  }

  function showFallback(why) {
    if (loading) loading.remove();
    mapEl.textContent = "";
    var box = el("div", "netfail");
    box.appendChild(el("p", null, why));
    var p = el("p");
    p.appendChild(link("Show the list instead", "/admin/network?view=list", "btn sm"));
    box.appendChild(p);
    mapEl.appendChild(box);
  }

  // ---- side panel / bottom sheet ------------------------------------------------------------------
  var openFrom = null;
  function section(title) {
    var s = el("section", "netsec");
    s.appendChild(el("h3", null, title));
    return s;
  }
  function list(items) {
    var ul = el("ul", "netlist");
    items.forEach(function (li) { ul.appendChild(li); });
    return ul;
  }
  function renderPanel(a) {
    panelTitle.textContent = a.name;
    panelBody.textContent = "";
    var top = el("div", "netpanel-state");
    top.appendChild(pill(a.state));
    if (a.address) top.appendChild(el("span", "muted", a.address));
    panelBody.appendChild(top);

    var dl = el("dl", "netsum");
    summaryRows(a).slice(1, 3).concat([["Open alerts", String(a.open_alerts)]]).forEach(function (r) {
      var d = el("div");
      d.appendChild(el("dt", null, r[0]));
      d.appendChild(el("dd", null, r[1]));
      dl.appendChild(d);
    });
    panelBody.appendChild(dl);

    var s = section("Routers serving it");
    if (a.routers.length) {
      s.appendChild(list(a.routers.map(function (r) {
        var li = el("li");
        li.appendChild(link(r.name, r.url));
        li.appendChild(document.createTextNode(" "));
        li.appendChild(el("span", "pill " + r.state, r.state));
        return li;
      })));
    } else s.appendChild(el("p", "muted", "No router serves this apartment yet."));
    panelBody.appendChild(s);

    s = section("OLT and PON ports");
    if (a.olts.length) {
      s.appendChild(list(a.olts.map(function (o) {
        var li = el("li");
        li.appendChild(link(o.name, o.url));
        var pons = o.pons && o.pons.length ? ", PON " + o.pons.join(", ") : "";
        li.appendChild(el("span", "muted", pons + " · ONUs " + o.onus_online + " of " + o.onus_total + " online"));
        return li;
      })));
    } else s.appendChild(el("p", "muted", "No OLT serves this apartment yet."));
    panelBody.appendChild(s);

    s = section("ONUs (" + a.onus_online + " of " + a.onus_total + " online)");
    if (a.onus.length) {
      s.appendChild(list(a.onus.map(function (n) {
        var li = el("li");
        li.appendChild(link("ONU " + (n.serial || n.id), n.url));
        var bits = [];
        if (n.house) bits.push("house " + n.house);
        if (n.customer) bits.push(n.customer);
        if (n.olt) bits.push(n.olt + (n.pon ? " PON " + n.pon : ""));
        li.appendChild(el("span", "muted", bits.length ? " · " + bits.join(" · ") + " " : " "));
        li.appendChild(el("span", "pill " + n.status, n.status));
        if (n.account_url) { li.appendChild(document.createTextNode(" ")); li.appendChild(link("customer", n.account_url)); }
        return li;
      })));
    } else s.appendChild(el("p", "muted", "No ONU is installed in a house here yet."));
    panelBody.appendChild(s);

    if (a.other && a.other.length) {
      s = section("Other equipment");
      s.appendChild(list(a.other.map(function (o) {
        var li = el("li");
        li.appendChild(link(o.label, o.url));
        li.appendChild(el("span", "muted", " · " + o.kind + (o.house ? " · house " + o.house : "") + " · " + o.status));
        return li;
      })));
      panelBody.appendChild(s);
    }

    var acts = el("p", "netacts");
    acts.appendChild(link("Open apartment", a.url, "btn sm"));
    if (a.maps_url) {
      acts.appendChild(document.createTextNode(" "));
      acts.appendChild(link("View on Google Maps", a.maps_url, "btn ghost sm", true));
    }
    panelBody.appendChild(acts);
  }
  function openPanel(a, from) {
    hideHover();
    renderPanel(a);
    openFrom = from || null;
    panel.hidden = false;
    wrap.classList.add("has-panel");
    panelClose.focus({ preventScroll: true });
  }
  function closePanel() {
    if (panel.hidden) return;
    panel.hidden = true;
    wrap.classList.remove("has-panel");
    if (openFrom && openFrom.focus) openFrom.focus({ preventScroll: true });
    openFrom = null;
  }
  panelClose.addEventListener("click", closePanel);
  document.addEventListener("keydown", function (e) { if (e.key === "Escape") closePanel(); });

  // ---- hover card (desktop) -------------------------------------------------------------------------
  function hideHover() { hoverCard.hidden = true; }
  function showHover(a, node) {
    if (!canHover) return;
    hoverCard.textContent = "";
    var h = el("div", "nethover-h");
    h.appendChild(el("b", null, a.name));
    h.appendChild(pill(a.state));
    hoverCard.appendChild(h);
    var dl = el("dl", "netsum");
    summaryRows(a).forEach(function (r) {
      var d = el("div");
      d.appendChild(el("dt", null, r[0]));
      d.appendChild(el("dd", null, r[1]));
      dl.appendChild(d);
    });
    hoverCard.appendChild(dl);
    hoverCard.hidden = false;
    var w = wrap.getBoundingClientRect(), n = node.getBoundingClientRect();
    var x = n.left - w.left + n.width / 2 - hoverCard.offsetWidth / 2;
    var y = n.top - w.top - hoverCard.offsetHeight - 8;
    if (y < 4) y = n.bottom - w.top + 8;
    x = Math.max(4, Math.min(x, w.width - hoverCard.offsetWidth - 4));
    hoverCard.style.left = x + "px";
    hoverCard.style.top = y + "px";
  }

  // ---- markers --------------------------------------------------------------------------------------
  var SVGNS = "http://www.w3.org/2000/svg";
  function pinSVG() {
    var svg = document.createElementNS(SVGNS, "svg");
    svg.setAttribute("viewBox", "0 0 24 30");
    svg.setAttribute("width", "30");
    svg.setAttribute("height", "38");
    svg.setAttribute("aria-hidden", "true");
    var p = document.createElementNS(SVGNS, "path");
    p.setAttribute("d", "M12 29S2 18.5 2 11a10 10 0 0 1 20 0c0 7.5-10 18-10 18z");
    var c = document.createElementNS(SVGNS, "circle");
    c.setAttribute("cx", "12"); c.setAttribute("cy", "11"); c.setAttribute("r", "4");
    svg.appendChild(p); svg.appendChild(c);
    return svg;
  }
  function worst(items) {
    var s = "unknown";
    items.forEach(function (a) { if ((RANK[a.state] || 0) > (RANK[s] || 0)) s = a.state; });
    return s;
  }

  // Greedy clustering in screen pixels: the first unassigned marker takes every other marker within CLUSTER_PX.
  // Pure (points are {x, y, item}), so it needs no map. Exposed for the page's own tests.
  function clusterPoints(points, radius) {
    var used = new Array(points.length), out = [];
    for (var i = 0; i < points.length; i++) {
      if (used[i]) continue;
      var group = [points[i]];
      used[i] = true;
      for (var j = i + 1; j < points.length; j++) {
        if (used[j]) continue;
        var dx = points[i].x - points[j].x, dy = points[i].y - points[j].y;
        if (dx * dx + dy * dy <= radius * radius) { used[j] = true; group.push(points[j]); }
      }
      out.push(group);
    }
    return out;
  }
  window.ftnNetCluster = clusterPoints;

  function start(apartments) {
    var gm = window.google.maps;
    gm.importLibrary("maps").then(function () {
      if (loading) loading.remove();
      var map = new gm.Map(mapEl, {
        center: { lat: apartments[0].lat, lng: apartments[0].lng }, zoom: 13,
        mapTypeControl: false, streetViewControl: false, fullscreenControl: true, clickableIcons: false
      });
      var bounds = new gm.LatLngBounds();
      apartments.forEach(function (a) { bounds.extend({ lat: a.lat, lng: a.lng }); });
      map.fitBounds(bounds, 56);
      gm.event.addListenerOnce(map, "idle", function () { if (map.getZoom() > 17) map.setZoom(17); });
      map.addListener("click", function () { closePanel(); hideHover(); });

      class Layer extends gm.OverlayView {
        constructor() { super(); this.root = null; this.nodes = {}; }
        onAdd() {
          this.root = el("div", "netlayer");
          this.getPanes().overlayMouseTarget.appendChild(this.root);
        }
        onRemove() { if (this.root && this.root.parentNode) this.root.parentNode.removeChild(this.root); this.root = null; }
        draw() {
          var proj = this.getProjection();
          if (!proj || !this.root) return;
          var points = apartments.map(function (a) {
            var p = proj.fromLatLngToDivPixel(new gm.LatLng(a.lat, a.lng));
            return { x: p.x, y: p.y, item: a };
          });
          var groups = map.getZoom() >= NO_CLUSTER_ZOOM ? points.map(function (p) { return [p]; }) : clusterPoints(points, CLUSTER_PX);
          var seen = {}, self = this;
          groups.forEach(function (g) {
            var id = g.map(function (p) { return p.item.id; }).sort().join("|");
            seen[id] = true;
            var node = self.nodes[id];
            if (!node) { node = self.nodes[id] = g.length === 1 ? self.pin(g[0].item) : self.bubble(g); self.root.appendChild(node); }
            var x = 0, y = 0;
            g.forEach(function (p) { x += p.x; y += p.y; });
            node.style.left = Math.round(x / g.length) + "px";
            node.style.top = Math.round(y / g.length) + "px";
          });
          Object.keys(this.nodes).forEach(function (id) {
            if (!seen[id]) { self.nodes[id].remove(); delete self.nodes[id]; }
          });
        }
        pin(a) {
          var b = el("button", "netpin net-" + a.state);
          b.type = "button";
          b.setAttribute("aria-label", a.name + ", " + (STATE_LABEL[a.state] || a.state));
          b.appendChild(pinSVG());
          b.addEventListener("click", function (e) { e.stopPropagation(); openPanel(a, b); });
          b.addEventListener("mouseenter", function () { showHover(a, b); });
          b.addEventListener("mouseleave", hideHover);
          b.addEventListener("blur", hideHover);
          return b;
        }
        bubble(g) {
          var items = g.map(function (p) { return p.item; });
          var b = el("button", "netbubble net-" + worst(items), items.length);
          b.type = "button";
          b.setAttribute("aria-label", plural(items.length, "apartment", "apartments") + " here, zoom in");
          b.addEventListener("click", function (e) {
            e.stopPropagation();
            var bb = new gm.LatLngBounds();
            items.forEach(function (a) { bb.extend({ lat: a.lat, lng: a.lng }); });
            map.fitBounds(bb, 80);
            // markers on the same spot never separate by zooming: open the first
            gm.event.addListenerOnce(map, "idle", function () { if (map.getZoom() >= NO_CLUSTER_ZOOM - 1) map.setZoom(NO_CLUSTER_ZOOM); });
          });
          return b;
        }
      }
      new Layer().setMap(map);
    }, function () { showFallback("The Google Maps library did not load."); });
  }

  // ---- load the data, then the Maps script ------------------------------------------------------------
  window.gm_authFailure = function () {
    showFallback("Google refused the key. Check that the Maps JavaScript API is enabled for it, that billing is on, and that its website restriction matches this address (https://your-admin-domain/*).");
  };

  fetch(jsonURL, { credentials: "same-origin", headers: { Accept: "application/json" } })
    .then(function (r) { if (!r.ok) throw new Error("status " + r.status); return r.json(); })
    .then(function (data) {
      var apts = (data.apartments || []).filter(function (a) { return a.has_gps && typeof a.lat === "number" && typeof a.lng === "number"; });
      if (!apts.length) { showFallback("No apartment has a GPS point yet, so there is nothing to put on the map. Add GPS to an apartment first."); return; }
      window.__ftnNetMapReady = function () { start(apts); };
      var s = document.createElement("script");
      s.async = true;
      s.src = "https://maps.googleapis.com/maps/api/js?key=" + encodeURIComponent(key) + "&loading=async&v=weekly&callback=__ftnNetMapReady";
      s.onerror = function () { showFallback("The Google Maps script could not be loaded (offline, or blocked)."); };
      document.head.appendChild(s);
    })
    .catch(function () { showFallback("The network data could not be loaded. Reload the page, or show the list."); });
})();
