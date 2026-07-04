(function () {
  "use strict";

  // confirmaciones
  document.addEventListener("click", function (e) {
    var btn = e.target.closest("[data-confirm]");
    if (btn && !window.confirm(btn.dataset.confirm)) {
      e.preventDefault();
      e.stopPropagation();
    }
  });

  // barras (ritmo del mes y reportes): evita estilos inline en el HTML por el CSP
  document.querySelectorAll("[data-w]").forEach(function (el) {
    requestAnimationFrame(function () { el.style.width = el.dataset.w + "%"; });
  });

  // ---- teclado ----
  var pad = document.getElementById("keypad");
  if (pad) {
    var kindInput = document.getElementById("kp-kind");
    var amountInput = document.getElementById("kp-amount");
    var display = document.getElementById("kp-display");
    var title = document.getElementById("kp-title");
    var creditToggle = document.getElementById("kp-credit");
    var concept = document.getElementById("kp-concept");
    var save = document.getElementById("kp-save");
    var raw = "";

    var titles = { card: "Gasto con tarjeta", cash: "Gasto en efectivo", withdrawal: "Retiro de cajero" };

    function fmt(s) {
      if (!s) return "$0";
      var parts = s.split(".");
      var whole = parts[0] || "0";
      whole = whole.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
      return "$" + whole + (parts.length > 1 ? "." + parts[1] : "");
    }

    function paint() {
      display.textContent = fmt(raw);
      amountInput.value = raw;
      save.disabled = !(parseFloat(raw) > 0);
    }

    function open(kind, preset) {
      kindInput.value = kind;
      title.textContent = titles[kind] || "Gasto";
      creditToggle.hidden = kind !== "card";
      if (kind !== "card") creditToggle.querySelector("input").checked = false;
      else creditToggle.querySelector("input").checked = true;
      document.querySelectorAll(".chips[data-for]").forEach(function (c) {
        c.hidden = c.dataset.for !== kind;
      });
      concept.value = kind === "withdrawal" ? "retiro cajero" : "";
      raw = preset ? String(preset / 100) : "";
      paint();
      pad.hidden = false;
      document.body.style.overflow = "hidden";
    }

    function close() {
      pad.hidden = true;
      document.body.style.overflow = "";
    }

    document.querySelectorAll("[data-open]").forEach(function (b) {
      b.addEventListener("click", function () {
        open(b.dataset.open, b.dataset.amount ? parseInt(b.dataset.amount, 10) : 0);
      });
    });
    pad.querySelector("[data-close]").addEventListener("click", close);

    pad.querySelectorAll(".keys button").forEach(function (b) {
      b.addEventListener("click", function () {
        var d = b.dataset.d;
        if (d === "del") raw = raw.slice(0, -1);
        else if (d === ".") { if (raw.indexOf(".") === -1) raw = (raw || "0") + "."; }
        else {
          var dot = raw.indexOf(".");
          if (dot !== -1 && raw.length - dot > 2) return; // máx 2 decimales
          if (raw.replace(".", "").length >= 7) return;    // máx 7 dígitos
          raw = raw === "0" ? d : raw + d;
        }
        paint();
      });
    });

    pad.querySelectorAll(".chip").forEach(function (c) {
      c.addEventListener("click", function () {
        var on = c.classList.contains("on");
        c.parentElement.querySelectorAll(".chip").forEach(function (x) { x.classList.remove("on"); });
        if (!on) { c.classList.add("on"); concept.value = c.textContent; }
        else concept.value = "";
      });
    });

    document.getElementById("txform").addEventListener("submit", function (e) {
      if (!(parseFloat(raw) > 0)) e.preventDefault();
    });
  }

  // ---- service worker ----
  if ("serviceWorker" in navigator) {
    navigator.serviceWorker.register("/sw.js").catch(function () {});
  }
})();
