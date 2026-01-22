APP := xhark
DIST := dist

PLATFORMS := \
	darwin/amd64 \
	darwin/arm64 \
	linux/amd64 \
	linux/arm64

.PHONY: build

build:
	@mkdir -p $(DIST)
	@set -e; \
	for p in $(PLATFORMS); do \
	GOOS=$${p%/*}; GOARCH=$${p#*/}; \
	OUT="$(DIST)/$(APP)-$${GOOS}-$${GOARCH}"; \
	echo "==> building $$OUT"; \
	CGO_ENABLED=0 GOOS=$$GOOS GOARCH=$$GOARCH go build -trimpath -ldflags="-s -w" -o $$OUT ./cmd/xhark; \
	done
