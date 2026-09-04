package app

import (
	"testing"
	"time"
)

func TestGovernanceRateRequiresBothKeyAndProjectCapacity(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 30, 0, time.UTC)
	var state governanceRateState
	if allowed, _ := state.allow(now, "key_1", "prj_1", true, 2, 2); !allowed {
		t.Fatal("first permit was refused")
	}
	if allowed, _ := state.allow(now, "key_2", "prj_1", true, 2, 2); !allowed {
		t.Fatal("second project permit was refused")
	}
	if allowed, retry := state.allow(now, "key_1", "prj_1", true, 2, 2); allowed || retry != "30" {
		t.Fatalf("project limit allowed=%t retry=%q", allowed, retry)
	}
	if allowed, _ := state.allow(now, "key_1", "prj_2", true, 2, 2); !allowed {
		t.Fatal("project refusal consumed the key's second permit")
	}
	if allowed, _ := state.allow(now, "key_1", "prj_2", false, 1, 1); !allowed {
		t.Fatal("read and write buckets were not independent")
	}
	if allowed, _ := state.allow(now.Add(30*time.Second), "key_1", "prj_1", true, 2, 2); !allowed {
		t.Fatal("new minute did not reset the bucket")
	}
}
