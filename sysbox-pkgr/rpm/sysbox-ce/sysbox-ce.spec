#
# Sysbox CE — RPM spec
#
# Mirror of the Debian packaging at
# `modules/sysbox/sysbox-pkgr/deb/sysbox-ce/` adapted for Fedora 42 (and
# binary-compatible RHEL/CentOS Stream 9/10 with libseccomp ≥ 2.5).
#
# Build sequence:
#   1. The build container (`rpm/fedora-42/Dockerfile`) stages the same
#      sources/ payload the deb pipeline consumes:
#        - sources/sysbox.tgz        (full sysbox source tree, excluded sysbox-pkgr)
#        - sources/sysbox.service
#        - sources/sysbox-fs.service
#        - sources/sysbox-mgr.service
#        - sources/99-sysbox-sysctl.conf
#        - sources/50-sysbox-mod.conf
#   2. `%prep` extracts sysbox.tgz into the build root.
#   3. `%build` invokes the same `make sysbox-static-local` target the deb
#      `rules` file calls — producing fully static sysbox-{fs,mgr,runc}
#      binaries under sysbox/sysbox-*/build/${ARCH}/.
#   4. `%install` copies the binaries + the systemd units + the sysctl/modules
#      drop-ins to the FHS paths used by the deb (`/usr/bin/`,
#      `/usr/lib/systemd/system/`, `/usr/lib/sysctl.d/`,
#      `/etc/modules-load.d/`).
#   5. `%post` / `%preun` / `%postun` use the systemd-rpm-macros and call
#      the vendored `sysbox-postinstall.sh` (a port of the deb postinst).
#

# Disable the automatic debuginfo / debugsource subpackages. sysbox's
# binaries are Go programs built with `make sysbox-static-local`, which
# passes `-trimpath -ldflags "-s -w"` (paths stripped + symbol table
# stripped). RPM's `find-debuginfo` finds no source-path debug records and
# produces an EMPTY `debugsourcefiles.list`, which `%files` then rejects
# with "Empty %files file …/debugsourcefiles.list" -- aborting the build.
# Disabling the debug package(s) up-front sidesteps this; the deb side
# doesn't generate a -dbgsym either, so behaviour parity holds.
%global debug_package %{nil}

Name:           sysbox-ce
Version:        0.7.0
# Release tag uses a `~rebuild1` suffix:
#   `~` sorts LOWER than `1`, so a hypothetical future upstream Nestybox
#   0.7.0-1.fc42.rpm would supersede ours cleanly. The `0~rebuild` marks
#   this as a downstream rebuild that sorts before a hypothetical upstream
#   -1 release.
Release:        0~rebuild1%{?dist}
Summary:        Sysbox Community Edition — secure container runtime

License:        ASL 2.0
URL:            https://github.com/nestybox/sysbox

# Architecture lock — sysbox today only supports x86_64 / aarch64.
ExclusiveArch:  x86_64 aarch64

# Sources are staged into ~/rpmbuild/SOURCES/ by the build container.
Source0:        sysbox.tgz
Source1:        sysbox.service
Source2:        sysbox-fs.service
Source3:        sysbox-mgr.service
Source4:        99-sysbox-sysctl.conf
Source5:        50-sysbox-mod.conf
Source6:        sysbox-postinstall.sh
# Licence text and static-linking notice (from ../licenses/).
Source7:        LGPL-2.1.txt
Source8:        STATIC-LINKING.md

# ----- Build requirements -----
# Mirrors the apt deps in deb/ubuntu-jammy/Dockerfile, translated for dnf.
# `golang` 1.22+ is satisfied by Fedora 42's default `golang` package (1.24
# as of 2026-05-24); the spec also accepts a vendored Go toolchain placed
# at /usr/local/go (which the Dockerfile does, mirroring the deb pattern).
BuildRequires:  make
BuildRequires:  gcc
BuildRequires:  gcc-c++
BuildRequires:  pkgconf-pkg-config
BuildRequires:  libseccomp-devel >= 2.5
BuildRequires:  libnet-devel
BuildRequires:  systemd-rpm-macros
BuildRequires:  systemd-devel
BuildRequires:  git
BuildRequires:  tar
BuildRequires:  curl
BuildRequires:  unzip
BuildRequires:  kernel-headers
BuildRequires:  glibc-static
BuildRequires:  libstdc++-static
# Go toolchain (1.22+) is provided by the build container at
# /usr/local/go (multi-stage `COPY --from=golang` in fedora-42/Dockerfile),
# NOT by dnf. No `BuildRequires: golang` here -- rpmbuild cannot satisfy
# it via dnf when the spec is built inside our container, and a redundant
# `dnf install golang` would land a second, conflicting toolchain. The
# build container's PATH puts /usr/local/go/bin first so `make
# sysbox-static-local` finds the vendored go.

