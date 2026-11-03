package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"officerwatch/internal/store"
)

// 页面登录（替代 Nginx 的 Basic Auth 弹框）。
//
// 起因：Chrome/Edge 的 HTTP Basic Auth 是**浏览器原生弹框**，不允许粘贴密码
// （也没法用密码管理器），16 位随机口令只能手打。把登录收进页面后，
// 密码框就是普通 input，粘贴/自动填充/手机上长按都正常。
//
// 实现要点：
//   - 口令存在 config.json（0600）的 auth.password；不配则不启用登录（适合只听 127.0.0.1 的场景）
//   - 会话是 HMAC 签名的 cookie，签名密钥在 SQLite 的 meta 里（重启不掉登录）
//   - 登录失败按 IP 限速，10 分钟内 10 次
//   - 只允许从本站页面进来（SameSite=Lax），cookie 不跨站

const sessionCookie = "ow_session"

// sessionTTL 登录有效期。抢官员是低频人工操作，30 天够用。
const sessionTTL = 30 * 24 * time.Hour

type authState struct {
	password string
	key      []byte
	enabled  bool

	mu    sync.Mutex
	fails map[string]*failBucket
}

type failBucket struct {
	count int
	reset time.Time
}

// initAuth 准备签名密钥与口令。
func (s *Server) initAuth() error {
	a := &authState{fails: map[string]*failBucket{}}
	a.password = s.cfg.Auth.Password
	a.enabled = a.password != ""

	k := s.st.GetMeta("session_key")
	if k == "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return fmt.Errorf("生成会话密钥失败: %w", err)
		}
		k = hex.EncodeToString(buf)
		if err := s.st.SetMeta("session_key", k); err != nil {
			return err
		}
	}
	key, err := hex.DecodeString(k)
	if err != nil || len(key) < 16 {
		return fmt.Errorf("meta.session_key 损坏，请删除该键后重启")
	}
	a.key = key
	s.auth = a
	return nil
}

// sign 生成会话值：exp|hmac(exp)。
func (a *authState) sign(exp int64) string {
	mac := hmac.New(sha256.New, a.key)
	fmt.Fprintf(mac, "v1|%d", exp)
	return strconv.FormatInt(exp, 10) + "|" + hex.EncodeToString(mac.Sum(nil))
}

// validSession 校验 cookie。
func (a *authState) validSession(r *http.Request) bool {
	if !a.enabled {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	parts := strings.SplitN(c.Value, "|", 2)
	if len(parts) != 2 {
		return false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	want := a.sign(exp)
	return hmac.Equal([]byte(want), []byte(c.Value))
}

// allow 登录失败限速：同一 IP 10 分钟内最多 10 次。
func (a *authState) allow(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	b, ok := a.fails[ip]
	if !ok || now.After(b.reset) {
		a.fails[ip] = &failBucket{count: 1, reset: now.Add(10 * time.Minute)}
		return true
	}
	b.count++
	return b.count <= 10
}

func clientIP(r *http.Request) string {
	// Nginx 会带 X-Real-IP；直连时用 RemoteAddr
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// handleLoginPage GET 登录页。
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.auth != nil && s.auth.validSession(r) {
		http.Redirect(w, r, s.basePath(), http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = s.loginTmpl.Execute(w, map[string]any{
		"Base":  s.basePath(),
		"Error": r.URL.Query().Get("e") == "1",
		"Off":   s.auth == nil || !s.auth.enabled,
	})
}

// handleLoginSubmit POST 登录。
func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || !s.auth.enabled {
		http.Redirect(w, r, s.basePath(), http.StatusFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, s.basePath()+"login?e=1", http.StatusFound)
		return
	}
	ip := clientIP(r)
	if !s.auth.allow(ip) {
		http.Redirect(w, r, s.basePath()+"login?e=2", http.StatusFound)
		return
	}
	got := r.PostFormValue("password")
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.auth.password)) != 1 {
		s.st.AddEvent("warn", "auth", "登录失败（口令不符）来源 "+ip)
		http.Redirect(w, r, s.basePath()+"login?e=1", http.StatusFound)
		return
	}

	exp := time.Now().Add(sessionTTL).Unix()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.auth.sign(exp),
		Path:     s.basePath(),
		Expires:  time.Unix(exp, 0),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	s.st.AddEvent("info", "auth", "登录成功，来源 "+ip)
	http.Redirect(w, r, s.basePath(), http.StatusFound)
}

// handleLogout GET 退出。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: s.basePath(),
		MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, s.basePath()+"login", http.StatusFound)
}

// requireAuth 未登录时：页面请求跳登录页，API 请求回 401 JSON。
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auth == nil || !s.auth.enabled || s.auth.validSession(r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeErr(w, http.StatusUnauthorized, "未登录或会话过期")
			return
		}
		http.Redirect(w, r, s.basePath()+"login", http.StatusFound)
	})
}

// basePath 应用对外的路径前缀（部署在子路径时为 "/officerwatch/"）。
func (s *Server) basePath() string {
	p := s.cfg.BasePath
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// authStatus 给状态页展示（不泄露口令本身）。
func (s *Server) authStatus() map[string]any {
	on := s.auth != nil && s.auth.enabled
	return map[string]any{"enabled": on, "cookie": sessionCookie, "ttlDays": int(sessionTTL.Hours() / 24)}
}

var _ = store.ErrNotFound
