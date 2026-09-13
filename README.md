# Cunnel (Caddy & Cloudflare Tunnel Panel)

<div align="center">

基于 **Caddy** 的 **Cloudflare 隧道与反向代理管理面板**。  
支持多隧道并发调度、Caddy 零停机热重载、VPS 进程巡检与自动化反向代理路由。

[![Build & Release](https://github.com/ustdbus/cunnel/actions/workflows/build.yml/badge.svg)](https://github.com/ustdbus/cunnel/actions/workflows/build.yml)
[![GitHub release](https://img.shields.io/github/v/release/ustdbus/cunnel?color=blue)](https://github.com/ustdbus/cunnel/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

</div>

---

## ⚡ 一键安装与部署 (Quick Start)

### 选项 A：Linux VPS 独立面板一键安装（推荐）

直接在服务器终端以 root 执行以下命令，自动检测系统架构、配置 systemd 服务守护并启动：

```bash
curl -fsSL https://raw.githubusercontent.com/ustdbus/cunnel/main/install.sh | bash
```

### 选项 B：作为 QwenPaw 插件安装

进入你的 QwenPaw 插件目录安装并生效：

```bash
cd /opt/qwenpaw/plugins
git clone https://github.com/ustdbus/cunnel.git caddy-tunnel-panel
# 重载或重启 QwenPaw 即可在应用列表中打开 Cunnel 控制台
```

---

## 🌟 核心特性

- 🌐 **Cloudflare 隧道多模式支持**：
  - **Quick 临时隧道**：无需任何域名与凭证，一键秒级获取公网 `*.trycloudflare.com` 临时域名。
  - **Named 固定隧道**：输入你的固定域名与 Tunnel Token，持久化托管自有域名。
- 🔀 **Caddy 零停机多隧道聚合分流**：
  - 单个 Caddy 实例接管多条隧道流量，根据 HTTP `Host` 域名与路径规则精准分发至对应后端端口。
  - 规则变动通过 `caddy reload` 实现**零停机毫秒级热重载**，已建立的长连接不中断。
- 🛡️ **高鲁棒性与并发隔离**：
  - 每条隧道与受管进程拥有独立的 `context.Context` 生命周期控制，支持动态增删，互不干扰。
  - 后台轮询、日志跟踪、域名回填协程均挂载 `recover()` 异常兜底防护，彻底杜绝单点崩溃波及主面板。
- 📊 **VPS 进程与端口看板**：
  - 自动巡检 VPS 上的活跃进程及 TCP 监听端口，一键选择目标端口建立反代。
- ⚙️ **环境自动检测与拉取**：
  - 启动时自动检查本地是否存在 Caddy 和 cloudflared，缺失时支持一键自动下载并配置。

---

## 📐 架构拓扑

```text
               ┌───────────────────────┐
               │    公网访客 (Internet) │
               └───────────┬───────────┘
                           │ HTTPS (QUIC/H2)
            ┌──────────────┴──────────────┐
            ▼                             ▼
   【Cloudflare 隧道 A】          【Cloudflare 隧道 B】
    (app.domain.com)              (*.trycloudflare.com)
            │                             │
            └──────────────┬──────────────┘
                           │ 携带原始 Host 头回源
                           ▼
                  【Caddy 反向代理网关】
               (虚拟主机动态分流 / 零停机热重载)
                           │
       ┌───────────────────┴───────────────────┐
       ▼                                       ▼
【本地服务 A (如 :8080)】               【本地服务 B (如 :3000)】
  /api/* 路径反代                         根路径 / 默认反代
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
