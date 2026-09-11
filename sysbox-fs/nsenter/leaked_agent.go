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
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

const leakedAgentSweepInterval = 60 * time.Second

var (
	leakedAgentsMu  sync.Mutex
	leakedAgents    = map[int]struct{}{}
	leakedSweepOnce sync.Once
	abortFuseOnLeak bool // --nsenter-timeout-abort-fuse (default false)
)

// SetNsenterTimeoutAbortFuse enables automatic FUSE abort from the leaked-
// agent sweeper. Default remains off.
func SetNsenterTimeoutAbortFuse(enabled bool) {
	abortFuseOnLeak = enabled
}

// registerLeakedAgent records an nsenter agent pid that may remain stuck in
// FUSE after a hard timeout (SIGKILL may not land while in fuse_flush).
func registerLeakedAgent(pid int) {
	if pid <= 0 {
		return
	}
	leakedAgentsMu.Lock()
	leakedAgents[pid] = struct{}{}
	leakedAgentsMu.Unlock()
	leakedSweepOnce.Do(func() {
		go leakedAgentSweeper()
	})
}

func leakedAgentSweeper() {
	t := time.NewTicker(leakedAgentSweepInterval)
	defer t.Stop()
	for range t.C {
		sweepLeakedAgents()
	}
}

func sweepLeakedAgents() {
	leakedAgentsMu.Lock()
	pids := make([]int, 0, len(leakedAgents))
	for pid := range leakedAgents {
		pids = append(pids, pid)
	}
	leakedAgentsMu.Unlock()

	for _, pid := range pids {
		procDir := filepath.Join("/proc", strconv.Itoa(pid))
		if _, err := os.Stat(procDir); err != nil {
			leakedAgentsMu.Lock()
			delete(leakedAgents, pid)
			leakedAgentsMu.Unlock()
			continue
		}
		wchan, _ := os.ReadFile(filepath.Join(procDir, "wchan"))
		if !strings.Contains(string(wchan), "fuse") {
			continue
		}
		minors, err := findSysboxfsFuseMinors(filepath.Join(procDir, "mountinfo"))
		if err != nil {
			logrus.Errorf("sysbox-fs: leaked nsenter agent pid=%d still in fuse (wchan=%q); mountinfo read failed: %v",
				pid, strings.TrimSpace(string(wchan)), err)
			continue
		}
		logrus.Errorf("sysbox-fs: leaked nsenter agent pid=%d still in fuse (wchan=%q); fuse connection minors=%v (abort via /sys/fs/fuse/connections/<minor>/abort)",
			pid, strings.TrimSpace(string(wchan)), minors)

		if abortFuseOnLeak {
			for _, minor := range minors {
				p := filepath.Join("/sys/fs/fuse/connections", minor, "abort")
				if err := os.WriteFile(p, []byte("1\n"), 0600); err != nil {
					logrus.Warnf("sysbox-fs: auto-abort fuse minor %s failed: %v", minor, err)
				} else {
					logrus.Warnf("sysbox-fs: auto-aborted fuse connection minor %s (--nsenter-timeout-abort-fuse)", minor)
				}
			}
		}
	}
}

func findSysboxfsFuseMinors(mountinfoPath string) ([]string, error) {
	f, err := os.Open(mountinfoPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := bytes.Split(sc.Bytes(), []byte{' '})
		if len(fields) < 7 {
			continue
		}
		majmin := fields[2]
		idx := bytes.IndexByte(majmin, ':')
		if idx == -1 {
			continue
		}
		major := string(majmin[:idx])
		minor := string(majmin[idx+1:])
		mountpoint := string(fields[4])
		dash := -1
		for i := 6; i < len(fields); i++ {
			if bytes.Equal(fields[i], []byte{'-'}) {
				dash = i
				break
			}
		}
		if dash < 0 || dash+1 >= len(fields) {
			continue
		}
		fsType := string(fields[dash+1])
		if fsType != "fuse" && !strings.HasPrefix(fsType, "fuse.") {
			continue
		}
		if major != "0" {
			continue
		}
		if strings.Contains(mountpoint, "sysboxfs") || fsType == "fuse.sysboxfs" {
			out = append(out, minor)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("mountinfo scan: %w", err)
	}
	return out, nil
}
