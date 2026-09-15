package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------- 内置反向代理引擎 (In-Process Caddy Engine) ----------
//
// 彻底取代外部 caddy 独立进程与 Caddyfile，
// 直接在当前 Go 进程内实现多域名 (Virtual Host) 与多路径前缀反向代理路由，
// 支持 WebSocket、HTTP/2 及内存原子零停机热重载。

type CaddyInfo struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version"`
	Path      string `json:"path"`
	Running   bool   `json:"running"`
	PID       int    `json:"pid"`
	Latest    string `json:"latest"`
}

type ProxyRoute struct {
	Path         string
	StripPrefix  string
	UpstreamPort int
	Proxy        *httputil.ReverseProxy
}

type InProcessProxyEngine struct {
	mu          sync.RWMutex
	routes      map[string][]*ProxyRoute // domain -> list of routes (sorted by path length desc)
	server      *http.Server
	healthSrv   *http.Server
	ingressPort int
	running     int32
}

var globalProxyEngine = &InProcessProxyEngine{
	routes: make(map[string][]*ProxyRoute),
}

func (e *InProcessProxyEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}

	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSpace(host))

	e.mu.RLock()
	routes, ok := e.routes[host]
	e.mu.RUnlock()

	if !ok || len(routes) == 0 {
		// 回退匹配：查找默认匹配
		e.mu.RLock()
		defaultRoutes := e.routes["*"]
		e.mu.RUnlock()
		if len(defaultRoutes) > 0 {
			routes = defaultRoutes
		} else {
			http.NotFound(w, r)
			return
		}
	}

	reqPath := r.URL.Path
	if reqPath == "" {
		reqPath = "/"
	}

	// 匹配最长前缀
	for _, route := range routes {
		if route.Path == "/" || strings.HasPrefix(reqPath, route.Path) {
			route.Proxy.ServeHTTP(w, r)
			return
		}
	}

	http.NotFound(w, r)
}

func newReverseProxy(port int, stripPath string, incomingHost string) *httputil.ReverseProxy {
	targetURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	// 启用即时冲刷，禁止反向代理对 SSE (Server-Sent Events) 和流式数据进行缓冲
	proxy.FlushInterval = -1
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		// 记录原始客户端访问的 Host 与 Proto
		if incomingHost != "" {
			req.Header.Set("X-Forwarded-Host", incomingHost)
		}
		if req.Header.Get("X-Forwarded-Proto") == "" {
			req.Header.Set("X-Forwarded-Proto", "https")
		}
		// 上游 Host 统一设置为本地目标 (127.0.0.1:port)，确保本地服务主机安全检查 (isIP/Loopback) 100% 通过
		req.Host = targetURL.Host
		// 剥离可能触发 Next.js/WebUI 等框架跨站误判的头部 (如 HTTPS->HTTP 转换时的 Origin 协议不一致、Sec-Fetch-Site 等)
		// 从而确保本地 Node 服务无条件信任由 cunnel 安全代理进入的请求
		req.Header.Del("Origin")
		req.Header.Del("Sec-Fetch-Site")
		req.Header.Del("Sec-Fetch-Mode")
		req.Header.Del("Sec-Fetch-Dest")
		if stripPath != "" && stripPath != "/" {
			if strings.HasPrefix(req.URL.Path, stripPath) {
				req.URL.Path = strings.TrimPrefix(req.URL.Path, stripPath)
				if !strings.HasPrefix(req.URL.Path, "/") {
					req.URL.Path = "/" + req.URL.Path
				}
			}
		}
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("[InProcess-Proxy] 转发至 127.0.0.1:%d 出错: %v", port, err)
		http.Error(w, fmt.Sprintf("Bad Gateway: 无法连接本地上游服务 127.0.0.1:%d", port), http.StatusBadGateway)
	}
	return proxy
}

// UpdateRoutes 内存中原子更新路由表
func (e *InProcessProxyEngine) UpdateRoutes(proxies []*Proxy) {
	newRoutes := make(map[string][]*ProxyRoute)

	for _, p := range proxies {
		d := strings.ToLower(strings.TrimSpace(p.Domain))
		if d == "" || p.UpstreamPort <= 0 {
			continue
		}
		path := strings.TrimSpace(p.Path)
		if path == "" {
			path = "/"
		}
		if path != "/" && !strings.HasPrefix(path, "/") {
			path = "/" + path
		}

		stripPath := ""
		if path != "/" {
			stripPath = path
		}

		incomingHost := ""
		if d != "*" {
			incomingHost = d
		}
		route := &ProxyRoute{
			Path:         path,
			StripPrefix:  stripPath,
			UpstreamPort: p.UpstreamPort,
			Proxy:        newReverseProxy(p.UpstreamPort, stripPath, incomingHost),
		}
		newRoutes[d] = append(newRoutes[d], route)
	}

	// 每域名内路径按长度降序排序（保证最长前缀优先命中）
	for d := range newRoutes {
		list := newRoutes[d]
		sort.SliceStable(list, func(i, j int) bool {
			return len(list[i].Path) > len(list[j].Path)
		})
	}

	e.mu.Lock()
	e.routes = newRoutes
	e.mu.Unlock()
}

