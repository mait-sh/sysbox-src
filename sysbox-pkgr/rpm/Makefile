#
# Sysbox RPM Packager Makefile
#

.PHONY: help     \
	rpm          \
	centos       \
	centos-8     \
	fedora       \
	fedora-31    \
	fedora-32    \
	fedora-42    \
	clean

SHELL:=/bin/bash

# Obtain the current system architecture (deb-style abbreviation).
# Mirrors deb/Makefile so artefact paths line up with the existing pipeline.
UNAME_M := $(shell uname -m)
ifeq ($(UNAME_M),x86_64)
	ARCH := amd64
else ifeq ($(UNAME_M),aarch64)
	ARCH := arm64
else
	ARCH := $(UNAME_M)
endif

# Go version to utilize in build containers.
# Bumped from 1.13 → 1.22 to satisfy sysbox 0.7.0's requirements (matches
# deb/Makefile's GO_VERSION). The legacy fedora-31 / fedora-32 / centos-8
# targets are left in place but are not maintained — only `fedora-42`
# has a working Dockerfile under this directory.
GO_BASE_IMAGE=golang
GO_VERSION:=1.25
GO_IMAGE=$(GO_BASE_IMAGE):$(GO_VERSION)

# Sysbox source-code locations.
ifeq ($(EDITION),ce)
	SYSBOX_DIR  := $(CURDIR)/../sources/sysbox
else ifeq ($(EDITION),ee)
	SYSBOX_DIR  := $(CURDIR)/../sources/sysbox-internal
else
	echo "Unsupported Sysbox edition: $(EDITION)"
	exit 1
endif

# Sysbox component locations.
SYSBOX_IMAGE_SYSTEMD := ../systemd

SOURCE_FILES =  sysbox.tgz           \
		sysbox.service       \
		sysbox-fs.service    \
		sysbox-mgr.service   \
		99-sysbox-sysctl.conf \
		50-sysbox-mod.conf

SOURCES=$(addprefix sources/, $(SOURCE_FILES))

CHOWN:=docker run --rm -v $(CURDIR):/v -w /v alpine chown

DOCKER_BUILD=docker build                                  \
	--build-arg GO_IMAGE=$(GO_IMAGE)                       \
	--build-arg arch=$(ARCH)                               \
	--build-arg COMMON_FILES=$(SYSBOX_IMAGE_COMMON)        \
	-t rpmbuild-$@/$(ARCH)                                 \
	-f $(CURDIR)/$@/Dockerfile .

DOCKER_RUN=docker run --rm -i                    \
	-e EDITION                                   \
	-v $(CURDIR)/rpmbuild/$@:/build              \
	-v $(GOPATH)/pkg/mod:/go/pkg/mod             \
	rpmbuild-$@/$(ARCH)


help:
	@awk 'BEGIN {FS = ":.*##"; printf "\n\033[1mUsage:\n  \
	make <sysbox-package> \033[36m<rpm-distro>\033[0m\n"} \
	/^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-25s\033[0m %s\n", $$1, $$2 } /^##@/ \
	{ printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ RPM building targets

all: ## Build all RPM packages
all: centos fedora

centos: ## Build Centos packages
centos: centos-8

centos-8: ## Build Centos-8 package
centos-8: $(SOURCES)
	$(DOCKER_BUILD)
	$(DOCKER_RUN)
	$(CHOWN) -R $(shell id -u):$(shell id -g) rpmbuild/$@

fedora: ## Build Fedora packages
fedora: fedora-42

fedora-31: ## (legacy, no Dockerfile — kept for back-compat only)
fedora-31: $(SOURCES)
	$(DOCKER_BUILD)
	$(DOCKER_RUN)
	$(CHOWN) -R $(shell id -u):$(shell id -g) rpmbuild/$@

fedora-32: ## (legacy, no Dockerfile — kept for back-compat only)
fedora-32: $(SOURCES)
	$(DOCKER_BUILD)
	$(DOCKER_RUN)
	$(CHOWN) -R $(shell id -u):$(shell id -g) rpmbuild/$@

fedora-42: ## Build Fedora-42 RPM package
fedora-42: $(SOURCES)
	@echo "== Building RPM package for $@ =="
	$(DOCKER_BUILD)
	$(DOCKER_RUN)
	$(CHOWN) -R $(shell id -u):$(shell id -g) rpmbuild/$@

clean: ## Remove build artifacts
	@[ ! -d rpmbuild ] || $(CHOWN) -R $(shell id -u):$(shell id -g) rpmbuild
	@$(RM) -r rpmbuild
	@[ ! -d sources ] || $(CHOWN) -R $(shell id -u):$(shell id -g) sources
	@$(RM) -r sources

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
