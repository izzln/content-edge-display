VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -s -w -X github.com/izzln/content-edge-display/internal/agent.Version=$(VERSION)
HOST_ARCH = $(shell go env GOARCH)

# 口令文件：本地生成一次后长期沿用，make clean 不会删它（只写进服务端包的 server.json；
# 设备端包不含口令，装机时由装机人员在一键装机命令里给出）。
SECRETS = .secrets/tokens.env

# CI 上不把真实口令打进包里：本仓库是公开的，Actions 产物和 Release 附件谁都能下载。
# 此时包里是占位值，服务端带着占位值会拒绝启动，提示用 make tokens 生成。
BAKE_TOKENS ?= $(if $(CI),0,1)

.PHONY: build agent-arm tokens package package-agent package-server test clean

build:
	go build -ldflags="$(LDFLAGS)" -o bin/display-server ./cmd/display-server
	go build -ldflags="$(LDFLAGS)" -o bin/display-agent ./cmd/display-agent

# Orange Pi One (全志 H3, ARMv7) 交叉编译（打进设备端整包，见 package-agent）
agent-arm:
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" \
		-o bin/display-agent-armv7 ./cmd/display-agent
	@echo "→ bin/display-agent-armv7 (version=$(VERSION))"

# 生成两个口令（8 位字母数字，含大小写与数字）。已存在则原样沿用。
tokens:
	@if [ ! -f $(SECRETS) ]; then \
		mkdir -p $(dir $(SECRETS)); \
		printf 'ADMIN_TOKEN=%s\nENROLL_TOKEN=%s\n' \
			"$$(sh scripts/gen-token.sh)" "$$(sh scripts/gen-token.sh)" > $(SECRETS); \
		chmod 600 $(SECRETS); \
		echo "已生成 $(SECRETS)——请妥善保管并勿提交："; \
		echo "  admin_token  登录管理后台用"; \
		echo "  enroll_token 新设备装机时输入的注册口令"; \
	fi
	@sed 's/^/  /' $(SECRETS)

# 打成品包：把二进制与对应角色的部署资产合成一个自包含的压缩包，
# 部署时只需拷这一个文件到目标机器，不必再从 bin/ 和 deploy/ 里手工挑文件。
package: package-agent package-server
	@if [ "$(BAKE_TOKENS)" = "1" ]; then echo "服务端包的 server.json 已写入 $(SECRETS) 里的口令"; \
	 else echo "注意：未写入口令（CI 构建），装机前需用 make tokens 生成并填进 server.json"; fi

# 设备端整包：既是装机包（install.sh 下载后执行 install-agent.sh），也是上传到后台的 OTA 包
# （设备解开后执行 update.sh）。程序和配套脚本一起更新、一起回滚。注册口令不进包：装机时由人提供。
package-agent: agent-arm
	@rm -rf bin/stage && mkdir -p bin/stage/display-agent-$(VERSION)
	@cp deploy/agent/* bin/stage/display-agent-$(VERSION)/
	@cp bin/display-agent-armv7 bin/stage/display-agent-$(VERSION)/display-agent
	@printf '%s\n' "$(VERSION)" > bin/stage/display-agent-$(VERSION)/VERSION
	@# 校验包是自包含的：脚本引用的同目录文件必须都在包里
	@for f in $$(grep -oh '$$HERE/[A-Za-z0-9._-]*' deploy/agent/install-agent.sh deploy/agent/update.sh | sed 's|$$HERE/||' | sort -u); do \
		test -f bin/stage/display-agent-$(VERSION)/$$f || \
			{ echo "打包失败：脚本需要 $$f，但包里没有"; exit 1; }; \
	done
	@tar -czf bin/display-agent-$(VERSION)-armv7.tar.gz -C bin/stage display-agent-$(VERSION)
	@rm -rf bin/stage
	@echo "→ bin/display-agent-$(VERSION)-armv7.tar.gz (设备端整包：后台「程序更新」上传它，装机与 OTA 都用它)"

package-server: build
	@rm -rf bin/stage && mkdir -p bin/stage/display-server-$(VERSION)
	@cp bin/display-server deploy/server/display-server.service deploy/server/INSTALL.md \
		bin/stage/display-server-$(VERSION)/
	@# 包里直接给一份填好口令的 server.json，装机时不用再想口令怎么定
	@if [ "$(BAKE_TOKENS)" = "1" ]; then \
		$(MAKE) --no-print-directory tokens >/dev/null; . ./$(SECRETS); \
	else ADMIN_TOKEN=change-me; ENROLL_TOKEN=change-me-too; fi; \
	sed -e "s|\"change-me\"|\"$$ADMIN_TOKEN\"|" -e "s|\"change-me-too\"|\"$$ENROLL_TOKEN\"|" \
		deploy/server/server.example.json > bin/stage/display-server-$(VERSION)/server.json
	@tar -czf bin/display-server-$(VERSION)-$(HOST_ARCH).tar.gz -C bin/stage display-server-$(VERSION)
	@rm -rf bin/stage
	@echo "→ bin/display-server-$(VERSION)-$(HOST_ARCH).tar.gz (服务端)"

test:
	go vet ./...
	go test ./...

# 只删构建产物；$(SECRETS) 不动（换了口令要同步改服务端 server.json）
clean:
	rm -rf bin
