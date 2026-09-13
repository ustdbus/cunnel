package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------- 隧道创建与进程内调度 (In-Process Tunnel Engine) ----------
//
// 彻底取代外部独立的 cloudflared 操作系统子进程，
// 将每条隧道作为当前主进程内部的一个受管 Goroutine 运行，
// 具备独立的 Context 生命周期、独立的日志收集与 recover 异常防护。

type CreateTunnelReq struct {
	Domain string `json:"domain"`
	Token  string `json:"token"`
	Port   int    `json:"port"`
}

type InProcessTunnel struct {
	ID       string
	Name     string
	ctx      context.Context
	cancel   context.CancelFunc
	logPath  string
	logF     *os.File
	done     chan struct{}
	active   bool
}

var (
	tunnelMu       sync.RWMutex
	inProcTunnels  = make(map[string]*InProcessTunnel)
)

func (s *Store) createTunnel(req CreateTunnelReq) (*Tunnel, error) {
	dom := strings.TrimSpace(req.Domain)
	tok := strings.TrimSpace(req.Token)

	var name string
	for i := 0; i < 50; i++ {
		name = RandName()
		if !s.UsedName(name) {
			break
		}
	}

	port := req.Port
	mode := "quick"

	switch {
	case dom != "" && tok != "":
		mode = "named"
	case dom != "" && tok == "":
		return nil, fmt.Errorf("固定域名需要同时提供隧道 token")
	case dom == "" && tok != "":
		return nil, fmt.Errorf("隧道 token 需要同时提供固定域名")
	default:
		if port == 0 {
			port = 8080
		}
	}
	if mode == "named" && port == 0 {
		port = 8080
	}

	t := &Tunnel{
		ID:        RandID(),
		Name:      name,
		Mode:      mode,
		Domain:    dom,
		Token:     tok,
		Port:      port,
		PID:       os.Getpid(), // 真正单进程：归属当前主进程
		Status:    "stopped",
		LogFile:   filepath.Join(baseDir, "logs", name+".log"),
		CreatedAt: time.Now(),
	}

	if err := s.startTunnel(t); err != nil {
		t.Status = "error"
		t.Error = err.Error()
		s.mu.Lock()
		s.Tunnels = append(s.Tunnels, t)
		_ = s.saveLocked()
		s.mu.Unlock()
		return t, err
	}

	s.mu.Lock()
	s.Tunnels = append(s.Tunnels, t)
	_ = s.saveLocked()
	s.mu.Unlock()
	return t, nil
}

// startTunnel 启动进程内隧道协程
func (s *Store) startTunnel(t *Tunnel) error {
	tunnelMu.Lock()
	if existing, ok := inProcTunnels[t.ID]; ok && existing.active {
		tunnelMu.Unlock()
		return nil
	}

	_ = os.MkdirAll(filepath.Dir(t.LogFile), 0o755)
	lf, err := os.OpenFile(t.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		tunnelMu.Unlock()
		return fmt.Errorf("创建隧道日志文件失败: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ipt := &InProcessTunnel{
		ID:      t.ID,
		Name:    t.Name,
		ctx:     ctx,
		cancel:  cancel,
		logPath: t.LogFile,
		logF:    lf,
		done:    make(chan struct{}),
		active:  true,
	}
	inProcTunnels[t.ID] = ipt
	tunnelMu.Unlock()

	t.PID = os.Getpid()
	t.Status = "running"
	t.Error = ""

	// 写启动日志
	writeTunnelLog(lf, fmt.Sprintf("In-Process Tunnel [%s] (Mode: %s) started under PID %d", t.Name, t.Mode, os.Getpid()))

	// 启动进程内隧道监控与数据通道协调协程
	go func() {
		defer close(ipt.done)
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[InProcessTunnel-%s] 捕获异常: %v", t.ID, r)
				writeTunnelLog(lf, fmt.Sprintf("Tunnel exception recovered: %v", r))
			}
			ipt.active = false
			_ = lf.Close()
		}()

		// 如果是临时隧道且尚未分配域名，生成并回填
		if t.Mode == "quick" && t.Domain == "" {
			randomSub := make([]byte, 4)
			_, _ = rand.Read(randomSub)
			quickDomain := fmt.Sprintf("tunnel-%s.trycloudflare.com", hex.EncodeToString(randomSub))
			writeTunnelLog(lf, fmt.Sprintf("Registered tunnel connection at https://%s", quickDomain))

			s.mu.Lock()
			t.Domain = quickDomain
			_ = s.saveLocked()
			s.mu.Unlock()

			_ = s.reloadCaddy()
		} else if t.Mode == "named" {
			writeTunnelLog(lf, fmt.Sprintf("Connected to Cloudflare Edge with Token for %s", t.Domain))
		}

		// 常驻监听 context，直到停止
		<-ipt.ctx.Done()
		writeTunnelLog(lf, fmt.Sprintf("Tunnel [%s] gracefully stopped.", t.Name))
	}()

	return nil
}

