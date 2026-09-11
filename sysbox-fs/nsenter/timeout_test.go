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
	"os"
	"testing"
	"time"

	"github.com/nestybox/sysbox-fs/domain"
	"github.com/nestybox/sysbox-runc/libcontainer/utils"
	"golang.org/x/sys/unix"
)

// TestProcessResponseWaitIsBounded is the proof that the nsenter wait is bounded:
// with a 200 ms SO_RCVTIMEO, processResponse on a silent peer must return
// ErrNsenterTimeout quickly. Re-run with timeouts disabled (0) and the test
// harness -timeout must catch the hang (see timeout_mutation_test.go).
func TestProcessResponseWaitIsBounded(t *testing.T) {
	parent, child, err := utils.NewSockPair("nsenter-timeout-test")
	if err != nil {
		t.Fatalf("NewSockPair: %v", err)
	}
	defer parent.Close()
	defer child.Close()

	// Reproduce SendRequest: Fd() then SO_PASSCRED then SO_RCVTIMEO.
	fd := int(parent.Fd())
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_PASSCRED, 1); err != nil {
		t.Fatalf("SO_PASSCRED: %v", err)
	}
	budget := 200 * time.Millisecond
	if err := setSockTimeouts(fd, budget, budget); err != nil {
		t.Fatalf("setSockTimeouts: %v", err)
	}

	e := &NSenterEvent{
		ReqMsg: &domain.NSenterMessage{Type: domain.MountSyscallRequest},
	}

	start := time.Now()
	err = e.processResponse(parent)
	elapsed := time.Since(start)

	if !errors.Is(err, domain.ErrNsenterTimeout) {
		t.Fatalf("expected ErrNsenterTimeout, got %v", err)
	}
	if elapsed < budget {
		t.Fatalf("returned too fast (%v < %v); timeout may not have engaged", elapsed, budget)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("returned too slow (%v); bound ineffective", elapsed)
	}
}

// TestSetReadDeadlineIsInertAfterFd documents §A.1: after Fd(), Go deadlines
// do not apply to the raw Recvmsg path — SO_RCVTIMEO is required.
func TestSetReadDeadlineIsInertAfterFd(t *testing.T) {
	parent, child, err := utils.NewSockPair("nsenter-deadline-doc")
	if err != nil {
		t.Fatalf("NewSockPair: %v", err)
	}
	defer parent.Close()
	defer child.Close()

	_ = parent.Fd()
	err = parent.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if err == nil {
		t.Fatal("expected SetReadDeadline to fail after Fd(); got nil")
	}
	t.Logf("SetReadDeadline after Fd: %v", err)
}

func TestIdempotentRequestAllowList(t *testing.T) {
	if !idempotentRequest(domain.LookupRequest) {
		t.Fatal("LookupRequest should be idempotent")
	}
	if idempotentRequest(domain.MountSyscallRequest) {
		t.Fatal("MountSyscallRequest must NOT be retried")
	}
	if idempotentRequest(domain.UmountSyscallRequest) {
		t.Fatal("UmountSyscallRequest must NOT be retried")
	}
}

func TestCaptureAgentStateSelf(t *testing.T) {
	s := captureAgentState(os.Getpid())
	if s == "pid=<none>" {
		t.Fatal("unexpected empty capture")
	}
	t.Log(s)
}
