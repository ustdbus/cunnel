#!/usr/bin/env bash
# ==============================================================================
# Cunnel - Cloudflare 隧道 & Caddy 反向代理管理面板 一键更新脚本
# Repository: https://github.com/ustdbus/cunnel
# ==============================================================================

set -e

GREEN="\033[32m"
YELLOW="\033[33m"
RED="\033[31m"
PLAIN="\033[0m"

INSTALL_DIR="/opt/cfd-panel"
SERVICE_NAME="cunnel"

echo -e "${GREEN}>>> 开始检查并更新 Cunnel 管理面板...${PLAIN}"

# 1. 检查运行环境与现有安装
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

if [ ! -d "$INSTALL_DIR" ]; then
    echo -e "${YELLOW}>>> 未检测到安装目录 ${INSTALL_DIR}，将执行首次安装...${PLAIN}"
    curl -fsSL https://raw.githubusercontent.com/ustdbus/cunnel/main/install.sh | bash
    exit 0
fi

# 2. 下载最新发布版二进制至临时文件
TMP_BIN="/tmp/cfd-panel-new.$$"
BIN_URL="https://github.com/ustdbus/cunnel/releases/latest/download/cfd-panel-${BIN_ARCH}"

echo -e "${YELLOW}>>> 正在获取最新发布版本 (${BIN_ARCH})...${PLAIN}"
if curl -fsSL "$BIN_URL" -o "$TMP_BIN"; then
    chmod +x "$TMP_BIN"
    echo -e "${GREEN}>>> 最新版本下载成功!${PLAIN}"
else
    echo -e "${YELLOW}>>> 未找到 Release 发布包，尝试通过源码拉取构建...${PLAIN}"
    if command -v go >/dev/null 2>&1; then
        TMP_DIR=$(mktemp -d)
        git clone --depth 1 https://github.com/ustdbus/cunnel.git "$TMP_DIR"
        cd "$TMP_DIR/src"
        go build -ldflags="-s -w" -o "$TMP_BIN" .
        chmod +x "$TMP_BIN"
        rm -rf "$TMP_DIR"
        echo -e "${GREEN}>>> 源码编译更新成功!${PLAIN}"
    else
        echo -e "${RED}无法下载预编译包且无 Go 编译器，更新中止。${PLAIN}"
        rm -f "$TMP_BIN"
        exit 1
    fi
fi

# 3. 停止当前服务并安全替换二进制（保留 data/state.json 及 logs/）
echo -e "${YELLOW}>>> 平滑重启服务并应用新版本...${PLAIN}"
if systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
    systemctl stop "$SERVICE_NAME" || true
fi

mv -f "$TMP_BIN" "${INSTALL_DIR}/bin/cfd-panel"
chmod +x "${INSTALL_DIR}/bin/cfd-panel"

# 4. 重新启动服务
systemctl restart "$SERVICE_NAME"

# 5. 提示
SERVER_IP=$(curl -s4m 3 https://api.ipify.org || curl -s4m 3 https://ifconfig.me || echo "你的VPS公网IP")

TUNNEL_URL=""
for i in {1..15}; do
    sleep 1
    if [ -f "${INSTALL_DIR}/data/state.json" ]; then
        MATCH=$(grep -oE '[a-z0-9][a-z0-9-]*\.trycloudflare\.com' "${INSTALL_DIR}/data/state.json" 2>/dev/null | head -n 1 || true)
        if [ -n "$MATCH" ]; then
            TUNNEL_URL="https://${MATCH}"
            break
        fi
    fi
done

# 尝试从监听中获取实际绑定的端口
REAL_PORT=$(ss -tlnp 2>/dev/null | grep cfd-panel | grep -oE '127\.0\.0\.1:[0-9]+' | cut -d: -f2 | head -n 1 || echo "8971")

echo -e "${GREEN}====================================================${PLAIN}"
echo -e "${GREEN}  🎉 Cunnel 已成功更新至最新版并平滑重启！${PLAIN}"
echo -e "${GREEN}  数据状态: 现有隧道、代理规则与配置文件已完好保留${PLAIN}"
if [ -n "$TUNNEL_URL" ]; then
echo -e "${GREEN}  🚀 面板安全访问入口: ${TUNNEL_URL}${PLAIN}"
fi
echo -e "${GREEN}  本地监听地址: http://127.0.0.1:${REAL_PORT} (仅本地安全回环，冲突自动避让)${PLAIN}"
echo -e "${GREEN}  服务管理命令: systemctl status cunnel | restart cunnel${PLAIN}"
echo -e "${GREEN}====================================================${PLAIN}"