// EnsureRunning 确保反代服务在后台监听，并支持端口冲突自动递增避让
func (e *InProcessProxyEngine) EnsureRunning(preferredPort int) (int, error) {
	if preferredPort <= 0 {
		preferredPort = 2080
	}

	if atomic.LoadInt32(&e.running) == 1 && e.ingressPort == preferredPort {
		return preferredPort, nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	// 如果已经在不同端口运行，先优雅关闭旧监听器
	if e.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = e.server.Shutdown(ctx)
		cancel()
		e.server = nil
	}

	var ln net.Listener
	var chosenPort int
	var err error

	for p := preferredPort; p < preferredPort+50; p++ {
		if p == actualPort {
			continue // 严格避开面板自身守护端口
		}
		addr := fmt.Sprintf("127.0.0.1:%d", p)
		ln, err = net.Listen("tcp", addr)
		if err == nil {
			chosenPort = p
			if p != preferredPort {
				log.Printf("[PortGuard] 反代入口端口 %d 被占用，已自动避让切换至空闲端口 %d", preferredPort, p)
			}
			break
		}
	}

	if ln == nil {
		return 0, fmt.Errorf("内置反代引擎在 %d~%d 范围内均无法找到空闲端口: %w", preferredPort, preferredPort+49, err)
	}

	actualAddr := fmt.Sprintf("127.0.0.1:%d", chosenPort)
	srv := &http.Server{
		Addr:              actualAddr,
		Handler:           e,
		ReadHeaderTimeout: 10 * time.Second,
	}

	e.server = srv
	e.ingressPort = chosenPort
	atomic.StoreInt32(&e.running, 1)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[ProxyEngine] 异常恢复: %v", r)
			}
		}()
		log.Printf("内置反代引擎已启动 (In-Process): http://%s", actualAddr)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[ProxyEngine] 监听异常退出: %v", err)
			atomic.StoreInt32(&e.running, 0)
		}
	}()

	return chosenPort, nil
}

func (e *InProcessProxyEngine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	atomic.StoreInt32(&e.running, 0)
	if e.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = e.server.Shutdown(ctx)
		cancel()
		e.server = nil
	}
	if e.healthSrv != nil {
		_ = e.healthSrv.Close()
		e.healthSrv = nil
	}
}

// ---------- 外部接口兼容（与现有 Store/UI 对接） ----------

func detectCaddy() CaddyInfo {
	isRunning := atomic.LoadInt32(&globalProxyEngine.running) == 1
	return CaddyInfo{
		Installed: true,
		Version:   "Embedded In-Process Engine (Caddy-Compatible)",
		Path:      "in-process",
		Running:   isRunning,
		PID:       os.Getpid(),
		Latest:    "v2.9.1",
	}
}

func (s *Store) reloadCaddy() error {
	globalProxyEngine.UpdateRoutes(s.Proxies)
	for _, p := range s.Proxies {
		log.Printf("[Proxy] 分流规则生效: https://%s%s -> 127.0.0.1:%d", p.Domain, p.Path, p.UpstreamPort)
	}
	realPort, err := globalProxyEngine.EnsureRunning(s.IngressPort)
	if err == nil && realPort != s.IngressPort {
		s.mu.Lock()
		s.IngressPort = realPort
		_ = s.saveLocked()
		s.mu.Unlock()
	}
	return err
}

func (s *Store) setIngressPort(newPort int) (int, error) {
	realPort, err := globalProxyEngine.EnsureRunning(newPort)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	s.IngressPort = realPort
	_ = s.saveLocked()
	var toRestart []*Tunnel
	for _, t := range s.Tunnels {
		if t.Status == "running" && t.Mode == "quick" {
			toRestart = append(toRestart, t)
		}
	}
	s.mu.Unlock()

	// 异步热重连活跃临时隧道以对接新端口
	go func() {
		for _, t := range toRestart {
			_ = s.stopTunnel(t.ID)
			time.Sleep(500 * time.Millisecond)
			_ = s.startTunnel(t)
		}
	}()
	return realPort, nil
}

func ensureCaddy(force bool) (string, error) {
	// 内置引擎零下载零依赖，直接返回就绪
	return "embedded", nil
}

func caddyBinPath() string {
	return "embedded"
}

func (s *Store) startCaddy() error {
	return s.reloadCaddy()
}

func (s *Store) buildCaddyfile() string {
	var b strings.Builder
	b.WriteString("# Cunnel In-Process Reverse Proxy Engine (Caddy-Compatible)\n")
	b.WriteString(fmt.Sprintf("# Global Ingress: 127.0.0.1:%d\n\n", s.IngressPort))
	for _, p := range s.Proxies {
		b.WriteString(fmt.Sprintf("http://%s%s -> 127.0.0.1:%d\n", p.Domain, p.Path, p.UpstreamPort))
	}
	if len(s.Proxies) == 0 {
		b.WriteString("# 当前无已配置的反代规则\n")
	}
	return b.String()
}
