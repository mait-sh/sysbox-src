#
# Sysbox Arch Linux Packager Makefile
#
# Mirrors the shape of rpm/Makefile and deb/Makefile but for makepkg.
# Single target: `arch-current` (Arch Linux is rolling — there are no
# fixed releases to enumerate the way Fedora/Ubuntu does).
#

.PHONY: help          \
	arch-current      \
	clean

SHELL:=/bin/bash

# Architecture (deb-style abbreviation — matches deb/Makefile + rpm/Makefile).
UNAME_M := $(shell uname -m)
ifeq ($(UNAME_M),x86_64)
	ARCH := amd64
else ifeq ($(UNAME_M),aarch64)
	ARCH := arm64
else
	ARCH := $(UNAME_M)
endif

# Go base image (multi-stage source).
GO_BASE_IMAGE=golang
GO_VERSION:=1.22
GO_IMAGE=$(GO_BASE_IMAGE):$(GO_VERSION)

# Sysbox source location, same logic as deb/Makefile + rpm/Makefile.
ifeq ($(EDITION),ce)
	SYSBOX_DIR  := $(CURDIR)/../sources/sysbox
else ifeq ($(EDITION),ee)
	SYSBOX_DIR  := $(CURDIR)/../sources/sysbox-internal
else
	$(error Unsupported Sysbox edition '$(EDITION)' — expected 'ce' or 'ee')
endif

# Shared sources list — identical to deb/Makefile + rpm/Makefile, so all
# three packagers consume the same upstream payload.
SOURCE_FILES =  sysbox.tgz           \
		sysbox.service       \
		sysbox-fs.service    \
		sysbox-mgr.service   \
		99-sysbox-sysctl.conf \
		50-sysbox-mod.conf

SOURCES=$(addprefix sources/, $(SOURCE_FILES))

CHOWN:=docker run --rm -v $(CURDIR):/v -w /v alpine chown

DOCKER_BUILD=docker build                              \
	--build-arg GO_IMAGE=$(GO_IMAGE)                   \
	--build-arg arch=$(ARCH)                           \
	-t archbuild-$@/$(ARCH)                            \
	-f $(CURDIR)/$@/Dockerfile .

# makepkg-in-container — needs to mount the host GOPATH/pkg/mod so the
# Go module cache survives across rebuilds (parallel to deb/rpm).
DOCKER_RUN=docker run --rm -i                            \
	-e EDITION                                           \
	-v $(CURDIR)/archbuild/$@:/build                     \
	-v $(GOPATH)/pkg/mod:/go/pkg/mod                     \
	archbuild-$@/$(ARCH)

.DEFAULT := help

help:
	@awk 'BEGIN {FS = ":.*##"; printf "\n\033[1mUsage:\n  \
	make <sysbox-package> \033[36m<arch-target>\033[0m\n\n"} \
	/^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-25s\033[0m %s\n", $$1, $$2 } /^##@/ \
	{ printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Arch building targets

all: ## Build all Arch packages
all: arch-current

arch-current: ## Build Arch Linux (rolling) package
arch-current: $(SOURCES)
	@echo "== Building Arch package for $@ =="
	$(DOCKER_BUILD)
	$(DOCKER_RUN)
	$(CHOWN) -R $(shell id -u):$(shell id -g) archbuild/$@

clean: ## Remove all Arch build artefacts
	@[ ! -d archbuild ] || $(CHOWN) -R $(shell id -u):$(shell id -g) archbuild
	@$(RM) -r archbuild
	@[ ! -d sources ] || $(CHOWN) -R $(shell id -u):$(shell id -g) sources
	@$(RM) -r sources

# --- source materialisation (identical to deb/Makefile + rpm/Makefile) ---

sources/sysbox.tgz:
	mkdir -p $(@D)
	docker run --rm -i -w /v \
		-v $(SYSBOX_DIR):/sysbox \
		-v $(CURDIR)/$(@D):/v \
		alpine \
		tar -C / -czf /v/sysbox.tgz --exclude .git --exclude='sysbox-pkgr' sysbox

sources/sysbox.service: ../systemd/sysbox.service
	mkdir -p $(@D)
	cp $< $@

sources/sysbox-fs.service: ../systemd/sysbox-fs.service
	mkdir -p $(@D)
	cp $< $@

sources/sysbox-mgr.service: ../systemd/sysbox-mgr.service
	mkdir -p $(@D)
	cp $< $@

sources/99-sysbox-sysctl.conf: ../systemd/99-sysbox-sysctl.conf
	mkdir -p $(@D)
	cp $< $@

sources/50-sysbox-mod.conf: ../systemd/50-sysbox-mod.conf
	mkdir -p $(@D)
	cp $< $@
