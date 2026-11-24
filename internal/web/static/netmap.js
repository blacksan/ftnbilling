/* The Network page's map: MapLibre GL JS (vendored in /static/vendor/maplibre) on OpenFreeMap tiles; no key, no account.
   External file: the admin CSP allows no inline script. The library is an ES module loaded with a dynamic import() of
   the versioned URL in data-lib, and its tile worker is the self-hosted module worker in data-worker, so the page needs
   no eval, no blob script and no remote script (see cspNetwork in security_headers.go). Every name, address and serial
   comes from user-entered data: it is only ever put on the page with textContent and setAttribute on links we vet
   (safeHref), never as HTML. */
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
  var styleURL = wrap.getAttribute("data-basemap") || "";
  var libURL = wrap.getAttribute("data-lib") || "";
  var workerURL = wrap.getAttribute("data-worker") || "";
  var jsonURL = wrap.getAttribute("data-json") || "/admin/network/map.json";
  var canHover = !!(window.matchMedia && window.matchMedia("(hover: hover) and (pointer: fine)").matches);
  var STATE_LABEL = { ok: "All good", partial: "Some offline", down: "Router down", unknown: "No router" };
  var RANK = { down: 3, partial: 2, ok: 1, unknown: 0 };
  var RANK_STATE = ["unknown", "ok", "partial", "down"]; // the index is RANK; GeoJSON carries the number so a cluster can take the max
  var SRC = "apartments";
  var CLUSTER_RADIUS_PX = 44;
  var CLUSTER_MAX_ZOOM = 17;  // from zoom 18 every apartment is its own pin
  var MAX_ZOOM = 19;

  // ---- small DOM helpers (text only) -------------------------------------------------------------
  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = String(text);
    return n;
  }
  // Only our own pages and Google Maps links (the optional "View on Google Maps" action) become hrefs.
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

  function pinNode(a) {
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

  function start(ml, apartments) {
    var byID = {};
    var features = apartments.map(function (a) {
      byID[a.id] = a;
      return {
        type: "Feature",
        properties: { id: a.id, rank: RANK[a.state] || 0 },
        geometry: { type: "Point", coordinates: [a.lng, a.lat] }
      };
    });

    var map = new ml.Map({
      container: mapEl,
      style: styleURL,
      center: [apartments[0].lng, apartments[0].lat],
      zoom: 13,
      maxZoom: MAX_ZOOM,
      // The style's own attribution (OpenFreeMap, OpenMapTiles, OpenStreetMap) stays open, not folded into an "i" button.
      attributionControl: { compact: false },
      dragRotate: false,
      pitchWithRotate: false,
      touchPitch: false
    });
    map.touchZoomRotate.disableRotation();
    map.addControl(new ml.NavigationControl({ showCompass: false }), "top-right");
    map.addControl(new ml.ScaleControl({ unit: "metric" }), "bottom-left");

    var styleLoaded = false;
    map.on("error", function () {
      // A tile or glyph failing later is not fatal; a style that never loaded (offline, blocked) leaves nothing to show.
      if (styleLoaded) return;
      try { map.remove(); } catch (e) { /* already gone */ }
      showFallback("The base map could not be loaded (offline, or tiles.openfreemap.org is blocked).");
    });
    map.on("click", function () { closePanel(); hideHover(); });

    var markers = {};
    function updateMarkers() {
      if (!styleLoaded || !map.getSource(SRC) || !map.isSourceLoaded(SRC)) return;
      var seen = {};
      map.querySourceFeatures(SRC).forEach(function (f) {
        var p = f.properties || {};
        var id = p.cluster ? "c" + p.cluster_id : "a" + p.id;
        if (seen[id]) return; // the same feature comes once per tile it touches
        seen[id] = true;
        if (markers[id]) return;
        var coords = f.geometry.coordinates.slice();
        var node, anchor;
        if (p.cluster) {
          var n = Number(p.point_count) || 0;
          node = el("button", "netbubble net-" + (RANK_STATE[p.rank] || "unknown"), n);
          node.type = "button";
          node.setAttribute("aria-label", plural(n, "apartment", "apartments") + " here, zoom in");
          anchor = "center";
          (function (clusterID, at) {
            node.addEventListener("click", function (e) {
              e.stopPropagation();
              map.getSource(SRC).getClusterExpansionZoom(clusterID).then(function (z) {
                map.easeTo({ center: at, zoom: Math.min(z, MAX_ZOOM) });
              });
            });
          })(p.cluster_id, coords);
        } else {
          var a = byID[p.id];
          if (!a) return;
          node = pinNode(a);
          anchor = "bottom";
        }
        markers[id] = new ml.Marker({ element: node, anchor: anchor }).setLngLat(coords).addTo(map);
      });
      Object.keys(markers).forEach(function (id) {
        if (!seen[id]) { markers[id].remove(); delete markers[id]; }
      });
    }

    map.on("load", function () {
      styleLoaded = true;
      if (loading) loading.remove();
      map.addSource(SRC, {
        type: "geojson",
        data: { type: "FeatureCollection", features: features },
        cluster: true,
        clusterRadius: CLUSTER_RADIUS_PX,
        clusterMaxZoom: CLUSTER_MAX_ZOOM,
        clusterProperties: { rank: ["max", ["get", "rank"]] }
      });
      // The pins and bubbles are DOM buttons (keyboard and screen-reader reachable), drawn by updateMarkers from what the
      // source says is on screen. A source with no layer loads no tiles, so one invisible layer keeps it live.
      map.addLayer({ id: "apartments-live", type: "circle", source: SRC, paint: { "circle-radius": 1, "circle-opacity": 0 } });
      if (apartments.length > 1) {
        var bounds = new ml.LngLatBounds();
        apartments.forEach(function (a) { bounds.extend([a.lng, a.lat]); });
        map.fitBounds(bounds, { padding: 56, maxZoom: 17, duration: 0 });
      } else {
        map.jumpTo({ center: [apartments[0].lng, apartments[0].lat], zoom: 16 });
      }
      map.on("sourcedata", function (e) { if (e.sourceId === SRC && e.isSourceLoaded) updateMarkers(); });
      map.on("moveend", updateMarkers);
      updateMarkers();
    });
  }

  // ---- load the data, then the library ----------------------------------------------------------------
  fetch(jsonURL, { credentials: "same-origin", headers: { Accept: "application/json" } })
    .then(function (r) { if (!r.ok) throw new Error("status " + r.status); return r.json(); })
    .then(function (data) {
      var apts = (data.apartments || []).filter(function (a) { return a.has_gps && typeof a.lat === "number" && typeof a.lng === "number"; });
      if (!apts.length) { showFallback("No apartment has a GPS point yet, so there is nothing to put on the map. Add GPS to an apartment first."); return; }
      return import(libURL).then(function (mod) {
        var ml = mod.Map ? mod : mod.default;
        if (!ml || !ml.Map) throw new Error("no Map in the library");
        if (workerURL) ml.setWorkerUrl(new URL(workerURL, window.location.href).href);
        start(ml, apts);
      }, function () { showFallback("The map library could not be loaded."); });
    })
    .catch(function () { showFallback("The network data could not be loaded. Reload the page, or show the list."); });
})();
