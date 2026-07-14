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

  // ---- offline: cola local con sincronización ----
  // Los formularios con data-offline se envían por fetch. Si no hay red, el
  // registro se guarda en una cola local (localStorage) con un client_id
  // único y se reenvía solo al volver la conexión; el servidor deduplica por
  // client_id, así que un reintento nunca duplica.
  var QKEY = "oink-queue";

  function readQueue() {
    try { return JSON.parse(localStorage.getItem(QKEY)) || []; } catch (e) { return []; }
  }
  function writeQueue(q) {
    try { localStorage.setItem(QKEY, JSON.stringify(q)); } catch (e) {}
    paintSyncbar();
  }
  function uuid() {
    if (window.crypto && crypto.randomUUID) return crypto.randomUUID();
    return "q" + Date.now() + "-" + Math.random().toString(36).slice(2, 10);
  }
  function localDate() {
    var d = new Date();
    return d.getFullYear() + "-" + String(d.getMonth() + 1).padStart(2, "0") + "-" + String(d.getDate()).padStart(2, "0");
  }
  function paintSyncbar() {
    var bar = document.getElementById("syncbar");
    if (!bar) return;
    var n = readQueue().length;
    bar.hidden = n === 0;
    bar.textContent = n === 1
      ? "1 registro guardado en este dispositivo · se sincronizará al volver la conexión"
      : n + " registros guardados en este dispositivo · se sincronizarán al volver la conexión";
  }

  var flushing = false;
  function flush() {
    if (flushing) return;
    var q = readQueue();
    if (!q.length) return;
    flushing = true;
    var sent = 0;
    (function next() {
      if (!q.length) {
        flushing = false;
        writeQueue(q);
        if (sent) location.reload(); // refresca los números ya sincronizados
        return;
      }
      fetch(q[0].url, {
        method: "POST",
        headers: { "Content-Type": "application/x-www-form-urlencoded" },
        body: q[0].body,
        credentials: "same-origin"
      }).then(function () {
        // cualquier respuesta = el servidor lo recibió (client_id deduplica)
        q.shift();
        sent++;
        writeQueue(q);
        next();
      }).catch(function () {
        flushing = false; // sigue sin red: la cola espera
        writeQueue(q);
      });
    })();
  }

  function appendPendingTodo(text) {
    var list = document.getElementById("todolist");
    if (!list) return;
    var empty = document.getElementById("todo-empty");
    if (empty) empty.remove();
    var li = document.createElement("li");
    li.className = "queued";
    var p = document.createElement("p");
    p.className = "todobody";
    p.textContent = text;
    var tag = document.createElement("span");
    tag.className = "muted small tododate";
    tag.textContent = "⏳ por sincronizar";
    li.appendChild(p);
    li.appendChild(tag);
    list.appendChild(li);
  }

  function offlineAfter(f) {
    if (f.id === "txform") {
      var padEl = document.getElementById("keypad");
      if (padEl) { padEl.hidden = true; document.body.style.overflow = ""; }
    } else if (f.classList.contains("todoadd")) {
      var ta = f.querySelector("textarea");
      if (ta) { appendPendingTodo(ta.value); ta.value = ""; }
    } else if (f.dataset.optimistic) {
      var li = f.closest("li");
      if (li) li.classList.toggle("offdone", f.dataset.optimistic === "done");
    }
  }

  document.addEventListener("submit", function (e) {
    if (e.defaultPrevented) return;
    var f = e.target;
    if (!f.hasAttribute || !f.hasAttribute("data-offline")) return;
    e.preventDefault();
    if (f.dataset.busy) return;
    f.dataset.busy = "1";
    var cid = f.querySelector('input[name="client_id"]');
    if (cid) cid.value = uuid();
    var made = f.querySelector('input[name="made_on"]');
    if (made) made.value = localDate();
    var body = new URLSearchParams(new FormData(f)).toString();
    var url = f.getAttribute("action");
    var buttons = f.querySelectorAll("button");
    buttons.forEach(function (b) { b.disabled = true; });
    fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: body,
      credentials: "same-origin"
    }).then(function () {
      location.reload();
    }).catch(function () {
      // sin conexión: a la cola, la interfaz sigue funcionando
      var q = readQueue();
      q.push({ url: url, body: body, ts: Date.now() });
      writeQueue(q);
      offlineAfter(f);
      delete f.dataset.busy;
      buttons.forEach(function (b) { b.disabled = false; });
    });
  });

  // al cargar: pinta lo pendiente de la cola y trata de sincronizar
  readQueue().forEach(function (it) {
    if (it.url === "/todo") {
      appendPendingTodo(new URLSearchParams(it.body).get("body") || "");
    } else {
      var m = it.url.match(/^\/todo\/(\d+)\/toggle$/);
      if (m) {
        var li = document.querySelector('#todolist li[data-id="' + m[1] + '"]');
        if (li && new URLSearchParams(it.body).get("set") === "done") li.classList.add("offdone");
      }
    }
  });
  paintSyncbar();
  flush();
  window.addEventListener("online", flush);

  // anti doble envío: con conexión lenta un segundo tap reenviaría el
  // formulario y duplicaría el movimiento. El primer envío marca el form y
  // apaga sus botones; volver a la página (bfcache) lo rearma.
  // (los formularios data-offline traen su propia guardia arriba)
  document.addEventListener("submit", function (e) {
    if (e.defaultPrevented) return;
    var f = e.target;
    if (f.hasAttribute && f.hasAttribute("data-offline")) return;
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
