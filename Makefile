# Makefile for the trading-bot toolkit.
#
# Positional-style invocation — write commands like prose, not flag soup:
#
#   make analyze 15m
#   make validate BTC long 74500 15m
#   make backtest 30 15m
#
# Service control on the VPS is not here: it is `ssh trading-bot ops ...`
# (scripts/ops), so one script owns it and only that script changes when the
# services move into containers.
#
# Run from this directory (trading-bot/).

SHELL    := /bin/bash
GO       := go
# gRPC MarketData (see docs/grpc.md). GRPC_BIND is read by cmd/web at
# startup; unset there means the gRPC listener is simply not created.
GRPC_BIND ?= 127.0.0.1:9090
PROTO_DIR := proto
PROTO_SRC := marketdata/v1/marketdata.proto
# protoc plugins install into GOPATH/bin, which is not always on PATH.
GOBIN     := $(shell $(GO) env GOPATH)/bin


# VPS deployment target. Three ways to set, in priority order:
#   1. `Makefile.local` in this directory (gitignored) — see Makefile.local.example
#   2. `ORACLE_HOST=1.2.3.4 make deploy-web` on the command line
#   3. `export ORACLE_HOST=1.2.3.4` in your shell rc
# The placeholders below are only used when none of the above is set.
-include Makefile.local
ORACLE_HOST ?= your.vps.ip
ORACLE_USER ?= ubuntu
ORACLE_KEY  ?= $(HOME)/.ssh/vps-key
SSH         := ssh -i $(ORACLE_KEY) $(ORACLE_USER)@$(ORACLE_HOST)
SCP         := scp -i $(ORACLE_KEY)

# Default target: print usage.
.DEFAULT_GOAL := help

# Catch-all so extra positional tokens (BTC, long, 74500, 15m, …) don't
# fail as missing targets.
%:
	@:

.PHONY: help analyze validate backtest proto proto-tools grpc grpc-probe test test-race vet lint check guard-clean deploy-web deploy-monitor deploy-all deploy-ops mcp-build mcp-install mcp-sync-journal

help:
	@echo "trading — Makefile commands"
	@echo
	@echo "  make check                                vet + test-race + lint (pre-commit gate)"
	@echo "  make test / test-race / vet / lint        Run one gate on its own"
	@echo
	@echo "  make analyze [TF]                         Snapshot all 4 symbols (default 1h)"
	@echo "  make validate SYM SIDE ENTRY [TF]         Score a proposed trade"
	@echo "  make backtest [DAYS] [TF]                 Run backtest (default 60d 1h)"
	@echo
	@echo "gRPC MarketData (docs/grpc.md):"
	@echo "  make proto-tools                          Install protoc-gen-go + protoc-gen-go-grpc"
	@echo "  make proto                                Regenerate proto/marketdata/v1/*.pb.go"
	@echo "  make grpc [BIND]                          Run cmd/web with the gRPC listener on (default $(GRPC_BIND))"
	@echo "  make grpc-probe [BIND]                    grpcurl smoke test of both RPCs against a running server"
	@echo
	@echo "Deploy to the VPS (refuses a dirty working tree; FORCE=1 overrides):"
	@echo "  make deploy-web                           Build web UI for linux/amd64, scp, restart trading-web"
	@echo "  make deploy-monitor                       Build monitor for linux/amd64, scp, restart trading-monitor"
	@echo "  make deploy-all                           Redeploy the CLI tools (analyze/validate/backtest/price/journal)"
	@echo "  make deploy-ops                           Install scripts/ops as /usr/local/bin/ops (asks for sudo)"
	@echo
	@echo "Operate the VPS (scripts/ops, over SSH):"
	@echo "  ssh trading-bot ops status                Units + monitor heartbeat"
	@echo "  ssh trading-bot ops logs monitor -f       Follow the monitor journal"
	@echo "  ssh trading-bot ops restart monitor|web   Restart a service"
	@echo "  ssh trading-bot ops zone [N]              Recent zone-alert events"
	@echo
	@echo "MCP server (docs/MCP_SETUP.md):"
	@echo "  make mcp-install / mcp-sync-journal       Build + install cmd/mcp; pull journal.csv"

