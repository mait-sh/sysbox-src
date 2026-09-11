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

package seccomp

import (
	"sync"
	"time"

	"github.com/nestybox/sysbox-libs/formatter"
	"github.com/sirupsen/logrus"
)

const (
	nsenterBreakerThreshold = 3
	nsenterBreakerCooldown   = 30 * time.Second
)

// nsenterBreaker is a per-container consecutive-hard-timeout circuit breaker.
// When open, mediation returns EIO without calling nsenter.
type nsenterBreaker struct {
	mu      sync.Mutex
	enabled bool
	states  map[string]*breakerState
	// clock is overridable in tests.
	clock func() time.Time
}

type breakerState struct {
	consecutive int
	openUntil   time.Time
	// halfOpenProbeInFlight allows exactly one request after cooldown.
	halfOpenProbeInFlight bool
}

func newNsenterBreaker(enabled bool) *nsenterBreaker {
	return &nsenterBreaker{
		enabled: enabled,
		states:  make(map[string]*breakerState),
		clock:   time.Now,
	}
}

func (b *nsenterBreaker) now() time.Time {
	if b.clock != nil {
		return b.clock()
	}
	return time.Now()
}

func (b *nsenterBreaker) stateLocked(cntrID string) *breakerState {
	st, ok := b.states[cntrID]
	if !ok {
		st = &breakerState{}
		b.states[cntrID] = st
	}
	return st
}

// allow reports whether processSyscall may run. When false, the caller must
// return EIO without invoking nsenter.
func (b *nsenterBreaker) allow(cntrID string) bool {
	if b == nil || !b.enabled {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	st := b.stateLocked(cntrID)
	now := b.now()

	if st.openUntil.IsZero() {
		return true
	}
	if now.Before(st.openUntil) {
		return false
	}
	// Cooldown elapsed: half-open — admit a single probe.
	if st.halfOpenProbeInFlight {
		return false
	}
	st.halfOpenProbeInFlight = true
	return true
}

// recordTimeout increments the consecutive hard-timeout counter and may open
// the breaker.
func (b *nsenterBreaker) recordTimeout(cntrID string) {
	if b == nil || !b.enabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	st := b.stateLocked(cntrID)
	st.consecutive++
	st.halfOpenProbeInFlight = false

	if st.consecutive < nsenterBreakerThreshold {
		return
	}

	wasOpen := !st.openUntil.IsZero() && b.now().Before(st.openUntil)
	st.openUntil = b.now().Add(nsenterBreakerCooldown)
	if !wasOpen {
		logrus.Errorf("sysbox-fs: mediation degraded for container %s (nsenter hard-timeouts=%d, cooldown=%v)",
			formatter.ContainerID{cntrID}, st.consecutive, nsenterBreakerCooldown)
	}
}

// recordSuccess clears consecutive timeouts and closes an open/half-open breaker.
func (b *nsenterBreaker) recordSuccess(cntrID string) {
	if b == nil || !b.enabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	st := b.stateLocked(cntrID)
	wasOpen := !st.openUntil.IsZero() || st.halfOpenProbeInFlight
	st.consecutive = 0
	st.openUntil = time.Time{}
	st.halfOpenProbeInFlight = false
	if wasOpen {
		logrus.Infof("sysbox-fs: mediation recovered for container %s",
			formatter.ContainerID{cntrID})
	}
}
