package server

import (
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/izzln/content-edge-display/internal/manifest"
)

// 离线依赖包（scripts/build-deps.sh 产出的 display-deps-<代号>-armhf.tar.gz）：设备播放所需的 Debian 软件包
// 连同全部下层依赖，外加 apt 索引。后台与程序包从同一个入口上传，解到 data/deps/<代号>/；设备的 update.sh（首次安装
// 与每次 OTA）把 HTTPS 端口的 /apt/<代号>/ 当作 apt 仓库，只信任服务端证书，从局域网安装，不访问外网。
// 里面都是公开的 Debian 软件包，不需要口令。

// depsCodenameFile 是依赖包里标明 Debian 版本代号的文件，也是区分依赖包与程序包的标志。
const depsCodenameFile = "CODENAME"

var codenamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// DepsRepo 是一个已上传的离线依赖包。
type DepsRepo struct {
	Codename   string    `json:"codename"`
	Packages   int       `json:"packages"` // .deb 个数
	Size       int64     `json:"size"`
	UploadedAt time.Time `json:"uploaded_at"`
}

func isDepsBundle(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, depsCodenameFile))
	return err == nil
}

// installDeps 校验解开的依赖包 dir，并整体替换 data/deps/<代号>/（dir 被移走）。
func (s *Server) installDeps(dir string) (DepsRepo, error) {
	b, _ := os.ReadFile(filepath.Join(dir, depsCodenameFile))
	codename := strings.TrimSpace(string(b))
	if !codenamePattern.MatchString(codename) {
		return DepsRepo{}, errBadRequest("依赖包里的 CODENAME %q 无效", codename)
	}
	for _, f := range []string{"Packages", "Release"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return DepsRepo{}, errBadRequest("依赖包缺少 %s——请上传 scripts/build-deps.sh 产出的包", f)
		}
	}
	// 先把旧的挪开再换上新的：装机脚本任何时候看到的都是一个完整的仓库
	dst := filepath.Join(s.depsDir(), codename)
	trash, err := os.MkdirTemp(s.incomingDir(), "old-deps-")
	if err != nil {
		return DepsRepo{}, err
	}
	defer os.RemoveAll(trash)
	old := filepath.Join(trash, codename)
	if err := os.Rename(dst, old); err != nil && !os.IsNotExist(err) {
		return DepsRepo{}, err
	}
	if err := os.Rename(dir, dst); err != nil {
		os.Rename(old, dst)
		return DepsRepo{}, err
	}
	return depsRepo(dst)
}

func depsRepo(dir string) (DepsRepo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return DepsRepo{}, err
	}
	r := DepsRepo{Codename: filepath.Base(dir)}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			continue
		}
		r.Size += fi.Size()
		if strings.HasSuffix(e.Name(), ".deb") {
			r.Packages++
		}
		if e.Name() == depsCodenameFile {
			r.UploadedAt = fi.ModTime()
		}
	}
	return r, nil
}

func (s *Server) handleListDeps(w http.ResponseWriter, r *http.Request) {
	out := []DepsRepo{}
	entries, _ := os.ReadDir(s.depsDir())
	for _, e := range entries {
		if e.IsDir() && codenamePattern.MatchString(e.Name()) {
			if repo, err := depsRepo(filepath.Join(s.depsDir(), e.Name())); err == nil {
				out = append(out, repo)
			}
		}
	}
	slices.SortFunc(out, func(a, b DepsRepo) int { return strings.Compare(a.Codename, b.Codename) })
	writeJSON(w, out)
}

func (s *Server) handleDeleteDeps(w http.ResponseWriter, r *http.Request) {
	codename := r.PathValue("codename")
	if !codenamePattern.MatchString(codename) {
		http.NotFound(w, r)
		return
	}
	if err := os.RemoveAll(filepath.Join(s.depsDir(), codename)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleApt 提供 /apt/<代号>/<文件>。apt 的平铺仓库会请求 /apt/<代号>/./<文件>，
// 而 ServeMux 会把带 . 的路径重定向，所以它不经路由、在 Handler 里直接处理。
func (s *Server) handleApt(w http.ResponseWriter, r *http.Request) {
	codename, file, ok := strings.Cut(strings.TrimPrefix(path.Clean(r.URL.Path), "/apt/"), "/")
	if r.Method != http.MethodGet && r.Method != http.MethodHead || !ok ||
		!codenamePattern.MatchString(codename) || !manifest.SafeFileName(file) {
		http.NotFound(w, r)
		return
	}
	if file == "Packages" {
		log.Printf("install: dependency repo %s used by %s", codename, clientIP(r))
	}
	http.ServeFile(w, r, filepath.Join(s.depsDir(), codename, file))
}
