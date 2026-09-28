# Statically linked LGPL libraries in the Sysbox packages

The `sysbox-fs`, `sysbox-mgr` and `sysbox-runc` binaries in the `.deb` and
`.rpm` packages built from this repository are statically linked with two
libraries under the GNU Lesser General Public License, version 2.1:

| Library | Licence | Linked into |
|---|---|---|
| GNU C Library (glibc) | LGPL-2.1-or-later (some parts carry other permissive licences; see the glibc source) | sysbox-fs, sysbox-mgr, sysbox-runc |
| libseccomp | LGPL-2.1-only | sysbox-fs, sysbox-runc |

The full licence text is in the `LGPL-2.1` file installed next to this file.
The `sysbox-deploy-k8s` container image reuses the `.deb` binaries, so the
same applies to it.

The Arch Linux package is linked dynamically: it uses the system's `glibc`
and `libseccomp` packages and contains no copy of either library.

Sysbox itself is licensed under the Apache License, Version 2.0. The Go
modules compiled into the binaries and their licences are listed in
`OSS_DISCLOSURES.md`.

## Library versions

The build links the static libraries that the build image's distribution
provides: `libc6-dev` and `libseccomp-dev` on Ubuntu 24.04 for the `.deb`
package, `glibc-static` and `libseccomp-static` on Fedora 42 for the `.rpm`
package. The exact package versions used for this build are recorded at the
end of this file when the package is built.

## Source code

Library sources:

- glibc: upstream <https://sourceware.org/glibc/>. The distribution source
  packages for the recorded versions are published at
  <https://launchpad.net/ubuntu/+source/glibc> (Ubuntu) and
  <https://src.fedoraproject.org/rpms/glibc> (Fedora).
- libseccomp: upstream <https://github.com/seccomp/libseccomp>. The
  distribution source packages are published at
  <https://launchpad.net/ubuntu/+source/libseccomp> (Ubuntu) and
  <https://src.fedoraproject.org/rpms/libseccomp> (Fedora).

Sysbox source, including the changes made in this modified version:

- <https://github.com/mait-sh/sysbox-src> holds the complete source of the
  packages, with every Sysbox component in its own directory.
- A package version of the form `<version>+<build>.g<commit>` was built from
  the tag `v<version>-fork.g<commit>` of that repository. For example, the
  source of a `0.7.1` package whose version ends in `.gacd77c9` is the tag
  `v0.7.1-fork.gacd77c9`.

## Relinking with a modified library

The object files are not published separately. You can rebuild the binaries
from the public source against a modified library:

1. Check out the source that matches the package:

   ```sh
   git clone https://github.com/mait-sh/sysbox-src.git
   cd sysbox-src
   git checkout v<version>-fork.g<commit>
   ```

2. Install your modified static library (`libc.a` or `libseccomp.a` and its
   headers) in the build environment in place of the distribution's copy.
   The build environments are defined by `sysbox-pkgr/deb/ubuntu-noble/Dockerfile`
   and `sysbox-pkgr/rpm/fedora-42/Dockerfile`.
3. Build the statically linked binaries with `make sysbox-static-local` in the
   top-level directory, the target the packages use. To build a whole package,
   run `make -C sysbox-pkgr sysbox-ce-repo "$PWD"` once, then
   `make -C sysbox-pkgr/deb generic EDITION=ce` or
   `make -C sysbox-pkgr/rpm fedora-42 EDITION=ce`.
4. Replace `/usr/bin/sysbox-fs`, `/usr/bin/sysbox-mgr` and `/usr/bin/sysbox-runc`
   with the rebuilt binaries.

`make sysbox-local` builds dynamically linked binaries instead, which then use
the shared libraries installed on the system.

## Versions linked into this build

