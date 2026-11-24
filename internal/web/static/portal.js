/* Customer portal: choosing "Use the number on file" empties the other-number box,
   so a number typed earlier is never sent along with that choice. */
(function () {
  var f = document.querySelector(".phonegrp");
  if (!f) return;
  f.addEventListener("change", function (e) {
    var t = e.target;
    if (t && t.name === "phone_src" && t.value === "file") {
      var p = f.querySelector("input[name=phone]");
      if (p) p.value = "";
    }
  });
})();
