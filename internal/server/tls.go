package server

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/izzln/content-edge-display/internal/sign"
)

// 服务端证书：首次启动自动生成自签证书（ECDSA P-256），存在 data_dir/tls/。
//
// 设备不靠 CA 信任它，而是在装机时记下公钥指纹（agent.json 的 tls_fingerprint），只认这把公钥。
// 所以证书给多长有效期都无所谓（设备不校验有效期），也不需要写对 IP/域名；浏览器打开后台时
// 会提示"不受信任"，点一次继续访问或把证书导入电脑即可。
// 换服务器时把 data_dir/tls/ 一起搬走，设备就不用重新装机。

func (s *Server) tlsDir() string { return filepath.Join(s.cfg.DataDir, "tls") }

// loadOrCreateCert 读取（没有就生成）服务端证书，并记下公钥指纹。
func (s *Server) loadOrCreateCert() error {
	certPath, keyPath := filepath.Join(s.tlsDir(), "server.crt"), filepath.Join(s.tlsDir(), "server.key")
	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		if err := generateCert(certPath, keyPath); err != nil {
			return fmt.Errorf("generate TLS certificate: %w", err)
		}
		log.Printf("generated a self-signed TLS certificate in %s", s.tlsDir())
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("load TLS certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return err
	}
	cert.Leaf = leaf
	s.cert, s.certFP = &cert, sign.CertFingerprint(leaf)
	return nil
}

func generateCert(certPath, keyPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	host, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "content-edge-display " + host},
		NotBefore:    time.Now().Add(-24 * time.Hour),
		NotAfter:     time.Now().AddDate(30, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host, "localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

// TLSConfig 是 HTTPS 端口用的配置。
func (s *Server) TLSConfig() *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{*s.cert}, MinVersion: tls.VersionTLS12}
}

// CertFingerprint 返回服务端证书的公钥指纹（装机时写进设备的 agent.json）。
func (s *Server) CertFingerprint() string { return s.certFP }

// HTTPErrorLog 是 HTTPS 端口的 http.Server.ErrorLog：其余照常写日志，只丢掉 TLS 握手失败
// （"http: TLS handshake error from …"）。浏览器还没信任自签证书时每建一条连接就有一行（remote error: tls:
// unknown certificate），端口扫描、半开连接也会有，都不需要处理；设备连错服务端时原因写在设备日志里。
func HTTPErrorLog() *log.Logger { return log.New(handshakeFilter{}, "", 0) }

type handshakeFilter struct{}

func (handshakeFilter) Write(p []byte) (int, error) {
	if !bytes.Contains(p, []byte("TLS handshake error")) {
		log.Print(string(p))
	}
	return len(p), nil
}
