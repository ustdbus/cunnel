package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ANSI 颜色代码
const (
	colorReset  = "\033[0m"
	colorBold   = "\033[1m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
	colorRed    = "\033[31m"
	colorGray   = "\033[90m"
)

// handleCLIIfRequested 检查并执行 CLI 命令，若是 CLI 模式则返回 true
func handleCLIIfRequested() bool {
	// 1. 如果带了参数
	if len(os.Args) > 1 {
		cmd := strings.ToLower(os.Args[1])
		switch cmd {
		case "server", "run", "daemon":
			return false // 作为服务端守护进程运行
		case "status", "info":
			printConnectInfo()
			return true
		case "restart":
			cliRestart()
			return true
		case "start":
			cliStart()
			return true
		case "stop":
			cliStop()
			return true
		case "log", "logs":
			cliLog()
			return true
		case "-h", "--help", "help":
			printCLIHelp()
			return true
		case "-v", "--version", "version":
			fmt.Printf("Cunnel v1.2.2 (Caddy & Cloudflare Tunnel Single-Process Platform)\n")
			return true
		}
	}

	// 2. 如果没有任何参数 (终端直接输入 cunnel)
	// 判断是否为 systemd 守护进程调起
	if os.Getenv("INVOCATION_ID") != "" || os.Getenv("JOURNAL_STREAM") != "" || os.Getenv("CUNNEL_DAEMON") == "1" {
		return false // 由 systemd 自动调用，运行服务端
	}

	// 用户在终端直接敲 cunnel，默认展示当前连接信息与运行状态
	printConnectInfo()
	return true
}

func printCLIHelp() {
	fmt.Printf(`
%s%sCunnel 命令行使用说明%s
  %s在终端直接输入 cunnel 即可查看当前面板与所有隧道的连接信息%s

%s用法:%s
  cunnel [命令]

%s可用命令:%s
  %s(无参数)%s       查看当前连接信息、公网地址与运行状态
  %sstatus / info%s   查看详细运行状态与连接信息
  %srestart%s         重启 Cunnel 守护进程
  %sstop%s            停止 Cunnel 守护进程
  %sstart%s           启动 Cunnel 守护进程
  %slog%s             查看服务实时运行日志
  %sserver%s          在前台以服务端守护模式启动
  %sversion%s         查看当前版本号
  %shelp%s            查看此帮助信息
`, colorBold, colorCyan, colorReset,
		colorYellow, colorReset,
		colorBold, colorReset,
		colorBold, colorReset,
		colorGreen, colorReset,
		colorGreen, colorReset,
		colorGreen, colorReset,
		colorGreen, colorReset,
		colorGreen, colorReset,
		colorGreen, colorReset,
		colorGreen, colorReset,
		colorGreen, colorReset,
		colorGreen, colorReset)
}

