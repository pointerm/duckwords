.DEFAULT_GOAL := help

GO ?= go
override REQUIRED_GO_VERSION := go1.26.6
GO_RUN := env GOFIPS140=off GOTOOLCHAIN=$(REQUIRED_GO_VERSION) $(GO)
RELEASE_GO_RUN := env -u GOOS -u GOARCH -u GOAMD64 -u GOARM64 -u GO386 -u GOARM \
	-u GOMIPS -u GOMIPS64 -u GOPPC64 -u GORISCV64 -u GOWASM -u GOROOT \
	CGO_ENABLED=0 GOFLAGS= GOWORK=off GOENV=off GOEXPERIMENT= GOFIPS140=off \
	GOTOOLCHAIN=$(REQUIRED_GO_VERSION) $(GO)
GOFMT = $(shell $(GO_RUN) env GOROOT)/bin/gofmt
BINARY := bin/duckwords
FIXTURE_BINARY := bin/duckwords-fixture
EVIDENCE_BINARY := bin/duckwords-evidence
MODULE := github.com/pointerm/duckwords
BUILDINFO_PACKAGE := $(MODULE)/internal/buildinfo
DOCKER ?= docker
DOCKER_IMAGE ?= duckwords:review
DOCKER_FIXTURE_IMAGE ?= duckwords:fixture-review
REVIEW_DIR ?= artifacts/review
SUBMISSION_DIR ?= artifacts/submission
GOVULNCHECK_VERSION ?= v1.6.0
STATICCHECK_VERSION ?= v0.7.0
# v8.30.1 is intentionally not adopted while upstream issue #2170 reports
# silent false negatives in that release. Pin both the v8.30.0 tag and the
# multi-platform OCI index so CI cannot receive an unreviewed scanner build.
GITLEAKS_IMAGE ?= ghcr.io/gitleaks/gitleaks:v8.30.0@sha256:691af3c7c5a48b16f187ce3446d5f194838f91238f27270ed36eef6359a574d9
JQ ?= jq

VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || printf unknown)
CANDIDATE_SHA ?= $(shell git rev-parse --verify HEAD 2>/dev/null || printf unknown)
BUILD_DATE ?= unknown
SHUFFLE_COUNT ?= 5
FUZZ_TIME ?= 2s
BENCH_COUNT ?= 1
LDFLAGS = -X '$(BUILDINFO_PACKAGE).version=$(VERSION)' \
	-X '$(BUILDINFO_PACKAGE).commit=$(COMMIT)' \
	-X '$(BUILDINFO_PACKAGE).buildDate=$(BUILD_DATE)'

.PHONY: help toolchain-check fmt fmt-check mod-tidy-check mod-verify vet lint vuln secret-scan test test-shuffle race fuzz-smoke bench bench-text \
	build fixture-build fixture-native docker-build docker-fixture-build docker-smoke \
	evidence-build submission-build submission-capture submission-capture-test fixture-verify docker-verify run version verify

help: ## Show available targets.
	@awk 'BEGIN {FS = ":.*## "; printf "DuckWords development targets:\n"} /^[a-zA-Z0-9_-]+:.*## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

toolchain-check: ## Require the exact Go 1.26.6 toolchain.
	@test "$$($(GO_RUN) env GOVERSION)" = "$(REQUIRED_GO_VERSION)"

fmt fmt-check mod-tidy-check mod-verify vet lint vuln test test-shuffle race fuzz-smoke bench bench-text \
	build fixture-build evidence-build submission-build run version: toolchain-check

fmt: ## Format all Go packages.
	$(GO_RUN) fmt ./...

fmt-check: ## Fail when Go files are not formatted.
	@test -z "$$($(GOFMT) -l .)"

mod-tidy-check: toolchain-check ## Fail when go.mod or go.sum is not tidy.
	$(GO_RUN) mod tidy -diff

mod-verify: ## Verify downloaded module content.
	$(GO_RUN) mod verify

vet: ## Run standard Go static analysis.
	$(GO_RUN) vet ./...

lint: ## Run the pinned Go 1.26-aware Staticcheck release.
	$(GO_RUN) run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

vuln: ## Scan reachable code with the pinned Go vulnerability scanner.
	$(GO_RUN) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

