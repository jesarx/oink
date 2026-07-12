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

  // anti doble envío: con conexión lenta un segundo tap reenviaría el
  // formulario y duplicaría el movimiento. El primer envío marca el form y
  // apaga sus botones; volver a la página (bfcache) lo rearma.
  document.addEventListener("submit", function (e) {
    if (e.defaultPrevented) return;
    var f = e.target;
    if (f.dataset.sent) { e.preventDefault(); return; }
    f.dataset.sent = "1";
    setTimeout(function () {
      f.querySelectorAll("button:not([type=button])").forEach(function (b) {
        b.disabled = true;
        b.dataset.lock = "1";
      });
    }, 0);
  });
  window.addEventListener("pageshow", function () {
    document.querySelectorAll("form[data-sent]").forEach(function (f) { delete f.dataset.sent; });
    document.querySelectorAll("button[data-lock]").forEach(function (b) {
      b.disabled = false;
      delete b.dataset.lock;
    });
  });

  // ---- teclado ----
  var pad = document.getElementById("keypad");
  if (pad) {
    var kindInput = document.getElementById("kp-kind");
    var creditInput = document.getElementById("kp-credit");
    var amountInput = document.getElementById("kp-amount");
    var categoryInput = document.getElementById("kp-category");
    var display = document.getElementById("kp-display");
    var title = document.getElementById("kp-title");
    var concept = document.getElementById("kp-concept");
    var save = document.getElementById("kp-save");
    var seg = document.getElementById("kp-seg");
    var cats = document.getElementById("kp-cats");
    var raw = "";
    var curSource = "debit";

    // fuentes de pago: cómo se traduce cada botón a kind+credit del servidor
    var sources = {
      debit:      { kind: "card", credit: "off", title: "Gasto con t. débito" },
      credit:     { kind: "card", credit: "on",  title: "Gasto con t. crédito" },
      cash:       { kind: "cash", credit: "off", title: "Gasto en efectivo" },
      withdrawal: { kind: "withdrawal", credit: "off", title: "Retiro de cajero" },
      cardpay:    { kind: "cardpay", credit: "off", title: "Pago de la tarjeta" }
    };
    var catLabels = { comida: "Comida", libros: "Libros", alcohol: "Alcohol" };

    function updateTitle() {
      var cat = categoryInput.value;
      title.textContent = cat ? (catLabels[cat] || "Gasto") : sources[curSource].title;
    }

    function setCategory(cat) {
      categoryInput.value = cat || "";
      if (cats) cats.querySelectorAll(".catbtn").forEach(function (b) {
        b.classList.toggle("on", b.dataset.cat === categoryInput.value && categoryInput.value !== "");
      });
      updateTitle();
    }

    // setSource fija la fuente (t. débito, t. crédito, efectivo, retiro o pago
    // de tarjeta) y muestra solo los controles que aplican a gastos.
    function setSource(src) {
      curSource = src;
      var m = sources[src];
      kindInput.value = m.kind;
      creditInput.value = m.credit;
      var isSpend = src === "debit" || src === "credit" || src === "cash";
      if (seg) {
        seg.hidden = !isSpend;
        seg.querySelectorAll("[data-source]").forEach(function (b) {
          b.classList.toggle("on", b.dataset.source === src);
        });
      }
      if (cats) {
        cats.hidden = !isSpend;
        if (!isSpend) setCategory(""); // retiros y pagos de tarjeta no llevan rubro
      }
      document.querySelectorAll(".chips[data-for]").forEach(function (c) {
        c.hidden = c.dataset.for !== m.kind;
      });
      updateTitle();
    }

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

    function open(src, preset, category) {
      setSource(src);
      setCategory(category || "");
      concept.value = src === "withdrawal" ? "retiro cajero" : (src === "cardpay" ? "pago de tarjeta" : "");
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
        open(b.dataset.open, b.dataset.amount ? parseInt(b.dataset.amount, 10) : 0, b.dataset.cat || "");
      });
    });
    pad.querySelector("[data-close]").addEventListener("click", close);

    if (seg) seg.querySelectorAll("[data-source]").forEach(function (b) {
      b.addEventListener("click", function () { setSource(b.dataset.source); });
    });
    if (cats) cats.querySelectorAll(".catbtn").forEach(function (b) {
      b.addEventListener("click", function () {
        setCategory(categoryInput.value === b.dataset.cat ? "" : b.dataset.cat);
      });
    });

    function pressKey(d) {
      if (d === "del") raw = raw.slice(0, -1);
      else if (d === ".") { if (raw.indexOf(".") === -1) raw = (raw || "0") + "."; }
      else {
        var dot = raw.indexOf(".");
        if (dot !== -1 && raw.length - dot > 2) return; // máx 2 decimales
        if (raw.replace(".", "").length >= 7) return;    // máx 7 dígitos
        raw = raw === "0" ? d : raw + d;
      }
      paint();
    }

    pad.querySelectorAll(".keys button").forEach(function (b) {
      b.addEventListener("click", function () { pressKey(b.dataset.d); });
    });

    // teclado físico (computadora): escribe el monto sin usar el mouse.
    // En el celular no aplica porque no hay teclado físico enviando estas
    // teclas para el monto; solo se ignora mientras se escribe el concepto.
    document.addEventListener("keydown", function (e) {
      if (pad.hidden) return;
      if (e.key === "Escape") { close(); return; }
      if (e.target && e.target.id === "kp-concept") return; // escribiendo el concepto
      if (e.ctrlKey || e.metaKey || e.altKey) return;       // respeta atajos del navegador
      if (e.key >= "0" && e.key <= "9") { pressKey(e.key); e.preventDefault(); }
      else if (e.key === "." || e.key === ",") { pressKey("."); e.preventDefault(); }
      else if (e.key === "Backspace") { pressKey("del"); e.preventDefault(); }
      else if (e.key === "Enter") { if (!save.disabled) save.click(); e.preventDefault(); }
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
