# -*- coding: utf-8 -*-
"""Caddy Tunnel Panel — PawApp backend.

Wraps the Go panel binary (``cfd-panel``):

* ensures the binary exists (prebuilt → local build with Go → error)
* launches it as a managed background process on a loopback port
* proxies the panel's REST API and static UI into QwenPaw so the
  Console page can embed it same-origin (no CORS / mixed-content pain)

The Go panel itself owns Caddy + cloudflared lifecycle, so QwenPaw keeps
a single managed reference and cleaning up the panel reaps every tunnel.
"""

from __future__ import annotations

import asyncio
import logging
import os
import shutil
import subprocess
import sys
from pathlib import Path
from typing import Any, Optional

import httpx
from fastapi import APIRouter, Depends, HTTPException, Request
from fastapi.responses import HTMLResponse, Response

from qwenpaw.pawapp import PawApp, get_ctx

logger = logging.getLogger(__name__)

APP_ID = "caddy-tunnel-panel"
PANEL_PORT = int(os.environ.get("CFD_PANEL_PORT", "8971"))
PANEL_ADDR = f"127.0.0.1:{PANEL_PORT}"
PANEL_BASE = f"http://{PANEL_ADDR}"

PLUGIN_DIR = Path(__file__).resolve().parent.parent
INSTALL_DIR = Path(os.environ.get("CFD_PANEL_DIR", "/opt/cfd-panel"))

# Panel process owned by this plugin instance.
_PANEL_PROC: Optional[subprocess.Popen] = None
_PANEL_LOCK = asyncio.Lock()


# ── binary discovery / build ─────────────────────────────────────────


def _binary_candidates() -> list[Path]:
    return [
        Path(os.environ["CFD_PANEL_BIN"]) if os.environ.get("CFD_PANEL_BIN") else None,
        PLUGIN_DIR / "bin" / "cfd-panel",
        INSTALL_DIR / "bin" / "cfd-panel",
    ]


def _find_binary() -> Optional[Path]:
    for cand in _binary_candidates():
        if cand and cand.is_file() and os.access(cand, os.X_OK):
            return cand
    return None


def _build_binary() -> Optional[Path]:
    """Build the Go panel from bundled source when Go is available."""
    src = PLUGIN_DIR / "src"
    go = shutil.which("go") or "/usr/local/go/bin/go"
    if not src.is_dir() or not Path(go).exists():
        return None
    out = INSTALL_DIR / "bin" / "cfd-panel"
    out.parent.mkdir(parents=True, exist_ok=True)
    try:
        subprocess.run(
            [go, "build", "-o", str(out), "."],
            cwd=str(src),
            check=True,
            capture_output=True,
            timeout=600,
        )
    except Exception:  # noqa: BLE001
        logger.exception("[caddy-tunnel-panel] Go build failed")
        return None
    return out if out.is_file() else None


async def _panel_healthy(timeout: float = 1.5) -> bool:
    try:
        async with httpx.AsyncClient(timeout=timeout) as cl:
            r = await cl.get(f"{PANEL_BASE}/api/overview")
            return r.status_code == 200
    except Exception:  # noqa: BLE001
        return False


async def ensure_panel() -> dict[str, Any]:
    """Make sure the Go panel is running. Idempotent."""
    global _PANEL_PROC
    async with _PANEL_LOCK:
        if await _panel_healthy():
            return {"running": True, "reused": True, "pid": _PANEL_PROC.pid if _PANEL_PROC else None}

        binary = _find_binary() or _build_binary()
        if binary is None:
            raise RuntimeError(
                "未找到 cfd-panel 可执行文件,且无法用 Go 构建。"
                "请设置 CFD_PANEL_BIN 或安装 Go 后重试。",
            )

        env = dict(os.environ)
        env["CFD_PANEL_DIR"] = str(INSTALL_DIR)
        env["CFD_PANEL_ADDR"] = PANEL_ADDR
        for d in ("bin", "logs", "data", "tmp"):
            (INSTALL_DIR / d).mkdir(parents=True, exist_ok=True)
        logf = open(INSTALL_DIR / "logs" / "panel-plugin.log", "ab", buffering=0)
        _PANEL_PROC = subprocess.Popen(  # noqa: S603
            [str(binary)],
            cwd=str(INSTALL_DIR),
            env=env,
            stdout=logf,
            stderr=logf,
            stdin=subprocess.DEVNULL,
            start_new_session=True,
        )
        for _ in range(40):
            await asyncio.sleep(0.25)
            if await _panel_healthy():
                logger.info("[caddy-tunnel-panel] panel started pid=%s", _PANEL_PROC.pid)
                return {"running": True, "reused": False, "pid": _PANEL_PROC.pid}
            if _PANEL_PROC.poll() is not None:
                raise RuntimeError("cfd-panel 启动后立即退出,请检查 logs/panel-plugin.log")
        raise RuntimeError("cfd-panel 启动超时")


