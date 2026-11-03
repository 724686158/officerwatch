#!/usr/bin/env python3
"""生成 internal/catalog/data/officers.json（官员中文译名 / 吨位 / NPC 角色 ID）。

数据来源：
  1. 船型、阵营、船组名 —— ESI（/universe/groups/{id}/、/universe/types/{id}/）
     （运行时由 `officerwatch -sync-catalog` 同步，本文件只补人工信息）
  2. NPC 角色 ID —— SDE 的 `npcCharacters.csv`（fuzzwork 转储），按**名字**匹配
  3. 中文译名 —— 本文件里的 ZH 表（音译，非官方译名）
  4. 吨位 —— 优先看船组名里的 Frigate/Cruiser 后缀，没有后缀就按船体质量推断

用法：
    curl -o /tmp/npcCharacters.csv https://www.fuzzwork.co.uk/dump/latest/csv/npcCharacters.csv
    python3 tools/build-officer-meta.py internal/catalog/data/officers.json
"""
import csv
import json
import sys
import urllib.request

UA = {"User-Agent": "officerwatch-build/1.0 (+https://github.com/724686158/officerwatch)"}

# 全部 16 个官员船组（与 internal/catalog/catalog.go 的 KnownGroups 一致）
GROUPS = [
    553, 559, 564, 569, 574, 1174,
    4795, 4796, 4797, 4798, 4799, 4800, 4801, 4802, 4803, 4804,
]

# 中文译名：音译自英文原名，非 CCP 官方译法（官方中文客户端未公开这层数据）
ZH = {
    # Angel Cartel
    "Gotan Kreiss": "戈坦·克赖斯",
    "Hakim Stormare": "哈基姆·斯托马雷",
    "Mizuro Cybon": "米祖罗·赛邦",
    "Tobias Kruzhor": "托比亚斯·克鲁佐尔",
    "Ramaku Basta": "拉马库·巴斯塔",
    "Sila Aajai": "西拉·阿贾伊",
    # Blood Raiders
    "Ahremen Arkah": "阿赫雷门·阿尔卡",
    "Draclira Merlonne": "德拉克利拉·梅隆",
    "Raysere Giant": "雷塞尔·吉安特",
    "Tairei Namazoth": "泰雷·纳马佐斯",
    "Makra Ozman": "马克拉·奥兹曼",
    "Ryhad Sethrim": "里哈德·塞斯里姆",
    # Guristas
    "Estamel Tharchon": "埃斯塔梅尔·萨松",
    "Kaikka Peunato": "凯卡·佩纳托",
    "Thon Eney": "托恩·埃尼",
    "Vepas Minimala": "维帕斯·米尼马拉",
    "Panola Paatama": "帕诺拉·帕塔马",
    "Hanaruwa Oittenen": "哈纳鲁瓦·奥伊特宁",
    # Rogue Drone（无人机没有"角色"，只有单位编号）
    "Unit D-34343": "单元 D-34343",
    "Unit F-435454": "单元 F-435454",
    "Unit P-343554": "单元 P-343554",
    "Unit W-634": "单元 W-634",
    # Sansha's Nation
    "Brokara Ryver": "布罗卡拉·里弗",
    "Chelm Soran": "凯尔姆·索兰",
    "Nija Zufa": "尼贾·祖法",
    "Selynne Mardakar": "塞琳·马尔达卡",
    "Usaras Koirola": "乌萨拉斯·科伊罗拉",
    "Vizan Ankonin": "维赞·安科宁",
    # Serpentis
    "Asine Hitama": "阿辛·希塔玛",
    "Brynn Jerdola": "布琳·杰尔多拉",
    "Cormack Vaaja": "科马克·瓦亚",
    "Gara Minsk": "加拉·明斯克",
    "Setele Schellan": "塞泰勒·谢兰",
    "Tuvan Orth": "图万·奥尔思",
}

FACTION_ZH = {
    "Angel Cartel": "天使",
    "Blood Raiders": "血袭者",
    "Guristas": "古斯塔斯",
    "Rogue Drone": "流浪无人机",
    "Sansha's Nation": "萨沙",
    "Serpentis": "天蛇",
}

# SDE 里名字与船型名不一致的个案（人工确认过是同一个 NPC）
NAME_ALIASES = {
    "Usaras Koirola": "Ursaras Koirola",
}

CLASS_ZH = {"battleship": "战列", "cruiser": "巡洋", "frigate": "护卫"}


def get(url):
    return json.loads(urllib.request.urlopen(urllib.request.Request(url, headers=UA), timeout=25).read())


def faction_of(group_name: str) -> str:
    s = group_name.replace("Asteroid ", "")
    i = s.find(" Officer")
    return s[:i].strip() if i > 0 else s.strip()


def hull_class(group_name: str, mass: float) -> str:
    g = group_name.lower()
    if "frigate" in g:
        return "frigate"
    if "cruiser" in g:
        return "cruiser"
    if mass < 3e6:
        return "frigate"
    if mass < 1.4e7:
        return "cruiser"
    return "battleship"


def main(out_path: str) -> None:
    chars = {}
    for row in csv.DictReader(open("/tmp/npcCharacters.csv", encoding="utf-8-sig")):
        chars[row["characterName"].lower()] = int(row["characterID"])

    officers = []
    for gid in GROUPS:
        g = get(f"https://esi.evetech.net/latest/universe/groups/{gid}/?datasource=tranquility")
        faction = faction_of(g["name"])
        for tid in g["types"]:
            t = get(f"https://esi.evetech.net/latest/universe/types/{tid}/?datasource=tranquility")
            name = t["name"]
            lookup = NAME_ALIASES.get(name, name).lower()
            cid = chars.get(lookup, 0)
            officers.append({
                "typeId": tid,
                "name": name,
                "zh": ZH.get(name, ""),
                "faction": faction,
                "factionZh": FACTION_ZH.get(faction, ""),
                "class": hull_class(g["name"], t.get("mass", 0)),
                "classZh": CLASS_ZH[hull_class(g["name"], t.get("mass", 0))],
                "characterId": cid,
                "group": g["name"],
            })

    officers.sort(key=lambda o: (o["faction"], ["battleship", "cruiser", "frigate"].index(o["class"]), o["typeId"]))
    doc = {
        "_说明": "官员元数据：中文译名（音译）、吨位、NPC 角色 ID。船型与阵营来自 ESI，由 -sync-catalog 同步；"
                 "本文件按 typeId 合并，只补人工信息。重新生成：python3 tools/build-officer-meta.py",
        "factions": FACTION_ZH,
        "classes": CLASS_ZH,
        "officers": officers,
    }
    with open(out_path, "w", encoding="utf-8") as f:
        json.dump(doc, f, ensure_ascii=False, indent=1)
    missing_zh = [o["name"] for o in officers if not o["zh"]]
    missing_cid = [o["name"] for o in officers if not o["characterId"]]
    print(f"写出 {len(officers)} 位 → {out_path}")
    print("缺中文名:", missing_zh or "无")
    print("无 NPC 角色 ID:", missing_cid or "无")


if __name__ == "__main__":
    main(sys.argv[1] if len(sys.argv) > 1 else "internal/catalog/data/officers.json")
