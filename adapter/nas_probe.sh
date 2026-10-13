#!/bin/bash
# 探测 NAS 上有哪些共享文件夹，确定可用的落盘根路径
# 用法：bash nas_probe.sh

NAS="https://nas.example.com:5001"
USER="FastGPT"
PASS="CHANGE_ME"

echo "=== 登录 ==="
SID=$(curl -s -k -m 15 "$NAS/webapi/auth.cgi?api=SYNO.API.Auth&version=6&method=login&account=$(python3 -c "import urllib.parse;print(urllib.parse.quote('$USER'))")&passwd=$(python3 -c "import urllib.parse;print(urllib.parse.quote('$PASS'))")&session=FileStation&format=sid" | python3 -c "import sys,json;print(json.load(sys.stdin)['data']['sid'])" 2>/dev/null)

if [ -z "$SID" ]; then
  echo "登录失败"
  exit 1
fi
echo "登录成功"
echo ""

echo "=== 列出所有共享文件夹（list_share）==="
curl -s -k -m 15 "$NAS/webapi/entry.cgi?api=SYNO.FileStation.List&version=2&method=list_share&_sid=$SID" \
  | python3 -c "
import sys, json
d = json.load(sys.stdin)
if not d.get('success'):
    print('查询失败:', d)
else:
    shares = d.get('data', {}).get('shares', [])
    print(f'共 {len(shares)} 个共享文件夹：')
    for s in shares:
        path = s.get('path','')
        name = s.get('name','')
        print(f'  {name:20s}  path={path}')
"
echo ""

echo "=== 登出 ==="
curl -s -k -m 10 "$NAS/webapi/auth.cgi?api=SYNO.API.Auth&version=6&method=logout&session=FileStation&_sid=$SID" > /dev/null
echo "完成"