# ----- Runtime requirements -----
# Mirrors deb/sysbox-ce/control `Depends:` translated for Fedora package
# names. `jq` covers what was the deb Pre-Depends (Pre-Depends has no RPM
# analogue; we rely on `Requires` + dnf's transitional install ordering).
Requires:       jq
Requires:       fuse
Requires:       rsync
Requires:       iptables
# `lsb-release` is intentionally dropped — the post-install hook uses
# `/etc/os-release` instead (always present on Fedora).

# ----- Conflicts -----
# Refuse to coexist with any other sysbox flavour. We do NOT conflict with
# `runc` or `containerd` — sysbox-runc is namespaced under its own binary
# and registered separately in /etc/docker/daemon.json.
Conflicts:      sysbox
Conflicts:      sysbox-ee

%description
Sysbox Community Edition (CE) is a next-generation container runtime,
developed by Nestybox, that enables deployment of containers that are
capable of running not just micro-services, but also system software such
as Docker, Kubernetes, systemd, etc., inside the container, easily and
securely.

Downstream rebuild of sysbox-ce.

%prep
# %setup -c creates and cd's into the build root, then we extract the
# pre-packaged sysbox tarball ourselves (it's not in standard top-level
# layout — the deb pipeline uses the same extraction pattern, see
# deb/build-deb line 11).
%setup -c -T
tar -xzf %{SOURCE0}

%build
# Locate the sysbox tree extracted by %prep (it lives at ./sysbox/).
cd sysbox

# ARCH mirrors the deb Makefile convention (amd64 / arm64).
case "$(uname -m)" in
    x86_64)  export ARCH=amd64 ;;
    aarch64) export ARCH=arm64 ;;
    *) echo "Unsupported host arch: $(uname -m)" >&2; exit 1 ;;
esac

# Same target the deb `rules` file invokes. `sysbox-static-local` is the
# host-local variant of `sysbox-static` (which would otherwise spin up a
# nested build container — unwanted inside our build container).
make sysbox-static-local

%install
rm -rf %{buildroot}

# ARCH again (matches the build phase).
case "$(uname -m)" in
    x86_64)  ARCH=amd64 ;;
    aarch64) ARCH=arm64 ;;
esac

# Binaries — same destinations as the deb (/usr/bin/, mode 0755).
install -D -m 0755 sysbox/sysbox-fs/build/${ARCH}/sysbox-fs   %{buildroot}%{_bindir}/sysbox-fs
install -D -m 0755 sysbox/sysbox-mgr/build/${ARCH}/sysbox-mgr %{buildroot}%{_bindir}/sysbox-mgr
install -D -m 0755 sysbox/sysbox-runc/build/${ARCH}/sysbox-runc %{buildroot}%{_bindir}/sysbox-runc

# systemd units — /usr/lib/systemd/system/ on rpm-based distros.
install -D -m 0644 %{SOURCE1} %{buildroot}%{_unitdir}/sysbox.service
install -D -m 0644 %{SOURCE2} %{buildroot}%{_unitdir}/sysbox-fs.service
install -D -m 0644 %{SOURCE3} %{buildroot}%{_unitdir}/sysbox-mgr.service

# sysctl drop-in — /usr/lib/sysctl.d/.
install -D -m 0644 %{SOURCE4} %{buildroot}%{_prefix}/lib/sysctl.d/99-sysbox-sysctl.conf

# modules-load drop-in — /etc/modules-load.d/ (the deb rules installs to
# the same path via the SOURCE_FILES list).
install -D -m 0644 %{SOURCE5} %{buildroot}%{_sysconfdir}/modules-load.d/50-sysbox-mod.conf

# Vendored post-install logic, dropped under libexec so we can invoke it
# from %post without inlining the entire script body.
install -D -m 0755 %{SOURCE6} %{buildroot}%{_libexecdir}/sysbox/sysbox-postinstall.sh

