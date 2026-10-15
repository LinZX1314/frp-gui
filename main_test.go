//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// findFrpcForTest 按优先级查找 frpc.exe：环境变量 FRPC > 当前目录 > 上级目录
func findFrpcForTest() string {
	if p := os.Getenv("FRPC"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	cands := []string{
		"frpc.exe",
		filepath.Join("..", "frpc.exe"),
		filepath.Join("..", "frpc-gui", "frpc.exe"),
	}
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	return ""
}

// verifyWithFrpc 用真实 frpc 校验生成的配置语法
func verifyWithFrpc(t *testing.T, toml string) {
	t.Helper()
	frpc := findFrpcForTest()
	if frpc == "" {
		t.Skip("未找到 frpc.exe，跳过语法校验（设 FRPC 环境变量可指定路径）")
	}
	tmp := filepath.Join(t.TempDir(), "frpc.toml")
	if err := os.WriteFile(tmp, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(frpc, "verify", "-c", tmp).CombinedOutput()
	s := string(out)
	if !strings.Contains(s, "syntax is ok") {
		t.Errorf("frpc verify 未通过: %s (err=%v)\n---\n%s", s, err, toml)
	} else {
		t.Logf("frpc verify 通过")
	}
}

// TestBuildTomlMixedTypes 覆盖 tcp / udp / http / https 四种类型
func TestBuildTomlMixedTypes(t *testing.T) {
	c := &config{
		ServerAddr: "203.0.113.9",
		ServerPort: "7000",
		Token:      "TEST-TOKEN-not-real",
		Proxies: []*proxy{
			{Name: "ssh", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: "22", RemotePort: "6022"},
			{Name: "game", Type: "udp", LocalIP: "127.0.0.1", LocalPort: "27015", RemotePort: "27015"},
			{Name: "web", Type: "http", LocalIP: "127.0.0.1", LocalPort: "8080",
				CustomDomains: []string{"a.example.com", "b.example.com"}},
			{Name: "secure", Type: "https", LocalIP: "127.0.0.1", LocalPort: "8443",
				CustomDomains: []string{"s.example.com"}},
		},
	}
	got := buildToml(c)
	t.Logf("生成的配置:\n%s", got)

	mustHave := []string{
		`name = "ssh"`, `type = "tcp"`, `remotePort = 6022`,
		`name = "game"`, `type = "udp"`, `remotePort = 27015`,
		`name = "web"`, `type = "http"`,
		`customDomains = ["a.example.com", "b.example.com"]`,
		`name = "secure"`, `type = "https"`,
		`customDomains = ["s.example.com"]`,
	}
	for _, w := range mustHave {
		if !strings.Contains(got, w) {
			t.Errorf("缺少 %q", w)
		}
	}
	if n := strings.Count(got, "[[proxies]]"); n != 4 {
		t.Errorf("[[proxies]] 块数 = %d，期望 4", n)
	}
	// http / https 绝不能出现 remotePort
	for _, blk := range strings.Split(got, "[[proxies]]") {
		if strings.Contains(blk, `type = "http"`) || strings.Contains(blk, `type = "https"`) {
			if strings.Contains(blk, "remotePort") {
				t.Errorf("http/https 代理不应包含 remotePort:\n%s", blk)
			}
		}
	}

	verifyWithFrpc(t, got)
}

// TestLegacyConfigDefaultsToTcp 旧配置没有 type 字段，应自动当作 tcp
func TestLegacyConfigDefaultsToTcp(t *testing.T) {
	p := &proxy{Name: "old", LocalIP: "127.0.0.1", LocalPort: "22", RemotePort: "6000"}
	p.setDefaults()
	if p.Type != "tcp" {
		t.Errorf("旧配置的 Type = %q，期望 tcp", p.Type)
	}
	c := &config{ServerAddr: "1.2.3.4", ServerPort: "7000", Proxies: []*proxy{p}}
	got := buildToml(c)
	if !strings.Contains(got, `type = "tcp"`) || !strings.Contains(got, "remotePort = 6000") {
		t.Errorf("旧配置生成的 TOML 不对:\n%s", got)
	}
	verifyWithFrpc(t, got)
}

// TestSplitDomains 域名解析：逗号/分号/空格分隔，去重去空
func TestSplitDomains(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"a.com", []string{"a.com"}},
		{"a.com,b.com", []string{"a.com", "b.com"}},
		{"a.com, b.com;c.com d.com", []string{"a.com", "b.com", "c.com", "d.com"}},
		{"a.com,,a.com", []string{"a.com"}},
		{"  a.com  ", []string{"a.com"}},
		{"", nil},
		{" ,; ", nil},
	}
	for _, c := range cases {
		got := splitDomains(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitDomains(%q) = %v，期望 %v", c.in, got, c.want)
			continue
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Errorf("splitDomains(%q) = %v，期望 %v", c.in, got, c.want)
				break
			}
		}
	}
}

// TestProxyTargetText 列表里「远程端口 / 域名」列的显示
func TestProxyTargetText(t *testing.T) {
	tcp := &proxy{Type: "tcp", RemotePort: "6000"}
	if got := tcp.targetText(); got != ":6000" {
		t.Errorf("tcp targetText = %q，期望 :6000", got)
	}
	udp := &proxy{Type: "udp", RemotePort: "27015"}
	if got := udp.targetText(); got != ":27015" {
		t.Errorf("udp targetText = %q，期望 :27015", got)
	}
	h := &proxy{Type: "http", CustomDomains: []string{"a.com", "b.com"}}
	if got := h.targetText(); got != "a.com, b.com" {
		t.Errorf("http targetText = %q，期望 a.com, b.com", got)
	}
}

// TestUniqueName 验证重名自动补全
func TestUniqueName(t *testing.T) {
	c := &config{Proxies: []*proxy{
		{Name: "tcp"},
		{Name: "tcp-2"},
	}}
	if got := c.uniqueName("tcp"); got != "tcp-3" {
		t.Errorf("uniqueName(tcp) = %q, 期望 tcp-3", got)
	}
	if got := c.uniqueName("other"); got != "other" {
		t.Errorf("uniqueName(other) = %q, 期望 other", got)
	}
}

// TestValidatePort 验证端口校验
func TestValidatePort(t *testing.T) {
	bad := []string{"", "0", "70000", "abc", "-1"}
	for _, b := range bad {
		if _, err := validatePort(b, "端口"); err == nil {
			t.Errorf("validatePort(%q) 应报错但通过了", b)
		}
	}
	good := []string{"1", "22", "7000", "65535"}
	for _, g := range good {
		if _, err := validatePort(g, "端口"); err != nil {
			t.Errorf("validatePort(%q) 不应报错: %v", g, err)
		}
	}
}
