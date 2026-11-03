// Package config 读配置文件。
//
// 安全约定：**凭据只在这里**（SMTP 密码、OneBot token、webhook secret），
// 永远不出现在 API 响应里、不进浏览器。页面里只显示"已配置/未配置"。
// 文件权限强制 0600。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config 是配置文件的结构。
type Config struct {
	Listen  string `json:"listen"`  // 监听地址，默认 127.0.0.1:8795（只给 Nginx 反代）
	DataDir string `json:"dataDir"` // 数据目录，默认 ./data
	DBPath  string `json:"dbPath"`  // SQLite 路径，默认 <dataDir>/officerwatch.db

	ESIBase            string `json:"esiBase"`            // 默认 https://esi.evetech.net/latest
	UserAgent          string `json:"userAgent"`          // 给 zkb / ESI / R2Z2 用，必须非空（Cloudflare 会拦空 UA）
	Source             string `json:"source"`             // r2z2 | zkb | both，默认 r2z2
	ZKBIntervalSeconds int    `json:"zkbIntervalSeconds"` // zkb 模式轮询间隔，默认 600

	OneBot struct {
		URL     string `json:"url"`
		Token   string `json:"token"`
		GroupID int64  `json:"groupId"`
	} `json:"onebot"`

	Webhook struct {
		URL    string `json:"url"`
		Secret string `json:"secret"`
	} `json:"webhook"`

	SMTP struct {
		Host string `json:"host"`
		Port string `json:"port"`
		User string `json:"user"`
		Pass string `json:"pass"`
		To   string `json:"to"`
	} `json:"smtp"`

	LogLevel string `json:"logLevel"` // debug | info | warn | error

	// BasePath 是应用对外的路径前缀（挂在 Nginx 子路径时填 "/officerwatch/"）。
	// 登录跳转和 cookie 的 Path 都靠它，填错会表现为"登录后又跳回登录页"。
	BasePath string `json:"basePath"`

	// Auth 页面登录。password 为空则不启用（只监听 127.0.0.1 时可以不配）。
	// 之所以不用 Nginx Basic Auth：Chrome 的原生弹框不允许粘贴口令。
	Auth struct {
		Password string `json:"password"`
	} `json:"auth"`
}

// Default 返回默认配置。
func Default() *Config {
	c := &Config{
		Listen:             "127.0.0.1:8795",
		DataDir:            "./data",
		ESIBase:            "https://esi.evetech.net/latest",
		UserAgent:          "officerwatch/1.0 (+https://github.com/724686158/officerwatch)",
		Source:             "r2z2",
		ZKBIntervalSeconds: 600,
		LogLevel:           "info",
	}
	c.DBPath = "data/officerwatch.db"
	return c
}

// Load 读配置；文件不存在时写一份模板并返回默认值。
func Load(path string) (*Config, error) {
	c := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		if err := Save(path, c); err != nil {
			return nil, err
		}
		return c, nil
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	c.applyDefaults()
	return c, nil
}

func (c *Config) applyDefaults() {
	d := Default()
	if c.Listen == "" {
		c.Listen = d.Listen
	}
	if c.DataDir == "" {
		c.DataDir = d.DataDir
	}
	if c.DBPath == "" {
		c.DBPath = filepath.Join(c.DataDir, "officerwatch.db")
	}
	if c.ESIBase == "" {
		c.ESIBase = d.ESIBase
	}
	if c.UserAgent == "" {
		c.UserAgent = d.UserAgent
	}
	if c.Source == "" {
		c.Source = "r2z2"
	}
	if c.ZKBIntervalSeconds <= 0 {
		c.ZKBIntervalSeconds = 600
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.BasePath == "" {
		c.BasePath = "/"
	}
}

// Save 写配置，权限 0600（内含密码）。
func Save(path string, c *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// Mask 给页面看的安全摘要：只暴露"有没有配"，不暴露值。
func (c *Config) Mask() map[string]any {
	return map[string]any{
		"source":  c.Source,
		"listen":  c.Listen,
		"onebot":  map[string]any{"configured": c.OneBot.URL != "" && c.OneBot.GroupID != 0, "groupID": c.OneBot.GroupID},
		"webhook": map[string]any{"configured": c.Webhook.URL != ""},
		"smtp": map[string]any{
			"configured": c.SMTP.Host != "" && (c.SMTP.User != "" || c.SMTP.To != ""),
			"from":       maskMail(c.SMTP.User),
			"fallbackTo": maskMail(c.SMTP.To),
		},
		"userAgent":     c.UserAgent,
		"basePath":      c.BasePath,
		"auth":          map[string]any{"enabled": c.Auth.Password != ""},
		"esiBase":       c.ESIBase,
		"configIsLocal": true,
	}
}

func maskMail(s string) string {
	i := strings.IndexByte(s, '@')
	if i <= 1 {
		return s
	}
	return s[:1] + "***" + s[i:]
}
