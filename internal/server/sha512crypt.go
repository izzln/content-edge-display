package server

import (
	"crypto/rand"
	"crypto/sha512"
	"strings"
)

// sha512Crypt 按 glibc 的 SHA-crypt 规范（$6$，默认 5000 轮）计算 /etc/shadow 用的密码哈希，
// 与 `openssl passwd -6` / `mkpasswd -m sha-512` 一致。服务端只保存哈希，设备用 chpasswd -e 写入。
// salt 为空时随机生成 16 位。
func sha512Crypt(password, salt string) string {
	if salt == "" {
		salt = randomSalt()
	}
	if len(salt) > 16 {
		salt = salt[:16]
	}
	p, s := []byte(password), []byte(salt)

	b := sha512.New()
	b.Write(p)
	b.Write(s)
	b.Write(p)
	sumB := b.Sum(nil)

	a := sha512.New()
	a.Write(p)
	a.Write(s)
	for i := len(p); i > 0; i -= 64 {
		a.Write(sumB[:min(i, 64)])
	}
	for i := len(p); i > 0; i >>= 1 {
		if i&1 != 0 {
			a.Write(sumB)
		} else {
			a.Write(p)
		}
	}
	sumA := a.Sum(nil)

	dp := sha512.New()
	for range len(p) {
		dp.Write(p)
	}
	pSeq := repeatTo(dp.Sum(nil), len(p))

	ds := sha512.New()
	for range 16 + int(sumA[0]) {
		ds.Write(s)
	}
	sSeq := repeatTo(ds.Sum(nil), len(s))

	c := sumA
	for r := range 5000 {
		h := sha512.New()
		if r&1 != 0 {
			h.Write(pSeq)
		} else {
			h.Write(c)
		}
		if r%3 != 0 {
			h.Write(sSeq)
		}
		if r%7 != 0 {
			h.Write(pSeq)
		}
		if r&1 != 0 {
			h.Write(c)
		} else {
			h.Write(pSeq)
		}
		c = h.Sum(nil)
	}

	var out strings.Builder
	out.WriteString("$6$" + salt + "$")
	b64 := func(b2, b1, b0 byte, n int) {
		w := uint(b2)<<16 | uint(b1)<<8 | uint(b0)
		for range n {
			out.WriteByte(cryptAlphabet[w&0x3f])
			w >>= 6
		}
	}
	for i := 0; i < 21; i++ {
		// 规范规定的字节打乱顺序：(0,21,42) (22,43,1) (44,2,23) …
		x, y, z := i, (i+21)%63, (i+42)%63
		switch i % 3 {
		case 1:
			x, y, z = (i+21)%63, (i+42)%63, i
		case 2:
			x, y, z = (i+42)%63, i, (i+21)%63
		}
		b64(c[x], c[y], c[z], 4)
	}
	b64(0, 0, c[63], 2)
	return out.String()
}

const cryptAlphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func randomSalt() string {
	b := make([]byte, 16)
	rand.Read(b)
	for i := range b {
		b[i] = cryptAlphabet[int(b[i])%len(cryptAlphabet)]
	}
	return string(b)
}

func repeatTo(sum []byte, n int) []byte {
	out := make([]byte, 0, n)
	for len(out) < n {
		out = append(out, sum[:min(len(sum), n-len(out))]...)
	}
	return out
}
