package passthrough

import (
	"fmt"
	"os"
	"syscall"

	"github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// Passthrough devicer implementation.
//
// This devicer handles user-requested devices (e.g., /dev/kvm, /dev/dri, /dev/dxg)
// that live outside of the paths handled by the other devicers. Unlike those devicers,
// the passthrough devicer clones the matched host device onto a regular filesystem under
// the devMgr's per-container directory (see DeviceMgr.createDevice). The clone is chowned
// to the container's mapped-root uid:gid and bind-mounted into the container, so the
// device shows up as "0:0" inside the container without requiring shiftfs. The original
// device is dropped from the container's oci-spec by the devMgr, so it reaches the
// container solely via the bind-mount.
//
// The major/minor numbers are always obtained by stat'ing the host device node; they are
// never hard-coded, as they may vary across hosts and kernels.

type passthroughDevicer struct{}

func NewPassthroughDevicer() *passthroughDevicer {
	return &passthroughDevicer{}
}

func (d *passthroughDevicer) Discover(dev *specs.LinuxDevice) (*specs.LinuxDevice, error) {

	// The passthrough devicer never creates devices by default; there is nothing to
	// discover unless a concrete device is requested.
	if dev == nil {
		return nil, nil
	}

	info, err := os.Stat(dev.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to stat device %s: %v", dev.Path, err)
	}

	rdev := uint64(info.Sys().(*syscall.Stat_t).Rdev)
	fileMode := info.Mode()

	passthroughDev := &specs.LinuxDevice{
		Path:     dev.Path,
		Type:     "c",
		Major:    int64(unix.Major(rdev)),
		Minor:    int64(unix.Minor(rdev)),
		FileMode: &fileMode,
		UID:      dev.UID,
		GID:      dev.GID,
	}

	return passthroughDev, nil
}

func (d *passthroughDevicer) Create(device *specs.LinuxDevice) error {
	return nil
}

func (d *passthroughDevicer) CreateByDefault() bool {
	return false
}

func (d *passthroughDevicer) Clone() bool {
	return true
}
