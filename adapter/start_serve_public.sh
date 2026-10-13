#!/bin/bash
# fastgpt-sync-adapter serve 模式 + cloudflared 公网隧道 一键启动（Mac）
# 流程：先起隧道拿到公网 URL → 以该 URL 作为 external-url 启动 serve
# 用法：cd 到 adapter 目录后执行  bash start_serve_public.sh

cd "$(dirname "$0")"

# ===== 配置区：按需修改 =====
ROOT_DIR="$(pwd)/../nas_demo"      # 要暴露的目录（NAS 挂载点或同步落盘目录）
TOKEN="dev-token-123"              # FastGPT 调用鉴权 token，建议改成随机长串
PORT=8080
CFLOG="/tmp/cloudflared_serve.log"
# ===========================

echo "=============================================="
echo " 1) 启动 cloudflared 公网隧道…"
echo "=============================================="
rm -f "$CFLOG"
./bin/cloudflared-mac tunnel --url "http://127.0.0.1:$PORT" --no-autoupdate > "$CFLOG" 2>&1 &
CF_PID=$!

PUBLIC_URL=""
for i in $(seq 1 30); do
  PUBLIC_URL=$(grep -oE 'https://[a-zA-Z0-9-]+\.trycloudflare\.com' "$CFLOG" 2>/dev/null | head -1)
  [ -n "$PUBLIC_URL" ] && break
  sleep 1
done

if [ -z "$PUBLIC_URL" ]; then
  echo "  ✗ 没拿到公网地址，查看日志: $CFLOG"
  kill $CF_PID 2>/dev/null
  exit 1
fi
echo "  ✓ 公网地址: $PUBLIC_URL"
echo ""

echo "=============================================="
echo " 2) 启动 serve（API 文件库 + 文件服务器）"
echo "    目录: $ROOT_DIR"
echo "=============================================="
./bin/adapter-mac serve \
  --addr "127.0.0.1:$PORT" \
  --root "$ROOT_DIR" \
  --token "$TOKEN" \
  --external-url "$PUBLIC_URL" &
APP_PID=$!
sleep 1

echo ""
echo "=============================================="
echo " ✅ 就绪。在 FastGPT 知识库 → API 文件库 填："
echo ""
echo "   baseURL       = $PUBLIC_URL"
echo "   authorization = $TOKEN"
echo "   basePath      = （留空）"
echo "=============================================="
echo ""
echo " 验证（另开终端）:"
echo "   curl -X POST $PUBLIC_URL/v1/file/list \\"
echo "     -H 'Authorization: Bearer $TOKEN' \\"
echo "     -H 'Content-Type: application/json' -d '{}'"
echo ""
echo " 按 Ctrl+C 停止服务和隧道"
echo ""

trap 'kill $APP_PID $CF_PID 2>/dev/null' INT TERM EXIT
wait
