#!/bin/bash
#
# Sysbox post-install configuration script (RPM / Arch port).
#
# This is a vendored copy of `deb/sysbox-ce/sysbox-ce.postinst` with two
# carefully-scoped changes so it can run from an RPM `%post` or an Arch
# `post_install()` without the debhelper-only conventions:
#
#   1. The Debian `. /usr/share/debconf/confmodule` line is replaced with a
#      no-op shim — debconf does not exist on RPM/Arch and is not needed
#      (this script never calls db_* helpers anyway).
#
#   2. The `case "$1" in configure) ...` dispatcher is removed; the script
#      is invoked unconditionally with `config_sysbox` as the entry point.
#      RPM/Arch hooks do not use the Debian configure/upgrade/abort verb
#      convention.
#
# All other logic — sysctl tuning, sysbox user creation, daemon.json merge,
# kernel-headers warning — is verbatim from the deb postinst so the two
# packages produce identical post-install state.
#
# Reference: modules/sysbox/sysbox-pkgr/deb/sysbox-ce/sysbox-ce.postinst
#

set -e

# debconf shim (no-op on rpm/arch)
: # nothing to source

# Dockerd default configuration dir/file.
dockerCfgDir="/etc/docker"
dockerCfgFile="${dockerCfgDir}/daemon.json"

# sysbox-fs' default mountpoint path.
sysboxfs_mountpoint="/var/lib/sysboxfs"

# UID-shifting module.
shiftfs_module="shiftfs"

# Kernel's pool-size of inotify resources.
inotify_pool_size=1048576

# Default docker network parameters.
bip_subnet="172.20.0.1/16"
pool_subnet="172.25.0.0/16"

# Docker config state.
docker_network_config_changed="false"
docker_runtime_config_changed="false"

# Temp file for jq write operations.
tmpfile=$(mktemp /tmp/installer-scr.XXXXXX)
trap 'rm -f "${tmpfile}"' EXIT

# Kernel keyring sizing (see deb postinst comments for rationale).
kernel_keys_maxkeys=20000
kernel_keys_maxbytes=1400000

# Kernel pid_max.
kernel_pid_max=4194304

create_sysboxfs_mountpoint() {
    if [[ -d ${sysboxfs_mountpoint} ]]; then
        return
    fi
    mkdir -p ${sysboxfs_mountpoint}
    if [[ ! -d ${sysboxfs_mountpoint} ]]; then
        exit 1
    fi
}

is_wsl() {
    case "$(uname -r)" in
    *microsoft*) true ;;
    *Microsoft*) true ;;
    *) false ;;
    esac
}

enable_unprivileged_userns() {
    if [ -f "/proc/sys/kernel/unprivileged_userns_clone" ]; then
        local val=$(sysctl kernel.unprivileged_userns_clone)
        if [[ "${val##* }" = 0 ]]; then
            sysctl -w kernel.unprivileged_userns_clone=1 >/dev/null 2>&1
        fi
    fi
}

define_inotify_resources() {
    local val=$(sysctl fs.inotify.max_queued_events)
    if [[ "${val##* }" -lt ${inotify_pool_size} ]]; then
        sysctl -w fs.inotify.max_queued_events=${inotify_pool_size} >/dev/null 2>&1
    fi

    val=$(sysctl fs.inotify.max_user_watches)
    if [[ "${val##* }" -lt ${inotify_pool_size} ]]; then
        sysctl -w fs.inotify.max_user_watches=${inotify_pool_size} >/dev/null 2>&1
    fi

    val=$(sysctl fs.inotify.max_user_instances)
    if [[ "${val##* }" -lt ${inotify_pool_size} ]]; then
        sysctl -w fs.inotify.max_user_instances=${inotify_pool_size} >/dev/null 2>&1
    fi
}

define_keyring_resources() {
    local val=$(sysctl kernel.keys.maxkeys)
    if [[ "${val##* }" -lt ${kernel_keys_maxkeys} ]]; then
        sysctl -w kernel.keys.maxkeys=${kernel_keys_maxkeys} >/dev/null 2>&1
    fi

    val=$(sysctl kernel.keys.maxbytes)
    if [[ "${val##* }" -lt ${kernel_keys_maxbytes} ]]; then
        sysctl -w kernel.keys.maxbytes=${kernel_keys_maxbytes} >/dev/null 2>&1
    fi
}

define_pidmax_resources() {
    local val=$(sysctl kernel.pid_max)
    if [[ "${val##* }" -lt ${kernel_pid_max} ]]; then
        sysctl -w kernel.pid_max=${kernel_pid_max} >/dev/null 2>&1
    fi
}

add_sysbox_user() {
    if ! getent passwd | grep "^sysbox:" >/dev/null 2>&1; then
        useradd -s /bin/false sysbox
    fi
}

adjust_docker_config_runtime() {
    if [ $(jq 'has("runtimes")' ${dockerCfgFile}) = "false" ]; then
        jq --indent 4 '. + {"runtimes": {"sysbox-runc": {"path": "/usr/bin/sysbox-runc"}}}' \
            ${dockerCfgFile} >${tmpfile} && cp ${tmpfile} ${dockerCfgFile}
        docker_runtime_config_changed="true"
    elif [ $(jq '.runtimes | has("sysbox-runc")' ${dockerCfgFile}) = "false" ]; then
        jq --indent 4 '.runtimes |= . + {"sysbox-runc": {"path": "/usr/bin/sysbox-runc"}}' \
            ${dockerCfgFile} >${tmpfile} && cp ${tmpfile} ${dockerCfgFile}
        docker_runtime_config_changed="true"
    elif grep -q "/usr/local/sbin/sysbox-runc" ${dockerCfgFile}; then
        sed -i "s@/usr/local/sbin/sysbox-runc@/usr/bin/sysbox-runc@g" ${dockerCfgFile}
        docker_runtime_config_changed="true"
    fi

    if [ ${docker_runtime_config_changed} = false ] &&
        command -v docker >/dev/null 2>&1 &&
        ! docker info 2>&1 | egrep -q "Runtimes:.*sysbox-runc"; then
        docker_runtime_config_changed="true"
    fi
}

