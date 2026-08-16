GO ?= go
INSTALL ?= install
PREFIX ?= /usr/local
BINDIR ?= $(PREFIX)/bin
DESTDIR ?=
BUILD_DIR ?= .build

BINARY := $(BUILD_DIR)/air
GO_SOURCES := $(wildcard *.go)

.PHONY: all build test install

all: build

build: $(BINARY)

$(BINARY): $(GO_SOURCES) go.mod go.sum
	mkdir -p "$(dir $@)"
	$(GO) build -o "$@" .

test:
	$(GO) test ./...

install: $(BINARY)
	$(INSTALL) -d "$(DESTDIR)$(BINDIR)"
	$(INSTALL) -m 0755 "$(BINARY)" "$(DESTDIR)$(BINDIR)/air"
