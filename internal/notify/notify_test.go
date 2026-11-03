package notify

import (
	"strings"
	"testing"
	"time"

	"officerwatch/internal/store"
)

func sampleKill() store.Kill {
	return store.Kill{
		KillmailID: 132678033, OfficerTypeID: 13654, OfficerName: "Cormack Vaaja",
		OfficerZh: "科马克·瓦亚", OfficerClass: "battleship",
		Faction: "Serpentis", Role: "npc_kill",
		KillmailTime: time.Date(2026, 11, 3, 11, 30, 0, 0, time.UTC),
		TotalValue:   1.163e9, DroppedValue: 4.2e8, DestroyedValue: 7.4e8,
		SystemName: "Jita", RegionName: "The Forge", SystemSec: 0.95,
		VictimShipName: "Vexor", VictimCharID: 2117711114, VictimName: "Some Pilot",
		LocationID: 40290795, LocationName: "9 - Asteroid Belt 3（小行星带）", Jumps: 12,
		VictimCorpName: "Tritanium Ltd", VictimAllianceName: "Brave Collective",
		AttackerCount: 3, Points: 1, OfficerKillRank: 1, OfficerKillTotal: 7,
	}
}

func TestFormatISK(t *testing.T) {
	cases := map[float64]string{0: "0", 999: "999", 1500: "2K", 2.5e6: "2.5M", 1.163e9: "1.16B"}
	for in, want := range cases {
		if got := FormatISK(in); got != want {
			t.Errorf("FormatISK(%v)=%q，期望 %q", in, got, want)
		}
	}
}

func TestFormatText(t *testing.T) {
	txt := FormatText(sampleKill(), "Jita")
	for _, want := range []string{"官员击杀", "Cormack Vaaja", "天蛇", "战列", "1.16B", "Jita", "Vexor",
		"Some Pilot", "Tritanium Ltd", "Brave Collective", "3 名攻击者", "历史最高损失",
		"安全 0.95", "链接打不开可以忽略", "zkillboard.com/kill/132678033",
		"星体：9 - Asteroid Belt 3（小行星带）", "距 Jita 12 跳"} {
		if !strings.Contains(txt, want) {
			t.Errorf("纯文本缺 %q:\n%s", want, txt)
		}
	}
	// 时间按北京时间显示（11:30 UTC → 19:30 CST）
	if !strings.Contains(txt, "19:30") {
		t.Errorf("时间没转成北京时间:\n%s", txt)
	}
	// CQ 码不该出现在通用文案里（否则邮件正文会带上）
	if strings.Contains(txt, "CQ:") {
		t.Error("通用文案里不该有 CQ 码")
	}
}

func TestFormatHTML(t *testing.T) {
	h := FormatHTML(sampleKill(), "Jita")
	for _, want := range []string{"Cormack Vaaja", "科马克·瓦亚", "1.16B ISK", "zkillboard.com/kill/132678033", "Vexor",
		"Some Pilot", "Brave Collective", "掉落", "损毁", "北京时间"} {
		if !strings.Contains(h, want) {
			t.Errorf("HTML 缺 %q", want)
		}
	}
	if strings.Contains(h, "<script") {
		t.Error("HTML 里不该有脚本")
	}
	// 阵营/吨位用中文展示（面向中文玩家），英文阵营名不出现
	if !strings.Contains(h, "天蛇") || !strings.Contains(h, "战列") {
		t.Error("HTML 里应有中文阵营与吨位")
	}
	// 官方船名里带单引号（Sansha's Nation）时不能破坏行内样式
	k := sampleKill()
	k.Faction = "Sansha's Nation"
	if !strings.Contains(FormatHTML(k, "Jita"), "萨沙") {
		t.Error("阵营名带引号时被吞了")
	}
}

func TestRecipientsFromMessage(t *testing.T) {
	// 页面管理的收件人优先于配置文件里的 smtp.to
	m := NewMessage(sampleKill(), false, "Jita", []string{"a@qq.com", "b@163.com"})
	if len(m.Recipients) != 2 {
		t.Fatalf("收件人没带上: %v", m.Recipients)
	}
}

func TestRecipients(t *testing.T) {
	m := NewMail("smtp.qq.com", "587", "a@qq.com", "x", "b@qq.com, c@163.com;d@x.com").(*Mail)
	got := m.Recipients()
	if len(got) != 3 {
		t.Fatalf("应解析出 3 个收件人，得到 %v", got)
	}
	if NewMail("", "", "", "", "a@b.c") != nil || NewMail("h", "587", "", "", "") != nil {
		t.Fatal("配置不全时不该创建通道")
	}
}

func TestOneBotAtAllIsChannelSpecific(t *testing.T) {
	// @全体 只该加在群消息里：邮件/纯文本文案保持干净
	k := sampleKill()
	m := NewMessage(k, true, "Jita", nil)
	if strings.Contains(m.Text, "CQ:") {
		t.Fatal("文案里不该预埋 CQ 码")
	}
	if !m.AtAll {
		t.Fatal("Message 应该记住 AtAll 供 OneBot 使用")
	}
}
