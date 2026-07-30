.PHONY: build agent-arm test clean

build:
	go build -o bin/display-server ./cmd/display-server
	go build -o bin/display-agent ./cmd/display-agent

# Orange Pi One (全志 H3, ARMv7) 交叉编译
agent-arm:
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags="-s -w" \
		-o bin/display-agent-armv7 ./cmd/display-agent

test:
	go vet ./...
	go test ./...

clean:
	rm -rf bin
