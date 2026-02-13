# Makefile — Build homescreen binaries for multiple platforms.
#
# Usage:
#   make              build for the current platform → ./homescreen
#   make all          build for all 6 OS/arch combos → dist/
#   make test         run tests (skip integration)
#   make test-all     run all tests including integration
#   make docker       build the Docker image
#   make clean        remove build artifacts

BINARY  := homescreen
MODULE  := homescreen
DIST    := dist

# Linker flags: strip debug info and symbol table for smaller binaries
LDFLAGS := -s -w

# All target platforms: OS/ARCH pairs
PLATFORMS := \
	linux/amd64 \
	linux/arm64 \
	darwin/amd64 \
	darwin/arm64 \
	freebsd/amd64 \
	freebsd/arm64

# Default: build for the current platform
.PHONY: build
build:
	CGO_ENABLED=0 go build -ldflags='$(LDFLAGS)' -o $(BINARY) .

# Build all platform binaries into dist/
.PHONY: all
all: $(PLATFORMS)

# Pattern rule: each platform target builds dist/homescreen-OS-ARCH
.PHONY: $(PLATFORMS)
$(PLATFORMS):
	$(eval OS   := $(word 1,$(subst /, ,$@)))
	$(eval ARCH := $(word 2,$(subst /, ,$@)))
	@mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=$(OS) GOARCH=$(ARCH) \
		go build -ldflags='$(LDFLAGS)' -o $(DIST)/$(BINARY)-$(OS)-$(ARCH) .
	@echo "built $(DIST)/$(BINARY)-$(OS)-$(ARCH)"

.PHONY: test
test:
	SKIP_INTEGRATION=1 go test ./... -count=1

.PHONY: test-all
test-all:
	go test ./... -count=1

.PHONY: docker
docker:
	docker build -t $(BINARY) .

.PHONY: clean
clean:
	rm -f $(BINARY)
	rm -rf $(DIST)