# ---- analyze: optional [TF]
analyze:
	@TF=$(word 2,$(MAKECMDGOALS)); \
	if [ -z "$$TF" ]; then $(GO) run ./cmd/analyze; \
	else $(GO) run ./cmd/analyze -tf=$$TF; fi

# ---- validate: SYM SIDE ENTRY [TF]
validate:
	@SYM=$(word 2,$(MAKECMDGOALS)); \
	SIDE=$(word 3,$(MAKECMDGOALS)); \
	ENTRY=$(word 4,$(MAKECMDGOALS)); \
	TF=$(word 5,$(MAKECMDGOALS)); \
	if [ -z "$$TF" ]; then TF=1h; fi; \
	if [ -z "$$SYM" ] || [ -z "$$SIDE" ] || [ -z "$$ENTRY" ]; then \
		echo "usage: make validate SYM SIDE ENTRY [TF]"; \
		echo "       make validate BTC long 74500 15m"; \
		exit 2; \
	fi; \
	$(GO) run ./cmd/validate -symbol=$$SYM -side=$$SIDE -entry=$$ENTRY -tf=$$TF

# ---- backtest: optional [DAYS] [TF]
backtest:
	@DAYS=$(word 2,$(MAKECMDGOALS)); \
	TF=$(word 3,$(MAKECMDGOALS)); \
	if [ -z "$$DAYS" ]; then DAYS=60; fi; \
	if [ -z "$$TF" ]; then TF=1h; fi; \
	$(GO) run ./cmd/backtest -tf=$$TF -days=$$DAYS -fee-bps=6 -sweep-only

# ---- gRPC MarketData ------------------------------------------------------
# Generated .pb.go files are COMMITTED, so neither a build nor CI needs protoc.
# These targets are only for the day the .proto changes.

