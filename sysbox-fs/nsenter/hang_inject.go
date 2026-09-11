//go:build sysbox_fs_faultinject

package nsenter

import (
	"os"
	"regexp"

	"github.com/sirupsen/logrus"
)

// maybeHangMountForTest blocks forever when SYSBOX_FS_TEST_HANG_MOUNT matches
// the mount target. Build with -tags sysbox_fs_faultinject only; never ship in
// release binaries.
func maybeHangMountForTest(target string) {
	pat := os.Getenv("SYSBOX_FS_TEST_HANG_MOUNT")
	if pat == "" {
		return
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		logrus.Warnf("SYSBOX_FS_TEST_HANG_MOUNT invalid regexp %q: %v", pat, err)
		return
	}
	logrus.Infof("faultinject mount check target=%q pat=%q match=%v", target, pat, re.MatchString(target))
	if !re.MatchString(target) {
		return
	}
	logrus.Warnf("SYSBOX_FS_TEST_HANG_MOUNT matched %q; hanging nsenter agent forever", target)
	select {}
}

func init() {
	logrus.Infof("sysbox-fs faultinject build active (SYSBOX_FS_TEST_HANG_MOUNT=%q)", os.Getenv("SYSBOX_FS_TEST_HANG_MOUNT"))
}
