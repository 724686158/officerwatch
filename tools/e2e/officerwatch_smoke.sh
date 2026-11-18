#!/bin/bash
# 端到端冒烟：真登录 officerwatch → 抓看板 → 查 /api/status。
#
# 用法：BASE=https://你的域名/officerwatch PASSWORD=看板密码 bash tools/e2e/officerwatch_smoke.sh
# 退出码 0 = 采集器在线且最近 15 分钟有抓取。
set -euo pipefail

BASE="${BASE:-http://127.0.0.1:8795/officerwatch}"
PASSWORD="${PASSWORD:?需要 PASSWORD（config.json 里的 auth.password）}"
JAR=$(mktemp)
trap 'rm -f "$JAR"' EXIT

code=$(curl -s -c "$JAR" -o /dev/null -w '%{http_code}' --max-time 20 "$BASE/login")
echo "  GET  /login  → $code"
code=$(curl -s -b "$JAR" -c "$JAR" -o /dev/null -w '%{http_code}' --max-time 20 \
  -d "password=$PASSWORD" "$BASE/login")
echo "  POST /login  → $code"
code=$(curl -s -b "$JAR" -o /dev/null -w '%{http_code}' --max-time 20 "$BASE/")
echo "  GET  /       → $code"

status=$(curl -s -b "$JAR" --max-time 20 "$BASE/api/status")
echo "  /api/status  → ${status:0:160}..."

python3 - "$status" <<'PY'
import json, sys, datetime
d = json.loads(sys.argv[1])
stats = d.get("stats", {})
last = stats.get("lastFetch", "")
age = None
if last:
    t = datetime.datetime.fromisoformat(last.replace("Z", "+00:00"))
    age = (datetime.datetime.now(datetime.timezone.utc) - t).total_seconds()
print(f"  已抓取 {stats.get('fetched')} 条｜匹配 {stats.get('matched')}｜最后抓取 {last}（{age/60:.0f} 分钟前）")
if age is None or age > 900:
    print("  FAIL：超过 15 分钟没有抓取")
    sys.exit(1)
if d.get("db", {}).get("killmailRows", 0) <= 0:
    print("  WARN：库里没有 killmail")
print("  PASS")
PY
