package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------- 隧道创建(功能一)----------
//
// 规则:
//   domain + token 都填  -> named 隧道(用户固定域名)
//   只填 port             -> quick 临时隧道,端口由用户定
//   全不填                -> quick 临时隧道,端口默认 8080

type CreateTunnelReq struct {
	Domain string `json:"domain"`
	Token  string `json:"token"`
	Port   int    `json:"port"`
}

// createTunnel 新建隧道:随机进程名 + 启动 cloudflared 监听
func (s *Store) createTunnel(req CreateTunnelReq) (*Tunnel, error) {
	dom := strings.TrimSpace(req.Domain)
	tok := strings.TrimSpace(req.Token)

	// 名称:4 小写字母 + 4 小写数字,避免撞车
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
			port = 8080 // 全不填:默认 8080
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

// startTunnel 拉起 cloudflared 进程。
// named: cloudflared tunnel --no-autoupdate run --token <token>
// quick: cloudflared tunnel --no-autoupdate --url http://127.0.0.1:<caddyIngress>
func (s *Store) startTunnel(t *Tunnel) error {
	bin := cloudflaredBinPath()
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("未找到 cloudflared: %s", bin)
	}
	var args []string
	var env []string
	if t.Mode == "named" {
		args = []string{"tunnel", "--no-autoupdate", "run", "--token", t.Token}
	} else {
		// 临时隧道把公网流量交给 caddy 入口接管(caddy 按域名+路径再分发)
		args = []string{
			"tunnel", "--no-autoupdate", "--url",
			fmt.Sprintf("http://127.0.0.1:%d", s.IngressPort),
		}
		if t.Domain != "" {
			// 已获取到的历史域名:不起来新进程,仅复用
			args = append(args, "--hostname", t.Domain)
		}
		env = append(env, "TUNNEL_METRICS=127.0.0.1:0")
	}
	p, err := startProc(t.Name, bin, args, env, t.LogFile)
	if err != nil {
		return fmt.Errorf("启动 cloudflared 失败: %w", err)
	}
	t.PID = p.cmd.Process.Pid
	t.Status = "running"
	t.Error = ""

	// 临时隧道:主动获取公网域名并回填(功能四要求临时域名也显示)
	if t.Mode == "quick" {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[quick-tunnel %s] 协程异常恢复: %v", t.ID, r)
				}
			}()
			url := waitQuickURL(t.LogFile, 60*time.Second)
			if url != "" {
				s.mu.Lock()
				t.Domain = strings.TrimPrefix(url, "https://")
				_ = s.saveLocked()
				s.mu.Unlock()
				// 域名就绪后刷新 caddy,把该域名的路径规则挂上
				_ = s.reloadCaddy()
			}
		}()
	}
	return nil
}

func (s *Store) stopTunnel(id string) error {
	s.mu.Lock()
	t := s.FindTunnel(id)
	s.mu.Unlock()
	if t == nil {
		return fmt.Errorf("隧道不存在")
	}
	if p := getProc(t.Name); p != nil {
		p.stop()
		dropProc(t.Name)
	}
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

// syncStatus 根据实际进程存活刷新状态
func (s *Store) syncStatus() {
	s.mu.Lock()
	for _, t := range s.Tunnels {
		alive := false
		if p := getProc(t.Name); p != nil && p.alive() {
			alive = true
		} else if t.PID > 0 && pidAlive(t.PID) {
			alive = true
		}
		if alive {
			t.Status = "running"
		} else if t.Status == "running" {
			t.Status = "stopped"
			t.PID = 0
		}
	}
	_ = s.saveLocked()
	s.mu.Unlock()
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}

// ---------- 代理规则(功能四)----------

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
	// 同一域名下路径不能相同
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
		return pr, fmt.Errorf("代理已保存,但 Caddy 重载失败: %w", err)
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
	return s.reloadCaddy()
}
