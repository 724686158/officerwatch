package web

import "testing"

func TestNormalizeEmails(t *testing.T) {
	valid, invalid := normalizeEmails([]string{
		"a@qq.com",
		" A@QQ.com ",             // 去空白 + 大小写去重
		"b@163.com, c@gmail.com", // 一次粘贴多个
		"bad@@x.com",             // 不合法
		"nope",                   // 不合法
		"",                       // 忽略
	})
	if len(valid) != 3 {
		t.Fatalf("应保留 3 个合法地址，得到 %v", valid)
	}
	if valid[0] != "a@qq.com" || valid[2] != "c@gmail.com" {
		t.Fatalf("顺序/内容不对: %v", valid)
	}
	if len(invalid) != 2 {
		t.Fatalf("应报出 2 个非法地址，得到 %v", invalid)
	}
}

func TestIsEmail(t *testing.T) {
	ok := []string{"a@b.co", "first.last@sub.example.com", "x+tag@qq.com"}
	bad := []string{"", "@x.com", "a@", "a b@x.com", "a@x", "a@@x.com", "<a@x.com>"}
	for _, s := range ok {
		if !isEmail(s) {
			t.Errorf("%q 应判为合法", s)
		}
	}
	for _, s := range bad {
		if isEmail(s) {
			t.Errorf("%q 应判为非法", s)
		}
	}
}
