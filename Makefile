# make            本機的 sse（build/sse）
# make release    各平台的 release 檔（dist/）
# make test       Go 的單元測試
VERSION ?= 0.0.0+dev
LDFLAGS := -s -w -X github.com/ExpTechTW/synology-photos-search-enhancement/internal/version.Version=$(VERSION)

build/sse: $(shell find cmd internal -name '*.go' -o -name '*.yml' -o -name '*.service' -o -name '*.path' -o -name '*.timer') go.mod go.sum
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $@ ./cmd/sse

.PHONY: test release
test:
	go test ./...

release:
	sh scripts/package.sh v$(VERSION) dist
