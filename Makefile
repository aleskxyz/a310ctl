VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO      ?= go
PREFIX  ?= /usr/local
SBINDIR ?= $(PREFIX)/sbin
UNITDIR ?= /etc/systemd/system
CONFDIR ?= /etc/a310ctl
DESTDIR ?=

a310ctl: $(shell find . -name '*.go') go.mod go.sum
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $@ ./cmd/a310ctl

test:
	$(GO) vet ./...
	$(GO) test ./...

# Does not build: run `make` as your user first, so Go is never needed as root.
install:
	@test -x a310ctl || { echo "a310ctl not built: run 'make' first"; exit 1; }
	install -D -m 0755 a310ctl $(DESTDIR)$(SBINDIR)/a310ctl
	install -d $(DESTDIR)$(UNITDIR)
	sed -e 's|/usr/sbin/a310ctl|$(SBINDIR)/a310ctl|' -e 's|/etc/a310ctl/config.yaml|$(CONFDIR)/config.yaml|' \
		packaging/a310ctl.service > $(DESTDIR)$(UNITDIR)/a310ctl.service
	[ -e $(DESTDIR)$(CONFDIR)/config.yaml ] || install -D -m 0644 packaging/config.example.yaml $(DESTDIR)$(CONFDIR)/config.yaml
	[ -n "$(DESTDIR)" ] || { systemctl daemon-reload && systemctl enable a310ctl.service && systemctl restart a310ctl.service; }

uninstall:
	-[ -n "$(DESTDIR)" ] || systemctl disable --now a310ctl.service
	rm -f $(DESTDIR)$(SBINDIR)/a310ctl $(DESTDIR)$(UNITDIR)/a310ctl.service
	[ -n "$(DESTDIR)" ] || systemctl daemon-reload

clean:
	rm -f a310ctl

.PHONY: test install uninstall clean
