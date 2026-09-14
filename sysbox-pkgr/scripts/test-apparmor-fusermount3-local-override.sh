#!/bin/bash
#
# Unit-style checks for the fusermount3 AppArmor local-override baked into
# sysbox-ce packaging. No root / no AppArmor daemon
# required — exercises the same idempotency + marker semantics against a
# fake root, and asserts packaging scripts stay in sync with the shared
# marker string.
#
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
PKGR_DIR=$(cd "${SCRIPT_DIR}/.." && pwd)

MARKER='/var/lib/sysboxfs/**/,'
RULE_LINE='mount fstype=@{fuse_types} options=(nosuid,nodev) options in (ro,rw,noatime,dirsync,nodiratime,noexec,sync) -> /var/lib/sysboxfs/**/,'

PACKAGING_SCRIPTS=(
	"${PKGR_DIR}/deb/sysbox-ce/sysbox-ce.postinst"
	"${PKGR_DIR}/rpm/sysbox-ce/sysbox-postinstall.sh"
	"${PKGR_DIR}/arch/sysbox-ce/sysbox-postinstall.sh"
	"${PKGR_DIR}/k8s/scripts/sysbox-deploy-k8s.sh"
)

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

pass() {
	echo "ok - $*"
}

# Portable copy of install_apparmor_fusermount3_local_override that honors
# ROOT= so we can exercise it without touching the host.
install_apparmor_fusermount3_local_override_under_root() {
	local root="${ROOT:?ROOT must be set}"
	local profile="${root}/etc/apparmor.d/fusermount3"
	local local_override="${root}/etc/apparmor.d/local/fusermount3"
	local marker='/var/lib/sysboxfs/**/,'
	local reloads_file="${root}/.apparmor_reloads"

	if [ ! -f "${profile}" ]; then
		return 0
	fi

	if ! grep -qF "${marker}" "${local_override}" 2>/dev/null; then
		mkdir -p "${root}/etc/apparmor.d/local"
		cat >>"${local_override}" <<'SYSBOX_APPARMOR_LOCAL_FUSERMOUNT3'
# Local addition (/etc/apparmor.d/local/fusermount3):
# sysbox-fs mounts its per-container FUSE filesystem under
# /var/lib/sysboxfs/<container-id>/, which the stock Ubuntu fusermount3
# profile's whitelist does not cover (HOME, /mnt, /media, /tmp,
# /run/user/<uid>, /cvmfs only). Without this, every sysbox container create
# fails at "pre-register with sysbox-fs" with an AppArmor DENIED
# (operation=mount, profile=fusermount3, info="failed mntpnt match") in dmesg.
mount fstype=@{fuse_types} options=(nosuid,nodev) options in (ro,rw,noatime,dirsync,nodiratime,noexec,sync) -> /var/lib/sysboxfs/**/,
umount /var/lib/sysboxfs/**/,
SYSBOX_APPARMOR_LOCAL_FUSERMOUNT3
	fi

	# Stand-in for apparmor_parser -r (best-effort in production).
	echo reload >>"${reloads_file}"
}

# --- packaging scripts carry the marker + call site -------------------
for scr in "${PACKAGING_SCRIPTS[@]}"; do
	[ -f "${scr}" ] || fail "missing packaging script: ${scr}"
	grep -qF "${MARKER}" "${scr}" || fail "${scr}: missing marker ${MARKER}"
	grep -q "install_apparmor_fusermount3_local_override" "${scr}" ||
		fail "${scr}: missing install_apparmor_fusermount3_local_override"
	grep -qF "${RULE_LINE}" "${scr}" || fail "${scr}: missing mount rule line"
	grep -qF "# Local addition (/etc/apparmor.d/local/fusermount3):" "${scr}" ||
		fail "${scr}: missing local-addition header"
	grep -q "failed mntpnt match" "${scr}" || fail "${scr}: missing mntpnt citation"
	pass "$(basename "$(dirname "${scr}")")/$(basename "${scr}"): marker + call site"
done

# --- skip when stock profile absent -----------------------------------
tmp=$(mktemp -d)
trap 'rm -rf "${tmp}"' EXIT
ROOT="${tmp}"
mkdir -p "${tmp}/etc/apparmor.d"
install_apparmor_fusermount3_local_override_under_root
[ ! -e "${tmp}/etc/apparmor.d/local/fusermount3" ] ||
	fail "wrote local override despite absent stock profile"
pass "skip when stock /etc/apparmor.d/fusermount3 absent"

# --- append on first run; idempotent on second ------------------------
tmp2=$(mktemp -d)
ROOT="${tmp2}"
mkdir -p "${tmp2}/etc/apparmor.d"
echo "# stock fusermount3 stub" >"${tmp2}/etc/apparmor.d/fusermount3"
# Pre-existing unrelated local content must not be clobbered.
mkdir -p "${tmp2}/etc/apparmor.d/local"
echo "# pre-existing local rule" >"${tmp2}/etc/apparmor.d/local/fusermount3"

install_apparmor_fusermount3_local_override_under_root
grep -qF "# pre-existing local rule" "${tmp2}/etc/apparmor.d/local/fusermount3" ||
	fail "clobbered pre-existing local override content"
grep -qF "${MARKER}" "${tmp2}/etc/apparmor.d/local/fusermount3" ||
	fail "marker not appended on first run"
grep -qF "${RULE_LINE}" "${tmp2}/etc/apparmor.d/local/fusermount3" ||
	fail "mount rule not appended on first run"
[ "$(wc -l <"${tmp2}/.apparmor_reloads")" -eq 1 ] ||
	fail "expected one reload after first install"

before_bytes=$(wc -c <"${tmp2}/etc/apparmor.d/local/fusermount3")
before_markers=$(grep -cF "${MARKER}" "${tmp2}/etc/apparmor.d/local/fusermount3")
# Marker appears on both the mount and umount rule lines (by convention).
[ "${before_markers}" -ge 1 ] || fail "expected marker present after first install"
install_apparmor_fusermount3_local_override_under_root
after_bytes=$(wc -c <"${tmp2}/etc/apparmor.d/local/fusermount3")
after_markers=$(grep -cF "${MARKER}" "${tmp2}/etc/apparmor.d/local/fusermount3")
[ "${before_bytes}" -eq "${after_bytes}" ] || fail "second run appended again (not idempotent)"
[ "${before_markers}" -eq "${after_markers}" ] ||
	fail "marker count grew on re-run (${before_markers} -> ${after_markers})"
[ "$(wc -l <"${tmp2}/.apparmor_reloads")" -eq 2 ] ||
	fail "expected reload on every run (best-effort), got $(wc -l <"${tmp2}/.apparmor_reloads")"
pass "append once + idempotent re-run (reload still attempted)"

rm -rf "${tmp2}"
trap 'rm -rf "${tmp}"' EXIT

echo "All apparmor fusermount3 local-override packaging checks passed."
