package seccomp

import (
	"errors"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSeccompFdPollShouldBreak(t *testing.T) {
	cases := []struct {
		name    string
		revents int16
		want    bool
	}{
		{"POLLIN only", unix.POLLIN, false},
		{"POLLIN|POLLHUP", unix.POLLIN | unix.POLLHUP, false},
		{"POLLIN|POLLERR", unix.POLLIN | unix.POLLERR, false},
		{"POLLHUP only", unix.POLLHUP, true},
		{"POLLERR only", unix.POLLERR, false}, // retryable, not filter-dead
		{"POLLNVAL only", unix.POLLNVAL, false},
		{"zero", 0, true},
	}
	for _, tc := range cases {
		if got := seccompFdPollShouldBreak(tc.revents); got != tc.want {
			t.Fatalf("%s: got %v want %v (revents=0x%x)", tc.name, got, tc.want, tc.revents)
		}
	}
}

func TestPidStateFromStat(t *testing.T) {
	cases := []struct {
		stat string
		want byte
	}{
		{"1234 (bash) R 1 1234 1234 0 -1", 'R'},
		{"99 (docker exec) Z 1 99 99 0 -1", 'Z'},
		{"1 (systemd) S 0 1 1 0 -1", 'S'},
		{"bad", 0},
	}
	for _, tc := range cases {
		if got := pidStateFromStat([]byte(tc.stat)); got != tc.want {
			t.Fatalf("stat=%q: got %q want %q", tc.stat, got, tc.want)
		}
	}
}

func TestIsNotifReceiveWouldBlock(t *testing.T) {
	if !isNotifReceiveWouldBlock(syscall.EAGAIN) {
		t.Fatal("EAGAIN")
	}
	if !isNotifReceiveWouldBlock(unix.EWOULDBLOCK) {
		t.Fatal("EWOULDBLOCK")
	}
	if isNotifReceiveWouldBlock(errors.New("other")) {
		t.Fatal("other")
	}
}
