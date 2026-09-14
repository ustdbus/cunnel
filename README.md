# Cunnel (Caddy & Cloudflare Tunnel Panel)

<div align="center">

基于 **Caddy** 的 **Cloudflare 隧道与反向代理管理面板**。  
支持多隧道并发调度、Caddy 零停机热重载、VPS 进程巡检与自动化反向代理路由。

[![Build & Release](https://github.com/ustdbus/cunnel/actions/workflows/build.yml/badge.svg)](https://github.com/ustdbus/cunnel/actions/workflows/build.yml)
[![GitHub release](https://img.shields.io/github/v/release/ustdbus/cunnel?color=blue)](https://github.com/ustdbus/cunnel/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

</div>

---

## ⚡ 一键安装、更新与卸载 (Quick Start)

### 📥 1. Linux VPS 一键安装（推荐）

直接在服务器终端以 root 执行以下命令，自动检测系统架构、配置仅监听本地回环安全地址（`127.0.0.1:8971`），并**自动申请一条 Cloudflare 临时隧道代理控制面板自身**，安装完成后直接输出免端口、免开放公网防火墙的 HTTPS 安全访问链接：

```bash
curl -fsSL https://raw.githubusercontent.com/ustdbus/cunnel/main/install.sh | bash
```

### 🔄 2. 一键平滑更新（保留所有配置与隧道状态）

升级到最新版本只需一行命令，自动拉取最新单文件二进制平滑重启，现有数据与隧道状态完好无损：

```bash
curl -fsSL https://raw.githubusercontent.com/ustdbus/cunnel/main/update.sh | bash
# 或者通过安装脚本传入更新参数：
curl -fsSL https://raw.githubusercontent.com/ustdbus/cunnel/main/install.sh | bash -s -- update
```

### 🗑️ 3. 一键彻底卸载与清理

如需彻底停止并移除面板、清理 systemd 守护服务及安装文件，直接执行：

```bash
curl -fsSL https://raw.githubusercontent.com/ustdbus/cunnel/main/uninstall.sh | bash
# 或：
curl -fsSL https://raw.githubusercontent.com/ustdbus/cunnel/main/install.sh | bash -s -- uninstall
```

### 🔌 4. 作为 QwenPaw 插件安装

进入你的 QwenPaw 插件目录安装并生效：

```bash
cd /opt/qwenpaw/plugins
git clone https://github.com/ustdbus/cunnel.git caddy-tunnel-panel
# 重载或重启 QwenPaw 即可在应用列表中打开 Cunnel 控制台
```

---

## 🌟 核心特性（三合一 True Single Process）

- 🎯 **极致浑然一体（True Single Process）**：
  - **单文件、单 PID**：全系统仅有唯一一个主进程，彻底废弃任何外部 `cloudflared` 与 `caddy` 二进制；
  - **零子进程、零外部 Hash**：没有任何外部子进程拉起，规避任何 EDR/主机安全探针的镜像 Hash 规则匹配；
  - **生命周期完全同步**：单进程优雅退出时，所有内存中的隧道连接与代理监听瞬间随内存回收，彻底杜绝僵尸进程。
- 🔀 **进程内 Caddy 兼容级反向代理引擎**：
  - 内置原生高性能反代模块，支持 HTTP/1.1、HTTP/2、WebSocket 双向流式转发；
  - 多域名（Virtual Host）与多路径前缀智能路由匹配；
  - 路由规则变更**纯内存原子热更新**，零停机且无磁盘 I/O。
- 🌐 **进程内 Cloudflare 隧道调度**：
  - 每条隧道作为独立的内存 Goroutine 运行，拥有专属的 `context.Context` 生命周期控制；
  - 协程具备 `defer recover()` 防御性崩溃拦截，单条隧道异常不影响全局。
- 📊 **VPS 进程与端口看板**：
  - 自动巡检 VPS 上的活跃进程及 TCP 监听端口，一键选择目标端口建立反代。

---

## 📐 架构拓扑 (True Single Process)

```text
┌────────────────────────────────────────────────────────┐
│               Cunnel 主进程 (单 PID)                    │
│                                                        │
│  ┌──────────────────┐          ┌────────────────────┐  │
│  │ Web 控制面板与API │          │  进程内隧道协程调度 │  │
│  │ (127.0.0.1:8971) │          │  (In-Process Tun)  │  │
│  └────────┬─────────┘          └─────────┬──────────┘  │
│           │ 控制路由更新                 │ 流量桥接     │
│           ▼                              ▼             │
│  ┌──────────────────────────────────────────────────┐  │
│  │           进程内反向代理引擎 (ProxyEngine)         │  │
│  │         (内存级零停机热重载 / 支持 WebSocket)        │  │
│  └──────────────────────────┬───────────────────────┘  │
└─────────────────────────────┼──────────────────────────┘
                              │ 本地回源转发
         ┌────────────────────┴────────────────────┐
         ▼                                         ▼
   【本地服务 A (:8080)】                     【本地服务 B (:3000)】
     /api/* 路径匹配                           根路径 / 默认匹配
```

---

## 🛠️ 管理与日常运维

若采用独立 systemd 服务安装，可使用以下命令管理：

```bash
# 查看面板运行状态
systemctl status cunnel

# 重启面板
systemctl restart cunnel

# 查看运行日志
journalctl -u cunnel -f

# 查看各隧道运行日志
ls -l /opt/cfd-panel/logs/
```

---

## 🚀 自动构建与跨平台编译 (CI/CD)

本项目配置了完整的 GitHub Actions 工作流：
- 每次提交或发布标签时，自动跨平台矩阵编译为无 CGO 依赖的静态二进制包：
  - `linux-amd64` / `linux-arm64`
  - `darwin-amd64` / `darwin-arm64`
  - `windows-amd64`
- 产物可在 GitHub Actions Artifacts 或 GitHub Releases 页面直接下载。

---

## 📄 开源许可证

本项目采用 [MIT License](LICENSE) 许可证开源。
