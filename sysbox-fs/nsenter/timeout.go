//
// Copyright 2026 The Sysbox fork maintainers
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

package nsenter

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nestybox/sysbox-fs/domain"
	"golang.org/x/sys/unix"
)

// Defaults for the nsenter response-wait budget. Zero disables the bound
// (used by mutation tests). Overridable via SetNsenterTimeouts for tests and
// (later) CLI flags. Continues sysbox-fs#121: that change made PARENT/CHILD
// waits async so fuse_flush could not hold the reaper RLock; the grand-child
// response wait at SendRequest/processResponse remained unbounded.
var (
	nsenterTimeoutSlow  = 30 * time.Second
	nsenterTimeoutFast  = 10 * time.Second
	nsenterSoftWarnSlow = 5 * time.Second
	nsenterSoftWarnFast = 2 * time.Second
)

func init() {
	// SYSBOX_FS_NSENTER_TIMEOUT_DISABLE=1 disables bounds (tests that need the
	// unbounded wait). Prefer this over rebuilding without the timeout code.
	if os.Getenv("SYSBOX_FS_NSENTER_TIMEOUT_DISABLE") == "1" {
		SetNsenterTimeouts(0, 0, 0, 0)
		return
	}
	// Optional millisecond overrides for live timing tests (0 = leave default).
	if v := os.Getenv("SYSBOX_FS_NSENTER_TIMEOUT_SLOW_MS"); v != "" {
		if ms, err := strconv.ParseInt(v, 10, 64); err == nil && ms > 0 {
			nsenterTimeoutSlow = time.Duration(ms) * time.Millisecond
			nsenterSoftWarnSlow = nsenterTimeoutSlow / 2
		}
	}
	if v := os.Getenv("SYSBOX_FS_NSENTER_TIMEOUT_FAST_MS"); v != "" {
		if ms, err := strconv.ParseInt(v, 10, 64); err == nil && ms > 0 {
			nsenterTimeoutFast = time.Duration(ms) * time.Millisecond
			nsenterSoftWarnFast = nsenterTimeoutFast / 2
		}
	}
}

// SetNsenterTimeouts overrides the package defaults. Pass 0 to disable a bound.
func SetNsenterTimeouts(slow, fast, softSlow, softFast time.Duration) {
	nsenterTimeoutSlow = slow
	nsenterTimeoutFast = fast
	nsenterSoftWarnSlow = softSlow
	nsenterSoftWarnFast = softFast
}

func setSockTimeouts(fd int, rcv, snd time.Duration) error {
	if rcv > 0 {
		if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, durationToTimeval(rcv)); err != nil {
			return fmt.Errorf("SO_RCVTIMEO: %w", err)
		}
	}
	if snd > 0 {
		if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, durationToTimeval(snd)); err != nil {
			return fmt.Errorf("SO_SNDTIMEO: %w", err)
		}
	}
	return nil
}

func durationToTimeval(d time.Duration) *unix.Timeval {
	if d < 0 {
		d = 0
	}
	sec := d / time.Second
	usec := (d % time.Second) / time.Microsecond
	return &unix.Timeval{Sec: int64(sec), Usec: int64(usec)}
}

func (e *NSenterEvent) responseBudget() time.Duration {
	if e.ReqMsg == nil {
		return nsenterTimeoutFast
	}
	switch e.ReqMsg.Type {
	case domain.MountSyscallRequest, domain.UmountSyscallRequest:
		return nsenterTimeoutSlow
	default:
		return nsenterTimeoutFast
	}
}

func (e *NSenterEvent) softWarnBudget() time.Duration {
	if e.ReqMsg == nil {
		return nsenterSoftWarnFast
	}
	switch e.ReqMsg.Type {
	case domain.MountSyscallRequest, domain.UmountSyscallRequest:
		return nsenterSoftWarnSlow
	default:
		return nsenterSoftWarnFast
	}
}

func isSockTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
		return true
	}
	if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
		return true
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		return isSockTimeout(pe.Err)
	}
	return false
}

func wrapSockTimeout(err error) error {
	if isSockTimeout(err) {
		return fmt.Errorf("%w: %v", domain.ErrNsenterTimeout, err)
	}
	return err
}

// idempotentRequest is the allow-list for a single retry after a hard timeout.
// Mutating mount/umount/chown/xattr requests are never retried: the killed
// agent may still complete an uninterruptible unix.Mount after we report EIO.
func idempotentRequest(t domain.NSenterMsgType) bool {
	switch t {
	case domain.LookupRequest,
		domain.OpenFileRequest,
		domain.ReadFileRequest,
		domain.ReadDirRequest,
		domain.ReadLinkRequest,
		domain.MountInfoRequest,
		domain.MountInodeRequest,
		domain.SleepRequest,
		domain.UidInfoRequest,
		domain.GidInfoRequest,
		domain.GetxattrSyscallRequest,
		domain.ListxattrSyscallRequest:
		return true
	default:
		return false
	}
}

func captureAgentState(pid int) string {
	if pid <= 0 {
		return "pid=<none>"
	}
	parts := []string{fmt.Sprintf("pid=%d", pid)}

	if b, err := ioutil.ReadFile(fmt.Sprintf("/proc/%d/wchan", pid)); err == nil {
		parts = append(parts, "wchan="+strings.TrimSpace(string(b)))
	}
	if b, err := ioutil.ReadFile(fmt.Sprintf("/proc/%d/status", pid)); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "State:") || strings.HasPrefix(line, "Threads:") {
				parts = append(parts, strings.TrimSpace(line))
			}
		}
	}
	if b, err := ioutil.ReadFile(fmt.Sprintf("/proc/%d/stack", pid)); err == nil {
		stack := strings.TrimSpace(string(b))
		if len(stack) > 200 {
			stack = stack[:200] + "..."
		}
		if stack != "" {
			parts = append(parts, "stack="+strings.ReplaceAll(stack, "\n", "|"))
		}
	}
	return strings.Join(parts, " ")
}
