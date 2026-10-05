package server

import (
	"os/exec"
	"strings"
	"testing"
)

// 与 glibc 规范的已知结果一致；本机有 openssl 时再对拍几组（含超过 64 字节的长密码、多字节字符；openssl 只取前 256 字节，所以不超过它）。
func TestSHA512Crypt(t *testing.T) {
	const want = "$6$saltstring$svn8UoSVapNtMuq1ukKS4tPQd8iKwSMHWjl/O817G3uBnIFNjnQJuesI68u4OTLiBFdcbYEdFCoEOfaS35inz1"
	if got := sha512Crypt("Hello world!", "saltstring"); got != want {
		t.Fatalf("规范向量不符：\n got %s\nwant %s", got, want)
	}
	if h := sha512Crypt("x", ""); !strings.HasPrefix(h, "$6$") || len(strings.Split(h, "$")[2]) != 16 {
		t.Fatalf("随机盐应为 16 位：%s", h)
	}
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("没有 openssl，跳过对拍")
	}
	for _, c := range [][2]string{{"p@ss w0rd", "abcdefgh"}, {strings.Repeat("长密码", 20), "0123456789abcdef"}, {"a", "./Zz"}} {
		out, err := exec.Command("openssl", "passwd", "-6", "-salt", c[1], c[0]).Output()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := sha512Crypt(c[0], c[1]), strings.TrimSpace(string(out)); got != want {
			t.Errorf("与 openssl 不符（%q）：\n got %s\nwant %s", c[0], got, want)
		}
	}
}
