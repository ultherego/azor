export CGO_ENABLED = 0
BIN = $(HOME)/.local/bin/azor
UNIT = $(HOME)/.config/systemd/user/azor.service

.PHONY: build test install uninstall
build:
	go build -trimpath -buildvcs=false -ldflags '-s -w -buildid=' -o azor .
test:
	test -z "$$(gofmt -l .)" && go vet ./... && go test ./...
install: test build
	install -Dm755 azor $(BIN)
	mkdir -p $(dir $(UNIT))
	sed 's|@BIN@|%h/.local/bin/azor|' azor.service > $(UNIT)
	systemctl --user daemon-reload
	systemctl --user enable azor.service
	systemctl --user restart azor.service
	$(BIN) install-skill
# Recorded data in ~/.local/state/azor is kept; remove it by hand if wanted.
uninstall:
	-systemctl --user disable --now azor.service
	rm -f $(UNIT) $(BIN)
	systemctl --user daemon-reload

.PHONY: install-system
install-system:
	install -Dm755 azor "$(DESTDIR)/usr/bin/azor"
	install -Dm644 azor.conf "$(DESTDIR)/etc/azor/azor.conf"
	install -d "$(DESTDIR)/usr/lib/systemd/user"
	sed 's|@BIN@|/usr/bin/azor|' azor.service > "$(DESTDIR)/usr/lib/systemd/user/azor.service"
	chmod 644 "$(DESTDIR)/usr/lib/systemd/user/azor.service"
	install -Dm644 README.md "$(DESTDIR)/usr/share/doc/azor/README.md"
	install -Dm644 SECURITY.md "$(DESTDIR)/usr/share/doc/azor/SECURITY.md"
	install -Dm644 claude/README.md "$(DESTDIR)/usr/share/doc/azor/claude/README.md"
	install -Dm644 claude/azor/SKILL.md "$(DESTDIR)/usr/share/doc/azor/claude/azor/SKILL.md"
