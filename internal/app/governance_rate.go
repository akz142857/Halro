package app

import (
	"strconv"
	"sync"
	"time"
)

const (
	governanceWriteKeyRPM     = 120
	governanceWriteProjectRPM = 1_000
	governanceReadKeyRPM      = 600
	governanceReadProjectRPM  = 5_000
	governanceTrackedKeys     = 16_384
	governanceTrackedProjects = 4_096
)

type governanceRateWindow struct {
	minute int64
	count  int
}

// governanceRateState owns the control plane's authenticated Key and Project
// buckets. Both checks and increments happen under one lock, so a Project
// refusal does not consume a Key permit and concurrent callers cannot pass the
// same final slot.
type governanceRateState struct {
	mu      sync.Mutex
	key     map[string]governanceRateWindow
	project map[string]governanceRateWindow
}

func (state *governanceRateState) allow(
	now time.Time,
	keyID, projectID string,
	write bool,
	keyLimit, projectLimit int,
) (bool, string) {
	minute := now.UTC().Unix() / 60
	prefix := "read\x00"
	if write {
		prefix = "write\x00"
	}
	keyName, projectName := prefix+keyID, prefix+projectID
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.key == nil {
		state.key = make(map[string]governanceRateWindow)
		state.project = make(map[string]governanceRateWindow)
	}
	keyName = boundedGovernanceBucket(state.key, keyName, prefix+"overflow", minute, governanceTrackedKeys)
	projectName = boundedGovernanceBucket(state.project, projectName, prefix+"overflow", minute, governanceTrackedProjects)
	keyWindow := currentGovernanceWindow(state.key[keyName], minute)
	projectWindow := currentGovernanceWindow(state.project[projectName], minute)
	if keyWindow.count >= keyLimit || projectWindow.count >= projectLimit {
		seconds := 60 - now.UTC().Unix()%60
		return false, strconv.FormatInt(seconds, 10)
	}
	keyWindow.count++
	projectWindow.count++
	state.key[keyName] = keyWindow
	state.project[projectName] = projectWindow
	return true, ""
}

func boundedGovernanceBucket(
	windows map[string]governanceRateWindow,
	wanted, overflow string,
	minute int64,
	limit int,
) string {
	if _, exists := windows[wanted]; exists || len(windows) < limit {
		return wanted
	}
	for key, window := range windows {
		if window.minute != minute {
			delete(windows, key)
		}
	}
	if len(windows) < limit {
		return wanted
	}
	return overflow
}

func currentGovernanceWindow(window governanceRateWindow, minute int64) governanceRateWindow {
	if window.minute != minute {
		return governanceRateWindow{minute: minute}
	}
	return window
}
