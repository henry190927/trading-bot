# Makefile for the trading-bot toolkit.
#
# Positional-style invocation — write commands like prose, not flag soup:
#
#   make analyze
#   make analyze 15m
#   make validate BTC long 74500 15m
#   make serve 15m
#   make serve 15m 4              # 15m timeframe, min-score=4
#   make serve-bg 15m
#   make serve-bg 15m 2           # detached, min-score=2 (more sensitive)
#   make stop
#   make logs
#   make backtest
#   make backtest 30 15m
#
# Run from this directory (trading-bot/).

SHELL    := /bin/bash
GO       := go
SERVE_BIN := /tmp/trading-serve
LOG      := $(HOME)/trading.log

# buildvcs=false: this Go module lives in a subdirectory of a parent
# (/myFirstGo) that is not itself a git repo. Without this flag, Go 1.24
# errors trying to read VCS metadata from the module root.
export GOFLAGS := -buildvcs=false

# VPS deployment target. Three ways to set, in priority order:
#   1. `Makefile.local` in this directory (gitignored) — see Makefile.local.example
#   2. `ORACLE_HOST=1.2.3.4 make deploy` on the command line
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

.PHONY: help analyze validate serve serve-bg stop logs backtest build deploy deploy-all deploy-web ssh remote-status remote-logs jopen jclose jlist jstats jupdate jdelete janchors tconfig tstart tstop trestart web-status web-logs web-restart deploy-monitor monitor-status monitor-logs monitor-restart

help:
	@echo "trading — Makefile commands"
	@echo
	@echo "  make analyze [TF]                         Snapshot all 4 symbols (default 1h)"
	@echo "  make validate SYM SIDE ENTRY [TF]         Score a proposed trade"
	@echo "  make serve [TF] [MIN_SCORE]               Run serve in foreground (default 1h, score 3)"
	@echo "  make serve-bg [TF] [MIN_SCORE]            Run serve detached in background"
	@echo "  make stop                                 Kill background serve"
	@echo "  make logs                                 Tail $(LOG) (Ctrl+C to exit)"
	@echo "  make backtest [DAYS] [TF]                 Run backtest (default 60d 1h)"
	@echo "  make build                                Rebuild the serve binary"
	@echo
	@echo "Trade journal (use on VPS for centralized records):"
	@echo "  make jopen SYM SIDE ENTRY STOP TP1 TP2 ANCHOR TF [NOTES] Record a newly opened position"
	@echo "  make jclose ID|SYM OUTCOME EXIT [NOTES]   Mark a trade closed (outcome: tp1/tp2/stop/manual/timeout)"
	@echo "  make jlist [N]                            Show recent trades"
	@echo "  make jstats                               WR / avgR by symbol / tf / anchor"
	@echo "  make jupdate ID=<n> SET='field=val ...'    Modify a journal entry (R auto-recomputed if closed)"
	@echo "                                            e.g. make jupdate ID=1 SET='entry=77.50 stop=78.20'"
	@echo "  make jdelete <id>                         Remove a journal entry"
	@echo "  make janchors                             Print recommended ANCHOR values"
	@echo
	@echo "Oracle VPS — daemon control:"
	@echo "  make tstart / tstop / trestart            Control the systemd daemon"
	@echo "  make tconfig TF=15m MS=3                  Change live daemon TF and/or MIN_SCORE, restart"
	@echo "  make tconfig TF=1h                        Change TF only, keep MIN_SCORE"
	@echo "  make deploy                               Build serve for linux/amd64, scp, restart systemd unit"
	@echo "  make deploy-all                           Same but also redeploy analyze/validate/backtest binaries"
	@echo "  make deploy-web                           Build web UI for linux/amd64, scp, restart web unit"
	@echo "  make web-status                           Web UI systemctl status"
	@echo "  make web-logs                             Tail web UI journal"
	@echo "  make web-restart                          Restart the web UI"
	@echo "  make ssh                                  SSH into the VPS"
	@echo "  make remote-status                        VPS daemon systemctl status"
	@echo "  make remote-logs                          Tail VPS journal (Ctrl+C exits)"
	@echo
	@echo "Examples:"
	@echo "  make analyze 15m"
	@echo "  make validate BTC long 74500 15m"
	@echo "  make validate XAG short 75.80 1h"
	@echo "  make serve-bg 15m"
	@echo "  make serve-bg 15m 2          (more sensitive: alert on score≥2)"
	@echo "  make backtest 30 15m"
	@echo "  make deploy                  (push code changes to Oracle VPS)"

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

