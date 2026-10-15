//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildTomlMultiProxy 验证多条代理生成的 toml
func TestBuildTomlMultiProxy(t *testing.T) {
	c := &config{
		ServerAddr: "203.0.113.9",
		ServerPort: "7000",
		Token:      "TEST-TOKEN-not-real",
		Proxies: []*proxy{
			{Name: "ssh", LocalIP: "127.0.0.1", LocalPort: "22", RemotePort: "6022"},
			{Name: "web", LocalIP: "127.0.0.1", LocalPort: "8080", RemotePort: "8080"},
			{Name: "rdp", LocalIP: "192.168.1.5", LocalPort: "3389", RemotePort: "63389"},
		},
	}
	got := buildToml(c)

	checks := []string{
		`serverAddr = "203.0.113.9"`,
		`serverPort = 7000`,
		`auth.token = "TEST-TOKEN-not-real"`,
		`name = "ssh"`,
		`remotePort = 6022`,
		`name = "web"`,
		`remotePort = 8080`,
		`name = "rdp"`,
		`localIP = "192.168.1.5"`,
		`localPort = 3389`,
		`remotePort = 63389`,
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("生成结果缺少 %q\n---\n%s", want, got)
		}
	}
	// 必须有 3 个 [[proxies]] 块
	if n := strings.Count(got, "[[proxies]]"); n != 3 {
		t.Errorf("[[proxies]] 块数 = %d, 期望 3\n%s", n, got)
	}
	// 不应再出现旧字段
	if strings.Contains(got, "proxy_name") {
		t.Errorf("残留旧字段 proxy_name")
	}
	t.Logf("生成的配置:\n%s", got)

	// 用真实 frpc 校验语法（可选：找不到就跳过，避免硬编码机器路径）
	frpc := findFrpcForTest()
	if frpc == "" {
		t.Skip("未找到 frpc.exe，跳过语法校验（设 FRPC 环境变量可指定路径）")
	}
	tmp := filepath.Join(t.TempDir(), "frpc.toml")
	if err := os.WriteFile(tmp, []byte(got), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(frpc, "verify", "-c", tmp).CombinedOutput()
	s := string(out)
	if !strings.Contains(s, "syntax is ok") {
		t.Errorf("frpc verify 未通过: %s (err=%v)", s, err)
	} else {
		t.Logf("frpc verify: %s", strings.TrimSpace(s))
	}
}

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
