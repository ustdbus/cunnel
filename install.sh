#!/usr/bin/env bash
# ==============================================================================
# Cunnel - Cloudflare 隧道 & Caddy 反向代理管理面板 一键安装脚本
# Repository: https://github.com/ustdbus/cunnel
# ==============================================================================

set -e

GREEN="\033[32m"
YELLOW="\033[33m"
RED="\033[31m"
PLAIN="\033[0m"

echo -e "${GREEN}>>> 开始安装 Cunnel 管理面板...${PLAIN}"

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

INSTALL_DIR="/opt/cfd-panel"
echo -e "${YELLOW}>>> 安装目录: ${INSTALL_DIR}${PLAIN}"
mkdir -p "${INSTALL_DIR}/bin" "${INSTALL_DIR}/logs" "${INSTALL_DIR}/data" "${INSTALL_DIR}/tmp"

# 2. 下载或构建 cfd-panel
BIN_URL="https://github.com/ustdbus/cunnel/releases/latest/download/cfd-panel-${BIN_ARCH}"
echo -e "${YELLOW}>>> 下载可执行文件 (${BIN_ARCH})...${PLAIN}"
if curl -fsSL "$BIN_URL" -o "${INSTALL_DIR}/bin/cfd-panel"; then
    chmod +x "${INSTALL_DIR}/bin/cfd-panel"
    echo -e "${GREEN}>>> 下载成功!${PLAIN}"
else
    echo -e "${YELLOW}>>> 未找到 Release 二进制，正在尝试从源码编译 (需要本地 Go 环境)...${PLAIN}"
    if command -v go >/dev/null 2>&1; then
        TMP_DIR=$(mktemp -d)
        git clone https://github.com/ustdbus/cunnel.git "$TMP_DIR"
        cd "$TMP_DIR/src"
        go build -ldflags="-s -w" -o "${INSTALL_DIR}/bin/cfd-panel" .
        chmod +x "${INSTALL_DIR}/bin/cfd-panel"
        rm -rf "$TMP_DIR"
        echo -e "${GREEN}>>> 源码编译成功!${PLAIN}"
    else
        echo -e "${RED}无法下载预编译二进制且未检测到 Go 编译器，安装失败。${PLAIN}"
        exit 1
    fi
fi

# 3. 创建 Systemd 服务守护
echo -e "${YELLOW}>>> 配置 systemd 守护进程...${PLAIN}"
cat > /etc/systemd/system/cunnel.service <<EOF
[Unit]
Description=Cunnel - Caddy & Cloudflare Tunnel Management Panel
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=${INSTALL_DIR}
Environment="CFD_PANEL_DIR=${INSTALL_DIR}"
Environment="CFD_PANEL_ADDR=127.0.0.1:8971"
ExecStart=${INSTALL_DIR}/bin/cfd-panel
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable cunnel
systemctl restart cunnel

echo -e "${GREEN}====================================================${PLAIN}"
echo -e "${GREEN}  Cunnel 安装成功并已在后台启动！${PLAIN}"
echo -e "${GREEN}  监听地址: http://127.0.0.1:8971${PLAIN}"
echo -e "${GREEN}  管理命令: systemctl status cunnel | restart cunnel${PLAIN}"
echo -e "${GREEN}====================================================${PLAIN}"
