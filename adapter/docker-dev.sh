#!/usr/bin/env bash
# 一键开发流程：编译 + 启动 adapter（全部在 Docker 里完成）
#
# 用法（在 Mac 终端，adapter 目录下）：
#   ./docker-dev.sh            # 默认：编译 + 启动（容器直连 NAS）
#   ./docker-dev.sh check      # 检测容器能否直连到 NAS
#   ./docker-dev.sh build      # 只编译
#   ./docker-dev.sh run        # 只启动（复用已编好的二进制）
#   ./docker-dev.sh stop       # 停容器
#
# 容器直连 NAS 的前提：Docker Desktop 能路由到 NAS 所在网段。
# 若 check 失败（容器连不上，但 Mac 能连），加 USE_RELAY=1 让脚本在宿主机起 TCP 中继：
#   USE_RELAY=1 ./docker-dev.sh
#
# 可覆盖的环境变量：
#   USE_RELAY=0|1       是否启用宿主机 NAS 中继（默认 0，直连）
#   RELAY_PORT=9501     中继监听端口（避开 9500，Encore 默认占用）
#   IMAGE=golang:1.25   编译/运行镜像
#   CONFIG=config.yaml  配置文件名
set -euo pipefail

cd "$(dirname "$0")"

USE_RELAY="${USE_RELAY:-0}"
CLEAR_PROXY="${CLEAR_PROXY:-0}"
RELAY_PORT="${RELAY_PORT:-9501}"
IMAGE="${IMAGE:-golang:1.25}"
NAME="${NAME:-adapter}"
CONFIG="${CONFIG:-config.yaml}"
TMPBASE="${TMPDIR:-/tmp}"
RELAY_SCRIPT="${TMPBASE}/nas-relay-${RELAY_PORT}.py"
RELAY_LOG="${TMPBASE}/nas-relay-${RELAY_PORT}.log"
RELAY_PID_FILE="${TMPBASE}/nas-relay-${RELAY_PORT}.pid"

