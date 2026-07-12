var CACHE = "oink-v2";

self.addEventListener("install", function (e) {
  e.waitUntil(self.skipWaiting());
});

self.addEventListener("activate", function (e) {
  e.waitUntil(caches.keys().then(function (keys) {
    return Promise.all(keys.filter(function (k) { return k !== CACHE; }).map(function (k) { return caches.delete(k); }));
  }).then(function () { return self.clients.claim(); }));
});

// estáticos: cache-first con caché de runtime. Las URLs van versionadas
// (?v=hash), así que un deploy nuevo produce URLs nuevas y nunca se sirve
// css/js viejo; las entradas de versiones anteriores mueren con el CACHE.
self.addEventListener("fetch", function (e) {
  var url = new URL(e.request.url);
  if (e.request.method !== "GET" || url.origin !== location.origin) return;
  if (url.pathname.startsWith("/static/") || url.pathname === "/favicon.svg") {
    e.respondWith(caches.open(CACHE).then(function (c) {
      return c.match(e.request).then(function (hit) {
        return hit || fetch(e.request).then(function (res) {
          if (res.ok) c.put(e.request, res.clone());
          return res;
        });
      });
    }));
  }
});
