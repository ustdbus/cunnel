package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed web/*
var webFS embed.FS

var (
	baseDir    string
	store      *Store
	actualPort = 8971
)

func main() {
	if handleCLIIfRequested() {
		return
	}
	runServer()
}

func runServer() {
	var err error
	baseDir = os.Getenv("CUNNEL_DIR")
	if baseDir == "" {
		baseDir = os.Getenv("CFD_PANEL_DIR")
	}
	if baseDir == "" {
		if _, err := os.Stat("/opt/cunnel"); err == nil {
			baseDir = "/opt/cunnel"
		} else if _, err := os.Stat("/opt/cfd-panel"); err == nil {
			baseDir = "/opt/cfd-panel"
		} else {
			baseDir = "/opt/cunnel"
		}
	}
	for _, d := range []string{"bin", "logs", "data", "tmp"} {
		_ = os.MkdirAll(filepath.Join(baseDir, d), 0o755)
	}
	// 配置 Cunnel 主业务日志文件，记录隧道连通与分流代理核心运行记录
	if cLogFile, lErr := os.OpenFile(filepath.Join(baseDir, "logs", "cunnel.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); lErr == nil {
		log.SetOutput(io.MultiWriter(os.Stdout, cLogFile))
	}
	store, err = NewStore(filepath.Join(baseDir, "data", "state.json"))
	if err != nil {
		log.Fatalf("状态加载失败: %v", err)
	}

	rawHost := "127.0.0.1"
	prefPort := 8971
	envAddr := os.Getenv("CUNNEL_ADDR")
	if envAddr == "" {
		envAddr = os.Getenv("CFD_PANEL_ADDR")
	}
	if envAddr != "" {
		if h, pStr, err := net.SplitHostPort(envAddr); err == nil {
			rawHost = h
			if p, perr := strconv.Atoi(pStr); perr == nil && p > 0 {
				prefPort = p
			}
		}
	}

	// 智能避让：如果端口被占用，自动递增寻找可用端口
	ln, listenAddr, realPort, err := listenWithFallback(rawHost, prefPort)
	if err != nil {
		log.Fatalf("启动监听失败 (8971~8999 端口均不可用): %v", err)
	}
	actualPort = realPort

	mux := http.NewServeMux()
	registerRoutes(mux)

	// 静态面板
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 启动内置反代引擎
	_ = store.reloadCaddy()

	// 服务启动自恢复与首次自举引导
	go func() {
		time.Sleep(1 * time.Second)
		store.resumeActiveTunnels()
		autoBootstrapPanelProxy(actualPort)
	}()

	// 面板自身退出时,优雅释放内置反代引擎与所有运行中的隧道协程
	go func() {
		c := make(chan os.Signal, 1)
		notifyShutdown(c)
		<-c
		log.Println("面板退出,清理内置引擎与隧道协程...")
		stopAllManaged()
		_ = srv.Close()
	}()

	// 状态巡检
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[inspection] 巡检协程异常恢复: %v", r)
			}
		}()
		for {
			time.Sleep(5 * time.Second)
			store.syncStatus()
		}
	}()

	log.Printf("Cunnel 三合一服务已启动 (True Single Process): http://%s", listenAddr)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatalf("服务退出: %v", err)
	}
}

// listenWithFallback 尝试在首选端口监听，若被占用则自动向后顺延
func listenWithFallback(host string, startPort int) (net.Listener, string, int, error) {
	for port := startPort; port < startPort+50; port++ {
		addr := fmt.Sprintf("%s:%d", host, port)
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			if port != startPort {
				log.Printf("[PortGuard] 守护端口 %d 被占用，已自动避让切换至空闲端口 %d", startPort, port)
			}
			return ln, addr, port, nil
		}
	}
	return nil, "", 0, fmt.Errorf("无法找到空闲端口")
}

// autoBootstrapPanelProxy 检查并自动创建一条临时隧道直连控制面板自身实际端口
func autoBootstrapPanelProxy(port int) {
	store.mu.Lock()
	needInit := len(store.Tunnels) == 0 && len(store.Proxies) == 0
	store.mu.Unlock()

	if !needInit {
		return
	}

	log.Printf("[Bootstrap] 检测到全新安装，正在自动申请 Cloudflare 临时隧道并直连本地面板端口 :%d ...", port)
	tun, err := store.createTunnel(CreateTunnelReq{
		Port: port,
	})
	if err != nil {
		log.Printf("[Bootstrap] 自动申请临时隧道失败: %v", err)
		return
	}

	// 等待分配临时隧道域名 (最多等待 60 秒)
	for i := 0; i < 120; i++ {
		time.Sleep(500 * time.Millisecond)
		store.mu.Lock()
		t := store.FindTunnel(tun.ID)
		hasDomain := t != nil && t.Domain != ""
		store.mu.Unlock()
		if hasDomain {
			break
		}
	}

	store.mu.Lock()
	finalTun := store.FindTunnel(tun.ID)
	domain := ""
	if finalTun != nil {
		domain = finalTun.Domain
	}
	store.mu.Unlock()

	if domain != "" {
		log.Printf("[Bootstrap] 面板临时安全隧道初始化成功! 外网访问入口: https://%s (直连本地 %d 端口)", domain, port)
	} else {
		log.Printf("[Bootstrap] 临时安全隧道正在后台建立，待 Cloudflare 边缘下发域名后将自动生效")
	}
}

func stopAllManaged() {
	globalProxyEngine.Stop()
	procMu.Lock()
	for _, p := range procs {
		p.stop()
	}
	procs = make(map[string]*proc)
	procMu.Unlock()
}

func registerRoutes(mux *http.ServeMux) {
	// 概览
	mux.HandleFunc("/api/overview", wrap(func(w http.ResponseWriter, r *http.Request) (any, error) {
		store.syncStatus()
		panelTunnelURL := ""
		store.mu.Lock()
		for _, t := range store.Tunnels {
			if t.Port == actualPort && t.Domain != "" && t.Status == "running" {
				panelTunnelURL = "https://" + t.Domain
				break
			}
		}
		if panelTunnelURL == "" {
			for _, p := range store.Proxies {
				if p.UpstreamPort == actualPort || p.Path == "/" {
					if t := store.FindTunnel(p.TunnelID); t != nil && t.Domain != "" {
						panelTunnelURL = "https://" + t.Domain
						break
					}
				}
			}
		}
		store.mu.Unlock()

		return map[string]any{
			"caddy":            detectCaddy(),
			"cloudflared":      detectCloudflared(),
			"tunnel_count":     len(store.Tunnels),
			"running_count":    countRunning(),
			"proxy_count":      len(store.Proxies),
			"panel_dir":        baseDir,
			"panel_port":       actualPort,
			"panel_tunnel_url": panelTunnelURL,
		}, nil
	}))

	// 功能一:添加隧道
	mux.HandleFunc("/api/tunnels", wrap(func(w http.ResponseWriter, r *http.Request) (any, error) {
		switch r.Method {
		case http.MethodGet:
			store.syncStatus()
			return map[string]any{"tunnels": store.Tunnels}, nil
		case http.MethodPost:
			var req CreateTunnelReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				return nil, fmt.Errorf("请求格式错误: %w", err)
			}
			t, err := store.createTunnel(req)
			if err != nil {
				if t != nil {
					return map[string]any{"tunnel": t, "error": err.Error()}, nil
				}
				return nil, err
			}
			return map[string]any{"tunnel": t}, nil
		default:
			return nil, fmt.Errorf("不支持的方法")
		}
	}))

	mux.HandleFunc("/api/tunnels/", wrap(func(w http.ResponseWriter, r *http.Request) (any, error) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/tunnels/")
		parts := strings.Split(rest, "/")
		id := parts[0]
		action := ""
		if len(parts) > 1 {
			action = parts[1]
		}
		switch {
		case action == "stop" && r.Method == http.MethodPost:
			return map[string]any{"ok": true}, store.stopTunnel(id)
		case action == "start" && r.Method == http.MethodPost:
			t := store.FindTunnel(id)
			if t == nil {
				return nil, fmt.Errorf("隧道不存在")
			}
			return map[string]any{"ok": true}, store.startTunnel(t)
		case action == "logs":
			t := store.FindTunnel(id)
			if t == nil {
				return nil, fmt.Errorf("隧道不存在")
			}
			var lines []string
			if p := getProc(t.Name); p != nil {
				lines = p.logs()
			} else if b, err := os.ReadFile(t.LogFile); err == nil {
				lines = strings.Split(string(b), "\n")
			}
			if len(lines) > 300 {
				lines = lines[len(lines)-300:]
			}
			return map[string]any{"logs": lines, "name": t.Name}, nil
		case action == "" && r.Method == http.MethodDelete:
			return map[string]any{"ok": true}, store.deleteTunnel(id)
		case action == "domain" && r.Method == http.MethodPost:
			// 重新抓取临时隧道域名
			t := store.FindTunnel(id)
			if t == nil {
				return nil, fmt.Errorf("隧道不存在")
			}
			if p := getProc(t.Name); p != nil {
				if u := waitQuickURL(t.LogFile, 0, 5*time.Second); u != "" {
					cleanDomain := strings.TrimPrefix(u, "https://")
					cleanDomain = strings.TrimPrefix(cleanDomain, "http://")
					cleanDomain = strings.TrimRight(cleanDomain, "/")
					store.mu.Lock()
					t.Domain = cleanDomain
					_ = store.saveLocked()
					store.mu.Unlock()
				}
			}
			return map[string]any{"domain": t.Domain}, nil
		}
		return nil, fmt.Errorf("未知操作")
	}))

	// 功能二:列出 VPS 进程
	mux.HandleFunc("/api/processes", wrap(func(w http.ResponseWriter, r *http.Request) (any, error) {
		ps, err := listProcesses()
		if err != nil {
			return nil, err
		}
		q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
		if q != "" {
			filtered := make([]PSProc, 0, len(ps))
			for _, p := range ps {
				if strings.Contains(strings.ToLower(p.Name), q) ||
					strconv.Itoa(p.PID) == q {
					filtered = append(filtered, p)
				}
			}
			ps = filtered
		}
		return map[string]any{"processes": ps}, nil
	}))

	// 功能三:显示当前 Caddy 代理的进程
	mux.HandleFunc("/api/caddy/proxies", wrap(func(w http.ResponseWriter, r *http.Request) (any, error) {
		store.syncStatus()
		type row struct {
			*Proxy
			TunnelName   string `json:"tunnel_name"`
			TunnelPort   int    `json:"tunnel_port"`
			TunnelStatus string `json:"tunnel_status"`
			Locked       bool   `json:"locked"`
		}

		cunnelProxyCount := 0
		for _, p := range store.Proxies {
			if p.UpstreamPort == actualPort {
				cunnelProxyCount++
			}
		}

		hasDirectPanelTunnel := false
		for _, tu := range store.Tunnels {
			if tu.Port == actualPort && tu.Status == "running" && tu.Domain != "" {
				hasDirectPanelTunnel = true
				break
			}
		}

		rows := make([]row, 0, len(store.Proxies))
		for _, p := range store.Proxies {
			t := store.FindTunnel(p.TunnelID)
			tn, ts, tp := "-", "-", 0
			if t != nil {
				tn, ts, tp = t.Name, t.Status, t.Port
			}
			isLocked := (!hasDirectPanelTunnel && p.UpstreamPort == actualPort && cunnelProxyCount <= 1)
			rows = append(rows, row{
				Proxy:        p,
				TunnelName:   tn,
				TunnelPort:   tp,
				TunnelStatus: ts,
				Locked:       isLocked,
			})
		}
		cp := detectCaddy()
		var upstreams []map[string]any
		for _, p := range store.Proxies {
			upstreams = append(upstreams, map[string]any{
				"domain": p.Domain, "path": p.Path, "port": p.UpstreamPort,
			})
		}
		return map[string]any{
			"proxies":   rows,
			"caddy":     cp,
			"upstreams": upstreams,
		}, nil
	}))

	// 功能四:添加代理
	mux.HandleFunc("/api/proxies", wrap(func(w http.ResponseWriter, r *http.Request) (any, error) {
		switch r.Method {
		case http.MethodGet:
			return map[string]any{"proxies": store.Proxies}, nil
		case http.MethodPost:
			var req CreateProxyReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				return nil, fmt.Errorf("请求格式错误: %w", err)
			}
			p, err := store.createProxy(req)
			return map[string]any{"proxy": p}, err
		default:
			return nil, fmt.Errorf("不支持的方法")
		}
	}))

	mux.HandleFunc("/api/proxies/", wrap(func(w http.ResponseWriter, r *http.Request) (any, error) {
		id := strings.TrimPrefix(r.URL.Path, "/api/proxies/")
		if r.Method == http.MethodDelete {
			return map[string]any{"ok": true}, store.deleteProxy(id)
		}
		return nil, fmt.Errorf("未知操作")
	}))

	// 功能五:Caddy 检测与安装
	mux.HandleFunc("/api/caddy", wrap(func(w http.ResponseWriter, r *http.Request) (any, error) {
		if r.Method == http.MethodPost {
			action := r.URL.Query().Get("action")
			switch action {
			case "install":
				if _, err := ensureCaddy(true); err != nil {
					return nil, err
				}
				info := detectCaddy()
				return map[string]any{"caddy": info, "message": "Caddy 安装完成"}, nil
			case "start":
				return map[string]any{"ok": true}, store.startCaddy()
			case "reload":
				return map[string]any{"ok": true}, store.reloadCaddy()
			case "stop":
				globalProxyEngine.Stop()
				return map[string]any{"ok": true}, nil
			}
		}
		return map[string]any{"caddy": detectCaddy()}, nil
	}))

	// Caddyfile 预览
	mux.HandleFunc("/api/caddyfile", wrap(func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]any{"content": store.buildCaddyfile()}, nil
	}))

	// 依赖状态
	mux.HandleFunc("/api/deps", wrap(func(w http.ResponseWriter, r *http.Request) (any, error) {
		if r.Method == http.MethodPost {
			action := r.URL.Query().Get("action")
			if action == "install-cloudflared" {
				if _, err := ensureCloudflared(); err != nil {
					return nil, err
				}
				return map[string]any{"cloudflared": detectCloudflared()}, nil
			}
		}
		return map[string]any{
			"caddy":       detectCaddy(),
			"cloudflared": detectCloudflared(),
		}, nil
	}))

	// 面板退出:回收所有受管子进程(代理用完即关,不留残留进程)
	mux.HandleFunc("/api/shutdown", wrap(func(w http.ResponseWriter, r *http.Request) (any, error) {
		go func() {
			time.Sleep(300 * time.Millisecond)
			stopAllManaged()
			os.Exit(0)
		}()
		return map[string]any{"message": "面板正在关闭,已回收全部受管子进程"}, nil
	}))
}

func countRunning() int {
	n := 0
	for _, t := range store.Tunnels {
		if t.Status == "running" {
			n++
		}
	}
	return n
}

type CloudflaredInfo struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version"`
	Path      string `json:"path"`
	Latest    string `json:"latest"`
}

func detectCloudflared() CloudflaredInfo {
	bin := cloudflaredBinPath()
	installed := false
	if _, err := os.Stat(bin); err == nil {
		installed = true
	}
	return CloudflaredInfo{
		Installed: installed,
		Version:   "Cunnel 内置隧道核心 (Tunnel Engine)",
		Path:      "cunnel (内置一体化组件)",
		Latest:    "v1.0.8",
	}
}



// wrap 统一 JSON 响应与错误处理
func wrap(fn func(http.ResponseWriter, *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		res, err := fn(w, r)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": false, "error": err.Error(),
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": res})
	}
}
