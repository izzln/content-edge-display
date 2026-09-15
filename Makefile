VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -s -w -X github.com/izzln/content-edge-display/internal/agent.Version=$(VERSION)

.PHONY: build agent-arm test clean

build:
	go build -ldflags="$(LDFLAGS)" -o bin/display-server ./cmd/display-server
	go build -ldflags="$(LDFLAGS)" -o bin/display-agent ./cmd/display-agent

# Orange Pi One (全志 H3, ARMv7) 交叉编译；产物可直接在管理后台"固件"页上传下发
agent-arm:
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" \
		-o bin/display-agent-armv7 ./cmd/display-agent
	@echo "built bin/display-agent-armv7 version=$(VERSION)"

test:
	go vet ./...
	go test ./...

clean:
	rm -rf bin
