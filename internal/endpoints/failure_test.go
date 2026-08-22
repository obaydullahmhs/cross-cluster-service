/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package endpoints

import (
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
	"github.com/obaydullahmhs/cross-cluster-service/internal/resolver"
)

// TestI9_TransientFailureMustNotEmptyTheSlice covers invariant I9.
func TestI9_TransientFailureMustNotEmptyTheSlice(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	boom := errors.New("CoreDNS unreachable")

	lastGood := []netv1alpha1.PersistedEndpoint{
		{Address: addrA, Ready: true, Ports: map[string]int32{"": 80}},
		{Address: "10.0.0.2", Ready: true, Ports: map[string]int32{"": 80}},
	}
	statusAt := func(ago time.Duration, consecutive int32) *netv1alpha1.SourceStatus {
		ts := metav1.NewTime(now.Add(-ago))
		return &netv1alpha1.SourceStatus{
			LastSuccessTime:   &ts,
			ConsecutiveErrors: consecutive,
			LastKnownGood:     lastGood,
		}
	}

	policy := &netv1alpha1.FailurePolicy{
		FailureThreshold: 3,
		StaleThreshold:   &metav1.Duration{Duration: 5 * time.Minute},
		OnStale:          netv1alpha1.StaleActionMarkNotReady,
	}

	cases := []struct {
		name          string
		policy        *netv1alpha1.FailurePolicy
		prev          *netv1alpha1.SourceStatus
		err           error
		wantCount     int
		wantAllReady  bool
		wantStale     bool
		wantDegraded  bool
		wantConsecErr int32
	}{
		{
			name:         "success serves fresh endpoints",
			policy:       policy,
			prev:         statusAt(time.Minute, 0),
			err:          nil,
			wantCount:    1,
			wantAllReady: true,
		},
		{
			name:          "a two second blip keeps serving and is not yet degraded",
			policy:        policy,
			prev:          statusAt(2*time.Second, 0),
			err:           boom,
			wantCount:     2,
			wantAllReady:  true,
			wantStale:     false,
			wantDegraded:  false,
			wantConsecErr: 1,
		},
		{
			name:          "one below the threshold still keeps serving",
			policy:        policy,
			prev:          statusAt(time.Minute, 1),
			err:           boom,
			wantCount:     2,
			wantAllReady:  true,
			wantDegraded:  false,
			wantConsecErr: 2,
		},
		{
			name:          "at the threshold inside the stale window serves stale but ready",
			policy:        policy,
			prev:          statusAt(time.Minute, 2),
			err:           boom,
			wantCount:     2,
			wantAllReady:  true,
			wantStale:     true,
			wantDegraded:  true,
			wantConsecErr: 3,
		},
		{
			name:          "past the stale window MarkNotReady keeps the addresses but flips ready",
			policy:        policy,
			prev:          statusAt(10*time.Minute, 5),
			err:           boom,
			wantCount:     2,
			wantAllReady:  false,
			wantStale:     true,
			wantDegraded:  true,
			wantConsecErr: 6,
		},
		{
			name: "past the stale window Remove empties the slice",
			policy: &netv1alpha1.FailurePolicy{
				FailureThreshold: 3,
				StaleThreshold:   &metav1.Duration{Duration: 5 * time.Minute},
				OnStale:          netv1alpha1.StaleActionRemove,
			},
			prev:          statusAt(10*time.Minute, 5),
			err:           boom,
			wantCount:     0,
			wantStale:     true,
			wantDegraded:  true,
			wantConsecErr: 6,
		},
		{
			name:          "a source that never succeeded has nothing to keep serving",
			policy:        policy,
			prev:          nil,
			err:           boom,
			wantCount:     0,
			wantConsecErr: 1,
		},
	}

	fresh := []resolver.Endpoint{ep(t, "10.0.0.9", map[string]int32{"": 80})}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ApplyFailurePolicy(tc.policy, tc.prev, fresh, tc.err, now)

			if len(got.Endpoints) != tc.wantCount {
				t.Fatalf("served %d endpoints, want %d", len(got.Endpoints), tc.wantCount)
			}
			for _, e := range got.Endpoints {
				if e.Ready != tc.wantAllReady {
					t.Errorf("endpoint %s ready = %v, want %v", e.Address, e.Ready, tc.wantAllReady)
				}
			}
			if got.Stale != tc.wantStale {
				t.Errorf("stale = %v, want %v", got.Stale, tc.wantStale)
			}
			if got.Degraded != tc.wantDegraded {
				t.Errorf("degraded = %v, want %v", got.Degraded, tc.wantDegraded)
			}
			if tc.err != nil && got.ConsecutiveErrors != tc.wantConsecErr {
				t.Errorf("consecutiveErrors = %d, want %d", got.ConsecutiveErrors, tc.wantConsecErr)
			}
		})
	}
}

func TestFailurePolicyDefaults(t *testing.T) {
	// The defaults are the invariant: never immediate Remove.
	threshold, stale, onStale := FailurePolicyDefaults(nil)
	if threshold != 3 {
		t.Errorf("threshold = %d, want 3", threshold)
	}
	if stale != 5*time.Minute {
		t.Errorf("staleThreshold = %v, want 5m", stale)
	}
	if onStale != netv1alpha1.StaleActionMarkNotReady {
		t.Errorf("onStale = %q, want MarkNotReady", onStale)
	}
}

func TestToPersistedIsBounded(t *testing.T) {
	in := make([]resolver.Endpoint, 0, MaxPersistedEndpoints+50)
	for i := range MaxPersistedEndpoints + 50 {
		in = append(in, ep(t, netipFromIndex(t, i), map[string]int32{"": 80}))
	}
	// Status must not grow without limit, or a large source turns every
	// reconcile into a large write.
	if got := len(ToPersisted(in)); got != MaxPersistedEndpoints {
		t.Errorf("persisted %d endpoints, want the %d cap", got, MaxPersistedEndpoints)
	}
}

func netipFromIndex(t *testing.T, i int) string {
	t.Helper()
	return "10." + itoa(i/65536%256) + "." + itoa(i/256%256) + "." + itoa(i%256)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
