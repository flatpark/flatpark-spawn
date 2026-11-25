include names.mk

VERSION ?= $(shell git describe --tags --match 'v*' --dirty 2>/dev/null | sed 's/^v//' || true)
ifeq ($(VERSION),)
VERSION := 0.0.0-dev
endif
ARCH    ?= $(shell uname -m)
GO      ?= go
GOFMT   ?= gofmt
CC      ?= gcc
BUILD   ?= .build
SOURCE_DATE_EPOCH ?= $(shell git log -1 --format=%ct 2>/dev/null || echo 0)
# Highest glibc symbol version libflatpark-spawn.so may need: it must load on
# the freedesktop 25.08 and 26.08 bases (and everything built on them).
GLIBC_MAX ?= 2.34

GOARCH_x86_64  := amd64
GOARCH_aarch64 := arm64
GOARCH := $(GOARCH_$(ARCH))

GEN_GO  := internal/names/names.go
GEN_H   := $(BUILD)/gen/names.h
BIN     := $(BUILD)/payload/bin/$(NAME)
SO      := $(BUILD)/payload/lib/$(LIB)
ENABLE  := $(BUILD)/payload/enable.sh
TARBALL := $(BUILD)/dist/$(NAME)-$(VERSION)-$(ARCH).tar.gz

GO_SRC  := $(shell find cmd internal -name '*.go' -not -name '*_test.go' 2>/dev/null) go.mod go.sum
C_SRC   := preload/preload.c preload/route.c preload/route.h
CFLAGS_ALL := -std=c11 -O2 -fPIC -Wall -Wextra -Werror -fvisibility=hidden \
	-D_FORTIFY_SOURCE=2 -I$(BUILD)/gen -Ipreload

SED_NAMES := sed -e 's|@NAME@|$(NAME)|g' -e 's|@ENV_PREFIX@|$(ENV_PREFIX)|g' \
	-e 's|@LIB@|$(LIB)|g' -e 's|@MOUNT@|$(MOUNT)|g' -e 's|@MODE_PROBE@|$(MODE_PROBE)|g' -e 's|@MODE_SHELL@|$(MODE_SHELL)|g' \
	-e 's|@CACHE_HOST@|$(CACHE_HOST)|g' -e 's|@CACHE_NONE@|$(CACHE_NONE)|g'

.PHONY: all gen check-gen build payload test lint e2e clean

all: lint test build

gen: $(GEN_GO) $(GEN_H)

$(GEN_GO): names.mk scripts/gen-names.sh
	mkdir -p $(dir $@)
	NAME='$(NAME)' MODULE='$(MODULE)' ENV_PREFIX='$(ENV_PREFIX)' LIB='$(LIB)' \
	EXT_ID='$(EXT_ID)' EXT_BRANCH='$(EXT_BRANCH)' MOUNT='$(MOUNT)' COMPAT_NAME='$(COMPAT_NAME)' \
	CACHE_CWD='$(CACHE_CWD)' CACHE_HOST='$(CACHE_HOST)' CACHE_NONE='$(CACHE_NONE)' HOST_PATH_CACHE='$(HOST_PATH_CACHE)' \
	MODE_PROBE='$(MODE_PROBE)' MODE_EXEC='$(MODE_EXEC)' MODE_LOOKUP='$(MODE_LOOKUP)' \
	MODE_SHELL='$(MODE_SHELL)' MODE_SOCKET='$(MODE_SOCKET)' MODE_AGENT='$(MODE_AGENT)' \
	./scripts/gen-names.sh go > $@.tmp && mv $@.tmp $@ && $(GOFMT) -w $@

$(GEN_H): names.mk scripts/gen-names.sh
	mkdir -p $(dir $@)
	NAME='$(NAME)' ENV_PREFIX='$(ENV_PREFIX)' LIB='$(LIB)' MOUNT='$(MOUNT)' \
	CACHE_CWD='$(CACHE_CWD)' CACHE_HOST='$(CACHE_HOST)' CACHE_NONE='$(CACHE_NONE)' \
	MODE_EXEC='$(MODE_EXEC)' MODE_LOOKUP='$(MODE_LOOKUP)' MODE_SHELL='$(MODE_SHELL)' \
	MODE_SOCKET='$(MODE_SOCKET)' \
	./scripts/gen-names.sh c > $@.tmp && mv $@.tmp $@

# Fails when names.mk and the committed Go file disagree (CI).
check-gen: $(GEN_H)
	@$(MAKE) -s -B $(GEN_GO) >/dev/null
	@git diff --quiet -- $(GEN_GO) || { echo "$(GEN_GO) is stale: run make gen and commit"; exit 1; }

build: $(BIN) $(SO) $(ENABLE)

$(BIN): $(GO_SRC) $(GEN_GO)
	mkdir -p $(dir $@)
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) $(GO) build -trimpath -buildvcs=false \
		-ldflags '-s -w -X $(MODULE)/internal/names.Version=$(VERSION)' -o $@ ./cmd/$(NAME)

$(SO): $(C_SRC) $(GEN_H)
	mkdir -p $(dir $@)
	$(CC) $(CFLAGS_ALL) -shared -Wl,-z,relro,-z,now -Wl,--as-needed -o $@ \
		preload/preload.c preload/route.c

$(ENABLE): runtime/enable.sh.in names.mk
	mkdir -p $(dir $@)
	$(SED_NAMES) $< > $@
	chmod 644 $@

# The release tarball: what extra-data downloads and apply_extra unpacks.
# Reproducible: sorted, fixed owner and mtime, gzip without a timestamp.
payload: $(TARBALL)

$(TARBALL): $(BIN) $(SO) $(ENABLE) LICENSE
	mkdir -p $(dir $@)
	cp LICENSE $(BUILD)/payload/LICENSE
	tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@$(SOURCE_DATE_EPOCH) \
		-C $(BUILD)/payload -cf - . | gzip -n -9 > $@.tmp && mv $@.tmp $@
	@echo "$@: $$(sha256sum $@ | cut -d' ' -f1) $$(stat -c %s $@)"

test: $(GEN_H)
	$(GO) test ./...

lint: $(GEN_H) build
	$(GO) vet ./...
	@out="$$($(GOFMT) -l cmd internal)"; \
	if [ -n "$$out" ]; then echo "gofmt -l reports:"; echo "$$out"; exit 1; fi
	./scripts/check-glibc.sh $(SO) $(GLIBC_MAX)
	@for f in $$(find . -path ./.git -prune -o -path ./.build -prune -o -path ./.pi-task -prune -o \
		\( -name '*.sh' -o -name '*.sh.in' \) -print) tests/e2e/fixtures/spawn-test $(ENABLE); do \
		sh -n "$$f" || exit 1; done; echo "sh -n: ok"

# Builds the payload, the extension and two consumer apps (freedesktop 26.08,
# GNOME 51), installs them from a temporary local remote, runs the matrix and
# removes everything it added.
e2e: payload
	TARBALL=$(TARBALL) ./tests/e2e/run.sh

clean:
	rm -rf $(BUILD)