func printConnectInfo() {
	fmt.Println()
	fmt.Printf("%s================================================================%s\n", colorCyan, colorReset)
	fmt.Printf("               %s%sCunnel 隧道与反向代理连接信息%s\n", colorBold, colorCyan, colorReset)
	fmt.Printf("%s================================================================%s\n", colorCyan, colorReset)

	// 探测运行状态
	port := findActivePort()
	overviewData, tunnelsData, proxiesData, isRunning := fetchLiveStatus(port)

	if isRunning && overviewData != nil {
		ov := overviewData["data"].(map[string]any)
		panelPort := int(ov["panel_port"].(float64))
		ingressPort := int(ov["ingress_port"].(float64))
		publicURL := ""
		if u, ok := ov["panel_tunnel_url"].(string); ok && u != "" {
			publicURL = u
		}

		// 运行状态
		fmt.Printf("  %s● 运行状态:%s  %s运行中 (Active: running)%s\n", colorBold, colorReset, colorGreen, colorReset)
		fmt.Printf("  %s🏠 本地面板:%s  %shttp://127.0.0.1:%d%s\n", colorBold, colorReset, colorYellow, panelPort, colorReset)
		fmt.Printf("  %s🔀 反代入口:%s  %shttp://127.0.0.1:%d%s %s(统一高位安全端口)%s\n", colorBold, colorReset, colorYellow, ingressPort, colorReset, colorGray, colorReset)

		// 公网入口
		if publicURL != "" {
			fmt.Printf("  %s🌐 公网入口:%s  %s%s%s%s %s(Cloudflare 临时安全隧道)%s\n",
				colorBold, colorReset, colorBold, colorGreen, publicURL, colorReset, colorGray, colorReset)
		} else {
			fmt.Printf("  %s🌐 公网入口:%s  %s正在等待 Cloudflare 分配安全域名...%s\n", colorBold, colorReset, colorYellow, colorReset)
		}

		// 隧道列表
		fmt.Printf("\n  %s【当前活跃隧道】%s\n", colorBold, colorReset)
		fmt.Printf("  %s--------------------------------------------------------------%s\n", colorGray, colorReset)
		tunnelsList := extractList(tunnelsData, "tunnels")
		if len(tunnelsList) == 0 {
			fmt.Printf("  %s(暂无活跃隧道)%s\n", colorGray, colorReset)
		} else {
			for _, tItem := range tunnelsList {
				t := tItem.(map[string]any)
				name := t["name"].(string)
				mode := t["mode"].(string)
				status := t["status"].(string)
				domain := ""
				if d, ok := t["domain"].(string); ok {
					domain = d
				}
				statusColor := colorGreen
				if status != "running" {
					statusColor = colorRed
				}
				fmt.Printf("  * %s%s%s [%s模式] -> %s%s%s\n", colorBold, name, colorReset, mode, statusColor, status, colorReset)
				if domain != "" {
					fmt.Printf("    域名: %shttps://%s%s\n", colorCyan, domain, colorReset)
				} else {
					fmt.Printf("    域名: %s正在申请分配最新安全公网域名 (3~8秒)...%s\n", colorYellow, colorReset)
				}
			}
		}

		// 代理规则
		fmt.Printf("\n  %s【反向代理分流规则】%s\n", colorBold, colorReset)
		fmt.Printf("  %s--------------------------------------------------------------%s\n", colorGray, colorReset)
		proxiesList := extractList(proxiesData, "proxies")
		if len(proxiesList) == 0 {
			fmt.Printf("  %s(暂无分流规则)%s\n", colorGray, colorReset)
		} else {
			for _, pItem := range proxiesList {
				p := pItem.(map[string]any)
				domain := p["domain"].(string)
				path := p["path"].(string)
				upPort := int(p["upstream_port"].(float64))
				desc := "业务服务"
				if upPort == panelPort {
					desc = "Cunnel 控制面板"
				}
				fmt.Printf("  * %shttps://%s%s%s  ==>  %s127.0.0.1:%d%s (%s)\n",
					colorCyan, domain, path, colorReset,
					colorYellow, upPort, colorReset, desc)
			}
		}
	} else {
		// 未能通过本地 API 读取，尝试从 state.json 读取配置
		fmt.Printf("  %s○ 运行状态:%s  %s未运行 (Stopped)%s\n", colorBold, colorReset, colorRed, colorReset)
		statePath := findStatePath()
		if statePath != "" {
			if data, err := os.ReadFile(statePath); err == nil {
				var st struct {
					IngressPort int `json:"ingress_port"`
					Tunnels     []struct {
						Name   string `json:"name"`
						Domain string `json:"domain"`
						Mode   string `json:"mode"`
					} `json:"tunnels"`
				}
				if json.Unmarshal(data, &st) == nil {
					if st.IngressPort > 0 {
						fmt.Printf("  %s🔀 配置入口:%s  http://127.0.0.1:%d\n", colorBold, colorReset, st.IngressPort)
					}
					if len(st.Tunnels) > 0 && st.Tunnels[0].Domain != "" {
						fmt.Printf("  %s🌐 历史入口:%s  https://%s\n", colorBold, colorReset, st.Tunnels[0].Domain)
					}
				}
			}
		}
		fmt.Printf("\n  %s提示:%s 可执行 %scunnel start%s 或 %ssystemctl start cunnel%s 启动服务\n",
			colorYellow, colorReset, colorGreen, colorReset, colorGreen, colorReset)
	}

	fmt.Printf("\n%s================================================================%s\n", colorCyan, colorReset)
	fmt.Printf("  %s常用操作:%s\n", colorBold, colorReset)
	fmt.Printf("    %scunnel%s            刷新查看此连接信息\n", colorGreen, colorReset)
	fmt.Printf("    %scunnel restart%s    重启服务\n", colorGreen, colorReset)
	fmt.Printf("    %scunnel log%s        查看实时运行日志\n", colorGreen, colorReset)
	fmt.Printf("%s================================================================%s\n\n", colorCyan, colorReset)
}

