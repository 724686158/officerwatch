// Package notify 是通知层。
//
// 渠道优先级（2026-11 起）：**邮件是主通道**。QQ 群那套（OneBot）依赖朋友自建的
// 机器人，现在不具备条件了，所以它保留实现但默认不注册 —— 谁哪天又有机器人了，
// 在 config.json 里填上 onebot 段就会自动启用。
//
// 设计要点：
//   - 各通道互相独立：一个挂了不影响别的，也不影响采集
//   - 文案只生成一次（Message），各通道按自己的格式取用（邮件要 HTML，群要纯文本 + CQ 码）
//   - 凭据只在配置文件里，永不出现在 API 响应/页面
package notify

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"time"

	"officerwatch/internal/catalog"
	"officerwatch/internal/store"
)

// Message 是一条待发送的通知，文案在这里生成一次。
type Message struct {
	Kill       store.Kill
	AtAll      bool     // 只对 QQ 群有意义
	Home       string   // "家"星系名（抢官员的参照点），用于"距 X N 跳"
	Recipients []string // 收件人（页面上管理；空则退回 config 里的 smtp.to）
	Raw        string   // 非击杀类通知（如"采集器停摆"）直接用它当正文
	Text       string
	HTML       string
}

// NewMessage 由击杀记录生成文案。
func NewMessage(k store.Kill, atAll bool, home string, recipients []string) Message {
	return Message{
		Kill: k, AtAll: atAll, Home: home, Recipients: recipients,
		Text: FormatText(k, home), HTML: FormatHTML(k, home),
	}
}

// Channel 一个通知通道。
type Channel interface {
	Name() string
	Send(ctx context.Context, m Message) error
}

// Notifier 把消息发给所有已注册通道。
type Notifier struct {
	chans []Channel
	log   *slog.Logger
}

// New 构造。
func New(log *slog.Logger) *Notifier { return &Notifier{log: log} }

// Add 注册通道（nil 会被忽略）。
func (n *Notifier) Add(c Channel) {
	if c == nil {
		return
	}
	for _, x := range n.chans {
		if x.Name() == c.Name() {
			return
		}
	}
	n.chans = append(n.chans, c)
}

// Channels 已注册的通道。
func (n *Notifier) Channels() []Channel { return n.chans }

// Notify 广播一条通知。全部通道都失败才返回 error（部分失败只记日志）。
func (n *Notifier) Notify(ctx context.Context, m Message) error {
	var firstErr error
	okCount := 0
	for _, c := range n.chans {
		cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		err := c.Send(cctx, m)
		cancel()
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", c.Name(), err)
			}
			n.log.Error("通道发送失败", "channel", c.Name(), "kill", m.Kill.KillmailID, "err", err)
			continue
		}
		okCount++
	}
	if okCount == 0 && firstErr != nil {
		return firstErr
	}
	return nil
}

// NotifyRaw 发送一条与击杀无关的通知（运维告警用）。
func (n *Notifier) NotifyRaw(ctx context.Context, text string) error {
	if len(n.chans) == 0 {
		return fmt.Errorf("没有已配置的通知通道")
	}
	m := Message{Raw: text, Text: text, HTML: "<pre style=\"white-space:pre-wrap;font:13px/1.6 ui-monospace,Menlo,monospace\">" + esc(text) + "</pre>"}
	return n.Notify(ctx, m)
}

// SendTest 给每个通道发一条测试消息，返回 通道名 → 结果。
// recipients 走和真实告警同一条路径（页面上管理的那份收件人）。
func (n *Notifier) SendTest(ctx context.Context, recipients []string) map[string]string {
	out := map[string]string{}
	if len(n.chans) == 0 {
		out["-"] = "没有已配置的通知通道"
		return out
	}
	k := store.Kill{
		KillmailID: 0, OfficerName: "Cormack Vaaja", Faction: "Serpentis",
		TotalValue: 1.234e9, DroppedValue: 4.2e8, SystemName: "Jita", RegionName: "The Forge",
		VictimShipName: "Vindicator", KillmailTime: time.Now(),
	}
	m := NewMessage(k, false, "Jita", recipients)
	m.Text = "🔧 officerwatch 测试消息（配置检查，可忽略）\n\n" + m.Text
	m.HTML = `<p style="color:#667085">🔧 officerwatch 测试消息（配置检查，可忽略）</p>` + m.HTML
	for _, c := range n.chans {
		cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		err := c.Send(cctx, m)
		cancel()
		if err != nil {
			out[c.Name()] = "失败: " + err.Error()
		} else {
			out[c.Name()] = "成功"
		}
	}
	return out
}