# Licence texts and notices. The binaries are statically linked, so record
# the glibc and libseccomp versions used.
install -m 0644 %{SOURCE7} LGPL-2.1
install -m 0644 %{SOURCE8} STATIC-LINKING.md
install -m 0644 sysbox/sysbox-runc/NOTICE NOTICE.sysbox-runc
rpm -q --qf '- %%{NAME} %%{VERSION}-%%{RELEASE} (%%{ARCH}), static\n' \
    glibc-static libseccomp-static >> STATIC-LINKING.md

%files
%license sysbox/LICENSE LGPL-2.1 NOTICE.sysbox-runc
%doc sysbox/README.md sysbox/OSS_DISCLOSURES.md STATIC-LINKING.md
%{_bindir}/sysbox-fs
%{_bindir}/sysbox-mgr
%{_bindir}/sysbox-runc
%{_unitdir}/sysbox.service
%{_unitdir}/sysbox-fs.service
%{_unitdir}/sysbox-mgr.service
%{_prefix}/lib/sysctl.d/99-sysbox-sysctl.conf
%{_sysconfdir}/modules-load.d/50-sysbox-mod.conf
%{_libexecdir}/sysbox/sysbox-postinstall.sh
%dir %{_libexecdir}/sysbox

%pre
# Mirror deb preinst: enforce a minimum kernel ≥ 5.5.0.
# The deb additionally gates against a distro-version matrix; we drop that
# matrix here because Fedora 42 (kernel 6.14) and modern RHEL-likes always
# pass the kernel check, and we don't want to refuse newer Fedora releases
# the matrix doesn't enumerate.
if [ "$1" -gt 0 ] 2>/dev/null; then  # install / upgrade, not removal
    kernel_ver=$(uname -r | cut -d'-' -f1)
    required="5.5.0"
    # Lexicographic compare suffices for SemVer parts (5.5 vs 5.4 etc).
    lowest=$(printf '%s\n%s\n' "$kernel_ver" "$required" | sort -V | head -n1)
    if [ "$lowest" != "$required" ]; then
        echo "ERROR Unsupported linux kernel \"$kernel_ver\" (sysbox requires >= $required)" >&2
        echo "  Suggestion: upgrade the kernel package, then retry the sysbox-ce install" >&2
        exit 1
    fi
fi

%post
# Systemd enable + start on install, restart on upgrade.
%systemd_post sysbox.service sysbox-fs.service sysbox-mgr.service

# Defer host-config work to the vendored post-install script (sysctl tuning,
# sysbox user, /etc/docker/daemon.json merge). The script is idempotent —
# safe to re-run on upgrade.
if [ -x %{_libexecdir}/sysbox/sysbox-postinstall.sh ]; then
    %{_libexecdir}/sysbox/sysbox-postinstall.sh || :
fi

%preun
%systemd_preun sysbox.service sysbox-fs.service sysbox-mgr.service

%postun
%systemd_postun_with_restart sysbox.service sysbox-fs.service sysbox-mgr.service

# On full uninstall ($1 == 0), strip sysbox-runc from /etc/docker/daemon.json
# — mirrors deb postrm's `purge` branch. We do NOT remove the sysbox user
# (file ownership concerns, same rationale as the deb).
if [ "$1" -eq 0 ]; then
    cfg=/etc/docker/daemon.json
    if [ -f "$cfg" ] && command -v jq >/dev/null 2>&1; then
        if [ "$(jq 'has("runtimes")' "$cfg")" = "true" ] && \
           [ "$(jq '.runtimes | has("sysbox-runc")' "$cfg")" = "true" ]; then
            tmp=$(mktemp /tmp/sysbox-postrm.XXXXXX)
            jq 'del(.runtimes."sysbox-runc")' "$cfg" > "$tmp" && cp "$tmp" "$cfg"
            rm -f "$tmp"
            if command -v docker >/dev/null 2>&1 && pidof dockerd >/dev/null 2>&1; then
                kill -SIGHUP "$(pidof dockerd)" || :
            fi
        fi
    fi
fi

%changelog
* Sun May 24 2026 Downstream rebuild - 0.7.0-0~rebuild1
- Initial downstream RPM packaging.
- Mirrors the Debian packaging at deb/sysbox-ce/ (control + rules + postinst).
- Target distribution: Fedora 42 (and RHEL/CentOS Stream 9+ best-effort).
