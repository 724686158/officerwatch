#!/usr/bin/env python3
"""校验 internal/catalog/data/officers.json 与 ESI 是否一致。

不联网就别跑这个（CI 里没跑，因为会依赖外部服务）。它检查三件事：

  1. ESI 里 16 个官员船组的所有船型，是否都在 officers.json 里（漏了 = 新官员）
  2. officers.json 里有没有 ESI 已经没有的条目（删了 = 过期数据）
  3. 名字是否一致（CCP 改名时会暴露）

用法：python3 tools/verify-officer-meta.py [路径]
退出码：0 一致；1 有差异（差异会逐条打印）。
"""
import json
import sys
import urllib.request

UA = {"User-Agent": "officerwatch-verify/1.0 (+https://github.com/724686158/officerwatch)"}
GROUPS = [553, 559, 564, 569, 574, 1174,
          4795, 4796, 4797, 4798, 4799, 4800, 4801, 4802, 4803, 4804]


def get(url):
    return json.loads(urllib.request.urlopen(urllib.request.Request(url, headers=UA), timeout=25).read())


def main(path: str) -> int:
    doc = json.load(open(path, encoding="utf-8"))
    local = {o["typeId"]: o for o in doc["officers"]}

    remote = {}
    for gid in GROUPS:
        g = get(f"https://esi.evetech.net/latest/universe/groups/{gid}/?datasource=tranquility")
        for tid in g["types"]:
            t = get(f"https://esi.evetech.net/latest/universe/types/{tid}/?datasource=tranquility")
            remote[tid] = t["name"]

    problems = 0
    missing = sorted(set(remote) - set(local))
    extra = sorted(set(local) - set(remote))
    renamed = [(tid, local[tid]["name"], remote[tid]) for tid in sorted(set(local) & set(remote))
               if local[tid]["name"] != remote[tid]]

    for tid in missing:
        print(f"[缺] ESI 有 typeID {tid}（{remote[tid]}），officers.json 里没有")
        problems += 1
    for tid in extra:
        print(f"[多] officers.json 有 typeID {tid}（{local[tid]['name']}），ESI 已没有")
        problems += 1
    for tid, a, b in renamed:
        print(f"[改名] typeID {tid}: officers.json={a!r} ESI={b!r}")
        problems += 1

    print(f"ESI {len(remote)} 位 / 本地 {len(local)} 位；差异 {problems} 处")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1] if len(sys.argv) > 1 else "internal/catalog/data/officers.json"))
