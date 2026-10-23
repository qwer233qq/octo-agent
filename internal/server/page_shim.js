// The script the server splices into every artifact and Light App page, ahead
// of the page's own scripts (artifact_pages.go). Configuration arrives in
// `window.__octoPage = { ns, download }` from the inline script just before
// it.
//
// Plain ES5 plus Proxy on purpose: it runs inside whatever the page is, and
// the desktop webviews are not all evergreen.
(function () {
  var cfg = window.__octoPage || {};
  var NS = String(cfg.ns || '');

  // ── localStorage namespace ────────────────────────────────────────────────
  //
  // The page shares its origin — and so its localStorage — with the octo UI.
  // Every key it touches is moved under `octo.page.<ns>:`, so a page calling
  // clear() or picking a common key name cannot wipe the UI's settings or
  // another page's data. The store underneath is the real one: synchronous,
  // persistent, the browser's own quota.
  var real;
  try { real = window.localStorage; } catch (e) { real = null; }
  if (real) {
    // Encoded so one namespace can never be a prefix of another (`a` vs `a:b`).
    var P = 'octo.page.' + encodeURIComponent(NS) + ':';
    var hasOwn = Object.prototype.hasOwnProperty;
    // What a real Storage inherits — hasOwnProperty, toString, valueOf,
    // constructor — resolves before a stored key of the same name, as it does
    // on the real thing. The target carries the same prototype so instanceof
    // Storage still holds.
    var proto = Object.getPrototypeOf(real) || Object.prototype;
    var inherited = function (k) { return k !== 'length' && !hasOwn.call(api, k) && k in proto; };
    var own = function () {
      var out = [];
      for (var i = 0; i < real.length; i++) {
        var k = real.key(i);
        if (k !== null && k.indexOf(P) === 0) out.push(k.slice(P.length));
      }
      return out;
    };
    var api = {
      getItem: function (k) { return real.getItem(P + String(k)); },
      setItem: function (k, v) { real.setItem(P + String(k), String(v)); },
      removeItem: function (k) { real.removeItem(P + String(k)); },
      clear: function () { var ks = own(); for (var i = 0; i < ks.length; i++) real.removeItem(P + ks[i]); },
      key: function (i) { var ks = own(); return i >= 0 && i < ks.length ? ks[i] : null; }
    };
    // Property access (`localStorage.foo = 'x'`, `Object.keys(localStorage)`,
    // `'foo' in localStorage`) behaves as on a real Storage, under the prefix.
    var wrapped = new Proxy(Object.create(proto), {
      get: function (t, k) {
        if (k === 'length') return own().length;
        if (typeof k !== 'string') return proto[k];
        if (hasOwn.call(api, k)) return api[k];
        if (inherited(k)) return proto[k];
        var v = real.getItem(P + k);
        return v === null ? undefined : v;
      },
      set: function (t, k, v) { if (typeof k === 'string') real.setItem(P + k, String(v)); return true; },
      deleteProperty: function (t, k) { if (typeof k === 'string') real.removeItem(P + k); return true; },
      has: function (t, k) { return typeof k === 'string' ? (k === 'length' || hasOwn.call(api, k) || inherited(k) || real.getItem(P + k) !== null) : k in proto; },
      ownKeys: function () { return own(); },
      getOwnPropertyDescriptor: function (t, k) {
        if (typeof k !== 'string') return undefined;
        var v = real.getItem(P + k);
        return v === null ? undefined : { value: v, writable: true, enumerable: true, configurable: true };
      }
    });
    try {
      Object.defineProperty(window, 'localStorage', { configurable: true, enumerable: true, get: function () { return wrapped; } });
    } catch (e) {}
  }

  // ── API fetch wrapper ─────────────────────────────────────────────────────
  //
  // The server plants the access key in cfg.key when the page load was
  // authenticated (token page, authed Light App; never the anonymous
  // public path — server side). The page is same-origin with the octo UI,
  // but that does not make its cookie and the UI's one agree: over plain
  // http a Secure cookie an https visit left behind is neither sent nor
  // readable, and an agent-written app (which knows nothing about any of
  // this) then renders the server's {"error":"unauthorized"} JSON. The
  // wrapper stamps X-Access-Key on same-origin calls the server already
  // accepts, which the wrapper keeps transparent: caller-supplied headers
  // win, cross-origin requests and non-API paths pass through untouched.
  var KEY = String(cfg.key || '');
  if (KEY && window.fetch) {
    var origFetch = window.fetch.bind(window);
    var needsKey = function (input) {
      try {
        var u = typeof input === 'string' ? input : (input && input.url) || '';
        if (!u) return false;
        var el = document.createElement('a');
        el.href = u; // relative hrefs resolve against the page's base
        if (el.protocol !== location.protocol || el.host !== location.host) return false;
        var p = el.pathname;
        return p === '/api' || p.indexOf('/api/') === 0 || p === '/ws' ||
          p === '/_artifacts' || p.indexOf('/_artifacts/') === 0 ||
          p === '/_apps' || p.indexOf('/_apps/') === 0;
      } catch (e) { return false; }
    };
    window.fetch = function (input, init) {
      if (!needsKey(input)) return origFetch(input, init);
      try {
        var src = init || (input && typeof input === 'object' ? input : {});
        var hdr = src.headers;
        // Stamp only the header shapes we can merge safely: absent ({}),
        // a Headers instance, or a plain object. fetch also accepts an
        // array of [name, value] pairs and a sequence string — converting
        // those would produce an invalid headers object and break a call
        // that worked before, so they pass through unmodified.
        var stampable = !hdr || hdr === null;
        var isHeaders = typeof Headers === 'function' && hdr instanceof Headers;
        if (isHeaders) { stampable = true; hdr = new Headers(hdr); }
        else if (hdr !== null && typeof hdr === 'object' && !Array.isArray(hdr)) {
          stampable = true;
          var merged = {};
          for (var h in hdr) { if (Object.prototype.hasOwnProperty.call(hdr, h)) merged[h] = hdr[h]; }
          hdr = merged;
        }
        if (!stampable) return origFetch(input, init);
        var existing = false;
        try {
          if (isHeaders) { existing = hdr.has('X-Access-Key'); }
          else { for (var n in hdr) { if (String(n).toLowerCase() === 'x-access-key') existing = true; } }
        } catch (e2) { existing = false; }
        if (!existing) {
          // A Headers instance stores headers behind internal slots, so a
          // property assignment here would land on the object and never be
          // sent — it has to go through set().
          if (isHeaders) hdr.set('X-Access-Key', KEY); else hdr['X-Access-Key'] = KEY;
        }
        var i2 = {};
        for (var k in src) { if (k !== 'url') i2[k] = src[k]; }
        i2.headers = hdr;
        return origFetch(input, i2);
      } catch (e) {
        return origFetch(input, init);
      }
    };
  }

  // ── Download bridge (Light Apps in the desktop shell only) ────────────────
  //
  // The desktop webview has no download delegate, so an `<a download>` click
  // does nothing there. The textbook idiom is intercepted and the bytes handed
  // to the host, which opens the OS save dialog (deliverLaDownload in
  // web/src/lib/laDownload.ts). In a browser the page downloads by itself and
  // this half stays off.
  if (!cfg.download || window.parent === window) return;
  function send(name, blob) {
    try { window.parent.postMessage({ __laBridge: 1, id: 0, ns: NS, op: 'download', name: name, blob: blob }, '*'); }
    catch (e) { console.warn('[octo] download bridge failed', e); }
  }
  // Resolve the anchor's target to bytes and hand them to the host. Returns
  // false when the anchor has nothing to download, so the caller leaves the
  // event alone.
  function save(a) {
    if (!a.getAttribute('href')) return false;
    var name = a.getAttribute('download') || '';
    fetch(a.href).then(function (r) { return r.blob(); })
      .then(function (b) { send(name, b); })
      .catch(function (e) { console.warn('[octo] download failed', e); });
    return true;
  }
  document.addEventListener('click', function (ev) {
    if (ev.defaultPrevented) return;
    // composedPath so an anchor inside a shadow root is found; target alone
    // is retargeted to the shadow host.
    var t = ev.composedPath ? ev.composedPath()[0] : ev.target;
    var a = t && t.closest ? t.closest('a[download]') : null;
    if (a && save(a)) ev.preventDefault();
  });
  var origClick = HTMLAnchorElement.prototype.click;
  HTMLAnchorElement.prototype.click = function () {
    if (!this.isConnected && this.hasAttribute('download') && save(this)) return;
    return origClick.apply(this, arguments);
  };
})();
