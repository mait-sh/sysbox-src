//
// Copyright 2019-2022 Nestybox, Inc.
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

package seccomp

import (
	"C"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nestybox/sysbox-fs/domain"
	unixIpc "github.com/nestybox/sysbox-ipc/unix"
	"github.com/nestybox/sysbox-libs/formatter"
	linuxUtils "github.com/nestybox/sysbox-libs/linuxUtils"
	libpidfd "github.com/nestybox/sysbox-libs/pidfd"
	libseccomp "github.com/seccomp/libseccomp-golang"
	"golang.org/x/sys/unix"

	"github.com/sirupsen/logrus"
)
import cap "github.com/nestybox/sysbox-libs/capability"

const seccompTracerSockAddr = "/run/sysbox/sysfs-seccomp.sock"

// libseccomp req/resp aliases.
type sysRequest = libseccomp.ScmpNotifReq
type sysResponse = libseccomp.ScmpNotifResp

// Slice of supported syscalls to monitor.
var monitoredSyscalls = []string{
	"openat2",
	"mount",
	"umount2",
	"reboot",
	"swapon",
	"swapoff",
	"chown",
	"fchown",
	"fchownat",
	"setxattr",
	"lsetxattr",
	"fsetxattr",
	"getxattr",
	"lgetxattr",
	"fgetxattr",
	"removexattr",
	"lremovexattr",
	"fremovexattr",
	"listxattr",
	"llistxattr",
	"flistxattr",
}

// Seccomp's syscall-monitoring/trapping service struct. External packages
// will solely rely on this struct for their syscall-monitoring demands.
type SyscallMonitorService struct {
	nss                    domain.NSenterServiceIface        // for nsenter functionality requirements
	css                    domain.ContainerStateServiceIface // for container-state interactions
	prs                    domain.ProcessServiceIface        // for process class interactions
	mts                    domain.MountServiceIface          // for mount-services purposes
	allowImmutableRemounts bool                              // allow immutable mounts to be remounted
	allowImmutableUnmounts bool                              // allow immutable mounts to be unmounted
	closeSeccompOnContExit bool                              // close seccomp fds on container exit, not on process exit
	nsenterBreakerEnabled  bool                              // nsenter timeout circuit breaker
	tracer                 *syscallTracer                    // pointer to actual syscall-tracer instance
}

func NewSyscallMonitorService() *SyscallMonitorService {
	return &SyscallMonitorService{}
}

func (scs *SyscallMonitorService) Setup(
	nss domain.NSenterServiceIface,
	css domain.ContainerStateServiceIface,
	prs domain.ProcessServiceIface,
	mts domain.MountServiceIface,
	allowImmutableRemounts bool,
	allowImmutableUnmounts bool,
	seccompFdReleasePolicy string,
	nsenterBreaker string) {

	scs.nss = nss
	scs.css = css
	scs.prs = prs
	scs.mts = mts
	scs.allowImmutableRemounts = allowImmutableRemounts
	scs.allowImmutableUnmounts = allowImmutableUnmounts
	// Default on; only "off" disables the breaker.
	scs.nsenterBreakerEnabled = nsenterBreaker != "off"

	if seccompFdReleasePolicy == "cont-exit" {
		scs.closeSeccompOnContExit = true
	}

	// Allocate a new syscall-tracer.
	scs.tracer = newSyscallTracer(scs)

	// Initialize and launch the syscall-tracer.
	if err := scs.tracer.start(); err != nil {
		logrus.Fatalf("syscallMonitorService initialization error (%v). Exiting ...",
			err)
	}
}

type seccompArchSyscallPair struct {
	archId    libseccomp.ScmpArch
	syscallId libseccomp.ScmpSyscall
}

// SeccompSession holds state associated to every seccomp tracee session.
type seccompSession struct {
	pid    uint32 // pid of the tracee process
	fd     int32  // tracee's seccomp-fd to allow kernel interaction
	pidfd  int32  // fd associated to tracee's pid to influence poll() cycle
	cntrId string // container(id) on which each seccomp session lives
}

// seccompFdPollShouldBreak reports whether a seccomp-fd poll result should
// end the session loop. POLLIN must be processed even when combined with
// POLLHUP/ERR; closing the listener while notifications
// are pending makes the kernel answer them with ENOSYS.
//
// Pure POLLHUP is the normal "filter users == 0" teardown on kernels with
// unused-notif support; callers must still decide whether the pid/filter is
// genuinely gone before closing (see sessionStillLive).
func seccompFdPollShouldBreak(revents int16) bool {
	if revents&unix.POLLIN != 0 {
		return false
	}
	// Keep retrying on ERR/NVAL alone — they do not mean the filter is dead.
	if revents&(unix.POLLHUP) != 0 {
		return true
	}
	return revents == 0
}

// sessionStillLive is a cheap liveness check before closing a notify fd on
// POLLHUP. A live pid with POLLHUP is the premature-close hazard that turns
// subsequent traps into ENOSYS for that filter.
func sessionStillLive(pid uint32) bool {
	if pid == 0 {
		return false
	}
	err := unix.Kill(int(pid), 0)
	return err == nil
}

