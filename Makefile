GO ?= go
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build fake-agent test-driver test verify herdr-integration crash-integration real-codex-integration dist clean

build:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o pika-go ./cmd/pika-go

fake-agent:
	$(GO) build -trimpath -o fake-agent ./cmd/fake-agent

test-driver:
	$(GO) build -trimpath -o /tmp/pika-go-test-driver ./cmd/pika-go-test-driver

test:
	$(GO) test ./...

verify:
	test -z "$$(gofmt -l cmd internal)"
	$(GO) vet ./...
	$(GO) test -race ./...

herdr-integration: build fake-agent
	PIKA_GO_HERDR_INTEGRATION=1 PIKA_GO_FAKE_AGENT_BIN="$(CURDIR)/fake-agent" PIKA_GO_BIN="$(CURDIR)/pika-go" $(GO) test ./internal/herdr ./internal/workruntime -run 'TestRuntimeWithRealHerdrAndFakeAgent|TestDaemonRestartCreatesFreshSessionMoveAndLostPaneReplacement|TestDaemonProcessCrashReplacesRunningHerdrAgentAndRecoversWork|TestDaemonProcessCrashAtDispatchingOutboxReconcilesWithoutDuplicateAgent|TestDaemonProcessCrashAfterTerminalCommitRecoversSingleSuccessor|TestDaemonProcessCrashAfterBestGitCommitRecoversIntegrationOnce|TestGracefulShutdownWaitsForRealHerdrAgentTerminalMCP|TestBaselineAcceptedEndToEndThroughMCP|TestFollowUpThroughRealHerdr|TestRejectedBaselineCreatesFreshDraftSession|TestOptimizationFIFOThroughRealHerdr' -v

crash-integration: build fake-agent
	PIKA_GO_HERDR_INTEGRATION=1 PIKA_GO_FAKE_AGENT_BIN="$(CURDIR)/fake-agent" PIKA_GO_BIN="$(CURDIR)/pika-go" $(GO) test ./internal/workruntime -run 'TestDaemonProcessCrashReplacesRunningHerdrAgentAndRecoversWork|TestDaemonProcessCrashAtDispatchingOutboxReconcilesWithoutDuplicateAgent|TestDaemonProcessCrashAfterTerminalCommitRecoversSingleSuccessor|TestDaemonProcessCrashAfterBestGitCommitRecoversIntegrationOnce' -v -count=2

real-codex-integration: build
	PIKA_GO_REAL_CODEX_INTEGRATION=1 PIKA_GO_BIN="$(CURDIR)/pika-go" $(GO) test ./internal/workruntime -run '^TestRealCodexCompletesDisposableOptimization$$' -v -count=1 -timeout 30m

dist: clean
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/pika-go_darwin_arm64/pika-go ./cmd/pika-go
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/pika-go_darwin_amd64/pika-go ./cmd/pika-go
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/pika-go_linux_arm64/pika-go ./cmd/pika-go
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/pika-go_linux_amd64/pika-go ./cmd/pika-go
	for dir in dist/pika-go_*; do cp herdr-plugin.toml README.md "$${dir}/"; cp -R defaults docs "$${dir}/"; done
	for dir in dist/pika-go_*; do archive="$${dir}.tar.gz"; tar -C dist -czf "$${archive}" "$$(basename "$${dir}")"; done
	cd dist && shasum -a 256 pika-go_*.tar.gz > SHA256SUMS

clean:
	rm -rf dist