log()  { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31mxx\033[0m %s\n' "$*" >&2; exit 1; }

# 从 config.yaml 里取 NAS 地址
nas_url() {
  local u
  u=$(grep -E '^[[:space:]]*url:[[:space:]]*"' "${CONFIG}" | head -1 \
      | sed -E 's/.*"(https?:\/\/[^"]+)".*/\1/')
  [ -n "${u}" ] || die "从 ${CONFIG} 里没读到 sinks.nas.url"
  printf '%s' "${u}"
}

nas_target() {
  local u
  u=$(nas_url)
  printf '%s' "${u#*://}"
}

# ---------------------------------------------------------------- 连通性检测
# 关键：必须走和 adapter 一样的路径（HTTPS 请求 + 代理环境变量），
# 只探 TCP 端口会漏掉代理问题——/dev/tcp 不读 HTTPS_PROXY，
# 而 Go 的 http.Client 会读，结果是 check 通过但实际请求 EOF。
do_check() {
  local url host
  url=$(nas_url)
  host=$(nas_target)
  log "检测容器 -> NAS (${host}) ..."

  # 1) 先看容器里有没有代理变量（Docker Desktop 配置了代理时会自动注入）
  local proxied
  proxied=$(docker run --rm "${IMAGE}" env 2>/dev/null | grep -iE '^(HTTPS?_PROXY|ALL_PROXY)=' || true)
  if [ -n "${proxied}" ]; then
    warn "容器内存在代理变量，NAS 是内网地址不应走代理："
    printf '     %s\n' "${proxied}"
  fi

  # 2) 真实 HTTPS 请求（带代理）
  local code
  code=$(docker run --rm "${IMAGE}" bash -c \
    "curl -sk --max-time 8 -o /dev/null -w '%{http_code}' '${url}/webapi/query.cgi?api=SYNO.API.Info&version=1&method=query'" 2>/dev/null || true)
  if [ "${code}" = "200" ]; then
    log "容器可直连 NAS（HTTP ${code}），无需中继"
    return 0
  fi
  warn "带代理的 HTTPS 请求失败（HTTP ${code:-无响应}）"

  # 3) 清空代理再试一次，定位是不是代理导致的
  local code2
  code2=$(docker run --rm "${IMAGE}" bash -c \
    "env -u HTTPS_PROXY -u HTTP_PROXY -u ALL_PROXY -u https_proxy -u http_proxy \
     curl -sk --max-time 8 -o /dev/null -w '%{http_code}' '${url}/webapi/query.cgi?api=SYNO.API.Info&version=1&method=query'" 2>/dev/null || true)
  if [ "${code2}" = "200" ]; then
    warn "清掉代理后可用（HTTP ${code2}）→ 运行 adapter 时也会强制清空代理变量"
    CLEAR_PROXY=1
    return 0
  fi

  warn "容器连不上 NAS（清代理后仍失败）。若 Mac 本机能连，加 USE_RELAY=1 重跑"
  return 1
}

# ---------------------------------------------------------------- 中继（可选）
relay_running() {
  lsof -nP -iTCP:"${RELAY_PORT}" -sTCP:LISTEN >/dev/null 2>&1
}

start_relay_bg() {
  if relay_running; then
    warn "端口 ${RELAY_PORT} 已有进程监听，复用现有中继"
    return 0
  fi
  local target host port
  target=$(nas_target)
  host="${target%:*}"
  port="${target##*:}"

  cat > "${RELAY_SCRIPT}" <<'PY'
import socket, threading, sys
LISTEN = ("0.0.0.0", int(sys.argv[1]))
HOST, PORT = sys.argv[2], int(sys.argv[3])
def pipe(a, b):
    try:
        while True:
            d = a.recv(65536)
            if not d:
                break
            b.sendall(d)
    except Exception:
        pass
    finally:
        for s in (a, b):
            try:
                s.shutdown(socket.SHUT_RDWR)
            except Exception:
                pass
            try:
                s.close()
            except Exception:
                pass
def handle(c):
    try:
        u = socket.create_connection((HOST, PORT), timeout=10)
    except Exception as e:
        print("connect target failed:", e, flush=True)
        c.close()
        return
    threading.Thread(target=pipe, args=(c, u), daemon=True).start()
    pipe(u, c)
srv = socket.socket()
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(LISTEN)
srv.listen(50)
print(f"relay ready {LISTEN[0]}:{LISTEN[1]} -> {HOST}:{PORT}", flush=True)
while True:
    c, _ = srv.accept()
    threading.Thread(target=handle, args=(c,), daemon=True).start()
PY

  python3 "${RELAY_SCRIPT}" "${RELAY_PORT}" "${host}" "${port}" >"${RELAY_LOG}" 2>&1 &
  echo $! > "${RELAY_PID_FILE}"
  sleep 1
  if ! relay_running; then
    cat "${RELAY_LOG}" >&2 || true
    die "中继启动失败"
  fi
  log "NAS 中继已启动: 0.0.0.0:${RELAY_PORT} -> ${target}"
}

stop_relay() {
  if [ -f "${RELAY_PID_FILE}" ]; then
    kill "$(cat "${RELAY_PID_FILE}")" 2>/dev/null || true
    rm -f "${RELAY_PID_FILE}"
    log "中继已停止"
  fi
}

# ---------------------------------------------------------------- 编译
do_build() {
  [ -f "${CONFIG}" ] || die "找不到 ${CONFIG}, 先 cp config.example.yaml config.yaml 并填凭证"
  [ -f lib/arm64/libWeWorkFinanceSdk_C.so ] || die "缺少 lib/arm64/libWeWorkFinanceSdk_C.so"
  log "在 ${IMAGE} 里编译 (cgo, linux/arm64) ..."
  docker run --rm \
    -v "${PWD}:/src" -w /src \
    -v adapter-gomod:/go/pkg/mod \
    -e CGO_ENABLED=1 \
    "${IMAGE}" \
    go build -o bin/adapter-linux-arm64 .
  log "编译完成: bin/adapter-linux-arm64"
}

# ---------------------------------------------------------------- 启动
do_run() {
  [ -x bin/adapter-linux-arm64 ] || die "还没有二进制, 先跑 ./docker-dev.sh build"
  docker rm -f "${NAME}" >/dev/null 2>&1 || true

  local nas
  if [ "${USE_RELAY}" = "1" ]; then
    nas="https://host.docker.internal:${RELAY_PORT}"
    log "启动容器 ${NAME} (经中继: host.docker.internal:${RELAY_PORT})"
  else
    nas=$(nas_url)
    log "启动容器 ${NAME} (直连 NAS: $(nas_target))"
  fi

  # 内网 NAS 不应走代理：Docker Desktop 配置过代理时会注入 HTTPS_PROXY，
  # Go 的 http.Client 会遵循它，导致请求发给代理后连接被掐断（表现为 EOF）。
  local -a envs
  envs=(-e "NAS_URL=${nas}" -e "LD_LIBRARY_PATH=/src/lib/arm64")
  if [ "${CLEAR_PROXY}" = "1" ]; then
    envs+=(-e "HTTPS_PROXY=" -e "HTTP_PROXY=" -e "ALL_PROXY=" -e "https_proxy=" -e "http_proxy=")
    log "已清空容器内代理变量（NAS 为内网地址）"
  fi

  log "Webhook: http://127.0.0.1:8089/oauth/feishu/start"
  echo

  docker run --rm -it --name "${NAME}" \
    -v "${PWD}:/src" -w /src \
    -p 8089:8089 \
    "${envs[@]}" \
    "${IMAGE}" \
    ./bin/adapter-linux-arm64 -config "${CONFIG}"
}

do_stop() {
  docker rm -f "${NAME}" >/dev/null 2>&1 && log "容器 ${NAME} 已停止" || true
  stop_relay
}

# ---------------------------------------------------------------- 入口
case "${1:-up}" in
  check) do_check ;;
  build) do_build ;;
  run)   do_run ;;
  stop)  do_stop ;;
  up)
    if [ "${USE_RELAY}" = "1" ]; then
      start_relay_bg
      trap 'stop_relay' EXIT INT TERM
    fi
    do_build
    do_run
    ;;
  *) die "未知子命令: $1 (可用: up / check / build / run / stop)" ;;
esac
