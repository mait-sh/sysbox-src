//go:build !sysbox_fs_faultinject

package nsenter

// maybeHangMountForTest is a no-op in production builds.
func maybeHangMountForTest(target string) {}
