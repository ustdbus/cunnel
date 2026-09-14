#!/usr/bin/env bash
# ==============================================================================
# Cunnel - Cloudflare 隧道 & Caddy 反向代理管理面板 一键卸载脚本
# Repository: https://github.com/ustdbus/cunnel
# ==============================================================================

set -e

GREEN="\033[32m"
YELLOW="\033[33m"
PLAIN="\033[0m"

INSTALL_DIR="/opt/cfd-panel"
SERVICE_FILE="/etc/systemd/system/cunnel.service"

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

if [ -d "$INSTALL_DIR" ]; then
    rm -rf "$INSTALL_DIR"
    echo -e "${GREEN}>>> 已删除安装目录 ${INSTALL_DIR}。${PLAIN}"
fi

echo -e "${GREEN}====================================================${PLAIN}"
echo -e "${GREEN}  Cunnel 已彻底从本机卸载清理完成！${PLAIN}"
echo -e "${GREEN}====================================================${PLAIN}"
