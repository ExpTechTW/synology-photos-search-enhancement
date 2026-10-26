# make            這台電腦用的 sse（build/sse）
# make nas        NAS 用的 sse：build/sse-linux-{amd64,arm64,armv7}
# make release    跟 GitHub 上發佈的一樣的檔案（dist/）；VERSION 預設是今天的日期
# make test       Go 的單元測試
VERSION ?= 0.0.0+dev
MODULE := github.com/ExpTechTW/synology-photos-search-enhancement
LDFLAGS = -s -w -X $(MODULE)/internal/version.Version=$(VERSION)
SOURCES := $(shell find cmd internal -name '*.go' -o -name '*.yml' -o -name '*.service' -o -name '*.path' -o -name '*.timer') go.mod go.sum
GOBUILD = CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)'

build/sse: $(SOURCES)
	$(GOBUILD) -o $@ ./cmd/sse

build/sse-linux-amd64: $(SOURCES)
	GOOS=linux GOARCH=amd64 $(GOBUILD) -o $@ ./cmd/sse

build/sse-linux-arm64: $(SOURCES)
	GOOS=linux GOARCH=arm64 $(GOBUILD) -o $@ ./cmd/sse

build/sse-linux-armv7: $(SOURCES)
	GOOS=linux GOARCH=arm GOARM=7 $(GOBUILD) -o $@ ./cmd/sse

.PHONY: nas test release
nas: build/sse-linux-amd64 build/sse-linux-arm64 build/sse-linux-armv7

test:
	go test ./...

# release 的版本格式是 vYYYY.MM.DD+build.N；沒指定就用今天的日期、build 0（正式的版本號由 CI 決定）
release:
	sh scripts/package.sh v$(if $(filter 0.0.0+dev,$(VERSION)),$(shell date -u +%Y.%m.%d)+build.0,$(VERSION)) dist
