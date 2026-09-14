package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	setProcSysAttr(cmd)
	// 让 ps 显示为统一进程名: 覆盖 argv[0] 为 cunnel
	cmd.Args = append([]string{"cunnel"}, args...)
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
	return isProcAlive(p)
}

func (p *proc) stop() {
	if p == nil {
		return
	}
	if p.cancel != nil {
		p.cancel()
	}
	killProcessGroup(p)
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

		// 1. 过滤 systemd 与内核孤立通知
		if pid == 1 {
			continue
		}

		// 2. 若是 Cunnel 自身主进程，明确标记并置顶呈现，进程名统一为 cunnel
		if pid == os.Getpid() {
			ports := listen[pid]
			mainAddr := fmt.Sprintf("127.0.0.1:%d", actualPort)
			hasMain := false
			for _, a := range ports {
				if strings.Contains(a, strconv.Itoa(actualPort)) {
					hasMain = true
					break
				}
			}
			if !hasMain {
				ports = append([]string{mainAddr}, ports...)
			}
			res = append(res, PSProc{
				PID:     pid,
				Name:    "cunnel",
				Listen:  ports,
				Managed: true,
			})
			continue
		}

		name := f[1]
		command := strings.Join(f[2:], " ")

		// 3. 过滤 Cunnel 内部隧道 worker 协程进程与指标端口
		procMu.Lock()
		isInternal := false
		for _, p := range procs {
			if p.cmd != nil && p.cmd.Process != nil && p.cmd.Process.Pid == pid {
				isInternal = true
				break
			}
		}
		procMu.Unlock()
		if isInternal {
			continue
		}

		// 4. 过滤其他 cunnel / cfd-panel 体系的内部工作进程
		if (strings.Contains(name, "cunnel") && pid != os.Getpid()) ||
			strings.Contains(command, "cunnel tunnel") ||
			strings.Contains(name, "cfd-panel") ||
			strings.Contains(command, "cfd-panel") {
			continue
		}

		// 只保留有 TCP 监听的真实服务，避免海量无监听内核线程干扰用户视线
		if len(listen[pid]) == 0 {
			continue
		}

		res = append(res, PSProc{
			PID: pid, Name: name, Listen: listen[pid],
			Managed: false,
		})
	}

	// 排序：Cunnel 自身置顶在第一位，其次其他业务监听进程
	sort.Slice(res, func(i, j int) bool {
		if res[i].PID == os.Getpid() {
			return true
		}
		if res[j].PID == os.Getpid() {
			return false
		}
		return res[i].PID < res[j].PID
	})
	return res, nil
}

// waitQuickURL 等待 cloudflared 临时隧道输出公网域名。
// 从指定的 startOffset 开始查找，始终取最新下发的那一条，防止读取到历史已废弃的旧域名。
func waitQuickURL(logPath string, startOffset int64, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f, err := os.Open(logPath); err == nil {
			if st, serr := f.Stat(); serr == nil && st.Size() > startOffset {
				buf := make([]byte, st.Size()-startOffset)
				if _, rerr := f.ReadAt(buf, startOffset); rerr == nil {
					matches := quickURL.FindAllString(string(buf), -1)
					if len(matches) > 0 {
						_ = f.Close()
						return matches[len(matches)-1]
					}
				}
			}
			_ = f.Close()
		}
		time.Sleep(400 * time.Millisecond)
	}
	return ""
}

func fmtLog(name string) string {
	return fmt.Sprintf("%s - %s", time.Now().Format("2006-01-02 15:04:05"), name)
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return checkProcessAlive(proc)
}

func downloadFile(url, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	cl := &http.Client{Timeout: 10 * time.Minute}
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

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
