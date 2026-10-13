#!/bin/bash
# fastgpt-sync-adapter 定时上传 daemon 启动脚本（Mac Apple Silicon）
# 监控本地目录，定时把源文件上传到 FastGPT 知识库
# 用法：cd 到 adapter 目录后执行  bash start.sh

cd "$(dirname "$0")"

# ===== 配置区：按需修改 =====
ROOT_DIR="$(pwd)/../nas_demo/飞书/云文档"          # 监控的目录（飞书同步落盘目录）
FASTGPT_URL="https://aanshixbkhrk.sealoshzh.site"  # FastGPT 地址
API_KEY="CHANGE_ME"  # FastGPT API key
DATASET_ID="CHANGE_ME"             # 知识库 id
INTERVAL="10m"                                     # 上传间隔，如 10m / 1h
STATE_FILE="$(pwd)/sync_state.json"                # 增量状态文件
IMAGE_INDEX="true"                                 # 图片索引
# ===========================

echo "=============================================="
echo " fastgpt-sync-adapter（定时上传 daemon）"
echo " 监控目录   : $ROOT_DIR"
echo " FastGPT    : $FASTGPT_URL"
echo " 知识库 id  : $DATASET_ID"
echo " 上传间隔   : $INTERVAL"
echo "=============================================="
echo " 按 Ctrl+C 停止服务"
echo ""

exec ./bin/adapter-mac \
  --root "$ROOT_DIR" \
  --fastgpt-url "$FASTGPT_URL" \
  --api-key "$API_KEY" \
  --dataset-id "$DATASET_ID" \
  --interval "$INTERVAL" \
  --state-file "$STATE_FILE" \
  --image-index "$IMAGE_INDEX" \
  --sync-delete true \
  --run-on-start true
