#!/usr/bin/env bash
# ==============================================================================
# Cunnel - Cloudflare 隧道 & Caddy 反向代理管理面板 一键安装与管理脚本
# Repository: https://github.com/ustdbus/cunnel
# ==============================================================================

set -e

GREEN="\033[32m"
YELLOW="\033[33m"
RED="\033[31m"
PLAIN="\033[0m"

INSTALL_DIR="/opt/cunnel"
LEGACY_DIR="/opt/cfd-panel"
SERVICE_FILE="/etc/systemd/system/cunnel.service"

# ---------- 卸载逻辑 ----------
do_uninstall() {
    echo -e "${YELLOW}>>> 正在停止并卸载 Cunnel 管理面板...${PLAIN}"
    
    if systemctl is-active --quiet cunnel 2>/dev/null; then
        systemctl stop cunnel || true
    fi
    if systemctl is-enabled --quiet cunnel 2>/dev/null; then
        systemctl disable cunnel || true
    fi

    if [ -f "$SERVICE_FILE" ]; then
        rm -f "$SERVICE_FILE"
        systemctl daemon-reload || true
        echo -e "${GREEN}>>> 已移除 systemd 服务配置。${PLAIN}"
    fi

    rm -rf "$INSTALL_DIR" "$LEGACY_DIR"
    echo -e "${GREEN}>>> 已删除安装目录 ${INSTALL_DIR}。${PLAIN}"

    echo -e "${GREEN}====================================================${PLAIN}"
    echo -e "${GREEN}  Cunnel 已彻底从本机卸载清理完成！${PLAIN}"
    echo -e "${GREEN}====================================================${PLAIN}"
    exit 0
}

# 检查是否传入了卸载或更新参数
if [ "$1" = "uninstall" ] || [ "$1" = "remove" ]; then
    do_uninstall
fi

if [ "$1" = "update" ] || [ "$1" = "upgrade" ]; then
    echo -e "${YELLOW}>>> 正在调用一键更新逻辑...${PLAIN}"
    curl -fsSL https://raw.githubusercontent.com/ustdbus/cunnel/main/update.sh | bash
    exit 0
fi

# ---------- 安装逻辑 ----------
echo -e "${GREEN}>>> 开始安装 Cunnel 管理面板 (True Single Process)...${PLAIN}"

# 1. 检查运行环境
ARCH=$(uname -m)
case "$ARCH" in
    x86_64|amd64)
        BIN_ARCH="linux-amd64"
        ;;
    aarch64|arm64)
        BIN_ARCH="linux-arm64"
        ;;
    *)
        echo -e "${RED}暂不支持该系统架构: $ARCH${PLAIN}"
        exit 1
        ;;
esac

# 兼容历史 /opt/cfd-panel 迁移
if [ -d "$LEGACY_DIR" ] && [ ! -L "$LEGACY_DIR" ] && [ ! -d "$INSTALL_DIR" ]; then
    echo -e "${YELLOW}>>> 迁移历史目录 ${LEGACY_DIR} -> ${INSTALL_DIR}...${PLAIN}"
    mv "$LEGACY_DIR" "$INSTALL_DIR"
fi

echo -e "${YELLOW}>>> 安装目录: ${INSTALL_DIR}${PLAIN}"
mkdir -p "${INSTALL_DIR}/bin" "${INSTALL_DIR}/logs" "${INSTALL_DIR}/data" "${INSTALL_DIR}/tmp"
# 建立向后兼容软链
ln -sfn "$INSTALL_DIR" "$LEGACY_DIR" 2>/dev/null || true

# 2. 下载或构建 cunnel
BIN_URL="https://github.com/ustdbus/cunnel/releases/latest/download/cunnel-${BIN_ARCH}"
FALLBACK_URL="https://github.com/ustdbus/cunnel/releases/latest/download/cfd-panel-${BIN_ARCH}"
echo -e "${YELLOW}>>> 下载最新单二进制发布包 (${BIN_ARCH})...${PLAIN}"
if curl -fsSL "$BIN_URL" -o "${INSTALL_DIR}/bin/cunnel" || curl -fsSL "$FALLBACK_URL" -o "${INSTALL_DIR}/bin/cunnel"; then
    chmod +x "${INSTALL_DIR}/bin/cunnel"
    ln -sfn "${INSTALL_DIR}/bin/cunnel" "${INSTALL_DIR}/bin/cfd-panel" 2>/dev/null || true
    echo -e "${GREEN}>>> 下载成功!${PLAIN}"