# ---- serve foreground: optional [TF] [MIN_SCORE]
serve:
	@TF=$(word 2,$(MAKECMDGOALS)); \
	MS=$(word 3,$(MAKECMDGOALS)); \
	if [ -z "$$TF" ]; then TF=1h; fi; \
	if [ -z "$$MS" ]; then MS=3; fi; \
	$(GO) run ./cmd/serve -tf=$$TF -min-score=$$MS -sweep-only

# ---- serve background: kill any running, rebuild, relaunch detached
serve-bg:
	@TF=$(word 2,$(MAKECMDGOALS)); \
	MS=$(word 3,$(MAKECMDGOALS)); \
	if [ -z "$$TF" ]; then TF=1h; fi; \
	if [ -z "$$MS" ]; then MS=3; fi; \
	pkill -f "$(SERVE_BIN)" 2>/dev/null; sleep 1; \
	$(GO) build -o $(SERVE_BIN) ./cmd/serve; \
	nohup $(SERVE_BIN) -tf=$$TF -min-score=$$MS -sweep-only >> $(LOG) 2>&1 & \
	sleep 1; \
	pgrep -fl trading-serve || echo "failed to start"

stop:
	@pkill -f "$(SERVE_BIN)" 2>/dev/null; sleep 1; \
	pgrep -fl trading-serve > /dev/null && echo "still running" || echo "stopped"

logs:
	@tail -f $(LOG)

# ---- backtest: optional [DAYS] [TF]
backtest:
	@DAYS=$(word 2,$(MAKECMDGOALS)); \
	TF=$(word 3,$(MAKECMDGOALS)); \
	if [ -z "$$DAYS" ]; then DAYS=60; fi; \
	if [ -z "$$TF" ]; then TF=1h; fi; \
	$(GO) run ./cmd/backtest -tf=$$TF -days=$$DAYS -fee-bps=6 -sweep-only

build:
	@$(GO) build -o $(SERVE_BIN) ./cmd/serve && echo "built $(SERVE_BIN)"

# ---- deploy: cross-compile serve for linux/amd64, scp to VPS, restart unit
deploy:
	@echo "▶ building linux/amd64 serve..."
	@GOOS=linux GOARCH=amd64 $(GO) build -o /tmp/trading-serve-linux ./cmd/serve
	@echo "▶ uploading to $(ORACLE_HOST)..."
	@$(SCP) /tmp/trading-serve-linux $(ORACLE_USER)@$(ORACLE_HOST):/tmp/trading-serve
	@echo "▶ installing + restarting systemd unit..."
	@$(SSH) 'mv /tmp/trading-serve /opt/trading/trading-serve && chown ubuntu:ubuntu /opt/trading/trading-serve && sudo systemctl restart trading-bot && sleep 1 && systemctl status trading-bot --no-pager | head -5'