// sessionIsZombie reports whether /proc/<pid>/stat shows state Z. Kill(pid,0)
// still succeeds for unreaped zombies, so POLLHUP deferral would otherwise
// burn ~1s per short-lived exec session teardown.
func sessionIsZombie(pid uint32) bool {
	if pid == 0 {
		return false
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	return pidStateFromStat(b) == 'Z'
}

// pidStateFromStat returns the process state byte from a /proc/<pid>/stat
// payload (field 3 after "pid (comm)").
func pidStateFromStat(stat []byte) byte {
	s := string(stat)
	i := strings.LastIndex(s, ")")
	if i < 0 || i+2 >= len(s) {
		return 0
	}
	return s[i+2]
}

func isNotifReceiveWouldBlock(err error) bool {
	return errors.Is(err, syscall.EAGAIN) ||
		errors.Is(err, syscall.EWOULDBLOCK) ||
		errors.Is(err, unix.EAGAIN) ||
		errors.Is(err, unix.EWOULDBLOCK)
}

// Seccomp's syscall-monitor/tracer.
type syscallTracer struct {
	srv                *unixIpc.Server                   // unix server listening to seccomp-notifs
	pollsrv            *unixIpc.PollServer               // unix pollserver for non-blocking i/o on seccomp-fd
	syscalls           map[seccompArchSyscallPair]string // hashmap of supported syscalls, indexed by seccomp architecture and syscall id
	memParser          memParser                         // memParser to utilize for tracee interactions
	seccompSessionCMap map[string][]seccompSession       // tracks all seccomp sessions associated with a given container
	pidToContMap       map[uint32]string                 // maps pid -> container id
	seccompSessionMu   sync.RWMutex                      // seccomp session table lock
	seccompUnusedNotif bool                              // seccomp-fd unused notification feature supported by kernel
	seccompNotifPidTrk *seccompNotifPidTracker           // Ensures seccomp notifs for the same pid are processed sequentially (not in parallel).
	nsenterBreaker     *nsenterBreaker                   // per-container hard-timeout circuit breaker
	service            *SyscallMonitorService            // backpointer to syscall-monitor service
}

func getSupportedCompatibleSyscalls(nativeArchId libseccomp.ScmpArch) map[libseccomp.ScmpArch][]string {
	switch nativeArchId {
	case libseccomp.ArchAMD64:
		return map[libseccomp.ScmpArch][]string{
			libseccomp.ArchAMD64: monitoredSyscalls,
			// TODO: Add x86 specific syscalls such as chown32
			libseccomp.ArchX86: monitoredSyscalls,
		}
	default:
		return map[libseccomp.ScmpArch][]string{
			nativeArchId: monitoredSyscalls,
		}
	}
}

// syscallTracer constructor.
func newSyscallTracer(sms *SyscallMonitorService) *syscallTracer {

	tracer := &syscallTracer{
		service:        sms,
		syscalls:       make(map[seccompArchSyscallPair]string),
		nsenterBreaker: newNsenterBreaker(sms.nsenterBreakerEnabled),
	}

	if sms.closeSeccompOnContExit {
		tracer.seccompSessionCMap = make(map[string][]seccompSession)
		tracer.pidToContMap = make(map[uint32]string)
	}

	// Populate hashmap of supported syscalls to monitor.
	nativeArchId, err := libseccomp.GetNativeArch()
	if err != nil {
		logrus.Warnf("Seccomp-tracer initialization error: Error obtaining native architecture")
		return nil
	}

	for archId, syscalls := range getSupportedCompatibleSyscalls(nativeArchId) {
		for _, syscall := range syscalls {
			syscallId, err := libseccomp.GetSyscallFromNameByArch(syscall, archId)
			if err != nil {
				logrus.Warnf("Seccomp-tracer initialization error: unknown syscall (%v, %v).",
					archId, syscall)
				return nil
			}
			tracer.syscalls[seccompArchSyscallPair{archId, syscallId}] = syscall
		}
	}

	// Elect the memParser to utilize based on the availability of process_vm_readv()
	// syscall.
	_, err = unix.ProcessVMReadv(int(1), nil, nil, 0)
	if err == syscall.ENOSYS {
		tracer.memParser = &memParserProcfs{}
		logrus.Info("Procfs memParser elected")
	} else {
		tracer.memParser = &memParserIOvec{}
		logrus.Info("IOvec memParser elected")
	}

	// Seccomp-fd's unused notification feature is provided by kernel starting with v5.8.
	cmp, err := linuxUtils.KernelCurrentVersionCmp(5, 8)
	if err != nil {
		logrus.Warnf("Seccomp-tracer initialization error: unable to parse kernel string (%v).",
			err)
		return nil
	}
	if cmp >= 0 {
		tracer.seccompUnusedNotif = true
	}

	tracer.seccompNotifPidTrk = newSeccompNotifPidTracker()

	return tracer
}

// Start syscall tracer.
func (t *syscallTracer) start() error {

	// Enforce proper support of seccomp-monitoring capabilities by the existing
	// kernel; bail otherwise.
	api, err := libseccomp.GetAPI()
	if err != nil {
		logrus.Errorf("Error while obtaining seccomp API level (%v).", err)
		return err
	} else if api < 5 {
		logrus.Errorf("Error: need seccomp API level >= 5; it's currently %d", api)
		return fmt.Errorf("Error: unsupported kernel")
	}

	// Launch a new server to listen to seccomp-tracer's socket. Incoming messages
	// will be handled through a separated / dedicated goroutine.
	srv, err := unixIpc.NewServer(seccompTracerSockAddr, t.connHandler)
	if err != nil {
		logrus.Errorf("Unable to initialize seccomp-tracer server")
		return err
	}
	t.srv = srv

	return nil
}

func (t *syscallTracer) seccompSessionAdd(s seccompSession) {

	t.seccompSessionMu.Lock()

	if t.service.closeSeccompOnContExit {

		// Return if seccomp session's pid is already registered.
		if _, ok := t.pidToContMap[s.pid]; ok {
			t.seccompSessionMu.Unlock()
			return
		}

		// Collect seccomp fds associated with container so we can
		// release them together when the container dies.
		t.pidToContMap[s.pid] = s.cntrId
		sessions, ok := t.seccompSessionCMap[s.cntrId]
		if ok {
			sessions = append(sessions, s)
			t.seccompSessionCMap[s.cntrId] = sessions
		} else {
			t.seccompSessionCMap[s.cntrId] = []seccompSession{s}
		}
	}

	t.seccompSessionMu.Unlock()

	logrus.Debugf("Created seccomp-tracee session for fd %d, pid %d, cntr-id %s",
		s.fd, s.pid, s.cntrId)
}

func (t *syscallTracer) seccompSessionDelete(s seccompSession) {
	var closeFds []int32

	t.seccompSessionMu.Lock()

	if t.service.closeSeccompOnContExit {
		var cntrInitPid uint32

		cntr := t.service.css.ContainerLookupById(s.cntrId)
		if cntr != nil {
			cntrInitPid = cntr.InitPid()
		}

		// If the container is no longer there, or the pid being deleted is the
		// container's init pid, we close all seccomp sessions for that container.
		if cntr == nil || s.pid == cntrInitPid {
			sessions := t.seccompSessionCMap[s.cntrId]
			for _, s := range sessions {
				closeFds = append(closeFds, s.fd)
			}
			delete(t.seccompSessionCMap, s.cntrId)
		}

		delete(t.pidToContMap, s.pid)

	} else {
		closeFds = []int32{s.fd}

		// pidfd = 0 implies we are not using pidfd to track the release of the seccomp-fd.
		if s.pidfd != 0 {
			closeFds = append(closeFds, s.pidfd)
		}
	}

	t.seccompSessionMu.Unlock()

	if len(closeFds) > 0 {
		for _, fd := range closeFds {
			// We are finally ready to close the seccomp-fd.
			if err := unix.Close(int(fd)); err != nil {
				logrus.Errorf("Failed to close seccomp fd %v for pid %d: %v",
					s.fd, s.pid, err)
			}
		}

		logrus.Debugf("Removed session for seccomp-tracee for pid %d, fd(s) %v",
			s.pid, closeFds)
	}
}

func (t *syscallTracer) seccompSessionPidfd(
	pid int32,
	cntrID string,
	fd int32) libpidfd.PidFd {

	var (
		pidfd libpidfd.PidFd
		err   error
	)

	// In scenarios lacking seccomp's unused-filter notifications, we rely on pidfd
	// constructs to help us identify the precise time at which we must stop polling
	// over seccomp-fds. Within these scenarios we handle the two following cases
	// separately attending to the value of the '--seccomp-fd-release' cli knob:
	//
	// 1) 'Cntr-Exit': In this scenario, all the seccomp sessions make use of the
	// same pidfd: the one associated with the container's initPid. By doing this
	// we ensure that all seccomp sessions are kept alive until the container's
	// initPid dies.
	//
	// 2) 'Proc-Exit' (default): In this case we want to associate the live-span of
	// the seccomp-fd polling session with the one of the user-process that exec()
	// into the container's namespaces (e.g., docker exec <cntr>). For this purpose
	// we obtain a pidfd associated to the user-process pid.
	if !t.seccompUnusedNotif {
		if t.service.closeSeccompOnContExit {
			cntr := t.service.css.ContainerLookupById(cntrID)
			if cntr == nil {
				logrus.Errorf("Unexpected error during cntr.Lookup(%s) execution on fd %d, pid %d",
					cntrID, fd, pid)
				return 0
			}
			pidfd = cntr.InitPidFd()

		} else {
			pidfd, err = libpidfd.Open(int(pid), 0)
			if err != nil {
				logrus.Errorf("Unexpected error during pidfd.Open() execution (%v) on fd %d, pid %d",
					err, fd, pid)
				return 0
			}
		}
	}

	return pidfd
}

// Tracer's connection-handler method. Executed within a dedicated goroutine (one
// per connection).
func (t *syscallTracer) connHandler(c *net.UnixConn) {

	// Obtain seccomp-notification's file-descriptor and associated context.
	pid, cntrID, fd, err := unixIpc.RecvSeccompInitMsg(c)
	if err != nil {
		logrus.Errorf("seccomp session: RecvSeccompInitMsg failed: %v (listener not registered — filter will ENOSYS)", err)
		return
	}

	// Send Ack message back to sysbox-runc.
	if err = unixIpc.SendSeccompInitAckMsg(c); err != nil {
		logrus.Errorf("seccomp session: SendSeccompInitAckMsg failed for pid %d cntr %s: %v",
			pid, formatter.ContainerID{cntrID}, err)
		return
	}

	// Non-blocking receive so a stale POLLIN (cancelled notif after tracee
	// exit) cannot wedge this session goroutine in SECCOMP_IOCTL_NOTIF_RECV.
	if err := unix.SetNonblock(int(fd), true); err != nil {
		logrus.Warnf("seccomp session: SetNonblock failed fd=%d pid=%d cntr=%s: %v — continuing blocking",
			fd, pid, formatter.ContainerID{cntrID}, err)
	}

	// If needed, obtain pidfd associated to this seccomp-bfd session.
	pidfd := t.seccompSessionPidfd(pid, cntrID, fd)

	// Register the new seccomp-fd session.
	session := seccompSession{uint32(pid), fd, int32(pidfd), cntrID}
	t.seccompSessionAdd(session)
	started := time.Now()

	var (
		inflight sync.WaitGroup
		notifs   uint64
		hupRetry int
		errRetry int
	)

	for {
		var fds []unix.PollFd

		if t.seccompUnusedNotif {
			fds = []unix.PollFd{
				{int32(fd), unix.POLLIN, 0},
			}
		} else {
			fds = []unix.PollFd{
				{int32(fd), unix.POLLIN, 0},
				{int32(pidfd), unix.POLLIN, 0},
			}
		}

		// Poll the obtained seccomp-fd for incoming syscalls.
		_, err := unix.Poll(fds, -1)
		if err != nil {
			// As per signal(7), poll() syscall isn't restartable by kernel, so we must
			// manually handle its potential interruption.
			if err == syscall.EINTR {
				logrus.Debugf("EINTR error during Poll() execution (%v) on fd %d, pid %d, cntr %s",
					err, fd, pid, formatter.ContainerID{cntrID})
				continue
			}
			if err == syscall.EBADF {
				logrus.Warnf("seccomp session end: Poll EBADF on fd %d, pid %d, cntr %s notifs=%d age=%v",
					fd, pid, formatter.ContainerID{cntrID}, notifs, time.Since(started))
				break
			}
			logrus.Warnf("seccomp session: Poll error (%v) on fd %d, pid %d, cntr %s — retrying",
				err, fd, pid, formatter.ContainerID{cntrID})
			time.Sleep(50 * time.Millisecond)
			continue
		}

		// As per pidfd_open(2), a pidfd becomes readable when its associated pid
		// terminates. Prefer draining pending seccomp POLLIN first so we do not
		// close the listener while notifications are still queued (ENOSYS).
		if !t.seccompUnusedNotif && fds[1].Revents&unix.POLLIN != 0 {
			if fds[0].Revents&unix.POLLIN == 0 {
				logrus.Debugf("seccomp session end: pidfd ready fd=%d pidfd=%d pid=%d cntr=%s seccompRevents=0x%x notifs=%d",
					fd, pidfd, pid, formatter.ContainerID{cntrID}, fds[0].Revents, notifs)
				break
			}
			logrus.Debugf("seccomp session: pidfd ready with pending POLLIN; draining fd=%d pid=%d cntr=%s revents=0x%x",
				fd, pid, formatter.ContainerID{cntrID}, fds[0].Revents)
		}

		// Keep processing while POLLIN is set, even if POLLHUP/ERR are also set.
		if seccompFdPollShouldBreak(fds[0].Revents) {
			live := sessionStillLive(uint32(pid))
			zombie := sessionIsZombie(uint32(pid))
			isInit := false
			if cntr := t.service.css.ContainerLookupById(cntrID); cntr != nil {
				isInit = uint32(pid) == cntr.InitPid()
			}
			// Unreaped zombies still pass Kill(0); do not defer close for them.
			if live && !zombie && hupRetry < 5 {
				hupRetry++
				// Non-init (docker exec) sessions often see a brief POLLHUP
				// while the pid is still live; deferring is expected. Init
				// premature HUP is the ENOSYS hazard — keep it loud.
				if isInit {
					logrus.Errorf("seccomp session: premature POLLHUP while init pid live fd=%d pid=%d cntr=%s revents=0x%x notifs=%d age=%v retry=%d — deferring close",
						fd, pid, formatter.ContainerID{cntrID}, fds[0].Revents, notifs, time.Since(started), hupRetry)
				} else {
					logrus.Debugf("seccomp session: premature POLLHUP while pid live fd=%d pid=%d cntr=%s revents=0x%x notifs=%d age=%v retry=%d — deferring close",
						fd, pid, formatter.ContainerID{cntrID}, fds[0].Revents, notifs, time.Since(started), hupRetry)
				}
				time.Sleep(200 * time.Millisecond)
				continue
			}
			if isInit || (live && notifs > 0) {
				logrus.Errorf("seccomp session end: fd=%d pid=%d cntr=%s revents=0x%x notifs=%d age=%v live=%v isInit=%v",
					fd, pid, formatter.ContainerID{cntrID}, fds[0].Revents, notifs, time.Since(started), live, isInit)
			} else {
				logrus.Debugf("seccomp session end: fd=%d pid=%d cntr=%s revents=0x%x notifs=%d age=%v",
					fd, pid, formatter.ContainerID{cntrID}, fds[0].Revents, notifs, time.Since(started))
			}
			break
		}
		hupRetry = 0

		// POLLERR/NVAL without POLLIN: retry with backoff, then exit. Leaving
		// this unbounded hot-spins NotifReceive→EBADF on a dead fd.
		if fds[0].Revents&unix.POLLIN == 0 &&
			fds[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
			errRetry++
			if errRetry > 5 {
				logrus.Warnf("seccomp session end: POLLERR/NVAL retries exhausted fd=%d pid=%d cntr=%s revents=0x%x notifs=%d",
					fd, pid, formatter.ContainerID{cntrID}, fds[0].Revents, notifs)
				break
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		errRetry = 0

		// Retrieves seccomp-notification message. Notice that we will not 'break'
		// upon error detection as libseccomp/kernel could return non-fatal errors
		// (i.e., ENOENT) to alert of a problem with a specific notification.
		req, err := libseccomp.NotifReceive(libseccomp.ScmpFd(fd))
		if err != nil {
			if isNotifReceiveWouldBlock(err) {
				logrus.Debugf("seccomp session: NotifReceive EAGAIN fd=%d pid=%d cntr=%s (stale POLLIN)",
					fd, pid, formatter.ContainerID{cntrID})
				continue
			}
			logrus.Infof("Unexpected error during NotifReceive() execution (%v) on fd %d, pid %d, cntr %s",
				err, fd, pid, formatter.ContainerID{cntrID})
			continue
		}

		// Process the incoming syscall and obtain response for seccomp-tracee.
		notifs++
		inflight.Add(1)
		go func(req *sysRequest) {
			defer inflight.Done()
			t.process(req, fd, cntrID)
		}(req)
	}

	// Brief drain so in-flight NotifRespond can finish before fd close.
	// Keep this short: a long drain under burst holds conn/goroutine/fd and
	// amplifies load without preventing ENOSYS once the filter is released.
	drainTimer := time.NewTimer(2 * time.Second)
	drainDone := make(chan struct{})
	go func() {
		inflight.Wait()
		close(drainDone)
	}()
	select {
	case <-drainDone:
		drainTimer.Stop()
	case <-drainTimer.C:
		logrus.Warnf("seccomp session drain timed out on fd %d, pid %d, cntr %s notifs=%d",
			fd, pid, formatter.ContainerID{cntrID}, notifs)
	}

	t.seccompSessionDelete(session)

	c.Close()
}

func (t *syscallTracer) process(
	req *sysRequest,
	fd int32,
	cntrID string) {

	// This ensures that for a given pid, we only process one syscall at a time.
	// Syscalls for different pids are processed in parallel.
	t.seccompNotifPidTrk.Lock(req.Pid)
	defer t.seccompNotifPidTrk.Unlock(req.Pid)

	// An open breaker short-circuits mediation with EIO.
	if t.nsenterBreaker != nil && !t.nsenterBreaker.allow(cntrID) {
		_ = libseccomp.NotifRespond(libseccomp.ScmpFd(fd), t.createErrorResponse(req.ID, syscall.EIO))
		return
	}

	// Process the incoming syscall and obtain response for seccomp-tracee.
	resp, err := t.processSyscall(req, fd, cntrID)
	// Always answer when we have a response. Skipping NotifRespond leaves the
	// notification outstanding until the listener fd closes → kernel ENOSYS.
	if resp != nil {
		if rerr := libseccomp.NotifRespond(libseccomp.ScmpFd(fd), resp); rerr != nil {
			logrus.Debugf("NotifRespond error on fd %d pid %d cntr %s: %v",
				fd, req.Pid, formatter.ContainerID{cntrID}, rerr)
		}
	}
	if err != nil {
		return
	}
}

// Syscall processing entrypoint. Returns the response to be delivered to the
// process (seccomp-tracee) generating the syscall.
func (t *syscallTracer) processSyscall(
	req *sysRequest,
	fd int32,
	cntrID string) (*sysResponse, error) {

	var (
		resp *sysResponse
		err  error
	)

	// Obtain container associated to the received containerId value.
	cntr := t.service.css.ContainerLookupById(cntrID)
	if cntr == nil {
		logrus.Warnf("Received seccompNotifMsg generated by unknown container: %s",
			formatter.ContainerID{cntrID})
		return t.createErrorResponse(req.ID, syscall.Errno(syscall.EPERM)), nil
	}

	archId := req.Data.Arch
	syscallId := req.Data.Syscall
	syscallName := t.syscalls[seccompArchSyscallPair{archId, syscallId}]

	switch syscallName {
	case "openat2":
		resp, err = t.processOpenat2(req, fd, cntr)

	case "mount":
		resp, err = t.processMount(req, fd, cntr)

	case "umount2":
		resp, err = t.processUmount(req, fd, cntr)

	case "reboot":
		resp, err = t.processReboot(req, fd, cntr)

	case "swapon":
		resp, err = t.processSwapon(req, fd, cntr)

	case "swapoff":
		resp, err = t.processSwapoff(req, fd, cntr)

	case "chown":
		resp, err = t.processChown(req, fd, cntr)

	case "fchown":
		resp, err = t.processFchown(req, fd, cntr)

	case "fchownat":
		resp, err = t.processFchownat(req, fd, cntr)

	case "setxattr", "lsetxattr":
		resp, err = t.processSetxattr(req, fd, cntr, syscallName)

	case "fsetxattr":
		resp, err = t.processFsetxattr(req, fd, cntr)

	case "getxattr", "lgetxattr":
		resp, err = t.processGetxattr(req, fd, cntr, syscallName)

	case "fgetxattr":
		resp, err = t.processFgetxattr(req, fd, cntr)

	case "removexattr", "lremovexattr":
		resp, err = t.processRemovexattr(req, fd, cntr, syscallName)

	case "fremovexattr":
		resp, err = t.processFremovexattr(req, fd, cntr)

	case "listxattr", "llistxattr":
		resp, err = t.processListxattr(req, fd, cntr, syscallName)

	case "flistxattr":
		resp, err = t.processFlistxattr(req, fd, cntr)

	default:
		logrus.Warnf("Unsupported syscall notification received (%v) on fd %d, pid %d, cntr %s",
			syscallId, fd, req.Pid, formatter.ContainerID{cntrID})
		return t.createErrorResponse(req.ID, syscall.EINVAL), nil
	}

	// If an 'infrastructure' error is encountered during syscall processing,
	// then return a common error back to tracee process. By 'infrastructure'
	// errors we are referring to problems beyond the end-user realm: EPERM
	// error during Open() doesn't qualify, whereas 'nsenter' operational
	// errors or inexistent "/proc/pid/mem" does.
	//
	// ErrNsenterTimeout is special-cased to EIO so it stays distinguishable
	// from status-quo EINVAL (handler failure) and mediation-gone ENOSYS
	// (seccomp listener gone). See nestybox/sysbox#1018.
	if err != nil {
		if errors.Is(err, domain.ErrNsenterTimeout) {
			logrus.Errorf("nsenter timeout during syscall %v processing on fd %d, pid %d, req Id %d, cntr %s (%v)",
				syscallName, fd, req.Pid, req.ID, formatter.ContainerID{cntrID}, err)
			if t.nsenterBreaker != nil {
				t.nsenterBreaker.recordTimeout(cntrID)
			}
			return t.createErrorResponse(req.ID, syscall.EIO), nil
		}
		logrus.Warnf("Error during syscall %v processing on fd %d, pid %d, req Id %d, cntr %s (%v)",
			syscallName, fd, req.Pid, req.ID, formatter.ContainerID{cntrID}, err)
		return t.createErrorResponse(req.ID, syscall.EINVAL), nil
	}

	// TOCTOU check.
	if err := libseccomp.NotifIDValid(libseccomp.ScmpFd(fd), req.ID); err != nil {
		logrus.Debugf("TOCTOU check failed on fd %d pid %d cntr %s: req.ID %d is no longer valid (%s)",
			fd, req.Pid, formatter.ContainerID{cntrID}, req.ID, err)
		return t.createErrorResponse(req.ID, err), fmt.Errorf("TOCTOU error")
	}

	if t.nsenterBreaker != nil {
		t.nsenterBreaker.recordSuccess(cntrID)
	}

	return resp, nil
}

func (t *syscallTracer) processMount(
	req *sysRequest,
	fd int32,
	cntr domain.ContainerIface) (*sysResponse, error) {

	logrus.Debugf("Received mount syscall from pid %d", req.Pid)

	// Extract the "path", "name", "fstype" and "data" syscall attributes.
	// Note: even though "data" is defined as a "void *" in the mount(2), we
	// assume it's a string because the mount syscall does not specify its
	// length.
	parsedArgs, err := t.memParser.ReadSyscallStringArgs(
		req.Pid,
		[]memParserDataElem{
			{req.Data.Args[0], unix.PathMax, nil},
			{req.Data.Args[1], unix.PathMax, nil},
			{req.Data.Args[2], unix.PathMax, nil},
			{req.Data.Args[4], unix.PathMax, nil},
		},
	)
	if err != nil {
		return t.createErrorResponse(req.ID, syscall.EPERM), nil
	}
	source := parsedArgs[0]
	target := parsedArgs[1]
	fstype := parsedArgs[2]
	data := parsedArgs[3]

	mount := &mountSyscallInfo{
		syscallCtx: syscallCtx{
			syscallNum: int32(req.Data.Syscall),
			reqId:      req.ID,
			pid:        req.Pid,
			cntr:       cntr,
			tracer:     t,
		},
		MountSyscallPayload: &domain.MountSyscallPayload{
			domain.NSenterMsgHeader{},
			domain.Mount{
				Source: source,
				Target: target,
				FsType: fstype,
				Data:   data,
				Flags:  req.Data.Args[3],
			},
		},
	}

	process := t.service.prs.ProcessCreate(req.Pid, 0, 0)

	// cap_sys_admin capability is required for mount operations.
	if !process.IsSysAdminCapabilitySet() {
		return t.createErrorResponse(req.ID, syscall.EPERM), nil
	}

	mount.Source, err = process.ResolveProcSelf(mount.Source)
	if err != nil {
		return t.createErrorResponse(req.ID, syscall.EACCES), nil
	}

	mount.Target, err = process.ResolveProcSelf(mount.Target)
	if err != nil {
		return t.createErrorResponse(req.ID, syscall.EACCES), nil
	}

	// Verify the process has the proper rights to access the target and
	// update it in case it requires path resolution.
	mount.Target, err = process.PathAccess(mount.Target, 0, true)
	if err != nil {
		return t.createErrorResponse(req.ID, err), nil
	}

	// Collect process attributes required for mount execution.
	mount.uid = process.Uid()
	mount.gid = process.Gid()
	mount.cwd = process.Cwd()
	mount.root = process.Root()
	mount.processInfo = process

	logrus.Debug(mount)

	// To simplify mount processing logic, convert to absolute path if dealing
	// with a relative path request.
	if !filepath.IsAbs(mount.Target) {
		mount.Target = filepath.Join(mount.cwd, mount.Target)
	}

	// Process mount syscall.
	return mount.process()
}

func (t *syscallTracer) processUmount(
	req *sysRequest,
	fd int32,
	cntr domain.ContainerIface) (*sysResponse, error) {

	logrus.Debugf("Received umount syscall from pid %d", req.Pid)

	// Extract "target" syscall attribute.
	parsedArgs, err := t.memParser.ReadSyscallStringArgs(
		req.Pid,
		[]memParserDataElem{{req.Data.Args[0], unix.PathMax, nil}},
	)
	if err != nil {
		return t.createErrorResponse(req.ID, syscall.EPERM), nil
	}
	target := parsedArgs[0]

	umount := &umountSyscallInfo{
		syscallCtx: syscallCtx{
			syscallNum: int32(req.Data.Syscall),
			reqId:      req.ID,
			pid:        req.Pid,
			cntr:       cntr,
			tracer:     t,
		},
		UmountSyscallPayload: &domain.UmountSyscallPayload{
			domain.NSenterMsgHeader{},
			domain.Mount{
				Target: target,
				Flags:  req.Data.Args[1],
			},
		},
	}

	// As per man's capabilities(7), cap_sys_admin capability is required for
	// umount operations. Otherwise, return here and let kernel handle the mount
	// instruction.
	process := t.service.prs.ProcessCreate(req.Pid, 0, 0)
	if !(process.IsSysAdminCapabilitySet()) {
		return t.createErrorResponse(req.ID, syscall.EPERM), nil
	}

	umount.Target, err = process.ResolveProcSelf(umount.Target)
	if err != nil {
		return t.createErrorResponse(req.ID, syscall.EACCES), nil
	}

	// Verify the process has the proper rights to access the target and
	// update it in case it requires path resolution.
	umount.Target, err = process.PathAccess(umount.Target, 0, true)
	if err != nil {
		if err == syscall.ENOENT {
			// XXX: in some cases PathAccess() will unexpectedly hit an ENOENT when
			// walking the path. We've seen this occur on user-created FUSE mounts
			// inside the container, where the path walk to the FUSE mountpoint
			// does not work unless we enter the container's user and mount
			// namespaces (see Sysbox issue 854). Since this never occurs for
			// sysbox-fs managed mounts, let the kernel handle the response (i.e.,
			// for user-created FUSE mounts, the kernel will do the proper path
			// walk within the required namespaces and allow or disallow the
			// unmount as appropriate).
			return t.createContinueResponse(req.ID), nil
		} else {
			return t.createErrorResponse(req.ID, err), nil
		}
	}

	// Collect process attributes required for umount execution.
	umount.uid = process.Uid()
	umount.gid = process.Gid()
	umount.cwd = process.Cwd()
	umount.root = process.Root()
	umount.processInfo = process

	logrus.Debug(umount)

	// To simplify umount processing logic, convert to absolute path if dealing
	// with a relative path request.
	if !filepath.IsAbs(umount.Target) {
		umount.Target = filepath.Join(umount.cwd, umount.Target)
	}

	// Process umount syscall.
	return umount.process()
}

func (t *syscallTracer) processChown(
	req *sysRequest,
	fd int32,
	cntr domain.ContainerIface) (*sysResponse, error) {

	// Extract "path" syscall attribute.
	parsedArgs, err := t.memParser.ReadSyscallStringArgs(
		req.Pid,
		[]memParserDataElem{{req.Data.Args[0], unix.PathMax, nil}},
	)
	if err != nil {
		return t.createErrorResponse(req.ID, syscall.EPERM), nil
	}
	path := parsedArgs[0]

	uid := int64(req.Data.Args[1])
	gid := int64(req.Data.Args[2])

	chown := &chownSyscallInfo{
		syscallCtx: syscallCtx{
			syscallNum: int32(req.Data.Syscall),
			reqId:      req.ID,
			pid:        req.Pid,
			cntr:       cntr,
			tracer:     t,
		},
		path:     path,
		ownerUid: uid,
		ownerGid: gid,
	}

	return chown.processChown()
}

func (t *syscallTracer) processFchown(
	req *sysRequest,
	fd int32,
	cntr domain.ContainerIface) (*sysResponse, error) {

	// We trap fchown() for the same reason we trap chown() (see processChown()).

	pathFd := int32(req.Data.Args[0])
	uid := int64(req.Data.Args[1])
	gid := int64(req.Data.Args[2])

	chown := &chownSyscallInfo{
		syscallCtx: syscallCtx{
			syscallNum: int32(req.Data.Syscall),
			reqId:      req.ID,
			pid:        req.Pid,
			cntr:       cntr,
			tracer:     t,
		},
		pathFd:   pathFd,
		ownerUid: uid,
		ownerGid: gid,
	}

	return chown.processFchown()
}

func (t *syscallTracer) processFchownat(
	req *sysRequest,
	fd int32,
	cntr domain.ContainerIface) (*sysResponse, error) {

	// We trap fchownat() for the same reason we trap chown() (see processChown()).

	// Extract "path" syscall attribute.
	parsedArgs, err := t.memParser.ReadSyscallStringArgs(
		req.Pid,
		[]memParserDataElem{{req.Data.Args[1], unix.PathMax, nil}},
	)
	if err != nil {
		return t.createErrorResponse(req.ID, syscall.EPERM), nil
	}
	path := parsedArgs[0]

	// Get the other args.
	dirFd := int32(req.Data.Args[0])
	uid := int64(req.Data.Args[2])
	gid := int64(req.Data.Args[3])
	flags := int(req.Data.Args[4])

	chown := &chownSyscallInfo{
		syscallCtx: syscallCtx{
			syscallNum: int32(req.Data.Syscall),
			reqId:      req.ID,
			pid:        req.Pid,
			cntr:       cntr,
			tracer:     t,
		},
		path:     path,
		ownerUid: uid,
		ownerGid: gid,
		dirFd:    dirFd,
		flags:    flags,
	}

	return chown.processFchownat()
}

func (t *syscallTracer) processSetxattr(
	req *sysRequest,
	fd int32,
	cntr domain.ContainerIface,
	syscallName string) (*sysResponse, error) {

	// Extract the "path" and "name" syscall attributes.
	parsedArgs, err := t.memParser.ReadSyscallStringArgs(
		req.Pid,
		[]memParserDataElem{
			{req.Data.Args[0], unix.PathMax, nil},
			{req.Data.Args[1], unix.PathMax, nil},
		},
	)
	if err != nil {
		return t.createErrorResponse(req.ID, syscall.EPERM), nil
	}
	path := parsedArgs[0]
	name := parsedArgs[1]

	// Per setxattr(2):
	// Value is a "void *", not necessarily a string (i.e., it may not be null terminated).
	// The size of value (in bytes) is defined by the args[3] parameter.
	parsedArgs, err = t.memParser.ReadSyscallBytesArgs(
		req.Pid,
		[]memParserDataElem{{req.Data.Args[2], int(req.Data.Args[3]), nil}},
	)
	if err != nil {
		return t.createErrorResponse(req.ID, syscall.EPERM), nil
	}
	val := parsedArgs[0]

	flags := int(req.Data.Args[4])

	si := &setxattrSyscallInfo{
		syscallCtx: syscallCtx{
			syscallNum:  int32(req.Data.Syscall),
			syscallName: syscallName,
			reqId:       req.ID,
			pid:         req.Pid,
			cntr:        cntr,
			tracer:      t,
		},
		path:  path,
		name:  name,
		val:   []byte(val),
		flags: flags,
	}

	return si.processSetxattr()
}

func (t *syscallTracer) processFsetxattr(
	req *sysRequest,
	fd int32,
	cntr domain.ContainerIface) (*sysResponse, error) {

	pathFd := int32(req.Data.Args[0])
	flags := int(req.Data.Args[4])

	// Extract "name" syscall attribute.
	parsedArgs, err := t.memParser.ReadSyscallStringArgs(
		req.Pid,
		[]memParserDataElem{{req.Data.Args[1], unix.PathMax, nil}},
	)
	if err != nil {
		return t.createErrorResponse(req.ID, syscall.EPERM), nil
	}
	name := parsedArgs[0]

	// Per setxattr(2):
	// Value is a "void *", not necessarily a string (i.e., it may not be null terminated).
	// The size of value (in bytes) is defined by the args[3] parameter.
	parsedArgs, err = t.memParser.ReadSyscallBytesArgs(
		req.Pid,
		[]memParserDataElem{{req.Data.Args[2], int(req.Data.Args[3]), nil}},
	)
	if err != nil {
		return t.createErrorResponse(req.ID, syscall.EPERM), nil
	}
	val := parsedArgs[0]

	si := &setxattrSyscallInfo{
		syscallCtx: syscallCtx{
			syscallNum: int32(req.Data.Syscall),
			reqId:      req.ID,
			pid:        req.Pid,
			cntr:       cntr,
			tracer:     t,
		},
		pathFd: pathFd,
		name:   name,
		val:    []byte(val),
		flags:  flags,
	}

	return si.processSetxattr()
}

func (t *syscallTracer) processGetxattr(
	req *sysRequest,
	fd int32,
	cntr domain.ContainerIface,
	syscallName string) (*sysResponse, error) {

	// Extract the "path" and "name" syscall attributes.
	parsedArgs, err := t.memParser.ReadSyscallStringArgs(
		req.Pid,
		[]memParserDataElem{
			{req.Data.Args[0], unix.PathMax, nil},
			{req.Data.Args[1], unix.NAME_MAX, nil},
		},
	)
	if err != nil {
		return t.createErrorResponse(req.ID, syscall.EPERM), nil
	}
	path := parsedArgs[0]
	name := parsedArgs[1]

	// "addr" is the mem address where the syscall's result is stored; it's an
	// address in the virtual memory of the process that performed the syscall.
	// We will write the result there.
	addr := uint64(req.Data.Args[2])

	// "size" is the number of bytes in the buffer pointed to by "addr"; we must
	// never write more than this amount of bytes into that buffer. If set to 0
	// then getxattr will return the size of the extended attribute (and
	// not write into the buffer pointed to by "addr").
	size := uint64(req.Data.Args[3])

	si := &getxattrSyscallInfo{
		syscallCtx: syscallCtx{
			syscallNum:  int32(req.Data.Syscall),
			syscallName: syscallName,
			reqId:       req.ID,
			pid:         req.Pid,
			cntr:        cntr,
			tracer:      t,
		},
		path: path,
		name: name,
		addr: addr,
		size: size,
	}

	return si.processGetxattr()
}

func (t *syscallTracer) processFgetxattr(
	req *sysRequest,
	fd int32,
	cntr domain.ContainerIface) (*sysResponse, error) {

	pathFd := int32(req.Data.Args[0])

	// Extract "name" syscall attribute.
	parsedArgs, err := t.memParser.ReadSyscallStringArgs(
		req.Pid,
		[]memParserDataElem{{req.Data.Args[1], unix.PathMax, nil}},
	)
	if err != nil {
		return t.createErrorResponse(req.ID, syscall.EPERM), nil
	}
	name := parsedArgs[0]

	// "addr" is the mem address where the syscall's result is stored; it's an
	// address in the virtual memory of the process that performed the syscall.
	// We will write the result there.
	addr := uint64(req.Data.Args[2])

	// "size" is the number of bytes in the buffer pointed to by "addr"; we must
	// never write more than this amount of bytes into that buffer. If set to 0
	// then getxattr will return the size of the extended attribute (and
	// not write into the buffer pointed to by "addr").
	size := uint64(req.Data.Args[3])

	si := &getxattrSyscallInfo{
		syscallCtx: syscallCtx{
			syscallNum: int32(req.Data.Syscall),
			reqId:      req.ID,
			pid:        req.Pid,
			cntr:       cntr,
			tracer:     t,
		},
		pathFd: pathFd,
		name:   name,
		addr:   addr,
		size:   size,
	}

	return si.processGetxattr()
}

func (t *syscallTracer) processRemovexattr(
	req *sysRequest,
	fd int32,
	cntr domain.ContainerIface,
	syscallName string) (*sysResponse, error) {

	// Extract the "path" and "name" syscall attributes.
	parsedArgs, err := t.memParser.ReadSyscallStringArgs(
		req.Pid,
		[]memParserDataElem{
			{req.Data.Args[0], unix.PathMax, nil},
			{req.Data.Args[1], unix.PathMax, nil},
		},
	)
	if err != nil {
		return t.createErrorResponse(req.ID, syscall.EPERM), nil
	}
	path := parsedArgs[0]
	name := parsedArgs[1]

	si := &removexattrSyscallInfo{
		syscallCtx: syscallCtx{
			syscallNum:  int32(req.Data.Syscall),
			syscallName: syscallName,
			reqId:       req.ID,
			pid:         req.Pid,
			cntr:        cntr,
			tracer:      t,
		},
		path: path,
		name: name,
	}

	return si.processRemovexattr()
}

func (t *syscallTracer) processFremovexattr(
	req *sysRequest,
	fd int32,
	cntr domain.ContainerIface) (*sysResponse, error) {

	pathFd := int32(req.Data.Args[0])

	// Extract "name" syscall attribute.
	parsedArgs, err := t.memParser.ReadSyscallStringArgs(
		req.Pid,
		[]memParserDataElem{{req.Data.Args[1], unix.PathMax, nil}},
	)
	if err != nil {
		return t.createErrorResponse(req.ID, syscall.EPERM), nil
	}
	name := parsedArgs[0]

	si := &removexattrSyscallInfo{
		syscallCtx: syscallCtx{
			syscallNum: int32(req.Data.Syscall),
			reqId:      req.ID,
			pid:        req.Pid,
			cntr:       cntr,
			tracer:     t,
		},
		pathFd: pathFd,
		name:   name,
	}

	return si.processRemovexattr()
}

func (t *syscallTracer) processListxattr(
	req *sysRequest,
	fd int32,
	cntr domain.ContainerIface,
	syscallName string) (*sysResponse, error) {

	// Extract "path" syscall attribute.
	parsedArgs, err := t.memParser.ReadSyscallStringArgs(
		req.Pid,
		[]memParserDataElem{{req.Data.Args[0], unix.PathMax, nil}},
	)
	if err != nil {
		return t.createErrorResponse(req.ID, syscall.EPERM), nil
	}
	path := parsedArgs[0]

	// "addr" is the mem address where the syscall's result is stored; it's an
	// address in the virtual memory of the process that performed the syscall.
	// We will write the result there.
	addr := uint64(req.Data.Args[1])

	// "size" is the number of bytes in the buffer pointed to by "addr"; we must
	// never write more than this amount of bytes into that buffer. If set to 0
	// then listxattr will return the size of the extended attribute (and
	// not write into the buffer pointed to by "addr").
	size := uint64(req.Data.Args[2])

	process := t.service.prs.ProcessCreate(req.Pid, 0, 0)

	// if the process doing the listing does not have CAP_SYS_ADMIN, then it can't access
	// trusted attributes, so no need to nsenter; let the kernel handle it.
	if !process.IsCapabilitySet(cap.EFFECTIVE, cap.CAP_SYS_ADMIN) {
		return t.createContinueResponse(req.ID), nil
	}

	si := &listxattrSyscallInfo{
		syscallCtx: syscallCtx{
			syscallNum:  int32(req.Data.Syscall),
			syscallName: syscallName,
			reqId:       req.ID,
			pid:         req.Pid,
			uid:         process.Uid(),
			gid:         process.Gid(),
			root:        process.Root(),
			cwd:         process.Cwd(),
			cntr:        cntr,
			tracer:      t,
			processInfo: process,
		},
		path: path,
		addr: addr,
		size: size,
	}

	return si.processListxattr()
}

func (t *syscallTracer) processFlistxattr(
	req *sysRequest,
	fd int32,
	cntr domain.ContainerIface) (*sysResponse, error) {

	pathFd := int32(req.Data.Args[0])

	// "addr" is the mem address where the syscall's result is stored; it's an
	// address in the virtual memory of the process that performed the syscall.
	// We will write the result there.
	addr := uint64(req.Data.Args[1])

	// "size" is the number of bytes in the buffer pointed to by "addr"; we must
	// never write more than this amount of bytes into that buffer. If set to 0
	// then listxattr will return the size of the extended attribute (and
	// not write into the buffer pointed to by "addr").
	size := uint64(req.Data.Args[2])

	process := t.service.prs.ProcessCreate(req.Pid, 0, 0)

	// if the process doing the listing does not have CAP_SYS_ADMIN, then it can't access
	// trusted attributes, so no need to nsenter; let the kernel handle it.
	if !process.IsCapabilitySet(cap.EFFECTIVE, cap.CAP_SYS_ADMIN) {
		return t.createContinueResponse(req.ID), nil
	}

	si := &listxattrSyscallInfo{
		syscallCtx: syscallCtx{
			syscallNum:  int32(req.Data.Syscall),
			reqId:       req.ID,
			pid:         req.Pid,
			uid:         process.Uid(),
			gid:         process.Gid(),
			root:        process.Root(),
			cwd:         process.Cwd(),
			cntr:        cntr,
			tracer:      t,
			processInfo: process,
		},
		pathFd: pathFd,
		addr:   addr,
		size:   size,
	}

	return si.processListxattr()
}

func (t *syscallTracer) processReboot(
	req *sysRequest,
	fd int32,
	cntrID domain.ContainerIface) (*sysResponse, error) {

	logrus.Warnf("Received reboot syscall")

	return t.createSuccessResponse(req.ID), nil
}

func (t *syscallTracer) processSwapon(
	req *sysRequest,
	fd int32,
	cntr domain.ContainerIface) (*sysResponse, error) {

	logrus.Warnf("Received swapon syscall")

	return t.createSuccessResponse(req.ID), nil
}

func (t *syscallTracer) processSwapoff(
	req *sysRequest,
	fd int32,
	cntr domain.ContainerIface) (*sysResponse, error) {

	logrus.Warnf("Received swapoff syscall")

	return t.createSuccessResponse(req.ID), nil
}

func (t *syscallTracer) processOpenat2(
	req *sysRequest,
	fd int32,
	cntr domain.ContainerIface) (*sysResponse, error) {

	dirfd := int32(req.Data.Args[0])
	howSize := int(req.Data.Args[3])

	// Extract "path" syscall argument.
	parsedArgs, err := t.memParser.ReadSyscallStringArgs(
		req.Pid,
		[]memParserDataElem{{req.Data.Args[1], unix.PathMax, nil}},
	)
	if err != nil {
		return t.createErrorResponse(req.ID, syscall.EPERM), nil
	}
	path := parsedArgs[0]

	// Extract "open_how" structure from process memory.
	// The open_how struct has at least 3 fields: flags (u64), mode (u64), resolve (u64)
	// We read the size specified by the caller to handle different struct versions.
	parsedArgs, err = t.memParser.ReadSyscallBytesArgs(
		req.Pid,
		[]memParserDataElem{{req.Data.Args[2], howSize, nil}},
	)
	if err != nil {
		return t.createErrorResponse(req.ID, syscall.EPERM), nil
	}
	howBytes := []byte(parsedArgs[0])

	// Parse the open_how structure (minimum 24 bytes: 3 x uint64)
	var flags, mode, resolve uint64
	if len(howBytes) >= 24 {
		flags = binary.LittleEndian.Uint64(howBytes[0:8])
		mode = binary.LittleEndian.Uint64(howBytes[8:16])
		resolve = binary.LittleEndian.Uint64(howBytes[16:24])
	} else {
		return t.createErrorResponse(req.ID, syscall.EINVAL), nil
	}

	si := &openat2SyscallInfo{
		syscallCtx: syscallCtx{
			syscallNum: int32(req.Data.Syscall),
			reqId:      req.ID,
			pid:        req.Pid,
			cntr:       cntr,
			tracer:     t,
		},
		dirfd:    dirfd,
		path:     path,
		flags:    flags,
		mode:     mode,
		resolve:  resolve,
		notifyFd: fd,
	}

	// Collect process attributes required for openat2 execution.
	process := t.service.prs.ProcessCreate(req.Pid, 0, 0)
	si.processInfo = process
	si.uid = process.Uid()
	si.gid = process.Gid()
	si.cwd = process.Cwd()
	si.root = process.Root()
	si.caps = process.GetEffCaps()

	return si.processOpenat2()
}

func (t *syscallTracer) createSuccessResponse(id uint64) *sysResponse {

	resp := &sysResponse{
		ID:    id,
		Error: 0,
		Val:   0,
		Flags: 0,
	}

	return resp
}

func (t *syscallTracer) createSuccessResponseWithRetValue(id, val uint64) *sysResponse {

	resp := &sysResponse{
		ID:    id,
		Error: 0,
		Val:   val,
		Flags: 0,
	}

	return resp
}

func (t *syscallTracer) createContinueResponse(id uint64) *sysResponse {

	resp := &sysResponse{
		ID:    id,
		Error: 0,
		Val:   0,
		Flags: libseccomp.NotifRespFlagContinue,
	}

	return resp
}

func (t *syscallTracer) createErrorResponse(id uint64, err error) *sysResponse {

	// Override the passed error if this one doesn't match the supported type.
	rcvdError, ok := err.(syscall.Errno)
	if !ok {
		rcvdError = syscall.EINVAL
	}

	resp := &sysResponse{
		ID:    id,
		Error: int32(rcvdError),
		Val:   0,
		Flags: 0,
	}

	return resp
}
