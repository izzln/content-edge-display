package server

import (
	_ "embed"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"text/template"

	"github.com/izzln/content-edge-display/internal/store"
)

//go:embed install.sh.tmpl
var installScript string

var installTmpl = template.Must(template.New("install.sh").Parse(installScript))

// BootstrapHandler 是 HTTP 端口（bootstrap_listen）的处理器：只提供装机入口，其余一律跳转到 HTTPS。
// 装机时设备还不知道服务端证书指纹，只能从这里用 HTTP 取一次安装脚本（脚本里带着指纹）。
func (s *Server) BootstrapHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /install.sh", s.handleInstallScript)
	mux.HandleFunc("GET /bootstrap/agent.tar.gz", s.handleBootstrapPackage)
	mux.HandleFunc("/", s.redirectToHTTPS)
	return mux
}

// handleInstallScript 生成一键安装脚本：填好 HTTPS 地址与证书指纹（主机名沿用装机人员访问时用的那个）。
func (s *Server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	installTmpl.Execute(w, map[string]string{
		"ServerURL":   s.httpsBase(r),
		"Fingerprint": s.certFP,
		"Bootstrap":   "http://" + r.Host,
	})
}

// handleBootstrapPackage 给装机脚本下载最新上传的设备程序包，凭注册口令（X-Enroll-Token）。
func (s *Server) handleBootstrapPackage(w http.ResponseWriter, r *http.Request) {
	if !tokenOK(r.Header.Get("X-Enroll-Token"), s.cfg.EnrollToken) {
		http.Error(w, "bad enroll token", http.StatusUnauthorized)
		return
	}
	var latest store.Package
	var ok bool
	s.store.View(func(st *store.State) { latest, ok = st.LatestPackage() })
	if !ok {
		http.Error(w, "no agent package uploaded yet", http.StatusNotFound)
		return
	}
	log.Printf("install: agent package %s downloaded by %s", latest.Version, r.RemoteAddr)
	w.Header().Set("Content-Type", "application/gzip")
	http.ServeFile(w, r, filepath.Join(s.packagesDir(), latest.File))
}

// redirectToHTTPS 把请求原样跳到 HTTPS 端口（主机名沿用请求里的）。
func (s *Server) redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, s.httpsBase(r)+r.URL.RequestURI(), http.StatusMovedPermanently)
}

// httpsBase 是这个请求对应的 HTTPS 地址（https://主机:端口），主机名取自请求，端口取自 listen。
func (s *Server) httpsBase(r *http.Request) string {
	host := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		host = h
	}
	_, port, _ := net.SplitHostPort(s.cfg.Listen)
	return "https://" + net.JoinHostPort(host, port)
}