// FormatISK 把 ISK 变成人看的单位。
func FormatISK(v float64) string {
	switch {
	case v >= 1e9:
		return fmt.Sprintf("%.2fB", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.0fK", v/1e3)
	default:
		return fmt.Sprintf("%.0f", v)
	}
}

// cstTime 统一按北京时间展示（服务器在 UTC+8，但日志/数据库都存 UTC，这里显式转换）。
func cstTime(k store.Kill) string {
	return k.KillmailTime.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04")
}

func roleLabel(k store.Kill) string {
	if k.Role == "officer_down" {
		return "官员被爆"
	}
	return "官员击杀"
}

func place(k store.Kill) string {
	if k.SystemName == "" {
		return ""
	}
	s := k.SystemName
	if k.RegionName != "" {
		s += " · " + k.RegionName
	}
	if k.SystemSec != 0 {
		s += fmt.Sprintf(" · 安全 %.2f", k.SystemSec)
	}
	return s
}

// FormatText 纯文本（邮件 text/plain 部分、群消息）。
//
// 2026-11 使用者反馈："很多人打不开 zKillboard 链接"（国内直连不稳/要 JS）。
// 所以正文必须是**自解释**的：受害者、座舰、价值构成、星系+安全、时间、战况都在正文里，
// 链接只作为"想看原始数据时"的补充。
func FormatText(k store.Kill, home string) string {
	var b strings.Builder
	emoji := "🩸"
	if k.Role == "officer_down" {
		emoji = "🎯"
	}
	b.WriteString(emoji + " " + roleLabel(k) + " · " + officerLine(k) + "\n\n")

	if v := victimLine(k); v != "" {
		b.WriteString("受害者：" + v + "\n")
	}
	if k.VictimShipName != "" {
		b.WriteString("座舰：" + k.VictimShipName + "\n")
	}
	if bd := valueBreakdown(k); bd != "" {
		b.WriteString("价值：" + FormatISK(k.TotalValue) + " ISK（" + bd + "）\n")
	} else {
		b.WriteString("价值：" + FormatISK(k.TotalValue) + " ISK\n")
	}
	if p := place(k); p != "" {
		b.WriteString("地点：" + p + "\n")
	}
	if k.LocationName != "" {
		b.WriteString("星体：" + k.LocationName + "\n")
	}
	if d := distance(k, home); d != "" {
		b.WriteString("距离：" + d + "\n")
	}
	b.WriteString("时间：" + cstTime(k) + "（北京时间）\n")
	if w := battleLine(k); w != "" {
		b.WriteString("战况：" + w + "\n")
	}
	if r := rankLine(k); r != "" {
		b.WriteString("参考：" + r + "\n")
	}
	b.WriteString("\nhttps://zkillboard.com/kill/" + fmt.Sprint(k.KillmailID) + "/\n")
	b.WriteString("（链接打不开可以忽略，上面的信息已经够了）")
	return b.String()
}

// officerLine 官员一行：英文名 · 中文译名（阵营 · 吨位）。
func officerLine(k store.Kill) string {
	s := k.OfficerName
	if k.OfficerZh != "" && k.OfficerZh != k.OfficerName {
		s += "（" + k.OfficerZh + "）"
	}
	tags := []string{}
	if zh := catalog.FactionZh(k.Faction); zh != "" {
		tags = append(tags, zh)
	}
	if zh := catalog.ClassZh(k.OfficerClass); zh != "" {
		tags = append(tags, zh)
	}
	if len(tags) > 0 {
		s += " · " + strings.Join(tags, " · ")
	}
	return s
}

// victimLine 受害者身份：名字 / 公司 · 联盟。
func victimLine(k store.Kill) string {
	if k.Role == "officer_down" {
		return "官员 NPC（" + k.OfficerName + "）"
	}
	name := k.VictimName
	if name == "" && k.VictimCharID > 0 {
		name = fmt.Sprintf("角色 %d", k.VictimCharID)
	}
	org := []string{}
	if k.VictimCorpName != "" {
		org = append(org, k.VictimCorpName)
	}
	if k.VictimAllianceName != "" {
		org = append(org, k.VictimAllianceName)
	}
	if len(org) > 0 {
		if name != "" {
			name += "（" + strings.Join(org, " · ") + "）"
		} else {
			name = strings.Join(org, " · ")
		}
	}
	return name
}

// valueBreakdown 掉落/损毁拆分。
func valueBreakdown(k store.Kill) string {
	parts := []string{}
	if k.DroppedValue > 0 {
		parts = append(parts, "掉落 "+FormatISK(k.DroppedValue))
	}
	if k.DestroyedValue > 0 {
		parts = append(parts, "损毁 "+FormatISK(k.DestroyedValue))
	}
	return strings.Join(parts, " / ")
}

// tagLine 阵营 · 吨位（中文）。
func tagLine(k store.Kill) string {
	tags := []string{}
	if zh := catalog.FactionZh(k.Faction); zh != "" {
		tags = append(tags, zh)
	}
	if zh := catalog.ClassZh(k.OfficerClass); zh != "" {
		tags = append(tags, zh)
	}
	return strings.Join(tags, " · ")
}

// distance "抢官员"的核心数字：从家门口到死亡点几跳。
func distance(k store.Kill, home string) string {
	if k.Jumps <= 0 || home == "" {
		return ""
	}
	return fmt.Sprintf("距 %s %d 跳", home, k.Jumps)
}

// battleLine 参战规模。
func battleLine(k store.Kill) string {
	if k.AttackerCount == 0 {
		return ""
	}
	s := fmt.Sprintf("%d 名攻击者", k.AttackerCount)
	if k.Solo {
		s = "solo（1v1）"
	}
	if k.Awox {
		s += " · awox"
	}
	if k.Points > 0 {
		s += fmt.Sprintf(" · %d 点", k.Points)
	}
	return s
}

// rankLine 这条损失在该官员历史上的排名。
func rankLine(k store.Kill) string {
	if k.OfficerKillTotal <= 1 || k.OfficerKillRank <= 0 {
		return ""
	}
	if k.OfficerKillRank == 1 {
		return fmt.Sprintf("该官员历史最高损失（共 %d 次）", k.OfficerKillTotal)
	}
	return fmt.Sprintf("该官员历史第 %d 高损失（共 %d 次）", k.OfficerKillRank, k.OfficerKillTotal)
}

// FormatHTML 邮件 HTML（内联样式，不依赖外部 CSS —— 邮件客户端会吃掉 <style>）。
func FormatHTML(k store.Kill, home string) string {
	accent := "#2563eb"
	dim := "#667085"
	line := "#e6e9ef"
	bg := "#f6f7f9"

	row := func(label, value string) string {
		if value == "" {
			return ""
		}
		return `<tr>
			<td style="padding:9px 0;color:` + dim + `;font-size:13px;width:76px;vertical-align:top;white-space:nowrap">` + label + `</td>
			<td style="padding:9px 0;color:#161a22;font-size:14px;border-bottom:1px solid ` + line + `;line-height:1.55">` + value + `</td>
		</tr>`
	}

	emoji := "🩸"
	if k.Role == "officer_down" {
		emoji = "🎯"
	}
	valueCell := `<b style="font-size:17px">` + FormatISK(k.TotalValue) + ` ISK</b>`
	if bd := valueBreakdown(k); bd != "" {
		valueCell += ` <span style="color:` + dim + `;font-size:12px">（` + bd + `）</span>`
	}
	rank := ""
	if r := rankLine(k); r != "" {
		rank = `<tr><td colspan="2" style="padding:10px 0 0;color:#d97706;font-size:12px">` + r + `</td></tr>`
	}
	zkb := "https://zkillboard.com/kill/" + fmt.Sprint(k.KillmailID) + "/"

	return `<div style="background:` + bg + `;padding:24px 12px;font-family:-apple-system,BlinkMacSystemFont,'PingFang SC','Microsoft YaHei',sans-serif">
  <div style="max-width:540px;margin:0 auto;background:#ffffff;border:1px solid ` + line + `;border-radius:14px;overflow:hidden">
    <div style="padding:16px 20px;border-bottom:1px solid ` + line + `">
      <div style="color:` + dim + `;font-size:12px;letter-spacing:.06em;text-transform:uppercase">` + emoji + ` ` + roleLabel(k) + `</div>
      <div style="font-size:20px;font-weight:650;color:#161a22;margin-top:4px">` + esc(k.OfficerName) + `</div>
      <div style="color:#161a22;font-size:14px;margin-top:2px">` + esc(k.OfficerZh) + `</div>
      <div style="color:` + dim + `;font-size:13px;margin-top:2px">` + esc(tagLine(k)) + `</div>
    </div>
    <div style="padding:6px 20px 18px">
      <table style="width:100%;border-collapse:collapse">
        ` + row("受害者", esc(victimLine(k))) + `
        ` + row("座舰", esc(k.VictimShipName)) + `
        ` + row("价值", valueCell) + `
        ` + row("地点", esc(place(k))) + `
        ` + row("星体", esc(k.LocationName)) + `
        ` + row("距离", esc(distance(k, home))) + `
        ` + row("时间", cstTime(k)+` <span style="color:`+dim+`;font-size:12px">北京时间</span>`) + `
        ` + row("战况", esc(battleLine(k))) + `
        ` + row("KillID", fmt.Sprint(k.KillmailID)) + `
        ` + rank + `
      </table>
      <div style="margin-top:16px;color:` + dim + `;font-size:12px">
        以上信息已足够判断，链接只在要看原始装备清单时需要。
      </div>
      <div style="margin-top:10px">
        <a href="` + zkb + `" style="color:` + accent + `;font-size:13px">zKillboard 原始页面 →</a>
      </div>
    </div>
  </div>
  <div style="max-width:540px;margin:12px auto 0;color:#98a2b3;font-size:11px;text-align:center">
    officerwatch · 自动发送
  </div>
</div>`
}

func esc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// ─────────────────────────────── 邮件（主通道）

// Mail 走 SMTP 发邮件。收件人支持逗号分隔多个。
type Mail struct {
	Host, Port, User, Pass, To string
	hc                         *http.Client
}

// NewMail 构造；host/to 缺一不启用。
//
// 返回接口而不是 *Mail：Go 里 `(*Mail)(nil)` 装进接口后 != nil，
// 会让"未配置的通道"照样被注册、然后在发送时 panic。返回 Channel 才能真正返回 nil。
func NewMail(host, port, user, pass, to string) Channel {
	if strings.TrimSpace(host) == "" || strings.TrimSpace(to) == "" {
		return nil
	}
	if port == "" {
		port = "587" // QQ/163 都用 587 + STARTTLS；465 隐式 TLS 不被 net/smtp 支持
	}
	return &Mail{Host: host, Port: port, User: user, Pass: pass, To: to}
}

// Name 通道名。
func (m *Mail) Name() string { return "email" }

// Recipients 解析收件人列表。
func (m *Mail) Recipients() []string {
	out := []string{}
	for _, p := range strings.FieldsFunc(m.To, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
		if strings.Contains(p, "@") {
			out = append(out, p)
		}
	}
	return out
}

// Send 发送 multipart/alternative 邮件（纯文本 + HTML 兜底）。
//
// 收件人优先级：消息里带的（页面上管理的列表）> 配置文件里的 smtp.to。
func (m *Mail) Send(_ context.Context, msg Message) error {
	to := msg.Recipients
	if len(to) == 0 {
		to = m.Recipients()
	}
	if len(to) == 0 {
		return fmt.Errorf("没有收件人：请在页面「设置 → 收件人」里添加，或配置 smtp.to")
	}
	from := m.User
	if !strings.Contains(from, "@") {
		from = to[0]
	}

	subject := fmt.Sprintf("[officerwatch] %s · %s · %s",
		roleLabel(msg.Kill), msg.Kill.OfficerName, FormatISK(msg.Kill.TotalValue))
	// 中文主题必须 RFC 2047 编码，否则 QQ/网易邮件客户端会显示乱码
	subject = "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(subject)) + "?="

	const boundary = "officerwatch-alt-boundary"
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	b.WriteString("Subject: " + subject + "\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n")
	b.WriteString("\r\n")
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(msg.Text + "\r\n\r\n")
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(msg.HTML + "\r\n\r\n")
	b.WriteString("--" + boundary + "--\r\n")

	addr := net.JoinHostPort(m.Host, m.Port)
	var auth smtp.Auth
	if m.User != "" {
		auth = smtp.PlainAuth("", m.User, m.Pass, m.Host)
	}
	if err := smtp.SendMail(addr, auth, from, to, []byte(b.String())); err != nil {
		// QQ 邮箱偶发 "short response" 但邮件已投递（前身踩过，保留兼容）
		if strings.Contains(err.Error(), "short response") {
			return nil
		}
		return err
	}
	return nil
}

// ─────────────────────────────── OneBot（QQ 群，可选）

// OneBot 通过 OneBot v11 HTTP 接口发群消息。
// 现在默认不启用：它依赖一个自建/朋友自建的机器人服务端。
type OneBot struct {
	URL     string
	Token   string
	GroupID int64
	hc      *http.Client
}

// NewOneBot 构造；配置不全返回 nil。
func NewOneBot(url, token string, groupID int64) Channel {
	if url == "" || groupID == 0 {
		return nil
	}
	return &OneBot{URL: strings.TrimSuffix(url, "/"), Token: token, GroupID: groupID,
		hc: &http.Client{Timeout: 15 * time.Second}}
}

// Name 通道名。
func (o *OneBot) Name() string { return "onebot" }

// Send 发一条群消息；@全体是 CQ 码，只在这个通道里加。
func (o *OneBot) Send(ctx context.Context, m Message) error {
	text := m.Text
	if m.Raw != "" {
		text = m.Raw
	}
	if m.AtAll {
		text = "[CQ:at,qq=all]\n" + text
	}
	body, _ := json.Marshal(map[string]any{"group_id": o.GroupID, "message": text, "auto_escape": false})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.URL+"/send_group_msg", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.Token != "" {
		req.Header.Set("Authorization", "Bearer "+o.Token)
	}
	resp, err := o.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Status  string `json:"status"`
		RetCode int    `json:"retcode"`
		Message string `json:"message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, out.Message)
	}
	if out.RetCode != 0 {
		return fmt.Errorf("retcode=%d %s", out.RetCode, out.Message)
	}
	return nil
}

// ─────────────────────────────── 通用 Webhook（可选）

// Webhook 把击杀以 JSON POST 出去（可接 ntfy / 飞书 / 钉钉的自建网关）。
type Webhook struct {
	URL    string
	Secret string
	hc     *http.Client
}

// NewWebhook 构造。
func NewWebhook(url, secret string) Channel {
	if url == "" {
		return nil
	}
	return &Webhook{URL: url, Secret: secret, hc: &http.Client{Timeout: 15 * time.Second}}
}

// Name 通道名。
func (w *Webhook) Name() string { return "webhook" }

// Send POST 一条结构化消息。
func (w *Webhook) Send(ctx context.Context, m Message) error {
	payload := map[string]any{"event": "officer_kill", "text": m.Text, "kill": m.Kill}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if w.Secret != "" {
		req.Header.Set("X-Officerwatch-Secret", w.Secret)
	}
	resp, err := w.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
