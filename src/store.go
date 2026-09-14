package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Tunnel 一条 cloudflare 隧道 = 一个 cloudflared 进程
type Tunnel struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`   // 4位小写字母 + 4位小写数字
	Mode      string    `json:"mode"`   // named | quick
	Domain    string    `json:"domain"` // named: 用户固定域名; quick: 自动获取的临时域名
	Token     string    `json:"token"`
	Port      int       `json:"port"` // 用户指定的监听/回源端口
	PID       int       `json:"pid"`
	Status    string    `json:"status"` // running | stopped | error
	Error     string    `json:"error,omitempty"`
	LogFile   string    `json:"log_file"`
	CreatedAt time.Time `json:"created_at"`
}

// Proxy 一条 caddy 反代规则:域名 + 路径 -> 内网端口
type Proxy struct {
	ID           string    `json:"id"`
	TunnelID     string    `json:"tunnel_id"`
	Domain       string    `json:"domain"`
	Path         string    `json:"path"` // 默认 /
	UpstreamPort int       `json:"upstream_port"`
	CreatedAt    time.Time `json:"created_at"`
}

// Store 全局持久化状态
type Store struct {
	mu          sync.Mutex       `json:"-"`
	Path        string           `json:"-"`
	Tunnels     []*Tunnel        `json:"tunnels"`
	Proxies     []*Proxy         `json:"proxies"`
	IngressPort int              `json:"ingress_port"` // caddy 监听端口(cloudflared 回源到它)
	procs       map[string]*proc `json:"-"`
}

func NewStore(path string) (*Store, error) {
	s := &Store{
		Path:        path,
		Tunnels:     []*Tunnel{},
		Proxies:     []*Proxy{},
		IngressPort: 2080,
		procs:       map[string]*proc{},
	}
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		if err := json.Unmarshal(b, s); err != nil {
			return nil, fmt.Errorf("config %s 解析失败: %w", path, err)
		}
	}
	if s.IngressPort <= 0 {
		s.IngressPort = 2080
	}
	if s.procs == nil {
		s.procs = map[string]*proc{}
	}
	return s, nil
}

func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

// RandName 生成 4 位小写字母 + 4 位小写数字的进程名,如 "kxmz4821"
func RandName() string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	const digits = "0123456789"
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		n := time.Now().UnixNano()
		for i := range buf {
			buf[i] = byte(n >> (i * 3))
		}
	}
	out := make([]byte, 8)
	for i := 0; i < 4; i++ {
		out[i] = letters[int(buf[i])%len(letters)]
	}
	for i := 4; i < 8; i++ {
		out[i] = digits[int(buf[i])%len(digits)]
	}
	return string(out)
}

func RandID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func (s *Store) FindTunnel(id string) *Tunnel {
	for _, t := range s.Tunnels {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (s *Store) FindTunnelByName(name string) *Tunnel {
	for _, t := range s.Tunnels {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// UsedName 避免进程名撞车
func (s *Store) UsedName(name string) bool {
	return s.FindTunnelByName(name) != nil
}
