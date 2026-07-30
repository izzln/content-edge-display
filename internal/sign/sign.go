// Package sign 实现设备请求的 HMAC-SHA256 签名，服务端与设备代理共用。
//
// 签名串格式: timestamp + "\n" + method + "\n" + path
// 其中 timestamp 为 Unix 秒的十进制字符串, path 为解码后的请求路径(r.URL.Path)。
package sign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// MaxClockSkew 允许的设备与服务器时钟偏差。
const MaxClockSkew = 5 * time.Minute

// Header 名称常量，两端共用。
const (
	HeaderDeviceID  = "X-Device-Id"
	HeaderTimestamp = "X-Timestamp"
	HeaderSign      = "X-Device-Sign"
)

var (
	ErrBadTimestamp = errors.New("sign: invalid timestamp")
	ErrExpired      = errors.New("sign: timestamp outside allowed window")
	ErrMismatch     = errors.New("sign: signature mismatch")
)

// Sign 计算请求签名。
func Sign(secret, timestamp, method, path string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s\n%s\n%s", timestamp, method, path)
	return hex.EncodeToString(mac.Sum(nil))
}

// Now 返回当前时间对应的 timestamp 字符串。
func Now() string {
	return strconv.FormatInt(time.Now().Unix(), 10)
}

// Verify 校验签名及时间窗。
func Verify(secret, timestamp, method, path, gotSign string, now time.Time) error {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrBadTimestamp
	}
	if d := now.Sub(time.Unix(ts, 0)); d > MaxClockSkew || d < -MaxClockSkew {
		return ErrExpired
	}
	want := Sign(secret, timestamp, method, path)
	if !hmac.Equal([]byte(want), []byte(gotSign)) {
		return ErrMismatch
	}
	return nil
}
