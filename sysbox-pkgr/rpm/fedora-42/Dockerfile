# syntax=docker/dockerfile:1
#
# Build container for the sysbox-ce RPM, Fedora 42 target.
#
# Mirrors the shape of `deb/ubuntu-jammy/Dockerfile`:
#  - multi-stage Go base for a pinned 1.22 toolchain (matches the deb build)
#  - dnf-installed userland matching the deb's apt list (translated 1:1)
#  - the same protoc 3.15.8 pin (sysbox's gRPC stubs are generated from
#    that version)
#  - the same `SECCOMP_IOCTL_NOTIF_ID_VALID` sed-patch (kernel API rename
#    compatibility; identical reason as the deb Dockerfile)
#
# Unlike the deb container, we do NOT start a nested dockerd inside the
# build container — `rpmbuild` does not need it, and `make sysbox-static-local`
# uses the host's compiler chain directly (no nested test container).
#

ARG GO_IMAGE=golang:1.25
ARG BASE_IMAGE=registry.fedoraproject.org/fedora:42

FROM ${GO_IMAGE} as golang

FROM ${BASE_IMAGE}

RUN dnf -y install \
        rpm-build \
        rpmdevtools \
        dnf-plugins-core \
        rpmlint \
        make \
        gcc \
        gcc-c++ \
        pkgconf-pkg-config \
        libseccomp \
        libseccomp-devel \
        libseccomp-static \
        libnet \
        libnet-devel \
        systemd-rpm-macros \
        systemd-devel \
        glibc-static \
        libstdc++-static \
        kernel-headers \
        git \
        tar \
        curl \
        unzip \
        which \
        iproute \
        iptables \
        kmod \
        ca-certificates \
        jq \
    && dnf clean all \
    && rm -rf /var/cache/dnf

ARG arch
ENV ARCH=${arch}
ENV GOPATH=/go
ENV PATH=$PATH:/usr/local/go/bin:$GOPATH/bin:/root/.local/bin

# Vendor the Go toolchain from the multi-stage golang image — identical
# pattern to the deb Dockerfile so the rpm build sees the SAME Go.
COPY --from=golang /usr/local/go /usr/local/go

ENV GO111MODULE=auto
RUN go env -w GONOSUMDB=github.com/nestybox

# protoc 3.15.8 — pinned to match the deb build (sysbox's .proto stubs
# were generated from this version).
RUN if [ "${arch}" = "amd64" ]; then arch_str="x86_64"; \
    elif [ "${arch}" = "arm64" ]; then arch_str="aarch_64"; \
    else echo "Unsupported platform: ${arch}"; exit 1; fi \
    && curl -LO https://github.com/protocolbuffers/protobuf/releases/download/v3.15.8/protoc-3.15.8-linux-${arch_str}.zip \
    && unzip protoc-3.15.8-linux-${arch_str}.zip -d $HOME/.local \
    && go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.10 \
    && go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1

# Same kernel-API compatibility sed patch as the deb Dockerfile.
# See deb/ubuntu-jammy/Dockerfile lines 76–86 for the rationale.
RUN sed -i 's/^#define SECCOMP_IOCTL_NOTIF_ID_VALID[ \t]*SECCOMP_IOW(2, __u64)/#define SECCOMP_IOCTL_NOTIF_ID_VALID   SECCOMP_IOR(2, __u64)/g' /usr/include/linux/seccomp.h

# Stage rpmbuild tree.
RUN mkdir -p /root/rpmbuild/{BUILD,BUILDROOT,RPMS,SOURCES,SPECS,SRPMS}

WORKDIR /root/build-rpm

# Per-spec inputs (staged by the orchestrator Makefile as bind mounts).
#   sources/   → /root/rpmbuild/SOURCES/
#   sysbox-ce/ → contains the .spec + sysbox-postinstall.sh
COPY sources/ /root/rpmbuild/SOURCES/
COPY sysbox-ce/sysbox-postinstall.sh /root/rpmbuild/SOURCES/sysbox-postinstall.sh
COPY sysbox-ce/sysbox-ce.spec /root/rpmbuild/SPECS/sysbox-ce.spec
COPY build-rpm /root/build-rpm/build-rpm

ENV SYSBOX_RELEASE=true

ENTRYPOINT ["/root/build-rpm/build-rpm"]
