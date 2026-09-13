package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// proc 一个被面板管理的子进程
type proc struct {
	name   string
	cmd    *exec.Cmd
	logF   *os.File
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	logBuf []string
}

var (
	procMu   sync.Mutex
	procs    = map[string]*proc{}
	quickURL = regexp.MustCompile(`https://[a-z0-9][a-z0-9-]*\.trycloudflare\.com`)
)

func (p *proc) appendLog(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logBuf = append(p.logBuf, line)
	if len(p.logBuf) > 2000 {
		p.logBuf = p.logBuf[len(p.logBuf)-1500:]
	}
}

func (p *proc) logs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.logBuf))
	copy(out, p.logBuf)
	return out
}

// startProc 启动一个受管子进程。name 同时作为 argv[0],让 ps 里显示为随机名。
func startProc(name string, bin string, args []string, env []string, logPath string) (*proc, error) {
	if err := os.MkdirAll(dirOf(logPath), 0o755); err != nil {
		return nil, err
	}
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// 让 ps 显示为随机进程名:覆盖 argv[0]
	cmd.Args = append([]string{name}, args...)
	if err := cmd.Start(); err != nil {
		_ = lf.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &proc{
		name:   name,
		cmd:    cmd,
		logF:   lf,
		ctx:    ctx,
		cancel: cancel,
	}
	procMu.Lock()
	procs[name] = p
	procMu.Unlock()
	go p.tailLog(logPath)
	return p, nil
}

// tailLog 实时读日志,用于抓取临时隧道域名与错误
func (p *proc) tailLog(path string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[%s tailLog] panic recovered: %v", p.name, r)
		}
	}()
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	// 从文件末尾前一点开始,略过历史
	if st, err := f.Stat(); err == nil && st.Size() > 64*1024 {
		_, _ = f.Seek(st.Size()-64*1024, 0)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		p.appendLog(sc.Text())
	}
	// 跟随新增内容
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
		for sc.Scan() {
			p.appendLog(sc.Text())
		}
		// 进程退出后结束
		if p.cmd.ProcessState != nil {
			return
		}
	}
}

func (p *proc) alive() bool {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return false
	}
	err := p.cmd.Process.Signal(syscall.Signal(0))
	return err == nil
}

func (p *proc) stop() {
	if p == nil {
		return
	}
	if p.cancel != nil {
		p.cancel()
	}
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	pgid := p.cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_, _ = p.cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	if p.logF != nil {
		_ = p.logF.Close()
	}
}

func getProc(name string) *proc {
	procMu.Lock()
	defer procMu.Unlock()
	return procs[name]
}

func dropProc(name string) {
	procMu.Lock()
	defer procMu.Unlock()
	delete(procs, name)
}

func dirOf(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "."
	}
	return p[:i]
}

// ---- 系统进程列表(功能二)----

type PSProc struct {
	PID    int      `json:"pid"`
	Name   string   `json:"name"`
	Listen []string `json:"listen"`
	Managed bool    `json:"managed"`
}

func listProcesses() ([]PSProc, error) {
	// 监听端口映射:pid -> 监听地址列表(lsof 一次拿全)
	listen := map[int][]string{}
	out, err := exec.Command("lsof", "-nP", "-iTCP", "-sTCP:LISTEN").Output()
	if err == nil {
		for i, ln := range strings.Split(string(out), "\n") {
			ln = strings.TrimSpace(ln)
			if i == 0 || ln == "" { // 首行为表头
				continue
			}
			f := strings.Fields(ln)
			if len(f) < 9 {
				continue
			}
			pid, perr := strconv.Atoi(f[1])
			if perr != nil {
				continue
			}
			listen[pid] = append(listen[pid], f[8])
		}
	}

	cmd := exec.Command("ps", "-eo", "pid,comm,args", "--no-headers")
	out2, err2 := cmd.Output()
	if err2 != nil {
		return nil, err2
	}
	res := make([]PSProc, 0, 32)
	for _, ln := range strings.Split(string(out2), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		f := strings.Fields(ln)
		if len(f) < 3 {
			continue
		}
		pid, perr := strconv.Atoi(f[0])
		if perr != nil {
			continue
		}
		name := f[1]
		command := strings.Join(f[2:], " ")
		managed := false
		procMu.Lock()
		if _, ok := procs[name]; ok {
			managed = true
		} else {
			// argv[0] 被改写为随机进程名,按命令行首字段兜底识别
			if fields := strings.Fields(command); len(fields) > 0 {
				if _, ok := procs[fields[0]]; ok {
					managed = true
				}
			}
		}
		procMu.Unlock()
		res = append(res, PSProc{
			PID: pid, Name: name, Listen: listen[pid],
			Managed: managed,
		})
	}
	// 有 TCP 监听的进程排最前,其次面板管理的,其余按 PID
	sort.Slice(res, func(i, j int) bool {
		li, lj := len(res[i].Listen) > 0, len(res[j].Listen) > 0
		if li != lj {
			return li
		}
		if res[i].Managed != res[j].Managed {
			return res[i].Managed
		}
		return res[i].PID < res[j].PID
	})
	return res, nil
}

// waitQuickURL 等待 cloudflared 临时隧道输出公网域名。
// 直接从日志文件读取,避免 tail 缓冲区带来的时序问题。
func waitQuickURL(logPath string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(logPath); err == nil {
			if m := quickURL.FindString(string(b)); m != "" {
				return m
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
	return ""
}

func fmtLog(name string) string {
	return fmt.Sprintf("%s - %s", time.Now().Format("2006-01-02 15:04:05"), name)
}
