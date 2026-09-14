package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ---------- 隧道创建与真实云端调度 ----------

type CreateTunnelReq struct {
	Domain string `json:"domain"`
	Token  string `json:"token"`
	Port   int    `json:"port"`
}

func cloudflaredBinPath() string {
	p := filepath.Join(baseDir, "bin", "cloudflared")
	if runtime.GOOS == "windows" {
		p += ".exe"
	}
	if _, err := os.Stat(p); err == nil {
		return p
	}

	name := "cloudflared"
	if runtime.GOOS == "windows" {
		name = "cloudflared.exe"
	}
	if sysP, err := exec.LookPath(name); err == nil {
		return sysP
	}

	for _, candidate := range []string{"/usr/local/bin/cloudflared", "/usr/bin/cloudflared"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return p
}

func ensureCloudflared() (string, error) {
	bin := cloudflaredBinPath()
	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}

	dst := filepath.Join(baseDir, "bin", "cloudflared")
	if runtime.GOOS == "windows" {
		dst += ".exe"
	}
	_ = os.MkdirAll(filepath.Dir(dst), 0o755)

	arch := runtime.GOARCH
	goos := runtime.GOOS
	var downloadURL string
	if goos == "linux" {
		switch arch {
		case "amd64":
			downloadURL = "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64"
		case "arm64":
			downloadURL = "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-arm64"
		default:
			return "", fmt.Errorf("不支持的系统架构: %s/%s", goos, arch)
		}
	} else if goos == "windows" {
		downloadURL = "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-windows-amd64.exe"
	} else {
		return "", fmt.Errorf("不支持的操作系统: %s", goos)
	}

	log.Printf("[Cloudflared] 未检测到核心隧道程序，正在自动拉取官方发布包: %s", downloadURL)
	tmpFile := filepath.Join(baseDir, "tmp", "cloudflared.download")
	if err := downloadFile(downloadURL, tmpFile); err != nil {
		return "", fmt.Errorf("下载 cloudflared 失败: %w", err)
	}

	_ = os.Remove(dst)
	if err := os.Rename(tmpFile, dst); err != nil {
		if err2 := copyFile(tmpFile, dst); err2 != nil {
			return "", fmt.Errorf("安装 cloudflared 文件失败: %w", err2)
		}
		_ = os.Remove(tmpFile)
	}
	_ = os.Chmod(dst, 0o755)
	log.Printf("[Cloudflared] cloudflared 已就绪: %s", dst)
	return dst, nil
}

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
		PID:       0,
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

// startTunnel 拉起受管 cloudflared 伪装进程并连接 Cloudflare 边缘
func (s *Store) startTunnel(t *Tunnel) error {
	bin := cloudflaredBinPath()
	if _, err := os.Stat(bin); err != nil {
		var downloadErr error
		bin, downloadErr = ensureCloudflared()
		if downloadErr != nil {
			return fmt.Errorf("未找到且无法下载 cloudflared: %w", downloadErr)
		}
	}

	var args []string
	var env []string
	if t.Mode == "named" {
		args = []string{"tunnel", "--no-autoupdate", "run", "--token", t.Token}
	} else {
		// 临时隧道把公网流量转发给内置反向代理引擎 (s.IngressPort 默认 8080)
		args = []string{
			"tunnel", "--no-autoupdate", "--url",
			fmt.Sprintf("http://127.0.0.1:%d", s.IngressPort),
		}
		env = append(env, "TUNNEL_METRICS=127.0.0.1:0")
	}

	p, err := startProc(t.Name, bin, args, env, t.LogFile)
	if err != nil {
		return fmt.Errorf("启动 cloudflared 失败: %w", err)
	}
	if p.cmd != nil && p.cmd.Process != nil {
		t.PID = p.cmd.Process.Pid
	}
	t.Status = "running"
	t.Error = ""

	// 临时隧道: 异步从日志捕获 Cloudflare 官方分配的公网合法域名并回填
	if t.Mode == "quick" {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[quick-tunnel %s] 协程异常恢复: %v", t.ID, r)
				}
			}()
			url := waitQuickURL(t.LogFile, 60*time.Second)
			if url != "" {
				cleanDomain := strings.TrimPrefix(url, "https://")
				cleanDomain = strings.TrimPrefix(cleanDomain, "http://")
				cleanDomain = strings.TrimRight(cleanDomain, "/")

				s.mu.Lock()
				t.Domain = cleanDomain

				// 如果该隧道指定了目标端口，且尚未存在代理规则，自动绑定根路径 "/"
				hasProxy := false
				for _, p := range s.Proxies {
					if p.TunnelID == t.ID {
						hasProxy = true
						break
					}
				}
				if !hasProxy && t.Port > 0 {
					pr := &Proxy{
						ID:           RandID(),
						TunnelID:     t.ID,
						Domain:       cleanDomain,
						Path:         "/",
						UpstreamPort: t.Port,
						CreatedAt:    time.Now(),
					}
					s.Proxies = append(s.Proxies, pr)
				}
				_ = s.saveLocked()
				s.mu.Unlock()

				// 域名就绪后重载内置反代路由
				_ = s.reloadCaddy()
				log.Printf("[Tunnel-%s] 临时安全隧道就绪: https://%s -> 本地反代", t.Name, cleanDomain)
			} else {
				log.Printf("[Tunnel-%s] 等待 Cloudflare 官方分配域名超时，请检查网络或查看日志 %s", t.Name, t.LogFile)
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

// syncStatus 根据系统实际进程存活性同步状态
func (s *Store) syncStatus() {
	s.mu.Lock()
	defer s.mu.Unlock()

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