secret-scan: ## Scan the current tree and, when present, complete Git history.
	$(DOCKER) run --rm --network=none --read-only --cap-drop=ALL \
		--security-opt=no-new-privileges \
		--mount type=bind,src="$(CURDIR)",dst=/repo,readonly \
		--workdir /repo $(GITLEAKS_IMAGE) dir \
		--redact=100 --no-banner --no-color --timeout=300 /repo
	@if git rev-parse --verify HEAD >/dev/null 2>&1; then \
		$(DOCKER) run --rm --network=none --read-only --cap-drop=ALL \
			--security-opt=no-new-privileges \
			--mount type=bind,src="$(CURDIR)",dst=/repo,readonly \
			--workdir /repo \
			-e GIT_CONFIG_COUNT=1 \
			-e GIT_CONFIG_KEY_0=safe.directory \
			-e GIT_CONFIG_VALUE_0=/repo \
			$(GITLEAKS_IMAGE) git \
			--redact=100 --no-banner --no-color --timeout=300 \
			--log-opts=--all /repo; \
	else \
		printf '%s\n' 'Git history scan skipped: repository has no HEAD yet.'; \
	fi

test: ## Run deterministic unit, property-seed, and fuzz-seed tests once.
	$(GO_RUN) test -count=1 ./...

test-shuffle: ## Repeat tests in shuffled order to expose hidden order dependencies.
	$(GO_RUN) test -shuffle=on -count=$(SHUFFLE_COUNT) ./...

race: ## Run tests with the race detector.
	$(GO_RUN) test -race ./...

fuzz-smoke: ## Run short bounded fuzz campaigns; override FUZZ_TIME as needed.
	$(GO_RUN) test -run='^$$' -fuzz='^FuzzDictionary$$' -fuzztime=$(FUZZ_TIME) ./internal/words
	$(GO_RUN) test -run='^$$' -fuzz='^FuzzTokenizer$$' -fuzztime=$(FUZZ_TIME) ./internal/words
	$(GO_RUN) test -run='^$$' -fuzz='^FuzzMatcher$$' -fuzztime=$(FUZZ_TIME) ./internal/words
	$(GO_RUN) test -run='^$$' -fuzz='^FuzzCounterASCIIPathMatchesUnicodeFallback$$' -fuzztime=$(FUZZ_TIME) ./internal/words
	$(GO_RUN) test -run='^$$' -fuzz='^FuzzParsePostURL$$' -fuzztime=$(FUZZ_TIME) ./internal/source
	$(GO_RUN) test -run='^$$' -fuzz='^FuzzLoadPostList$$' -fuzztime=$(FUZZ_TIME) ./internal/source
	$(GO_RUN) test -run='^$$' -fuzz='^FuzzValidateRemoteURL$$' -fuzztime=$(FUZZ_TIME) ./internal/acquire
	$(GO_RUN) test -run='^$$' -fuzz='^FuzzRetryAfter$$' -fuzztime=$(FUZZ_TIME) ./internal/reddit
	$(GO_RUN) test -run='^$$' -fuzz='^FuzzRateLimitHeaderNumbers$$' -fuzztime=$(FUZZ_TIME) ./internal/reddit
	$(GO_RUN) test -run='^$$' -fuzz='^FuzzPreflightJSONMatchesEncodingJSON$$' -fuzztime=$(FUZZ_TIME) ./internal/reddit
	$(GO_RUN) test -run='^$$' -fuzz='^FuzzWalkDecoded$$' -fuzztime=$(FUZZ_TIME) ./internal/reddit
	$(GO_RUN) test -run='^$$' -fuzz='^FuzzParseResult$$' -fuzztime=$(FUZZ_TIME) ./internal/evidence
	$(GO_RUN) test -run='^$$' -fuzz='^FuzzParseLog$$' -fuzztime=$(FUZZ_TIME) ./internal/evidence

bench: ## Run core, application, source, and Reddit benchmarks with allocations.
	$(GO_RUN) test -run='^$$' -bench=. -benchmem -count=$(BENCH_COUNT) ./internal/words ./internal/aggregate ./internal/source ./internal/reddit ./internal/app

bench-text: ## Verify and benchmark the readable local text fixtures.
	$(GO_RUN) test -run='^TestLocalTextFixtures$$' -bench='^BenchmarkCounterLocalTextFixtures$$' -benchmem -count=$(BENCH_COUNT) ./internal/words

build: ## Build a reproducible local CLI binary.
	mkdir -p bin
	$(RELEASE_GO_RUN) build -mod=readonly -trimpath -buildvcs=false \
		-ldflags "-s -w -buildid= $(LDFLAGS)" -o $(BINARY) ./cmd/duckwords

evidence-build: ## Build the offline submission-evidence finalizer.
	mkdir -p bin
	$(RELEASE_GO_RUN) build -mod=readonly -trimpath -buildvcs=false \
		-ldflags "-s -w -buildid=" -o $(EVIDENCE_BINARY) ./cmd/duckwords-evidence