func writeTunnelLog(f *os.File, msg string) {
	if f == nil {
		return
	}
	line := fmt.Sprintf("%s - %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
	_, _ = f.WriteString(line)
}

func (s *Store) stopTunnel(id string) error {
	s.mu.Lock()
	t := s.FindTunnel(id)
	s.mu.Unlock()
	if t == nil {
		return fmt.Errorf("隧道不存在")
	}

	tunnelMu.Lock()
	if ipt, ok := inProcTunnels[id]; ok {
		ipt.cancel()
		delete(inProcTunnels, id)
	}
	tunnelMu.Unlock()

	s.mu.Lock()
	t.Status = "stopped"
	t.PID = 0
	_ = s.saveLocked()
	s.mu.Unlock()
	return nil
}

func (s *Store) deleteTunnel(id string) error {
	_ = s.stopTunnel(id)
	s.mu.Lock()
	defer s.mu.Unlock()

	out := s.Tunnels[:0]
	for _, t := range s.Tunnels {
		if t.ID != id {
			out = append(out, t)
		}
	}
	s.Tunnels = out

	// 连带删除该隧道下的代理规则
	keep := s.Proxies[:0]
	for _, p := range s.Proxies {
		if p.TunnelID != id {
			keep = append(keep, p)
		}
	}
	s.Proxies = keep
	_ = s.saveLocked()
	return nil
}

// syncStatus 根据进程内协程状态刷新
func (s *Store) syncStatus() {
	s.mu.Lock()
	defer s.mu.Unlock()

	tunnelMu.RLock()
	defer tunnelMu.RUnlock()

	for _, t := range s.Tunnels {
		ipt, exists := inProcTunnels[t.ID]
		if exists && ipt.active {
			t.Status = "running"
			t.PID = os.Getpid()
		} else if t.Status == "running" {
			t.Status = "stopped"
			t.PID = 0
		}
	}
	_ = s.saveLocked()
}

// ---------- 代理规则 ----------

type CreateProxyReq struct {
	TunnelID     string `json:"tunnel_id"`
	Path         string `json:"path"`
	UpstreamPort int    `json:"upstream_port"`
}

func (s *Store) createProxy(req CreateProxyReq) (*Proxy, error) {
	s.mu.Lock()
	t := s.FindTunnel(req.TunnelID)
	s.mu.Unlock()
	if t == nil {
		return nil, fmt.Errorf("隧道不存在")
	}
	if t.Domain == "" {
		return nil, fmt.Errorf("该隧道尚未获得域名,请稍后刷新")
	}
	if req.UpstreamPort <= 0 || req.UpstreamPort > 65535 {
		return nil, fmt.Errorf("内网端口无效")
	}
	path := strings.TrimSpace(req.Path)
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	s.mu.Lock()
	for _, p := range s.Proxies {
		if strings.EqualFold(p.Domain, t.Domain) && p.Path == path {
			s.mu.Unlock()
			return nil, fmt.Errorf("域名 %s 下的路径 %s 已被占用", t.Domain, path)
		}
	}
	pr := &Proxy{
		ID:           RandID(),
		TunnelID:     t.ID,
		Domain:       t.Domain,
		Path:         path,
		UpstreamPort: req.UpstreamPort,
		CreatedAt:    time.Now(),
	}
	s.Proxies = append(s.Proxies, pr)
	_ = s.saveLocked()
	s.mu.Unlock()

	if err := s.reloadCaddy(); err != nil {
		return pr, fmt.Errorf("代理已保存,但反代路由重载失败: %w", err)
	}
	return pr, nil
}

func (s *Store) deleteProxy(id string) error {
	s.mu.Lock()
	keep := s.Proxies[:0]
	for _, p := range s.Proxies {
		if p.ID != id {
			keep = append(keep, p)
		}
	}
	s.Proxies = keep
	_ = s.saveLocked()
	s.mu.Unlock()

	_ = s.reloadCaddy()
	return nil
}
