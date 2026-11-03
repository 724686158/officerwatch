package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"officerwatch/internal/config"
)

func newTestAuth(password string) *authState {
	return &authState{password: password, enabled: password != "", key: []byte("0123456789abcdef0123456789abcdef"), fails: map[string]*failBucket{}}
}

func TestSessionSignVerify(t *testing.T) {
	a := newTestAuth("s3cret")
	exp := time.Now().Add(time.Hour).Unix()
	val := a.sign(exp)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: val})
	if !a.validSession(req) {
		t.Fatal("刚签发的会话应有效")
	}

	// 篡改
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.AddCookie(&http.Cookie{Name: sessionCookie, Value: val + "x"})
	if a.validSession(req2) {
		t.Fatal("被篡改的会话不该通过")
	}

	// 过期
	expired := a.sign(time.Now().Add(-time.Minute).Unix())
	req3 := httptest.NewRequest(http.MethodGet, "/", nil)
	req3.AddCookie(&http.Cookie{Name: sessionCookie, Value: expired})
	if a.validSession(req3) {
		t.Fatal("过期会话不该通过")
	}

	// 没有 cookie
	if a.validSession(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Fatal("没 cookie 不该通过")
	}
}

func TestSessionKeyIsolatesInstances(t *testing.T) {
	a := newTestAuth("x")
	b := newTestAuth("x")
	b.key = []byte("ffffffffffffffffffffffffffffffff")
	val := a.sign(time.Now().Add(time.Hour).Unix())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: val})
	if b.validSession(req) {
		t.Fatal("换了密钥后旧会话必须失效")
	}
}

func TestLoginRateLimit(t *testing.T) {
	a := newTestAuth("x")
	for i := 0; i < 10; i++ {
		if !a.allow("1.2.3.4") {
			t.Fatalf("第 %d 次不该被拦", i+1)
		}
	}
	if a.allow("1.2.3.4") {
		t.Fatal("超过 10 次应被限流")
	}
	if !a.allow("5.6.7.8") {
		t.Fatal("别的 IP 不该受牵连")
	}
}

func TestAuthDisabledByEmptyPassword(t *testing.T) {
	a := newTestAuth("")
	if !a.validSession(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Fatal("未配置口令时不应拦截")
	}
}

func TestBasePath(t *testing.T) {
	cases := map[string]string{"": "/", "/": "/", "/officerwatch": "/officerwatch/", "officerwatch/": "/officerwatch/"}
	for in, want := range cases {
		cfg := config.Default()
		cfg.BasePath = in
		s := &Server{cfg: cfg}
		if got := s.basePath(); got != want {
			t.Errorf("basePath(%q)=%q，期望 %q", in, got, want)
		}
	}
}