def _stop_panel() -> None:
    global _PANEL_PROC
    if _PANEL_PROC and _PANEL_PROC.poll() is None:
        try:
            _PANEL_PROC.terminate()
            _PANEL_PROC.wait(timeout=8)
        except Exception:  # noqa: BLE001
            try:
                _PANEL_PROC.kill()
            except Exception:  # noqa: BLE001
                pass
    _PANEL_PROC = None


# ── HTTP proxy into the panel ────────────────────────────────────────

router = APIRouter()
_client: Optional[httpx.AsyncClient] = None


def _shared_client() -> httpx.AsyncClient:
    global _client
    if _client is None:
        _client = httpx.AsyncClient(timeout=120.0)
    return _client


@router.api_route(
    "/proxy/{path:path}",
    methods=["GET", "POST", "PUT", "DELETE", "PATCH"],
)
async def proxy_panel(path: str, request: Request, ctx=Depends(get_ctx)):
    """Forward any request to the Go panel, injecting the auth-free loopback hop."""
    await ensure_panel()
    url = f"{PANEL_BASE}/{path.lstrip('/')}"
    body = await request.body()
    try:
        resp = await _shared_client().request(
            request.method,
            url,
            params=dict(request.query_params),
            content=body,
            headers={"Content-Type": request.headers.get("content-type", "application/json")},
        )
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status_code=502, detail=f"面板不可达: {exc}") from exc
    return Response(
        content=resp.content,
        status_code=resp.status_code,
        media_type=resp.headers.get("content-type", "application/json"),
    )


@router.get("/status")
async def panel_status(ctx=Depends(get_ctx)):
    """Report panel health + dependency detection."""
    try:
        await ensure_panel()
    except Exception as exc:  # noqa: BLE001
        return {"running": False, "error": str(exc), "install_dir": str(INSTALL_DIR)}
    try:
        r = await _shared_client().get(f"{PANEL_BASE}/api/overview")
        return {
            "running": True,
            "install_dir": str(INSTALL_DIR),
            "panel": r.json().get("data", {}),
        }
    except Exception as exc:  # noqa: BLE001
        return {"running": False, "error": str(exc), "install_dir": str(INSTALL_DIR)}


@router.get("/ui", response_class=HTMLResponse)
async def panel_ui(request: Request, ctx=Depends(get_ctx)):
    """Serve the panel UI as a same-origin page (for embedding in an iframe).

    The panel's own JS calls relative ``/api/...`` paths; we rewrite those to
    the plugin proxy prefix and inject a fetch shim carrying the QwenPaw auth
    token, since the iframe body has no access to the host's token store.
    """
    await ensure_panel()
    try:
        r = await _shared_client().get(f"{PANEL_BASE}/")
        html = r.text
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status_code=502, detail=f"面板不可达: {exc}") from exc

    token = request.headers.get("authorization", "") or request.headers.get("Authorization", "")
    agent_id = request.headers.get("x-agent-id", "")
    prefix = f"/api/{APP_ID}/proxy"
    shim = (
        "<script>(function(){"
        f"var P={prefix!r};"
        f"var T={token!r};"
        f"var A={agent_id!r};"
        "var of=window.fetch;"
        "window.fetch=function(u,o){"
        "if(typeof u==='string'&&u.indexOf('/api/')===0){u=P+u;}"
        "o=o||{};var hs=Object.assign({},o.headers||{});"
        "if(T){hs['Authorization']=T;}if(A){hs['X-Agent-Id']=A;}"
        "o.headers=hs;return of(u,o);};"
        "})();</script>"
    )
    html = html.replace("<head>", "<head>\n" + shim, 1)
    return HTMLResponse(html)


# ── PawApp definition ────────────────────────────────────────────────

app = PawApp(name="Caddy Tunnel Panel", app_id=APP_ID)
app.include_router(router)


@app.on_launch
async def _launch():
    """Start the Go panel together with the PawApp."""
    try:
        info = await ensure_panel()
        logger.info("[caddy-tunnel-panel] launched: %s", info)
    except Exception:  # noqa: BLE001
        logger.exception("[caddy-tunnel-panel] failed to start Go panel")


@app.on_terminate
async def _terminate():
    """Stop the panel and ask it to reap Caddy / cloudflared children."""
    try:
        if await _panel_healthy():
            await _shared_client().post(f"{PANEL_BASE}/api/shutdown")
    except Exception:  # noqa: BLE001
        pass
    _stop_panel()
    if _client is not None:
        await _client.aclose()
    logger.info("[caddy-tunnel-panel] panel stopped")


plugin = app
