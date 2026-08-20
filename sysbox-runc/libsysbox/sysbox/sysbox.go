//
// Copyright 2019-2020 Nestybox, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package sysbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	sh "github.com/nestybox/sysbox-libs/idShiftUtils"
	linuxUtils "github.com/nestybox/sysbox-libs/linuxUtils"
	libutils "github.com/nestybox/sysbox-libs/utils"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/urfave/cli"
)

// Holds sysbox-specific config
type Sysbox struct {
	Id                  string
	Mgr                 *Mgr
	Fs                  *Fs
	RootfsUidShiftType  sh.IDShiftType
	BindMntUidShiftType sh.IDShiftType
	RootfsCloned        bool
	SwitchDockerDns     bool
	OrigRootfs          string
	OrigMounts          []specs.Mount
	IDshiftIgnoreList   []string
}

func NewSysbox(id string, withMgr, withFs bool) *Sysbox {

	sysMgr := NewMgr(id, withMgr)
	sysFs := NewFs(id, withFs)

	return &Sysbox{
		Id:  id,
		Mgr: sysMgr,
		Fs:  sysFs,
	}
}

func checkKernelVersion(distro string) error {
	var (
		reqMaj, reqMin int
		major, minor   int
	)

	rel, err := linuxUtils.GetKernelRelease()
	if err != nil {
		return err
	}

	major, minor, err = linuxUtils.ParseKernelRelease(rel)
	if err != nil {
		return err
	}

	if distro == "ubuntu" {
		reqMaj = minKernelUbuntu.major
		reqMin = minKernelUbuntu.minor
	} else {
		reqMaj = minKernel.major
		reqMin = minKernel.minor
	}

	supported := false
	if major > reqMaj {
		supported = true
	} else if major == reqMaj {
		if minor >= reqMin {
			supported = true
		}
	}

	if !supported {
		s := []string{strconv.Itoa(reqMaj), strconv.Itoa(reqMin)}
		kver := strings.Join(s, ".")
		return fmt.Errorf("%s kernel release %v is not supported; need >= %v", distro, rel, kver)
	}

	return nil
}

// rootfsIDProbePaths are paths that exist in virtually every container image
// and are owned by root inside the image. On an overlayfs rootfs they are
// served from the image (lower) layers, so their ownership reflects whether the
// image content itself has been ID-shifted, independently of the ownership of
// the rootfs directory (which comes from the overlayfs upper layer).
var rootfsIDProbePaths = []string{
	"etc/passwd",
	"usr/bin",
	"bin",
	"sbin",
	"lib",
	"etc",
	"usr",
}

// idIsMapped reports whether the given host ID falls within the container's
// mapped range [hostIDMap, hostIDMap+size).
func idIsMapped(id, hostIDMap, size uint32) bool {
	return size > 0 && id >= hostIDMap && id-hostIDMap < size
}

// rootfsHasUnmappedIDs samples a few well-known image paths under the rootfs and
// reports whether any of them is still owned by host ID 0 while the container
// maps its root to a non-zero host ID. Such content is not resolvable inside the
// container's user-ns and would surface as nobody:nogroup (65534).
//
// The probe is a fixed, small number of lstat() calls (no walk), so its cost is
// negligible relative to container start.
func rootfsHasUnmappedIDs(rootfs string, hostUidMap, hostGidMap uint32) bool {
	for _, p := range rootfsIDProbePaths {
		fi, err := os.Lstat(filepath.Join(rootfs, p))
		if err != nil {
			continue
		}

		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}

		if (st.Uid == 0 && hostUidMap != 0) || (st.Gid == 0 && hostGidMap != 0) {
			return true
		}
	}

	return false
}

