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
	"os"
	"testing"

	"github.com/nestybox/sysbox-fs/domain"
	"github.com/nestybox/sysbox-runc/libcontainer/utils"
	"golang.org/x/sys/unix"
)

// TestProcessResponseWaitUnboundedMutation is the negative proof for
// TestProcessResponseWaitIsBounded. Without SO_RCVTIMEO, processResponse
// hangs forever. Run explicitly:
//
//	SYSBOX_FS_NSENTER_HANG_MUTATION=1 go test -timeout 2s -run TestProcessResponseWaitUnboundedMutation
//
// Expected: FAIL with "panic: test timed out after 2s".
func TestProcessResponseWaitUnboundedMutation(t *testing.T) {
	if os.Getenv("SYSBOX_FS_NSENTER_HANG_MUTATION") != "1" {
		t.Skip("set SYSBOX_FS_NSENTER_HANG_MUTATION=1 to run the hang mutation (expects go test -timeout)")
	}
	parent, child, err := utils.NewSockPair("nsenter-mutation")
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	defer child.Close()
	fd := int(parent.Fd())
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_PASSCRED, 1)
	// Deliberately do NOT call setSockTimeouts — this is the pre-fix path.
	e := &NSenterEvent{ReqMsg: &domain.NSenterMessage{Type: domain.MountSyscallRequest}}
	_ = e.processResponse(parent)
	t.Fatal("unreachable: processResponse returned without a bound")
}
