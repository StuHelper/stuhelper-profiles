GOWORK ?= off
export GOWORK

.PHONY: setup check build
setup:
	LEFTHOOK=0 pnpm install --frozen-lockfile
check:
	test -n "$(STUHELPER_PROFILES_TEST_DSN)"
	pnpm exec stuhelper-check repository
	test -z "$$(gofmt -l .)"
	go vet ./...
	go test -race -count=1 ./...
	go build ./...
	pnpm exec stuhelper-check workflows
build:
	go build ./...
