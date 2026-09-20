VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -s -w -X github.com/izzln/content-edge-display/internal/agent.Version=$(VERSION)
HOST_ARCH = $(shell go env GOARCH)

.PHONY: build agent-arm package package-agent package-server test clean

build:
	go build -ldflags="$(LDFLAGS)" -o bin/display-server ./cmd/display-server
	go build -ldflags="$(LDFLAGS)" -o bin/display-agent ./cmd/display-agent

# Orange Pi One (全志 H3, ARMv7) 交叉编译；产物也是管理后台"程序更新"页上传的文件
agent-arm:
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" \
		-o bin/display-agent-armv7 ./cmd/display-agent
	@echo "→ bin/display-agent-armv7 (version=$(VERSION))"

# 打成品包：把二进制与对应角色的部署资产合成一个自包含的压缩包，
# 部署时只需拷这一个文件到目标机器，不必再从 bin/ 和 deploy/ 里手工挑文件。
package: package-agent package-server

package-agent: agent-arm
	@rm -rf bin/stage && mkdir -p bin/stage/display-agent-$(VERSION)
	@cp bin/display-agent-armv7 deploy/agent/* bin/stage/display-agent-$(VERSION)/
	@# 校验包是自包含的：install-agent.sh 引用的同目录文件必须都在包里
	@for f in $$(grep -o '$$HERE/[A-Za-z0-9._-]*' deploy/agent/install-agent.sh | sed 's|$$HERE/||' | sort -u); do \
		test -f bin/stage/display-agent-$(VERSION)/$$f || \
			{ echo "打包失败：install-agent.sh 需要 $$f，但包里没有"; exit 1; }; \
	done
	@tar -czf bin/display-agent-$(VERSION)-armv7.tar.gz -C bin/stage display-agent-$(VERSION)
	@rm -rf bin/stage
	@echo "→ bin/display-agent-$(VERSION)-armv7.tar.gz (设备端，解开即可安装)"

package-server: build
	@rm -rf bin/stage && mkdir -p bin/stage/display-server-$(VERSION)
	@cp bin/display-server deploy/server/* bin/stage/display-server-$(VERSION)/
	@tar -czf bin/display-server-$(VERSION)-$(HOST_ARCH).tar.gz -C bin/stage display-server-$(VERSION)
	@rm -rf bin/stage
	@echo "→ bin/display-server-$(VERSION)-$(HOST_ARCH).tar.gz (服务端)"

test:
	go vet ./...
	go test ./...

clean:
	rm -rf bin
