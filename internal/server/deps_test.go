package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/izzln/content-edge-display/internal/testutil"
)

func depsBundle(codename string, files ...testutil.File) []byte {
	return testutil.TarGz("display-deps", append([]testutil.File{{Name: "CODENAME", Body: codename + "\n"}}, files...)...)
}

// 离线依赖包与程序包从同一个入口上传，按内容区分；设备经 HTTPS 端口的 /apt/<代号>/ 当作 apt 仓库使用。
func TestDepsRepo(t *testing.T) {
	s, h := newAdminTestServer(t)
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	index := []testutil.File{{Name: "Packages", Body: "Package: libfoo\n"}, {Name: "Release", Body: "Origin: test\n"}}

	for _, c := range []struct {
		desc string
		pkg  []byte
		want string
	}{
		{"代号非法", depsBundle("../etc", index...), "CODENAME"},
		{"缺 apt 索引", depsBundle("bookworm", testutil.File{Name: "Packages", Body: "x"}), "Release"},
	} {
		if w := uploadPackageRaw(t, h, c.pkg); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("%s：应被拒绝并提示 %q，得到 %d %s", c.desc, c.want, w.Code, w.Body.String())
		}
	}

	// apt 会对文件名里的 % 再编码一次（%3a → %253a），并且带着 ./ 请求
	deb := testutil.File{Name: "libfoo_1%3a2.0_armhf.deb", Body: "deb-v1"}
	if w := uploadPackageRaw(t, h, depsBundle("bookworm", append(index, deb)...)); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"codename":"bookworm"`) || !strings.Contains(w.Body.String(), `"packages":1`) {
		t.Fatalf("依赖包应上传成功：%d %s", w.Code, w.Body.String())
	}
	if w := get("/apt/bookworm/./Packages"); w.Code != http.StatusOK || w.Body.String() != "Package: libfoo\n" {
		t.Fatalf("应提供 apt 索引：%d %q", w.Code, w.Body.String())
	}
	if w := get("/apt/bookworm/./libfoo_1%253a2.0_armhf.deb"); w.Code != http.StatusOK || w.Body.String() != "deb-v1" {
		t.Fatalf("应提供 .deb：%d %q", w.Code, w.Body.String())
	}
	for _, p := range []string{"/apt/bookworm/../../state.json", "/apt/bookworm/missing.deb", "/apt/trixie/Packages"} {
		if w := get(p); w.Code != http.StatusNotFound {
			t.Errorf("%s 应 404：%d", p, w.Code)
		}
	}

	// 重新上传整体替换，旧文件不残留
	if w := uploadPackageRaw(t, h, depsBundle("bookworm", append(index, testutil.File{Name: "libbar_1_armhf.deb", Body: "bar"})...)); w.Code != http.StatusOK {
		t.Fatalf("重新上传失败：%d %s", w.Code, w.Body.String())
	}
	if w := get("/apt/bookworm/libfoo_1%253a2.0_armhf.deb"); w.Code != http.StatusNotFound {
		t.Fatalf("替换后旧包应消失：%d", w.Code)
	}
	if w := do(t, h, adminReq("GET", "/api/v1/admin/deps", nil), http.StatusOK); !strings.Contains(w.Body.String(), `"codename":"bookworm"`) {
		t.Fatalf("列表应有 bookworm：%s", w.Body.String())
	}
	if w := do(t, h, adminReq("GET", "/api/v1/admin/package", nil), http.StatusOK); w.Body.String() != "null\n" {
		t.Fatalf("依赖包不应被当成程序包：%s", w.Body.String())
	}
	do(t, h, adminReq("DELETE", "/api/v1/admin/deps/bookworm", nil), http.StatusNoContent)
	if w := get("/apt/bookworm/Packages"); w.Code != http.StatusNotFound {
		t.Fatalf("删除后应 404：%d", w.Code)
	}

	// HTTP 装机端口上没有仓库（跳到 HTTPS）；一键安装脚本带上证书，apt 只信任它
	boot := s.BootstrapHandler()
	w := httptest.NewRecorder()
	boot.ServeHTTP(w, httptest.NewRequest("GET", "http://10.0.0.5:9000/apt/bookworm/Packages", nil))
	if w.Code != http.StatusMovedPermanently {
		t.Fatalf("HTTP 端口不提供仓库：%d", w.Code)
	}
	w = httptest.NewRecorder()
	boot.ServeHTTP(w, httptest.NewRequest("GET", "http://10.0.0.5:9000/install.sh", nil))
	if script := w.Body.String(); !strings.Contains(script, "-----BEGIN CERTIFICATE-----") || !strings.Contains(script, `SERVER_CERT="$tmp/server.crt"`) {
		t.Fatalf("install.sh 应带上服务端证书：\n%s", script)
	}
}