else
    echo -e "${YELLOW}>>> 未找到 Release 二进制，正在尝试从源码编译 (需要本地 Go 环境)...${PLAIN}"
    if command -v go >/dev/null 2>&1; then
        TMP_DIR=$(mktemp -d)
        git clone https://github.com/ustdbus/cunnel.git "$TMP_DIR"
        cd "$TMP_DIR/src"
        go build -ldflags="-s -w" -o "${INSTALL_DIR}/bin/cunnel" .
        chmod +x "${INSTALL_DIR}/bin/cunnel"
        ln -sfn "${INSTALL_DIR}/bin/cunnel" "${INSTALL_DIR}/bin/cfd-panel" 2>/dev/null || true
        rm -rf "$TMP_DIR"
        echo -e "${GREEN}>>> 源码编译成功!${PLAIN}"
    else
        echo -e "${RED}无法下载预编译二进制且未检测到 Go 编译器，安装失败。${PLAIN}"
        exit 1
    fi
fi

# 2.1 确保内部隧道引擎就绪
if [ ! -f "${INSTALL_DIR}/bin/cunnel-engine" ]; then
    if [ -f "${INSTALL_DIR}/bin/cfd-panel-worker" ]; then
        cp -f "${INSTALL_DIR}/bin/cfd-panel-worker" "${INSTALL_DIR}/bin/cunnel-engine"
    elif command -v cloudflared >/dev/null 2>&1; then
        cp -f "$(command -v cloudflared)" "${INSTALL_DIR}/bin/cunnel-engine"
    else
        echo -e "${YELLOW}>>> 预载 Cunnel 隧道核心引擎 (${BIN_ARCH})...${PLAIN}"
        CFD_URL="https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-${BIN_ARCH}"
        if curl -fsSL "$CFD_URL" -o "${INSTALL_DIR}/bin/cunnel-engine"; then
            chmod +x "${INSTALL_DIR}/bin/cunnel-engine"
            echo -e "${GREEN}>>> 隧道核心引擎就绪!${PLAIN}"
        fi
    fi
    chmod +x "${INSTALL_DIR}/bin/cunnel-engine" 2>/dev/null || true
    ln -sfn "${INSTALL_DIR}/bin/cunnel-engine" "${INSTALL_DIR}/bin/cfd-panel-worker" 2>/dev/null || true
fi

# 3. 创建 Systemd 服务守护 (安全仅监听 127.0.0.1)
echo -e "${YELLOW}>>> 配置 systemd 守护进程 (本地回环 127.0.0.1:8971)...${PLAIN}"
cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=Cunnel - Cloudflare Tunnel & Proxy Management Platform
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=${INSTALL_DIR}
Environment="CUNNEL_DIR=${INSTALL_DIR}"
Environment="CFD_PANEL_DIR=${INSTALL_DIR}"
Environment="CFD_PANEL_ADDR=127.0.0.1:8971"
ExecStart=${INSTALL_DIR}/bin/cunnel
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable cunnel
systemctl restart cunnel

# 4. 等待进程自举申请临时隧道
echo -e "${YELLOW}>>> 正在通过内置引擎自动申请 Cloudflare 临时安全隧道 (通常耗时 5~15 秒)...${PLAIN}"
PANEL_TUNNEL_URL=""
for i in {1..30}; do
    sleep 1
    if [ -f "${INSTALL_DIR}/data/state.json" ]; then
        MATCH=$(grep -oE '[a-z0-9][a-z0-9-]*\.trycloudflare\.com' "${INSTALL_DIR}/data/state.json" 2>/dev/null | tail -n 1 || true)
        if [ -n "$MATCH" ]; then
            PANEL_TUNNEL_URL="https://${MATCH}"
            break
        fi
    fi
done

# 尝试从监听中获取实际绑定的面板端口 (排除内置反代 80 和 2080)
REAL_PORT=$(ss -tlnp 2>/dev/null | grep -E 'users:\(\("(cunnel|cfd-panel)"' | grep -oE '127\.0\.0\.1:[0-9]+' | cut -d: -f2 | grep -vE '^(80|2080)$' | head -n 1 || echo "8971")

echo -e "${GREEN}====================================================${PLAIN}"
echo -e "${GREEN}  🎉 Cunnel 安装成功并已在后台安全启动！${PLAIN}"
echo -e "${GREEN}  安全特性: 仅监听 127.0.0.1，零公网明文端口暴露${PLAIN}"
if [ -n "$PANEL_TUNNEL_URL" ]; then
echo -e "${GREEN}  🚀 面板安全访问入口: ${PANEL_TUNNEL_URL}${PLAIN}"
else
echo -e "${YELLOW}  🚀 临时隧道正在建立，请稍后查看: systemctl status cunnel${PLAIN}"
fi
echo -e "${GREEN}  本地监听地址: http://127.0.0.1:${REAL_PORT} (冲突自动顺延避让)${PLAIN}"
echo -e "${GREEN}  一键更新命令: curl -fsSL https://raw.githubusercontent.com/ustdbus/cunnel/main/update.sh | bash${PLAIN}"
echo -e "${GREEN}  一键卸载命令: curl -fsSL https://raw.githubusercontent.com/ustdbus/cunnel/main/uninstall.sh | bash${PLAIN}"
echo -e "${GREEN}  服务管理命令: systemctl status cunnel | restart cunnel${PLAIN}"
echo -e "${GREEN}====================================================${PLAIN}"
