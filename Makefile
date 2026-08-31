GO ?= go
INSTALL ?= install
PREFIX ?= /usr/local
BINDIR ?= $(PREFIX)/bin
MANDIR ?= $(PREFIX)/share/man
DESTDIR ?=
BUILD_DIR ?= .build

BINARY := $(BUILD_DIR)/air
MANPAGE := man/air.1
GO_SOURCES := $(wildcard *.go)

.PHONY: all build test install

all: build

build: $(BINARY)

$(BINARY): $(GO_SOURCES) go.mod go.sum
	mkdir -p "$(dir $@)"
	$(GO) build -o "$@" .

test:
	$(GO) test ./...

install: $(BINARY) $(MANPAGE)
	$(INSTALL) -d "$(DESTDIR)$(BINDIR)"
	$(INSTALL) -m 0755 "$(BINARY)" "$(DESTDIR)$(BINDIR)/air"
	$(INSTALL) -d "$(DESTDIR)$(MANDIR)/man1"
	$(INSTALL) -m 0644 "$(MANPAGE)" "$(DESTDIR)$(MANDIR)/man1/air.1"
