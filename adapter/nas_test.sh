#!/bin/bash
# 群晖 NAS 连通性 + 功能测试脚本（在你的 Mac 终端跑）
# 用法：bash nas_test.sh

NAS="https://nas.example.com:5001"
USER="FastGPT"
PASS="CHANGE_ME"
ROOT="/FastGPT-test"   # 实际共享文件夹名

echo "=============================================="
echo " 群晖 NAS 测试"
echo " 地址: $NAS"
echo "=============================================="

echo ""
echo "【1】网络连通性 + API 信息查询"
curl -s -k -m 10 "$NAS/webapi/query.cgi?api=SYNO.API.Info&version=1&method=query&query=SYNO.API.Auth,SYNO.FileStation" \
  | python3 -m json.tool 2>/dev/null || echo "  ✗ 连不上或返回非 JSON"
echo ""

echo "【2】登录（拿 sid）"
LOGIN_RESP=$(curl -s -k -m 15 "$NAS/webapi/auth.cgi?api=SYNO.API.Auth&version=6&method=login&account=$(python3 -c "import urllib.parse;print(urllib.parse.quote('$USER'))")&passwd=$(python3 -c "import urllib.parse;print(urllib.parse.quote('$PASS'))")&session=FileStation&format=sid")
echo "$LOGIN_RESP" | python3 -m json.tool 2>/dev/null || echo "  原始返回: $LOGIN_RESP"

SID=$(echo "$LOGIN_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin)['data']['sid'])" 2>/dev/null)
if [ -n "$SID" ]; then
  echo "  ✓ 登录成功，sid=$SID"
else
  echo "  ✗ 登录失败，后续步骤跳过"
  exit 1
fi
echo ""

echo "【3】创建测试目录 $ROOT/test_user"
curl -s -k -m 15 "$NAS/webapi/entry.cgi?api=SYNO.FileStation.CreateFolder&version=2&method=create&folder_path=$ROOT&name=test_user&_sid=$SID" \
  | python3 -m json.tool 2>/dev/null
echo ""

echo "【4】上传测试文件到 $ROOT/test_user/"
echo "这是 NAS 上传测试 - $(date)" > /tmp/nas_upload_test.txt
curl -s -k -m 20 -X POST "$NAS/webapi/entry.cgi?_sid=$SID" \
  -F "api=SYNO.FileStation.Upload" -F "version=2" -F "method=upload" \
  -F "path=$ROOT/test_user" -F "create_parents=true" -F "overwrite=true" \
  -F "file=@/tmp/nas_upload_test.txt" \
  | python3 -m json.tool 2>/dev/null
rm -f /tmp/nas_upload_test.txt
echo ""

echo "【5】列出目录确认文件已上传"
curl -s -k -m 15 "$NAS/webapi/entry.cgi?api=SYNO.FileStation.List&version=2&method=list&folder_path=$ROOT/test_user&_sid=$SID" \
  | python3 -m json.tool 2>/dev/null
echo ""

echo "【6】登出"
curl -s -k -m 10 "$NAS/webapi/auth.cgi?api=SYNO.API.Auth&version=6&method=logout&session=FileStation&_sid=$SID" > /dev/null
echo "  完成"
echo ""
echo "=============================================="
echo " 测试结束"
echo "=============================================="