# A target-specific full commit flows into the ordinary reproducible build through
# recursive LDFLAGS. The capture wrapper independently requires the same clean HEAD.
submission-build: ## Build candidate CLI and evidence finalizer without changing tracked source.
	@test "$(CANDIDATE_SHA)" != "unknown"
	$(MAKE) build evidence-build \
		VERSION="$(VERSION)" COMMIT="$(CANDIDATE_SHA)" BUILD_DATE="$(BUILD_DATE)"

submission-capture: ## Run one approved live invocation from an already built clean-SHA candidate.
	DUCKWORDS_RELEASE_VERSION="$(VERSION)" DUCKWORDS_BUILD_DATE="$(BUILD_DATE)" \
		SUBMISSION_DIR="$(SUBMISSION_DIR)" /bin/bash -p scripts/capture-submission.sh

submission-capture-test: ## Test the capture wrapper offline with deterministic process fixtures.
	bash scripts/capture-submission_test.sh

fixture-build: ## Compile the test-only deterministic offline process fixture.
	mkdir -p bin
	CGO_ENABLED=0 $(GO_RUN) test -c -mod=readonly -trimpath -buildvcs=false \
		-ldflags "-s -w -buildid=" -o $(FIXTURE_BINARY) ./cmd/duckwords

fixture-native: fixture-build ## Run the native offline fixture into ignored review artifacts.
	mkdir -p $(REVIEW_DIR)
	DUCKWORDS_OFFLINE_FIXTURE_PROCESS=1 $(FIXTURE_BINARY) \
		> $(REVIEW_DIR)/native-result.json \
		2> $(REVIEW_DIR)/native-stderr.log

docker-build: ## Build the pinned, non-root production image.
	$(DOCKER) build $(DOCKER_BUILD_FLAGS) --target runtime \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		--tag $(DOCKER_IMAGE) .

docker-fixture-build: ## Build the test-only offline fixture image.
	$(DOCKER) build $(DOCKER_BUILD_FLAGS) --target fixture --tag $(DOCKER_FIXTURE_IMAGE) .

docker-smoke: docker-build ## Check production image identity, metadata, and fail-closed execution.
	mkdir -p $(REVIEW_DIR)
	@test "$$($(DOCKER) inspect --format '{{.Config.User}}' $(DOCKER_IMAGE))" = "65532:65532"
	@test "$$($(DOCKER) inspect --format '{{json .Config.Entrypoint}}' $(DOCKER_IMAGE))" = '["/duckwords"]'
	@$(DOCKER) run --rm --network=none --read-only --cap-drop=ALL \
		--security-opt=no-new-privileges $(DOCKER_IMAGE) --help > /dev/null
	@test "$$($(DOCKER) run --rm --network=none --read-only --cap-drop=ALL \
		--security-opt=no-new-privileges $(DOCKER_IMAGE) --version)" = \
		"duckwords version=$(VERSION) commit=$(COMMIT) built=$(BUILD_DATE) go=go1.26.6"
	@set +e; \
		$(DOCKER) run --rm --network=none --read-only --cap-drop=ALL \
			--security-opt=no-new-privileges \
			-e DUCKWORDS_OFFLINE_FIXTURE_PROCESS=1 \
			$(DOCKER_IMAGE) --workers=1 \
			> /dev/null 2> $(REVIEW_DIR)/docker-fail-closed.log; \
		status=$$?; \
		set -e; \
		test $$status -eq 1

fixture-verify: fixture-native docker-fixture-build ## Compare native and isolated-container fixture JSON byte for byte.
	$(DOCKER) run --rm --network=none --read-only --cap-drop=ALL \
		--security-opt=no-new-privileges $(DOCKER_FIXTURE_IMAGE) \
		> $(REVIEW_DIR)/docker-result.json \
		2> $(REVIEW_DIR)/docker-stderr.log
	$(JQ) -e 'type == "array" and length <= 10' $(REVIEW_DIR)/native-result.json > /dev/null
	cmp testdata/phase5/expected.json $(REVIEW_DIR)/native-result.json
	cmp $(REVIEW_DIR)/native-result.json $(REVIEW_DIR)/docker-result.json

docker-verify: docker-smoke fixture-verify ## Run all Docker and native/container parity gates.

run: ## Run the CLI; pass options with ARGS='...'.
	$(GO_RUN) run ./cmd/duckwords $(ARGS)

version: ## Print CLI build metadata.
	$(GO_RUN) run ./cmd/duckwords --version

verify: fmt-check mod-tidy-check mod-verify vet test test-shuffle race build evidence-build submission-capture-test ## Run offline submission-blocking quality gates.
