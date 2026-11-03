#!/usr/bin/env bash
# 生成 internal/locs/data/celestials.tsv.gz（星体名索引）
#
# 为什么要它：zkillboard 的 killmail 只给一个 locationID（死亡点最近的星体），
# ESI 的 /universe/names 解不了星体 ID，而"官员在哪条带"恰恰是抢官员的核心情报。
# 所以把 SDE 的 mapDenormalize 裁成四列打包进来，本地查表，零运行时依赖。
#
# 数据源：fuzzwork 的 SDE CSV 转储（约 80MB，只在重建时需要）。
# 产物：约 2.4MB（gzip），首次启动灌进 SQLite 的 celestials 表。
set -euo pipefail
cd "$(dirname "$0")/.."
SRC="${1:-/tmp/mapDenorm.csv}"
OUT=internal/locs/data/celestials.tsv.gz

if [[ ! -f "$SRC" ]]; then
  echo "下载 mapDenormalize.csv（约 80MB）..."
  curl -fsSL -o "$SRC" "https://www.fuzzwork.co.uk/dump/latest/csv/mapDenormalize.csv"
fi

python3 - "$SRC" "$OUT" <<'PY'
import csv, gzip, sys
src, out = sys.argv[1], sys.argv[2]
csv.field_size_limit(10**9)
keep = {'7','8','9','10','15','995'}  # 行星/卫星/小行星带/星门/空间站/异常
n = 0
with open(src, newline='', encoding='utf-8-sig') as f, gzip.open(out, 'wt', encoding='utf-8', compresslevel=9) as g:
    for row in csv.DictReader(f):
        if not row['solarSystemID'] or row['groupID'] not in keep:
            continue
        g.write("\t".join([row['itemID'], row['solarSystemID'], row['groupID'], row['itemName']]) + "\n")
        n += 1
print("写出", out, "行数", n)
PY
