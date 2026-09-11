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

package fuse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Abort severs the kernel FUSE connection for this server's mountpoint.
// Reimplements bazil fuse-abort against s.mountPoint.
func (s *fuseServer) Abort() error {
	if s == nil || s.mountPoint == "" {
		return errors.New("fuse abort: empty mountpoint")
	}
	return AbortMountpoint(s.mountPoint)
}

// AbortMountpoint looks up the FUSE connection minor for mountpoint and
// writes to /sys/fs/fuse/connections/<minor>/abort.
func AbortMountpoint(mountpoint string) error {
	mounts, err := findFUSEMounts("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	id, ok := mounts[mountpoint]
	if !ok {
		// Already gone — treat as success (same as fuse-abort).
		return nil
	}
	if id == "" {
		return fmt.Errorf("fuse abort: %s is not a FUSE mount", mountpoint)
	}
	return abortFuseConnection(id)
}

func abortFuseConnection(id string) error {
	p := filepath.Join("/sys/fs/fuse/connections", id, "abort")
	f, err := os.OpenFile(p, os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString("1\n"); err != nil {
		return err
	}
	return f.Close()
}

// findFUSEMounts returns mountpoint -> connection minor (empty string for
// non-FUSE mounts that appear in the map for conflict detection).
func findFUSEMounts(mountinfoPath string) (map[string]string, error) {
	f, err := os.Open(mountinfoPath)
	if err != nil {
		return nil, fmt.Errorf("cannot open mountinfo: %w", err)
	}
	defer f.Close()

	r := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		info, err := parseMountinfoLine(sc.Bytes())
		if err != nil {
			return nil, err
		}
		if info.FSType != "fuse" && !strings.HasPrefix(info.FSType, "fuse.") {
			r[info.Mountpoint] = ""
			continue
		}
		if info.Major != "0" {
			return nil, fmt.Errorf("FUSE mount has weird device major number: %v:%v: %v",
				info.Major, info.Minor, info.Mountpoint)
		}
		if _, ok := r[info.Mountpoint]; ok {
			return nil, fmt.Errorf("mountpoint seen twice in mountinfo: %v", info.Mountpoint)
		}
		r[info.Mountpoint] = info.Minor
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return r, nil
}

// FindSysboxfsFuseMinorsInMountinfo returns FUSE connection minors for
// sysboxfs mounts found in the given mountinfo file (used by the leaked-
// agent reaper against /proc/<pid>/mountinfo).
func FindSysboxfsFuseMinorsInMountinfo(mountinfoPath string) ([]string, error) {
	f, err := os.Open(mountinfoPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		info, err := parseMountinfoLine(sc.Bytes())
		if err != nil {
			return nil, err
		}
		if info.FSType != "fuse" && !strings.HasPrefix(info.FSType, "fuse.") {
			continue
		}
		if info.Major != "0" {
			continue
		}
		// Prefer sysboxfs mounts; include any fuse.* under /var/lib/sysboxfs.
		if strings.Contains(info.Mountpoint, "sysboxfs") || info.FSType == "fuse.sysboxfs" {
			out = append(out, info.Minor)
		}
	}
	return out, sc.Err()
}

type mountInfo struct {
	Major, Minor, Mountpoint, FSType string
}

func parseMountinfoLine(line []byte) (*mountInfo, error) {
	fields := bytes.Split(line, []byte{' '})
	if len(fields) < 7 {
		return nil, fmt.Errorf("cannot parse mountinfo entry: %q", line)
	}
	majmin := fields[2]
	idx := bytes.IndexByte(majmin, ':')
	if idx == -1 {
		return nil, fmt.Errorf("cannot parse mountinfo maj:min: %q", line)
	}
	mountpoint, err := unescapeMountField(fields[4])
	if err != nil {
		return nil, err
	}
	dash := -1
	for i := 6; i < len(fields); i++ {
		if bytes.Equal(fields[i], []byte{'-'}) {
			dash = i
			break
		}
	}
	if dash < 0 || dash+1 >= len(fields) {
		return nil, fmt.Errorf("cannot find fstype in mountinfo: %q", line)
	}
	return &mountInfo{
		Major:      string(majmin[:idx]),
		Minor:      string(majmin[idx+1:]),
		Mountpoint: mountpoint,
		FSType:     string(fields[dash+1]),
	}, nil
}

func unescapeMountField(in []byte) (string, error) {
	buf := make([]byte, 0, len(in))
	for len(in) > 0 {
		if in[0] == '\\' {
			if len(in) < 4 {
				return "", fmt.Errorf("truncated octal sequence: %q", in[1:])
			}
			num, err := strconv.ParseUint(string(in[1:4]), 8, 8)
			if err != nil {
				return "", err
			}
			buf = append(buf, byte(num))
			in = in[4:]
			continue
		}
		buf = append(buf, in[0])
		in = in[1:]
	}
	return string(buf), nil
}
