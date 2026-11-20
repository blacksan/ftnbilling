/* "Use this device's location" on the apartment form. External file: the admin CSP allows no inline script.
   The button is hidden until the browser says it can do geolocation; the server still parses and checks the field. */
(function () {
  "use strict";
  var btn = document.querySelector("[data-gps-fill]");
  if (!btn) return;
  var field = document.getElementById(btn.getAttribute("data-gps-fill"));
  var note = document.getElementById(btn.getAttribute("data-gps-note"));
  if (!field || !("geolocation" in navigator)) return;
  btn.hidden = false;
  function say(t) { if (note) note.textContent = t; }
  btn.addEventListener("click", function () {
    say("Finding this device...");
    navigator.geolocation.getCurrentPosition(function (pos) {
      field.value = pos.coords.latitude.toFixed(6) + ", " + pos.coords.longitude.toFixed(6);
      say("Filled in. Accuracy about " + Math.round(pos.coords.accuracy) + " m. Save to keep it.");
    }, function (err) {
      say(err && err.code === 1 ? "Location permission was refused. Type or paste the coordinates instead."
        : "Could not get the location (it needs https and a device with GPS or Wi-Fi). Type or paste it instead.");
    }, { enableHighAccuracy: true, timeout: 15000, maximumAge: 0 });
  });
})();
