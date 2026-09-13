package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------- Caddy 检测 / 安装(功能五)----------

type CaddyInfo struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version"`
	Path      string `json:"path"`
	Running   bool   `json:"running"`
	PID       int    `json:"pid"`
	Latest    string `json:"latest"`
}

func caddyBinPath() string { return filepath.Join(baseDir, "bin", "caddy") }

func cloudflaredBinPath() string { return filepath.Join(baseDir, "bin", "cloudflared") }

// detectCaddy 检测 caddy(功能五:检测本地是否存在 Caddy)
func detectCaddy() CaddyInfo {
	info := CaddyInfo{Path: caddyBinPath()}
	if _, err := os.Stat(info.Path); err == nil {
		info.Installed = true
		if out, err := exec.Command(info.Path, "version").Output(); err == nil {
			info.Version = strings.TrimSpace(string(out))
		}
	} else if p, err := exec.LookPath("caddy"); err == nil {
		info.Installed = true
		info.Path = p
		if out, err := exec.Command(p, "version").Output(); err == nil {
			info.Version = strings.TrimSpace(string(out))
		}
	}
	if p := getProc("caddy"); p != nil && p.alive() {
		info.Running = true
		info.PID = p.cmd.Process.Pid
	}
	if v, err := latestGithubTag("caddyserver/caddy"); err == nil {
		info.Latest = v
	}
	return info
}

// ensureCaddy 不存在则拉取最新稳定版(功能五)
func ensureCaddy(force bool) (string, error) {
	bin := caddyBinPath()
	if !force {
		if _, err := os.Stat(bin); err == nil {
			return bin, nil
		}
	}
	tag, err := latestGithubTag("caddyserver/caddy")
	if err != nil {
		return "", fmt.Errorf("获取 Caddy 最新版本失败: %w", err)
	}
	ver := strings.TrimPrefix(tag, "v")
	url := fmt.Sprintf(
		"https://github.com/caddyserver/caddy/releases/download/%s/caddy_%s_linux_amd64.tar.gz",
		tag, ver)
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		return "", err
	}
	tmp := filepath.Join(baseDir, "tmp", "caddy.tar.gz")
	if err := downloadFile(url, tmp); err != nil {
		return "", fmt.Errorf("下载 Caddy 失败: %w", err)
	}
	if out, err := runCmd("tar", "-xzf", tmp, "-C", filepath.Dir(bin), "caddy"); err != nil {
		return "", fmt.Errorf("解压 Caddy 失败: %v %s", err, out)
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		return "", err
	}
	return bin, nil
}

func latestGithubTag(repo string) (string, error) {
	req, _ := http.NewRequest("GET", "https://api.github.com/repos/"+repo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "cfd-panel")
	cl := &http.Client{Timeout: 20 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	return v.TagName, nil
}

func downloadFile(url, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	cl := &http.Client{Timeout: 15 * time.Minute}
	resp, err := cl.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func runCmd(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// ---------- Caddyfile 生成与热重载 ----------

func caddyfilePath() string { return filepath.Join(baseDir, "Caddyfile") }

// buildCaddyfile 把隧道/代理渲染成 Caddyfile。
// 每个域名一个 site block;同域名下的多条路径按最长前缀优先排序。
func (s *Store) buildCaddyfile() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	ingress := s.IngressPort
	var b strings.Builder
	b.WriteString("# 由 cfd-panel 自动生成,请勿手工修改\n")
	b.WriteString("{\n")
	b.WriteString("\tadmin 127.0.0.1:2019\n")
	fmt.Fprintf(&b, "\tdefault_bind 127.0.0.1\n")
	b.WriteString("}\n\n")
	// 健康检查入口
	fmt.Fprintf(&b, ":%d {\n", caddyHealthPort)
	b.WriteString("\trespond /health \"ok\" 200\n")
	b.WriteString("\trespond 404\n")
	b.WriteString("}\n\n")

	// 按域名分组
	byDomain := map[string][]*Proxy{}
	for _, p := range s.Proxies {
		d := strings.ToLower(strings.TrimSpace(p.Domain))
		if d == "" || p.UpstreamPort <= 0 {
			continue
		}
		byDomain[d] = append(byDomain[d], p)
	}
	domains := make([]string, 0, len(byDomain))
	for d := range byDomain {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	for _, d := range domains {
		list := byDomain[d]
		// 路径长的优先,避免 / 抢走 /api
		sort.SliceStable(list, func(i, j int) bool {
			return len(list[i].Path) > len(list[j].Path)
		})
		fmt.Fprintf(&b, "http://%s {\n", d)
		fmt.Fprintf(&b, "\tbind 127.0.0.1\n")
		for _, p := range list {
			path := strings.TrimSpace(p.Path)
			if path == "" {
				path = "/"
			}
			if path != "/" && !strings.HasPrefix(path, "/") {
				path = "/" + path
			}
			if path == "/" {
				fmt.Fprintf(&b, "\thandle {\n\t\treverse_proxy 127.0.0.1:%d\n\t}\n", p.UpstreamPort)
			} else {
				fmt.Fprintf(&b, "\thandle_path %s* {\n\t\treverse_proxy 127.0.0.1:%d\n\t}\n",
					path, p.UpstreamPort)
			}
		}
		b.WriteString("\trespond 404\n")
		b.WriteString("}\n\n")
	}
	_ = ingress
	return b.String()
}

const caddyHealthPort = 2080

// writeCaddyfile 落盘
func (s *Store) writeCaddyfile() error {
	return os.WriteFile(caddyfilePath(), []byte(s.buildCaddyfile()), 0o644)
}

// reloadCaddy 重载配置;若 caddy 未运行则启动它
func (s *Store) reloadCaddy() error {
	if err := s.writeCaddyfile(); err != nil {
		return err
	}
	if p := getProc("caddy"); p != nil && p.alive() {
		out, err := runCmd(caddyBinPath(), "reload", "--config", caddyfilePath(), "--adapter", "caddyfile")
		if err != nil {
			return fmt.Errorf("caddy reload 失败: %v %s", err, out)
		}
		return nil
	}
	return s.startCaddy()
}

// startCaddy 以受管进程方式启动 caddy
func (s *Store) startCaddy() error {
	bin := caddyBinPath()
	if _, err := os.Stat(bin); err != nil {
		var e error
		bin, e = ensureCaddy(false)
		if e != nil {
			return e
		}
	}
	if err := s.writeCaddyfile(); err != nil {
		return err
	}
	logPath := filepath.Join(baseDir, "logs", "caddy.log")
	p, err := startProc("caddy", bin,
		[]string{"run", "--config", caddyfilePath(), "--adapter", "caddyfile"},
		nil, logPath)
	if err != nil {
		return fmt.Errorf("启动 Caddy 失败: %w", err)
	}
	// 等端口就绪
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if !p.alive() {
			return fmt.Errorf("Caddy 启动后立即退出,请查看日志")
		}
		if portOpen(caddyHealthPort) {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return nil
}

func portOpen(port int) bool {
	cl := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := cl.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}