func extractList(data map[string]any, key string) []any {
	if data == nil {
		return nil
	}
	d, ok := data["data"].(map[string]any)
	if !ok {
		return nil
	}
	list, ok := d[key].([]any)
	if !ok {
		return nil
	}
	return list
}

func findActivePort() int {
	ports := []int{8971, 8972, 8973, 8974, 8975}
	client := &http.Client{Timeout: 300 * time.Millisecond}
	for _, p := range ports {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/overview", p))
		if err == nil && resp.StatusCode == 200 {
			_ = resp.Body.Close()
			return p
		}
	}
	return 8971
}

func fetchLiveStatus(port int) (overview map[string]any, tunnels map[string]any, proxies map[string]any, ok bool) {
	client := &http.Client{Timeout: 800 * time.Millisecond}
	get := func(endpoint string) map[string]any {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, endpoint))
		if err != nil || resp.StatusCode != 200 {
			return nil
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil
		}
		var res map[string]any
		if json.Unmarshal(b, &res) != nil {
			return nil
		}
		return res
	}

	overview = get("/api/overview")
	if overview == nil {
		return nil, nil, nil, false
	}
	tunnels = get("/api/tunnels")
	proxies = get("/api/proxies")
	return overview, tunnels, proxies, true
}

func findStatePath() string {
	paths := []string{
		"/opt/cunnel/data/state.json",
		"/opt/cfd-panel/data/state.json",
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func cliRestart() {
	fmt.Printf("%s[Cunnel]%s 正在平滑重启服务并连接 Cloudflare 边缘节点...\n", colorCyan, colorReset)
	cmd := exec.Command("systemctl", "restart", "cunnel")
	if err := cmd.Run(); err != nil {
		fmt.Printf("%s重启失败: %v (若非 systemd 环境请直接运行 cunnel server)%s\n", colorRed, err, colorReset)
		return
	}
	waitForDomainAndPrint()
}

func cliStart() {
	fmt.Printf("%s[Cunnel]%s 正在启动服务并连接 Cloudflare 边缘节点...\n", colorCyan, colorReset)
	cmd := exec.Command("systemctl", "start", "cunnel")
	if err := cmd.Run(); err != nil {
		fmt.Printf("%s启动失败: %v%s\n", colorRed, err, colorReset)
		return
	}
	waitForDomainAndPrint()
}

func waitForDomainAndPrint() {
	fmt.Printf("%s[Cunnel]%s 等待 Cloudflare 官方边缘分配最新安全公网地址...\n", colorGray, colorReset)
	for i := 0; i < 12; i++ {
		time.Sleep(1 * time.Second)
		port := findActivePort()
		overviewData, _, _, ok := fetchLiveStatus(port)
		if ok && overviewData != nil {
			if ov, ok := overviewData["data"].(map[string]any); ok {
				if u, ok := ov["panel_tunnel_url"].(string); ok && u != "" {
					break
				}
			}
		}
	}
	printConnectInfo()
}

func cliStop() {
	fmt.Printf("%s[Cunnel]%s 正在停止 Cunnel 服务...\n", colorYellow, colorReset)
	cmd := exec.Command("systemctl", "stop", "cunnel")
	if err := cmd.Run(); err != nil {
		fmt.Printf("%s停止失败: %v%s\n", colorRed, err, colorReset)
		return
	}
	fmt.Printf("%s[Cunnel]%s 服务已成功停止。\n", colorGreen, colorReset)
}

func cliLog() {
	// 优先使用 journalctl
	if _, err := exec.LookPath("journalctl"); err == nil {
		c := exec.Command("journalctl", "-u", "cunnel", "-f", "--no-pager")
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		c.Stdin = os.Stdin
		_ = c.Run()
		return
	}
	// 备选 tail 业务日志
	logFile := "/opt/cunnel/logs/cunnel.log"
	if _, err := os.Stat(logFile); err != nil {
		logFile = "/opt/cfd-panel/logs/cunnel.log"
	}
	if _, err := exec.LookPath("tail"); err == nil {
		c := exec.Command("tail", "-f", "-n", "50", logFile)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		_ = c.Run()
		return
	}
	fmt.Printf("未找到可用的日志查看器，请直接查看日志文件: %s\n", logFile)
}
