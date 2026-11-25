# The one place where flatpark-spawn's names are defined.
#
# `make gen` writes them into internal/names/names.go (Go, committed so that
# `go vet`/`go test` work without make) and .build/gen/names.h (C); the shell
# templates in runtime/*.in are filled at build time. Code never spells these
# out. Manifests and documentation are written by hand and name them literally.

NAME        := flatpark-spawn
MODULE      := github.com/flatpark/flatpark-spawn
ENV_PREFIX  := FLATPARK_SPAWN
LIB         := lib$(NAME).so
EXT_ID      := org.flatpark.Spawn
# Extension branch = ABI major. Consumers declare `version: '1'`.
EXT_BRANCH  := 1
MOUNT       := /app/lib/$(NAME)
# The basename that switches on 1player/host-spawn's command line.
COMPAT_NAME := host-spawn

# Sandbox-side lookup cache, below $XDG_RUNTIME_DIR/app/$FLATPAK_ID/$(NAME)/,
# one directory per working directory: $(CACHE_CWD)/<fnv64 of the cwd>/.
CACHE_CWD   := d
CACHE_HOST  := host
CACHE_NONE  := none
# Shims, made by enable.sh at app start next to the cache:
# $XDG_RUNTIME_DIR/app/$FLATPAK_ID/$(NAME)/$(SHIM_DIR)/<name>.
SHIM_DIR    := shims
# Host-side login PATH cache (one file per working directory), below the
# host's $XDG_RUNTIME_DIR/$(NAME)/.
HOST_PATH_CACHE := login-path.d

# Internal protocol between the preload library, the command and its host
# agent. Not a user interface.
MODE_PROBE  := --x-probe
MODE_EXEC   := --x-exec
MODE_LOOKUP := --x-lookup
MODE_SHELL  := --x-shell
MODE_SOCKET := --x-socket
MODE_SHIMS  := --x-shims
MODE_AGENT  := --x-agent