# ---- deploy-all: also redeploy analyze/validate/backtest/price/journal binaries
deploy-all:
	@echo "▶ building all linux/amd64 binaries..."
	@GOOS=linux GOARCH=amd64 $(GO) build -o /tmp/trading-serve-linux    ./cmd/serve
	@GOOS=linux GOARCH=amd64 $(GO) build -o /tmp/trading-analyze-linux  ./cmd/analyze
	@GOOS=linux GOARCH=amd64 $(GO) build -o /tmp/trading-validate-linux ./cmd/validate
	@GOOS=linux GOARCH=amd64 $(GO) build -o /tmp/trading-backtest-linux ./cmd/backtest
	@GOOS=linux GOARCH=amd64 $(GO) build -o /tmp/trading-price-linux    ./cmd/price
	@GOOS=linux GOARCH=amd64 $(GO) build -o /tmp/trading-journal-linux  ./cmd/journal
	@echo "▶ uploading to $(ORACLE_HOST)..."
	@$(SCP) /tmp/trading-serve-linux /tmp/trading-analyze-linux /tmp/trading-validate-linux /tmp/trading-backtest-linux /tmp/trading-price-linux /tmp/trading-journal-linux $(ORACLE_USER)@$(ORACLE_HOST):/tmp/
	@echo "▶ installing + restarting daemon..."
	@$(SSH) 'mv /tmp/trading-serve-linux    /opt/trading/trading-serve    && \
	          mv /tmp/trading-analyze-linux  /opt/trading/trading-analyze  && \
	          mv /tmp/trading-validate-linux /opt/trading/trading-validate && \
	          mv /tmp/trading-backtest-linux /opt/trading/trading-backtest && \
	          mv /tmp/trading-price-linux    /opt/trading/trading-price    && \
	          mv /tmp/trading-journal-linux  /opt/trading/trading-journal  && \
	          chown ubuntu:ubuntu /opt/trading/trading-* && \
	          sudo systemctl restart trading-bot && \
	          sleep 1 && systemctl status trading-bot --no-pager | head -5'

ssh:
	@$(SSH)

remote-status:
	@$(SSH) 'systemctl status trading-bot --no-pager'

remote-logs:
	@$(SSH) 'journalctl -u trading-bot -n 30 -f'

# ---- web UI deployment + ops ----
deploy-web:
	@echo "▶ building linux/amd64 web..."
	@GOOS=linux GOARCH=amd64 $(GO) build -o /tmp/trading-web-linux ./cmd/web
	@echo "▶ uploading to $(ORACLE_HOST)..."
	@$(SCP) /tmp/trading-web-linux $(ORACLE_USER)@$(ORACLE_HOST):/tmp/trading-web
	@echo "▶ installing + restarting trading-web unit..."
	@$(SSH) 'mv /tmp/trading-web /opt/trading/trading-web && chown ubuntu:ubuntu /opt/trading/trading-web && sudo systemctl restart trading-web && sleep 1 && systemctl status trading-web --no-pager | head -5'

web-status:
	@$(SSH) 'systemctl status trading-web --no-pager'

web-logs:
	@$(SSH) 'journalctl -u trading-web -n 30 -f'

web-restart:
	@$(SSH) 'sudo systemctl restart trading-web && systemctl status trading-web --no-pager | head -4'

# ---- trading-monitor: multi-TF confluence daemon ----
deploy-monitor:
	@echo "▶ building linux/amd64 monitor..."
	@CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -o /tmp/trading-monitor-linux ./cmd/monitor
	@echo "▶ uploading to $(ORACLE_HOST)..."
	@$(SCP) /tmp/trading-monitor-linux $(ORACLE_USER)@$(ORACLE_HOST):/tmp/trading-monitor
	@echo "▶ installing + restarting trading-monitor unit..."
	@$(SSH) 'mv /tmp/trading-monitor /opt/trading/trading-monitor && chown ubuntu:ubuntu /opt/trading/trading-monitor && chmod +x /opt/trading/trading-monitor && sudo systemctl restart trading-monitor && sleep 1 && systemctl status trading-monitor --no-pager | head -5'

monitor-status:
	@$(SSH) 'systemctl status trading-monitor --no-pager'

monitor-logs:
	@$(SSH) 'journalctl -u trading-monitor -n 30 -f'

monitor-restart:
	@$(SSH) 'sudo systemctl restart trading-monitor && systemctl status trading-monitor --no-pager | head -4'

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
	@CGO_ENABLED=0 $(GO) build -buildvcs=false -o /tmp/trading-bot-mcp ./cmd/mcp