// needUidShiftOnRootfs checks if uid/gid shifting is required on the container's rootfs.
func needUidShiftOnRootfs(spec *specs.Spec) (bool, error) {
	var hostUidMap, hostGidMap uint32
	var uidMapSize, gidMapSize uint32

	// the uid map is assumed to be present
	for _, mapping := range spec.Linux.UIDMappings {
		if mapping.ContainerID == 0 {
			hostUidMap = mapping.HostID
			uidMapSize = mapping.Size
			break
		}
	}

	// the gid map is assumed to be present
	for _, mapping := range spec.Linux.GIDMappings {
		if mapping.ContainerID == 0 {
			hostGidMap = mapping.HostID
			gidMapSize = mapping.Size
			break
		}
	}

	// find the rootfs owner
	rootfs := spec.Root.Path

	fi, err := os.Stat(rootfs)
	if err != nil {
		return false, err
	}

	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("failed to convert to syscall.Stat_t")
	}

	rootfsUid := st.Uid
	rootfsGid := st.Gid

	// Use shifting when the rootfs is owned by true root and the containers uid/gid root
	// mapping don't match the container's rootfs owner.
	if rootfsUid == 0 && rootfsGid == 0 &&
		hostUidMap != rootfsUid && hostGidMap != rootfsGid {
		return true, nil
	}

	// The ownership of the rootfs directory alone is not conclusive: when the
	// rootfs is on overlayfs, that directory inherits its ownership from the
	// upper layer, not from the image.
	//
	// sysbox-mgr chowns the upper layer into the container's ID range while the
	// container runs and reverts that chown when the container stops or pauses.
	// If the revert never happens (host reboot, or sysbox-mgr killed while
	// containers are running), the upper layer -- and hence the rootfs directory
	// -- stays in the mapped range across the restart, while the image content
	// in the lower layers is still owned by host ID 0.
	//
	// The check above then concludes "no shift needed" and we skip ID-shifting
	// for the whole rootfs. Every image file is then left unmapped inside the
	// container's user-ns: files show up as nobody:nogroup (65534) and setuid
	// binaries such as sudo stop working.
	//
	// So when the rootfs directory already looks mapped, sample a few image-owned
	// paths underneath it. If any of them is still owned by an unmapped host ID,
	// shifting is still required. Scoping the probe to the "already mapped"
	// case keeps the original behavior everywhere else, and sampling image paths
	// (rather than any file) avoids mistaking a stray root-owned file written
	// through the merged mount for an unshifted rootfs.
	if idIsMapped(rootfsUid, hostUidMap, uidMapSize) ||
		idIsMapped(rootfsGid, hostGidMap, gidMapSize) {
		if rootfsHasUnmappedIDs(rootfs, hostUidMap, hostGidMap) {
			return true, nil
		}
	}

	return false, nil
}

// checkUidShifting returns the type of UID shifting needed (if any) for the
// container. The first return value indicates the type of UID shifting to be
// used for the container's rootfs, while the second indicates the type of UID
// shifting for container bind-mounts.
func CheckUidShifting(sysMgr *Mgr, spec *specs.Spec) (sh.IDShiftType, sh.IDShiftType, error) {

	shiftfsOk := sysMgr.Config.ShiftfsOk
	shiftfsOnOvfsOk := sysMgr.Config.ShiftfsOnOverlayfsOk

	idMapMountOk := sysMgr.Config.IDMapMountOk
	ovfsOnIDMapMountOk := sysMgr.Config.OverlayfsOnIDMapMountOk

	rootfsShiftType := sysMgr.Config.RootfsUidShiftType

	if rootfsShiftType == sh.NoShift {

		useShiftfsOnRootfs := false
		useIDMapMountOnRootfs := false

		rootPathFs, err := libutils.GetFsName(spec.Root.Path)
		if err != nil {
			return sh.NoShift, sh.NoShift, err
		}

		if idMapMountOk {
			if rootPathFs == "overlayfs" && ovfsOnIDMapMountOk {
				useIDMapMountOnRootfs = true
			}
		}

		if shiftfsOk {
			if rootPathFs == "overlayfs" && shiftfsOnOvfsOk {
				useShiftfsOnRootfs = true
			}
		}

		needShiftOnRootfs, err := needUidShiftOnRootfs(spec)
		if err != nil {
			return sh.NoShift, sh.NoShift, fmt.Errorf("failed to check uid-shifting requirement on rootfs: %s", err)
		}

		// Check uid shifting type to be used for the container's rootfs.
		//
		// We do it via ID-mapping (preferably) or via shiftfs (if available on
		// the host) or by chown'ing the rootfs hierarchy. Chowning is the least
		// preferred and slowest approach, but won't disrupt anything on the host
		// since the container's rootfs is dedicated to the container (no other
		// entity in the system will use it while the container is running).
		if needShiftOnRootfs {
			if useIDMapMountOnRootfs {
				rootfsShiftType = sh.IDMappedMount
			} else if useShiftfsOnRootfs {
				rootfsShiftType = sh.Shiftfs
			} else {
				rootfsShiftType = sh.Chown
			}
		}
	}

	// Check uid shifting type to be used for the container's bind mounts.
	//
	// For bind mounts, we use ID-mapping or shiftfs, but never chown. Chowning
	// for bind mounts is not a good idea since we don't know what's being bind
	// mounted (e.g., the bind mount could be a user's home dir, a critical
	// system file, etc.).
	bindMountShiftType := sh.NoShift

	if idMapMountOk && shiftfsOk {
		bindMountShiftType = sh.IDMappedMountOrShiftfs
	} else if idMapMountOk {
		bindMountShiftType = sh.IDMappedMount
	} else if shiftfsOk {
		bindMountShiftType = sh.Shiftfs
	}

	return rootfsShiftType, bindMountShiftType, nil
}

// CheckHostConfig checks if the host is configured appropriately to run a
// container with sysbox
func CheckHostConfig(context *cli.Context, spec *specs.Spec) error {

	distro, err := linuxUtils.GetDistro()
	if err != nil {
		return err
	}

	if !context.GlobalBool("no-kernel-check") {
		if err := checkKernelVersion(distro); err != nil {
			return fmt.Errorf("kernel version check failed: %v", err)
		}
	}

	if err := checkUnprivilegedUserns(); err != nil {
		return err
	}

	return nil
}
