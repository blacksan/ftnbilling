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
  // Browsers only offer location on https (or localhost).
  var secure = window.isSecureContext !== false;
  function say(t) { if (note) note.textContent = t; }
  btn.addEventListener("click", function () {
    if (!secure) { say("Location only works on the https address of this site. Open the site over https, or type or paste the coordinates instead."); return; }
    say("Finding this device...");
    navigator.geolocation.getCurrentPosition(function (pos) {
      field.value = pos.coords.latitude.toFixed(6) + ", " + pos.coords.longitude.toFixed(6);
      say("Filled in. Accuracy about " + Math.round(pos.coords.accuracy) + " m. Save to keep it.");
    }, function (err) {
      var code = err && err.code;
      if (code === 1) {
        say("Location permission was refused. Allow location for this site in the browser's site settings (the location button needs the https address), or type or paste the coordinates instead.");
      } else if (code === 3) {
        say("Finding the location timed out. Try again near a window or with Wi-Fi on, or type or paste the coordinates instead.");
      } else {
        say("The position is not available on this device right now (it needs GPS or Wi-Fi location). Type or paste the coordinates instead.");
      }
    }, { enableHighAccuracy: true, timeout: 15000, maximumAge: 0 });
  });
})();
