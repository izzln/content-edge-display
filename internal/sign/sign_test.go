package sign

import (
	"strconv"
	"testing"
	"time"
)

func TestSignAndVerify(t *testing.T) {
	now := time.Now()
	ts := strconv.FormatInt(now.Unix(), 10)
	s := Sign("secret", ts, "GET", "/api/v1/device/manifest")

	if err := Verify("secret", ts, "GET", "/api/v1/device/manifest", s, now); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := Verify("wrong", ts, "GET", "/api/v1/device/manifest", s, now); err == nil {
		t.Fatal("wrong secret accepted")
	}
	if err := Verify("secret", ts, "GET", "/media/dev/other.mp4", s, now); err == nil {
		t.Fatal("tampered path accepted")
	}
	if err := Verify("secret", ts, "POST", "/api/v1/device/manifest", s, now); err == nil {
		t.Fatal("tampered method accepted")
	}
}

func TestVerifyClockSkew(t *testing.T) {
	now := time.Now()
	old := strconv.FormatInt(now.Add(-MaxClockSkew-time.Minute).Unix(), 10)
	s := Sign("secret", old, "GET", "/p")
	if err := Verify("secret", old, "GET", "/p", s, now); err != ErrExpired {
		t.Fatalf("expected ErrExpired, got %v", err)
	}

	future := strconv.FormatInt(now.Add(MaxClockSkew+time.Minute).Unix(), 10)
	s = Sign("secret", future, "GET", "/p")
	if err := Verify("secret", future, "GET", "/p", s, now); err != ErrExpired {
		t.Fatalf("expected ErrExpired for future ts, got %v", err)
	}

	if err := Verify("secret", "not-a-number", "GET", "/p", "x", now); err != ErrBadTimestamp {
		t.Fatalf("expected ErrBadTimestamp, got %v", err)
	}
}
