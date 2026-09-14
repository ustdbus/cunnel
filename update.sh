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

INSTALL_DIR="/opt/cunnel"
LEGACY_DIR="/opt/cfd-panel"
SERVICE_NAME="cunnel"
SERVICE_FILE="/etc/systemd/system/cunnel.service"

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

# 兼容历史 /opt/cfd-panel 迁移
if [ -d "$LEGACY_DIR" ] && [ ! -L "$LEGACY_DIR" ] && [ ! -d "$INSTALL_DIR" ]; then
    echo -e "${YELLOW}>>> 迁移历史目录 ${LEGACY_DIR} -> ${INSTALL_DIR}...${PLAIN}"
    mv "$LEGACY_DIR" "$INSTALL_DIR"
fi
ln -sfn "$INSTALL_DIR" "$LEGACY_DIR" 2>/dev/null || true

if [ ! -d "$INSTALL_DIR" ]; then
    echo -e "${YELLOW}>>> 未检测到安装目录 ${INSTALL_DIR}，将执行首次安装...${PLAIN}"
    curl -fsSL https://raw.githubusercontent.com/ustdbus/cunnel/main/install.sh | bash
    exit 0
fi

# 2. 下载最新发布版二进制至临时文件
TMP_BIN="/tmp/cunnel-new.$$"
BIN_URL="https://github.com/ustdbus/cunnel/releases/latest/download/cunnel-${BIN_ARCH}"
FALLBACK_URL="https://github.com/ustdbus/cunnel/releases/latest/download/cfd-panel-${BIN_ARCH}"

echo -e "${YELLOW}>>> 正在获取最新发布版本 (${BIN_ARCH})...${PLAIN}"
if curl -fsSL "$BIN_URL" -o "$TMP_BIN" || curl -fsSL "$FALLBACK_URL" -o "$TMP_BIN"; then
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

mv -f "$TMP_BIN" "${INSTALL_DIR}/bin/cunnel"
chmod +x "${INSTALL_DIR}/bin/cunnel"
ln -sfn "${INSTALL_DIR}/bin/cunnel" "${INSTALL_DIR}/bin/cfd-panel" 2>/dev/null || true

# 确保内部隧道引擎 cunnel-engine 就绪
if [ ! -f "${INSTALL_DIR}/bin/cunnel-engine" ]; then
    if [ -f "${INSTALL_DIR}/bin/cfd-panel-worker" ]; then
        cp -f "${INSTALL_DIR}/bin/cfd-panel-worker" "${INSTALL_DIR}/bin/cunnel-engine"
    elif command -v cloudflared >/dev/null 2>&1; then
        cp -f "$(command -v cloudflared)" "${INSTALL_DIR}/bin/cunnel-engine"
    fi
fi
chmod +x "${INSTALL_DIR}/bin/cunnel-engine" 2>/dev/null || true
ln -sfn "${INSTALL_DIR}/bin/cunnel-engine" "${INSTALL_DIR}/bin/cfd-panel-worker" 2>/dev/null || true

# 更新 systemd ExecStart 为 cunnel 并补充 CUNNEL_ADDR
if [ -f "$SERVICE_FILE" ]; then
    sed -i "s|ExecStart=.*|ExecStart=${INSTALL_DIR}/bin/cunnel|" "$SERVICE_FILE"
    sed -i "s|WorkingDirectory=.*|WorkingDirectory=${INSTALL_DIR}|" "$SERVICE_FILE"
    if ! grep -q 'CUNNEL_ADDR=' "$SERVICE_FILE"; then
        sed -i '/CFD_PANEL_ADDR=/i Environment="CUNNEL_ADDR=127.0.0.1:8971"' "$SERVICE_FILE"
    fi
    systemctl daemon-reload
fi

# 4. 重新启动服务
systemctl restart "$SERVICE_NAME"

# 5. 提示
SERVER_IP=$(curl -s4m 3 https://api.ipify.org || curl -s4m 3 https://ifconfig.me || echo "你的VPS公网IP")

TUNNEL_URL=""
for i in {1..20}; do
    sleep 1
    if [ -f "${INSTALL_DIR}/data/state.json" ]; then
        MATCH=$(grep -oE '[a-z0-9][a-z0-9-]*\.trycloudflare\.com' "${INSTALL_DIR}/data/state.json" 2>/dev/null | tail -n 1 || true)
        if [ -n "$MATCH" ]; then
            TUNNEL_URL="https://${MATCH}"
            if [ $i -ge 4 ]; then
                break
            fi
        fi
    fi
done

INGRESS_P=$(grep -oE '"ingress_port":\s*[0-9]+' "${INSTALL_DIR}/data/state.json" 2>/dev/null | grep -oE '[0-9]+' || echo "2080")
REAL_PORT=$(ss -tlnp 2>/dev/null | grep -E 'users:\(\("(cunnel|cfd-panel)"' | grep -oE '127\.0\.0\.1:[0-9]+' | cut -d: -f2 | grep -vE "^(${INGRESS_P}|80|2080)$" | head -n 1 || echo "8971")

echo -e "${GREEN}====================================================${PLAIN}"
echo -e "${GREEN}  🎉 Cunnel 已成功更新至最新版并平滑重启！${PLAIN}"
echo -e "${GREEN}  数据状态: 现有隧道、代理规则与配置文件已完好保留${PLAIN}"
if [ -n "$TUNNEL_URL" ]; then
echo -e "${GREEN}  🚀 面板安全访问入口: ${TUNNEL_URL}${PLAIN}"
fi
echo -e "${GREEN}  本地监听地址: http://127.0.0.1:${REAL_PORT} (仅本地安全回环，冲突自动避让)${PLAIN}"
echo -e "${GREEN}  服务管理命令: systemctl status cunnel | restart cunnel${PLAIN}"
echo -e "${GREEN}====================================================${PLAIN}"