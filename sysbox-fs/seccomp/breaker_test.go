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
	"testing"
	"time"
)

func TestNsenterBreakerOpensAfterThreshold(t *testing.T) {
	b := newNsenterBreaker(true)
	now := time.Unix(1_000, 0)
	b.clock = func() time.Time { return now }

	cntr := "c1"
	if !b.allow(cntr) {
		t.Fatal("expected allow before any timeouts")
	}
	b.recordTimeout(cntr)
	b.recordTimeout(cntr)
	if !b.allow(cntr) {
		t.Fatal("expected allow before threshold")
	}
	b.recordTimeout(cntr) // 3rd → open
	if b.allow(cntr) {
		t.Fatal("expected deny while open during cooldown")
	}

	// During cooldown still deny.
	now = now.Add(10 * time.Second)
	if b.allow(cntr) {
		t.Fatal("expected deny mid-cooldown")
	}

	// After cooldown: one half-open probe allowed.
	now = now.Add(25 * time.Second)
	if !b.allow(cntr) {
		t.Fatal("expected half-open probe after cooldown")
	}
	if b.allow(cntr) {
		t.Fatal("expected second probe denied while first in flight")
	}

	// Probe success closes breaker.
	b.recordSuccess(cntr)
	if !b.allow(cntr) {
		t.Fatal("expected allow after recovery")
	}
}

func TestNsenterBreakerDisabled(t *testing.T) {
	b := newNsenterBreaker(false)
	cntr := "c1"
	for i := 0; i < 10; i++ {
		b.recordTimeout(cntr)
	}
	if !b.allow(cntr) {
		t.Fatal("disabled breaker must always allow")
	}
}

func TestNsenterBreakerHalfOpenTimeoutReopens(t *testing.T) {
	b := newNsenterBreaker(true)
	now := time.Unix(2_000, 0)
	b.clock = func() time.Time { return now }

	cntr := "c2"
	for i := 0; i < nsenterBreakerThreshold; i++ {
		b.recordTimeout(cntr)
	}
	now = now.Add(nsenterBreakerCooldown)
	if !b.allow(cntr) {
		t.Fatal("expected half-open probe")
	}
	b.recordTimeout(cntr) // probe fails
	if b.allow(cntr) {
		t.Fatal("expected deny after failed probe re-opens cooldown")
	}
}
