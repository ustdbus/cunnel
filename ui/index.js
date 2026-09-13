/**
 * Caddy Tunnel Panel — frontend (runtime-loaded plugin module).
 *
 * Registers a Console page at /apps/caddy-tunnel-panel that embeds the Go
 * panel UI. The Go panel is served through the plugin backend under
 * /api/caddy-tunnel-panel/ui (same-origin), so no CORS or mixed-content
 * issues and the QwenPaw auth token is reused automatically.
 */
(function () {
  var QwenPaw = window.QwenPaw;
  if (!QwenPaw || !QwenPaw.host || !QwenPaw.registerRoutes) {
    console.error("[caddy-tunnel-panel] window.QwenPaw not ready — cannot register.");
    return;
  }

  var host = QwenPaw.host;
  var React = host.React;
  var antd = host.antd;
  var h = React.createElement;
  var useHostLocale = typeof host.useLocale === "function" ? host.useLocale : function () { return "en"; };

  var PLUGIN_ID = "caddy-tunnel-panel";
  var useTheme = typeof host.useTheme === "function" ? host.useTheme : function () { return "light"; };

  var MSG = {
    zh: {
      title: "🌐 Cunnel",
      openNewTab: "新窗口打开",
      refresh: "刷新",
      loading: "正在启动面板…",
      failed: "面板加载失败",
      retry: "重试",
      starting: "首次打开会启动 Go 面板进程,请稍候…",
    },
    en: {
      title: "🌐 Cunnel",
      openNewTab: "Open in new tab",
      refresh: "Refresh",
      loading: "Starting panel…",
      failed: "Failed to load panel",
      retry: "Retry",
      starting: "First open starts the Go panel process, please wait…",
    },
  };

  function loc(locale) {
    return String(locale || "").toLowerCase().indexOf("zh") === 0 ? "zh" : "en";
  }

  function PanelPage() {
    var locale = loc(useHostLocale());
    var theme = useTheme();
    var t = MSG[locale];
    var uiUrl = host.getApiUrl("/" + PLUGIN_ID + "/ui");
    var token = host.getApiToken ? host.getApiToken() : "";

    var _useState = React.useState("loading");
    var status = _useState[0];
    var setStatus = _useState[1];
    var _nonce = React.useState(0);
    var nonce = _nonce[0];
    var setNonce = _nonce[1];

    React.useEffect(function () {
      var cancelled = false;
      fetch(host.getApiUrl("/" + PLUGIN_ID + "/status"), {
        headers: token ? { Authorization: "Bearer " + token } : {},
      })
        .then(function (r) { return r.json(); })
        .then(function (d) {
          if (cancelled) return;
          setStatus(d && d.running ? "ready" : "failed");
        })
        .catch(function () { if (!cancelled) setStatus("failed"); });
      return function () { cancelled = true; };
    }, [nonce]);

    // 面板页面需要同源携带 token;用 fetch 拿 HTML 后写入 iframe
    var _html = React.useState("");
    var html = _html[0];
    var setHtml = _html[1];
    React.useEffect(function () {
      if (status !== "ready") return;
      var cancelled = false;
      fetch(uiUrl, { headers: token ? { Authorization: "Bearer " + token } : {} })
        .then(function (r) { return r.text(); })
        .then(function (txt) { if (!cancelled) setHtml(txt); })
        .catch(function () { if (!cancelled) setStatus("failed"); });
      return function () { cancelled = true; };
    }, [status, nonce]);

    var header = h(
      "div",
      {
        style: {
          display: "flex", alignItems: "center", justifyContent: "space-between",
          padding: "12px 16px", borderBottom: "1px solid rgba(15,23,42,0.08)",
          background: theme === "dark" ? "#1f1f1f" : "#fff",
          flexWrap: "wrap", gap: 8,
        },
      },
      h("div", null,
        h("div", { style: { fontSize: 15, fontWeight: 600 } }, t.title),
      ),
      h("div", { style: { display: "flex", gap: 8 } },
        h(antd.Button, { size: "small", onClick: function () { setNonce(nonce + 1); } }, t.refresh),
        h(antd.Button, {
          size: "small", type: "primary",
          onClick: function () { window.open(uiUrl, "_blank"); },
        }, t.openNewTab),
      ),
    );

    var body;
    if (status === "loading") {
      body = h("div", { style: { padding: 48, textAlign: "center" } },
        h(antd.Spin, null),
        h("div", { style: { marginTop: 12, color: "#6b7280", fontSize: 13 } }, t.starting),
      );
    } else if (status === "failed") {
      body = h(antd.Result, {
        status: "warning",
        title: t.failed,
        subTitle: t.starting,
        extra: h(antd.Button, { type: "primary", onClick: function () { setNonce(nonce + 1); } }, t.retry),
      });
    } else if (!html) {
      body = h("div", { style: { padding: 48, textAlign: "center" } }, h(antd.Spin, null));
    } else {
      body = h("iframe", {
        title: PLUGIN_ID,
        srcDoc: html,
        style: { width: "100%", height: "calc(100vh - 150px)", border: "none", display: "block" },
      });
    }

    return h("div", { style: { display: "flex", flexDirection: "column", height: "100%" } },
      header, h("div", { style: { flex: 1, overflow: "auto" } }, body));
  }

  QwenPaw.registerRoutes(PLUGIN_ID, [
    {
      path: "/apps/caddy-tunnel-panel",
      component: PanelPage,
      label: MSG.zh.title,
      icon: "🌐",
    },
  ]);

  console.info("[caddy-tunnel-panel] registered route /apps/caddy-tunnel-panel");
})();