mcp-install: mcp-build
	@mkdir -p $(HOME)/bin
	@mv /tmp/trading-bot-mcp $(HOME)/bin/trading-bot-mcp
	@chmod +x $(HOME)/bin/trading-bot-mcp
	@echo "✓ installed: $(HOME)/bin/trading-bot-mcp"
	@echo ""
	@echo "Next steps (one-time):"
	@echo "  1. mkdir -p ~/trading-bot-data && scp -i ~/.ssh/oracle-trading.key \\"
	@echo "       ubuntu@your.vps.ip:/opt/trading/journal.csv \\"
	@echo "       ~/trading-bot-data/journal.csv"
	@echo "  2. Register the server in your Claude Code config — see docs/MCP_SETUP.md"
	@echo "  3. Restart 'claude' to pick up the new tools"

mcp-sync-journal:
	@mkdir -p $(HOME)/trading-bot-data
	@$(SCP) $(ORACLE_USER)@$(ORACLE_HOST):/opt/trading/journal.csv $(HOME)/trading-bot-data/journal.csv
	@echo "✓ journal.csv synced to ~/trading-bot-data/journal.csv"

# ---- journal: positional pass-through to the VPS ----
# We always run the journal on the VPS so all entries are in one place.
jopen:
	@$(SSH) "JOURNAL_PATH=/opt/trading/journal.csv /opt/trading/trading-journal open $(filter-out jopen,$(MAKECMDGOALS))"
jclose:
	@$(SSH) "JOURNAL_PATH=/opt/trading/journal.csv /opt/trading/trading-journal close $(filter-out jclose,$(MAKECMDGOALS))"
jlist:
	@$(SSH) "JOURNAL_PATH=/opt/trading/journal.csv /opt/trading/trading-journal list $(filter-out jlist,$(MAKECMDGOALS))"
jstats:
	@$(SSH) "JOURNAL_PATH=/opt/trading/journal.csv /opt/trading/trading-journal stats"
# `make jupdate ID=1 SET='entry=77.50 stop=78.20'`
# Pass field=value pairs in SET because make would otherwise treat them as
# its own variable assignments and strip them from MAKECMDGOALS.
jupdate:
	@if [ -z "$(ID)" ] || [ -z "$(SET)" ]; then \
		echo "usage: make jupdate ID=<n> SET='field=value [field=value ...]'"; \
		echo "       make jupdate ID=1 SET='entry=77.50 stop=78.20'"; \
		exit 2; \
	fi
	@$(SSH) "JOURNAL_PATH=/opt/trading/journal.csv /opt/trading/trading-journal update $(ID) $(SET)"

jdelete:
	@ID=$(word 2,$(MAKECMDGOALS)); \
	if [ -z "$$ID" ]; then echo "usage: make jdelete <id>"; exit 2; fi; \
	$(SSH) "JOURNAL_PATH=/opt/trading/journal.csv /opt/trading/trading-journal delete $$ID"

janchors:
	@$(SSH) "/opt/trading/trading-journal anchors"

# ---- daemon control on the VPS ----
# start/stop/restart are the only VPS operations needing root; sudoers
# grants ubuntu exactly those three verbs on the three trading units.
# status / journalctl / mv / chown run unprivileged (adm group +
# ubuntu-owned /opt/trading), so they carry no sudo.
tstart:
	@$(SSH) "sudo systemctl start trading-bot && systemctl status trading-bot --no-pager | head -4"
tstop:
	@$(SSH) "sudo systemctl stop trading-bot && echo stopped"
trestart:
	@$(SSH) "sudo systemctl restart trading-bot && systemctl status trading-bot --no-pager | head -4"

# `make tconfig TF=15m MS=3` updates .env and restarts the daemon.
# `make tconfig TF=1h`        keeps current MS.
# `make tconfig`              prints current config.
tconfig:
	@if [ -z "$(TF)" ]; then \
		$(SSH) "grep -E '^TRADING_(TF|MIN_SCORE)=' /opt/trading/.env"; \
		echo; echo "usage: make tconfig TF=<tf> [MS=<min-score>]"; \
		echo "       make tconfig TF=15m MS=3"; \
		exit 0; \
	fi; \
	$(SSH) "source /etc/profile.d/trading-aliases.sh && tconfig $(TF) $(MS)"
