#!/bin/bash
# 一键启动：fastgpt-sync-adapter + cloudflared 公网隧道（Mac Apple Silicon）
# 用法：cd 到 adapter 目录后执行  bash start_public.sh

cd "$(dirname "$0")"

ADDR="127.0.0.1:8080"
ROOT="$(pwd)/../nas_demo"
TOKEN="dev-token-123"   # 改成你自己的；FastGPT 里 authorization 要填同一个
CFLOG="/tmp/cloudflared_public.log"

echo "=============================================="
echo " 1) 启动 adapter（监听 $ADDR）"
echo "    文件目录: $ROOT"
echo "    token   : $TOKEN"
echo "=============================================="
./bin/adapter-mac --addr "$ADDR" --root "$ROOT" --token "$TOKEN" &
ADAPTER_PID=$!
sleep 1

echo " 2) 启动 cloudflared 公网隧道…"
rm -f "$CFLOG"
./bin/cloudflared-mac tunnel --url "http://$ADDR" --no-autoupdate > "$CFLOG" 2>&1 &
CF_PID=$!

echo " 3) 等待公网地址生成…"
URL=""
for i in $(seq 1 30); do
  URL=$(grep -oE 'https://[a-zA-Z0-9-]+\.trycloudflare\.com' "$CFLOG" 2>/dev/null | head -1)
  [ -n "$URL" ] && break
  sleep 1
done

echo ""
echo "=============================================="
if [ -n "$URL" ]; then
  echo " ✅ 公网地址已生成"
  echo ""
  echo " 在 FastGPT 知识库 → API 文件库 中填："
  echo "   baseURL       = $URL"
  echo "   authorization = $TOKEN"
  echo "   basePath      = （留空）"
else
  echo " ⚠️  还没拿到公网地址，稍后手动查看："
  echo "     grep trycloudflare $CFLOG"
fi
echo "=============================================="
echo ""
echo " 验证命令（另开一个终端）："
echo "   curl $URL/healthz"
echo ""
echo " 按 Ctrl+C 停止 adapter 和隧道"
echo ""

# 捕获退出信号，两个进程一起停
trap 'kill $ADAPTER_PID $CF_PID 2>/dev/null' INT TERM EXIT
wait