system_local_subnet() {
    if ip route get ${1} | egrep -q "via $(ip route | awk '/default/ {print $3}')"; then
        return 1
    fi
    return 0
}

adjust_docker_config_network() {
    local bip_host=$(echo ${bip_subnet} | cut -d'/' -f 1)
    local pool_host=$(echo ${pool_subnet} | cut -d'/' -f 1)

    if [ $(jq 'has("bip")' ${dockerCfgFile}) = "false" ] ||
        [ $(jq '."bip"' ${dockerCfgFile}) = "\"\"" ]; then
        if system_local_subnet ${bip_host} &&
            ! ip -4 address show dev docker0 2>/dev/null | egrep -q "${bip_subnet}"; then
            echo -e "\nDocker bridge-ip network to configure (${bip_subnet}) overlaps" \
                "with existing system subnet. Installation process will skip this docker" \
                "network setting. Please manually configure docker's 'bip' subnet to" \
                "avoid connectivity issues.\n"
        else
            jq --arg bip ${bip_subnet} --indent 4 '. + {"bip": $bip}' ${dockerCfgFile} \
                >${tmpfile} && cp ${tmpfile} ${dockerCfgFile}
            docker_network_config_changed="true"
        fi
    fi

    if [ $(jq 'has("default-address-pools")' ${dockerCfgFile}) = "false" ] ||
        [ $(jq '."default-address-pools" | length' ${dockerCfgFile}) -eq "0" ]; then
        if system_local_subnet ${pool_host}; then
            echo -e "\nDocker default-address-pool to configure (${pool_subnet}) overlaps" \
                "with existing system subnet. Installation process will skip this docker" \
                "network setting. Please manually configure docker's 'default-address-pool'" \
                "subnet to avoid connectivity issues.\n"
        else
            jq --arg subnet ${pool_subnet} --indent 4 \
                '."default-address-pools"[0] |= . + {"base": $subnet, "size": 24}' ${dockerCfgFile} \
                >${tmpfile} && cp ${tmpfile} ${dockerCfgFile}
            docker_network_config_changed="true"
        fi
    fi
}

adjust_docker_config() {
    if [[ ! -f ${dockerCfgFile} ]] || [[ ! -s ${dockerCfgFile} ]]; then
        mkdir -p ${dockerCfgDir}
        touch ${dockerCfgFile}
        echo -e "{\n}" >${dockerCfgFile}
    fi

    adjust_docker_config_runtime
    adjust_docker_config_network

    if ! docker_running; then
        return
    fi

    if [[ ${docker_network_config_changed} = "true" ]]; then
        if ! docker ps -a | wc -l | egrep -q "1$"; then
            echo -e "\nDocker service was not restarted to avoid affecting existing" \
                "containers. Please remove them and restart Docker by doing:\n" \
                "\t\"docker rm \$(docker ps -a -q) -f &&" \
                "sudo systemctl restart docker\"\n"
        else
            systemctl restart docker
            return
        fi
    fi

    if [ ${docker_runtime_config_changed} = true ]; then
        kill -SIGHUP $(pidof dockerd)
    fi
}

docker_installed() {
    command -v dockerd >/dev/null 2>&1
}

docker_running() {
    pidof dockerd >/dev/null 2>&1
}

#
# Verify if kernel-headers are properly installed and alert the user otherwise.
# Uses rpm/pacman query depending on what is available — the deb postinst
# uses dpkg.
#
check_kernel_headers() {
    local kver=$(uname -r)
    if command -v rpm >/dev/null 2>&1; then
        if ! rpm -q "kernel-devel-${kver}" >/dev/null 2>&1 && \
           ! rpm -q "kernel-headers" >/dev/null 2>&1; then
            echo -e "\nThe linux kernel headers package was not found. This may be" \
                "expected by user applications running within Sysbox containers." \
                "Please install it with this command:\n" \
                "\t\"sudo dnf install -y kernel-devel-\$(uname -r) kernel-headers\"\n"
        fi
    elif command -v pacman >/dev/null 2>&1; then
        if ! pacman -Qi linux-headers >/dev/null 2>&1; then
            echo -e "\nThe linux kernel headers package was not found. This may be" \
                "expected by user applications running within Sysbox containers." \
                "Please install it with this command:\n" \
                "\t\"sudo pacman -S --noconfirm linux-headers\"\n"
        fi
    fi
}

config_sysbox() {
    create_sysboxfs_mountpoint

    if is_wsl; then
        echo "WSL2 detected, enable_unprivileged_userns skipped."
    else
        enable_unprivileged_userns
    fi

    define_inotify_resources
    define_keyring_resources
    define_pidmax_resources
    add_sysbox_user

    if docker_installed; then
        adjust_docker_config
    fi

    if is_wsl; then
        echo "WSL2 detected, check_kernel_headers skipped."
    else
        check_kernel_headers
    fi
}

# Entry point — RPM/Arch hooks invoke us with no arguments.
config_sysbox

exit 0
