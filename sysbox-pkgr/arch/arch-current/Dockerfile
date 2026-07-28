# syntax=docker/dockerfile:1
#
# Build container for the sysbox-ce Arch package (.pkg.tar.zst).
#
# Mirrors `deb/ubuntu-jammy/Dockerfile` and `rpm/fedora-42/Dockerfile`:
#  - multi-stage Go base for a pinned 1.22 toolchain
#  - pacman-installed userland matching the deb apt list
#  - same protoc 3.15.8 pin
#  - same SECCOMP_IOCTL_NOTIF_ID_VALID sed-patch
#
# Differences:
#  - makepkg REFUSES to run as root, so we create a non-root `build`
#    user and execute the helper script under it.
#  - No Docker-in-Docker: makepkg drives the build directly.
#

ARG GO_IMAGE=golang:1.25
ARG BASE_IMAGE=archlinux:latest

FROM ${GO_IMAGE} as golang

FROM ${BASE_IMAGE}

# Keep pacman db fresh and install build deps.
RUN pacman -Sy --noconfirm --needed \
        base-devel \
        sudo \
        go \
        git \
        libseccomp \
        libnet \
        protobuf \
        iptables-nft \
        kmod \
        curl \
        wget \
        unzip \
        tar \
        which \
        jq \
        ca-certificates \
        linux-api-headers \
        fakeroot \
        fuse3 \
        rsync \
    && pacman -Scc --noconfirm

ARG arch
ENV ARCH=${arch}
ENV GOPATH=/go
ENV PATH=$PATH:/usr/local/go/bin:$GOPATH/bin:/home/build/.local/bin

# Vendor the Go toolchain from the multi-stage golang image — identical
# pattern to the deb and rpm Dockerfiles.
COPY --from=golang /usr/local/go /usr/local/go

ENV GO111MODULE=auto
RUN go env -w GONOSUMDB=github.com/nestybox

# protoc 3.15.8 — installed system-wide so both root and the build user
# see it. Same pin as deb / rpm.
RUN if [ "${arch}" = "amd64" ]; then arch_str="x86_64"; \
    elif [ "${arch}" = "arm64" ]; then arch_str="aarch_64"; \
    else echo "Unsupported platform: ${arch}"; exit 1; fi \
    && curl -LO https://github.com/protocolbuffers/protobuf/releases/download/v3.15.8/protoc-3.15.8-linux-${arch_str}.zip \
    && unzip protoc-3.15.8-linux-${arch_str}.zip -d /usr/local \
    && rm -f protoc-3.15.8-linux-${arch_str}.zip \
    && go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.10 \
    && go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1 \
    && chmod -R a+rX /usr/local/include/google

# Same kernel-API compatibility sed patch as the deb/rpm Dockerfiles.
RUN sed -i 's/^#define SECCOMP_IOCTL_NOTIF_ID_VALID[ \t]*SECCOMP_IOW(2, __u64)/#define SECCOMP_IOCTL_NOTIF_ID_VALID   SECCOMP_IOR(2, __u64)/g' /usr/include/linux/seccomp.h

# Non-root build user — makepkg refuses to run as root. Passwordless sudo
# so the helper can call `pacman -S` for any missing deps without prompts.
RUN useradd -m -G wheel -s /bin/bash build \
    && echo 'build ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/build \
    && chmod 0440 /etc/sudoers.d/build

# Stage the build helper and PKGBUILD layout under the build user's home.
# Explicitly create the build dir with build:build ownership BEFORE the
# WORKDIR instruction; WORKDIR creates the directory as root if absent
# and `COPY --chown` only chowns the FILES it copies, not the parent
# directory. Without this, makepkg (running as `build`) hits
# "ERROR: You do not have write permission for the directory $BUILDDIR"
# because it cannot write its intermediate src/ pkg/ subdirs into the
# root-owned WORKDIR.
RUN install -d -o build -g build /home/build/build-arch
WORKDIR /home/build/build-arch
COPY --chown=build:build sysbox-ce/PKGBUILD /home/build/build-arch/PKGBUILD
COPY --chown=build:build sysbox-ce/sysbox-ce.install /home/build/build-arch/sysbox-ce.install
COPY --chown=build:build sysbox-ce/sysbox-postinstall.sh /home/build/build-arch/sysbox-postinstall.sh
COPY --chown=build:build sources/ /home/build/build-arch/
COPY --chown=build:build build-arch /home/build/build-arch/build-arch

USER build
ENV SYSBOX_RELEASE=true

ENTRYPOINT ["/home/build/build-arch/build-arch"]