proto-tools:
	@echo "▶ installing protoc plugins into $(GOBIN)..."
	@$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
	@$(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
	@echo "✅ $$($(GOBIN)/protoc-gen-go --version), $$($(GOBIN)/protoc-gen-go-grpc --version)"
	@command -v protoc >/dev/null || echo "⚠ protoc itself is missing — brew install protobuf"

proto:
	@command -v protoc >/dev/null || { echo "protoc not found — brew install protobuf"; exit 2; }
	@test -x $(GOBIN)/protoc-gen-go || { echo "protoc-gen-go not found — make proto-tools"; exit 2; }
	@test -x $(GOBIN)/protoc-gen-go-grpc || { echo "protoc-gen-go-grpc not found — make proto-tools"; exit 2; }
	@PATH="$(GOBIN):$$PATH" protoc \
		--proto_path=$(PROTO_DIR) \
		--go_out=$(PROTO_DIR) --go_opt=paths=source_relative \
		--go-grpc_out=$(PROTO_DIR) --go-grpc_opt=paths=source_relative \
		$(PROTO_SRC)
	@$(GO) build ./$(PROTO_DIR)/... && echo "✅ regenerated $(PROTO_DIR)/marketdata/v1/*.pb.go"

# ---- grpc: run the web process with the gRPC listener on. Optional [BIND].
grpc:
	@BIND=$(word 2,$(MAKECMDGOALS)); \
	if [ -z "$$BIND" ]; then BIND=$(GRPC_BIND); fi; \
	echo "▶ cmd/web with GRPC_BIND=$$BIND (HTTP still on WEB_BIND / :8080)"; \
	GRPC_BIND=$$BIND $(GO) run ./cmd/web

# ---- grpc-probe: prove both RPCs over the wire. Needs a server already up.
# Uses server reflection, so no .proto file is passed.
grpc-probe:
	@command -v grpcurl >/dev/null || { echo "grpcurl not found — brew install grpcurl"; exit 2; }
	@BIND=$(word 2,$(MAKECMDGOALS)); \
	if [ -z "$$BIND" ]; then BIND=$(GRPC_BIND); fi; \
	echo "▶ list:"; \
	grpcurl -plaintext $$BIND list tradingbot.marketdata.v1.MarketData; \
	echo "▶ GetMarkPrice BTC:"; \
	grpcurl -plaintext -d '{"symbol":"BTC"}' $$BIND tradingbot.marketdata.v1.MarketData/GetMarkPrice; \
	echo "▶ StreamMarkPrices BTC,ETH (first 3 messages):"; \
	grpcurl -plaintext -d '{"symbols":["BTC","ETH"]}' $$BIND tradingbot.marketdata.v1.MarketData/StreamMarkPrices | head -24

# ---- quality gates -------------------------------------------------------
# `test-race` is the one that earns its keep. 13 goroutine start points live
# outside the tests, and only ONE of them (cmd/web/stream.go — the SSE hub) has
# a test that drives it concurrently. So a green -race run says "the concurrent
# paths the tests actually exercise are clean", NOT "this codebase is race
# free". Read the result that way; the other 12 sites are where a race would
# still be hiding.
test:
	@$(GO) test ./...

test-race:
	@$(GO) test -race ./...

vet:
	@$(GO) vet ./...

lint:
	@golangci-lint run ./...

# One command before a commit or in CI.
check: vet test-race lint
	@echo "✅ vet + race + lint all clean"

# ---- guard-clean: deploy only what is committed ---------------------------
# A deploy compiles the working tree, so it ships whatever is lying in it —
# including another session's uncommitted or untracked code. Refuse when
# anything that ends up in a binary is dirty; commit it, deploy from a clean
# `git worktree`, or override with FORCE=1.
guard-clean:
	@if [ -z "$(FORCE)" ]; then \
		dirty=$$(git status --porcelain --untracked-files=all -- '*.go' go.mod go.sum cmd/web/templates cmd/web/static); \
		if [ -n "$$dirty" ]; then \
			echo "✋ uncommitted changes would ship in this binary:"; \
			echo "$$dirty" | sed 's/^/   /'; \
			echo "   commit them, deploy from a clean worktree, or re-run with FORCE=1"; \
			exit 1; \
		fi; \
	fi

# ---- deploy-web / deploy-monitor: cross-compile, scp, restart the unit -----
deploy-web: guard-clean
	@echo "▶ building linux/amd64 web..."
	@GOOS=linux GOARCH=amd64 $(GO) build -o /tmp/trading-web-linux ./cmd/web
	@echo "▶ uploading to $(ORACLE_HOST)..."
	@$(SCP) /tmp/trading-web-linux $(ORACLE_USER)@$(ORACLE_HOST):/tmp/trading-web
	@echo "▶ installing + restarting trading-web unit..."
	@$(SSH) 'mv /tmp/trading-web /opt/trading/trading-web && chown ubuntu:ubuntu /opt/trading/trading-web && sudo systemctl restart trading-web && sleep 1 && systemctl status trading-web --no-pager | head -5'

deploy-monitor: guard-clean
	@echo "▶ building linux/amd64 monitor..."
	@CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -o /tmp/trading-monitor-linux ./cmd/monitor
	@echo "▶ uploading to $(ORACLE_HOST)..."
	@$(SCP) /tmp/trading-monitor-linux $(ORACLE_USER)@$(ORACLE_HOST):/tmp/trading-monitor
	@echo "▶ installing + restarting trading-monitor unit..."
	@$(SSH) 'mv /tmp/trading-monitor /opt/trading/trading-monitor && chown ubuntu:ubuntu /opt/trading/trading-monitor && chmod +x /opt/trading/trading-monitor && sudo systemctl restart trading-monitor && sleep 1 && systemctl status trading-monitor --no-pager | head -5'

# ---- deploy-all: the CLI tools only ---------------------------------------
# Restarts nothing. It used to also ship cmd/serve and restart the retired
# trading-bot unit, which would have brought it back up beside the monitor.
deploy-all: guard-clean
	@echo "▶ building linux/amd64 CLI tools..."
	@GOOS=linux GOARCH=amd64 $(GO) build -o /tmp/trading-analyze-linux  ./cmd/analyze
	@GOOS=linux GOARCH=amd64 $(GO) build -o /tmp/trading-validate-linux ./cmd/validate
	@GOOS=linux GOARCH=amd64 $(GO) build -o /tmp/trading-backtest-linux ./cmd/backtest
	@GOOS=linux GOARCH=amd64 $(GO) build -o /tmp/trading-price-linux    ./cmd/price
	@GOOS=linux GOARCH=amd64 $(GO) build -o /tmp/trading-journal-linux  ./cmd/journal
	@echo "▶ uploading to $(ORACLE_HOST)..."
	@$(SCP) /tmp/trading-analyze-linux /tmp/trading-validate-linux /tmp/trading-backtest-linux /tmp/trading-price-linux /tmp/trading-journal-linux $(ORACLE_USER)@$(ORACLE_HOST):/tmp/
	@echo "▶ installing..."
	@$(SSH) 'mv /tmp/trading-analyze-linux  /opt/trading/trading-analyze  && \
	          mv /tmp/trading-validate-linux /opt/trading/trading-validate && \
	          mv /tmp/trading-backtest-linux /opt/trading/trading-backtest && \
	          mv /tmp/trading-price-linux    /opt/trading/trading-price    && \
	          mv /tmp/trading-journal-linux  /opt/trading/trading-journal  && \
	          chown ubuntu:ubuntu /opt/trading/trading-* && echo "✅ CLI tools installed"'

# ---- deploy-ops: install the operator CLI --------------------------------
# Into /usr/local/bin so a plain `ssh trading-bot ops status` finds it — a
# non-login SSH command never sees ~/.local/bin. Root-owned on purpose: an
# executable on PATH that the ubuntu user could rewrite would let anyone who
# gets that account change what `ops` does the next time it is run. -t gives
# sudo a terminal for the password.
deploy-ops:
	@$(SCP) scripts/ops $(ORACLE_USER)@$(ORACLE_HOST):/tmp/ops
	@$(SSH) -t 'sudo install -m 755 -o root -g root /tmp/ops /usr/local/bin/ops && rm -f /tmp/ops ~/.local/bin/ops && echo "✅ /usr/local/bin/ops" && ls -l /usr/local/bin/ops'

# ---- MCP server for Claude Code (zero-cost AI advisor path) ----
#
# Builds cmd/mcp and installs to ~/bin/trading-bot-mcp so Claude Code
# can launch it as a stdio subprocess. After install, register in your
# Claude Code config — see docs/MCP_SETUP.md.
#
# Uses CGO_ENABLED=0 so the binary works on either a vanilla mac or
# any future linux deployment without runtime cgo deps.
mcp-build:
	@echo "▶ building trading-bot MCP server..."
	@CGO_ENABLED=0 $(GO) build -o /tmp/trading-bot-mcp ./cmd/mcp

mcp-install: mcp-build
	@mkdir -p $(HOME)/bin
	@mv /tmp/trading-bot-mcp $(HOME)/bin/trading-bot-mcp
	@chmod +x $(HOME)/bin/trading-bot-mcp
	@echo "✓ installed: $(HOME)/bin/trading-bot-mcp"
	@echo ""
	@echo "Next steps (one-time):"
	@echo "  1. mkdir -p ~/trading-bot-data && scp -i ~/.ssh/oracle-trading.key \\"
	@echo "       ubuntu@<your-vps-ip>:/opt/trading/journal.csv \\"
	@echo "       ~/trading-bot-data/journal.csv"
	@echo "  2. Register the server in your Claude Code config — see docs/MCP_SETUP.md"
	@echo "  3. Restart 'claude' to pick up the new tools"

mcp-sync-journal:
	@mkdir -p $(HOME)/trading-bot-data
	@$(SCP) $(ORACLE_USER)@$(ORACLE_HOST):/opt/trading/journal.csv $(HOME)/trading-bot-data/journal.csv
	@echo "✓ journal.csv synced to ~/trading-bot-data/journal.csv"
