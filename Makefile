GO ?= go
NPM ?= npm
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)
PREFIX ?= /usr/local
DESTDIR ?=

.PHONY: build webui-install webui-build webui-verify hot-update-candidate install fake-agent test-driver test verify herdr-integration crash-integration real-codex-integration real-cursor-integration real-codex-hot-reload-integration real-cursor-hot-reload-integration real-mixed-provider-integration real-pause-resume-integration dist clean

webui-install:
	cd webui && $(NPM) ci

webui-build: webui-install
	cd webui && $(NPM) run build

webui-verify: webui-install
	cd webui && $(NPM) run lint
	cd webui && $(NPM) run typecheck
	cd webui && $(NPM) run test
	cd webui && $(NPM) run e2e
	cd webui && $(NPM) run build

build: webui-build
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o pika-go ./cmd/pika-go

hot-update-candidate:
	$(GO) build -trimpath -ldflags '-s -w -X main.version=$(VERSION)-hot' -o pika-go-hot ./cmd/pika-go

# Keep a local build artifact and install the user-facing command to PATH.
# Set PREFIX= to only build the binary in the plugin root.
install: build
	@if [ -n "$(PREFIX)" ]; then \
		install -d "$(DESTDIR)$(PREFIX)/bin"; \
		install -m 755 pika-go "$(DESTDIR)$(PREFIX)/bin/pika-go"; \
	fi

fake-agent:
	$(GO) build -trimpath -o fake-agent ./cmd/fake-agent

test-driver:
	$(GO) build -trimpath -o /tmp/pika-go-test-driver ./cmd/pika-go-test-driver

test:
	$(GO) test ./...

.PHONY: hot-update-integration
hot-update-integration:
	$(GO) test -tags=integration ./internal/cli -run '^TestHotUpdate(HandsOffLiveSocketToNewProcess|RollsBackAcrossActivationCheckpoints)$$' -count=1 -v

verify: webui-verify
	test -z "$$(gofmt -l cmd internal)"
	$(GO) vet ./...
	$(GO) test -race ./...

herdr-integration: build fake-agent
	PIKA_GO_HERDR_INTEGRATION=1 PIKA_GO_FAKE_AGENT_BIN="$(CURDIR)/fake-agent" PIKA_GO_BIN="$(CURDIR)/pika-go" $(GO) test ./internal/herdr ./internal/workruntime -run 'TestRuntimeWithRealHerdrAndFakeAgent|TestDaemonRestartCreatesFreshSessionMoveAndLostPaneReplacement|TestDaemonProcessCrashReplacesRunningHerdrAgentAndRecoversWork|TestDaemonProcessCrashAtDispatchingOutboxReconcilesWithoutDuplicateAgent|TestDaemonProcessCrashAfterTerminalCommitRecoversSingleSuccessor|TestDaemonProcessCrashAfterBestGitCommitRecoversIntegrationOnce|TestGracefulShutdownWaitsForRealHerdrAgentTerminalMCP|TestBaselineAcceptedEndToEndThroughMCP|TestFollowUpThroughRealHerdr|TestRejectedBaselineCreatesFreshDraftSession|TestOptimizationFIFOThroughRealHerdr' -v

crash-integration: build fake-agent
	PIKA_GO_HERDR_INTEGRATION=1 PIKA_GO_FAKE_AGENT_BIN="$(CURDIR)/fake-agent" PIKA_GO_BIN="$(CURDIR)/pika-go" $(GO) test ./internal/workruntime -run 'TestDaemonProcessCrashReplacesRunningHerdrAgentAndRecoversWork|TestDaemonProcessCrashAtDispatchingOutboxReconcilesWithoutDuplicateAgent|TestDaemonProcessCrashAfterTerminalCommitRecoversSingleSuccessor|TestDaemonProcessCrashAfterBestGitCommitRecoversIntegrationOnce' -v -count=2

real-codex-integration: build
	PIKA_GO_REAL_CODEX_INTEGRATION=1 PIKA_GO_BIN="$(CURDIR)/pika-go" $(GO) test ./internal/workruntime -run '^TestRealCodexCompletesDisposableOptimization$$' -v -count=1 -timeout 30m

real-cursor-integration: build
	PIKA_GO_REAL_CURSOR_INTEGRATION=1 PIKA_GO_BIN="$(CURDIR)/pika-go" $(GO) test ./internal/workruntime -run '^TestRealCursorCompletesDisposableOptimization$$' -v -count=1 -timeout 30m

real-codex-hot-reload-integration: build hot-update-candidate
	PIKA_GO_REAL_CODEX_HOT_RELOAD_INTEGRATION=1 PIKA_GO_BIN="$(CURDIR)/pika-go" PIKA_GO_HOT_BIN="$(CURDIR)/pika-go-hot" $(GO) test ./internal/workruntime -run '^TestRealCodexHotReloadCompletesSimpleOptimization$$' -v -count=1 -timeout 20m

real-cursor-hot-reload-integration: build hot-update-candidate
	PIKA_GO_REAL_CURSOR_HOT_RELOAD_INTEGRATION=1 PIKA_GO_BIN="$(CURDIR)/pika-go" PIKA_GO_HOT_BIN="$(CURDIR)/pika-go-hot" $(GO) test ./internal/workruntime -run '^TestRealCursorHotReloadCompletesSimpleOptimization$$' -v -count=1 -timeout 20m

real-mixed-provider-integration: build
	PIKA_GO_REAL_MIXED_PROVIDER_INTEGRATION=1 PIKA_GO_BIN="$(CURDIR)/pika-go" $(GO) test ./internal/workruntime -run '^TestRealMixedProviderCompletesBothAlternatingMatrices$$' -v -count=1 -timeout 60m

real-pause-resume-integration: build
	PIKA_GO_REAL_CODEX_INTEGRATION=1 PIKA_GO_REAL_CURSOR_INTEGRATION=1 PIKA_GO_REAL_MIXED_PROVIDER_INTEGRATION=1 PIKA_GO_BIN="$(CURDIR)/pika-go" $(GO) test ./internal/workruntime -run '^TestReal(CodexCompletesDisposableOptimization|CursorCompletesDisposableOptimization|MixedProviderCompletesBothAlternatingMatrices)$$' -v -count=1 -timeout 90m

dist: clean webui-build
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/pika-go_darwin_arm64/pika-go ./cmd/pika-go
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/pika-go_darwin_amd64/pika-go ./cmd/pika-go
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/pika-go_linux_arm64/pika-go ./cmd/pika-go
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/pika-go_linux_amd64/pika-go ./cmd/pika-go
	for dir in dist/pika-go_*; do cp herdr-plugin.toml README.md "$${dir}/"; cp -R defaults docs "$${dir}/"; done
	for dir in dist/pika-go_*; do archive="$${dir}.tar.gz"; tar -C dist -czf "$${archive}" "$$(basename "$${dir}")"; done
	cd dist && shasum -a 256 pika-go_*.tar.gz > SHA256SUMS

clean:
	rm -rf dist pika-go-hot
