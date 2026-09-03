package server

import (
	"errors"
	"sync"
	"time"
)

// The errors the request guards return when an upload has to be refused
// before any bytes are read.
var (
	ErrUploadRateLimited = errors.New("resumable upload request rate exceeded")
	ErrUploadBusy        = errors.New("too many resumable upload transfers are in progress")
)

// UploadRateLimitError reports when every blocked rate scope will accept another request.
type UploadRateLimitError struct {
	RetryAfter time.Duration
}

// Error returns the stable rate-limit description.
func (e *UploadRateLimitError) Error() string {
	return ErrUploadRateLimited.Error()
}

// Unwrap supports errors.Is checks against the public rate-limit sentinel.
func (e *UploadRateLimitError) Unwrap() error {
	return ErrUploadRateLimited
}

// uploadRequestGuards bounds request rates and simultaneous streaming bodies at node scope.
type uploadRequestGuards struct {
	mu sync.Mutex

	creationNode    uploadRateWindow
	creationServers map[string]uploadRateWindow
	creationUsers   map[string]uploadRateWindow
	requestNode     uploadRateWindow
	requestServers  map[string]uploadRateWindow
	requestUsers    map[string]uploadRateWindow

	concurrentNode    int
	concurrentServers map[string]int
	concurrentUsers   map[string]int
}

// uploadRateWindow records an exact fixed-minute request count and its last activity.
type uploadRateWindow struct {
	started  time.Time
	lastSeen time.Time
	count    int
}

// newUploadRequestGuards creates empty bounded-request accounting maps.
func newUploadRequestGuards() uploadRequestGuards {
	return uploadRequestGuards{
		creationServers:   make(map[string]uploadRateWindow),
		creationUsers:     make(map[string]uploadRateWindow),
		requestServers:    make(map[string]uploadRateWindow),
		requestUsers:      make(map[string]uploadRateWindow),
		concurrentServers: make(map[string]int),
		concurrentUsers:   make(map[string]int),
	}
}

// AllowCreation consumes one creation request from the configured fixed-minute windows.
func (m *UploadManager) AllowCreation(serverUUID, userUUID string) error {
	return m.guards.allow(
		serverUUID,
		userUUID,
		m.limits.CreationRatePerMinuteNode,
		m.limits.CreationRatePerMinuteServer,
		m.limits.CreationRatePerMinuteUser,
		&m.guards.creationNode,
		m.guards.creationServers,
		m.guards.creationUsers,
	)
}

// AllowRequest consumes one session request from the configured fixed-minute windows.
func (m *UploadManager) AllowRequest(serverUUID, userUUID string) error {
	return m.guards.allow(
		serverUUID,
		userUUID,
		m.limits.RequestRatePerMinuteNode,
		m.limits.RequestRatePerMinuteServer,
		m.limits.RequestRatePerMinuteUser,
		&m.guards.requestNode,
		m.guards.requestServers,
		m.guards.requestUsers,
	)
}

// AcquireTransfer reserves one streaming slot and returns its idempotent release function.
func (m *UploadManager) AcquireTransfer(serverUUID, userUUID string) (func(), error) {
	m.guards.mu.Lock()
	defer m.guards.mu.Unlock()

	userKey := uploadUserKey(serverUUID, userUUID)
	if uploadLimitReached(m.guards.concurrentNode, m.limits.MaxConcurrentNode) ||
		uploadLimitReached(m.guards.concurrentServers[serverUUID], m.limits.MaxConcurrentPerServer) ||
		uploadLimitReached(m.guards.concurrentUsers[userKey], m.limits.MaxConcurrentPerUser) {
		return nil, ErrUploadBusy
	}
	m.guards.concurrentNode++
	m.guards.concurrentServers[serverUUID]++
	m.guards.concurrentUsers[userKey]++

	var once sync.Once
	return func() {
		once.Do(func() {
			m.guards.mu.Lock()
			defer m.guards.mu.Unlock()
			m.guards.concurrentNode--
			decrementUploadCounter(m.guards.concurrentServers, serverUUID)
			decrementUploadCounter(m.guards.concurrentUsers, userKey)
		})
	}, nil
}

// cleanupRequestGuards drops inactive rate keys so one-time users are not retained forever.
func (m *UploadManager) cleanupRequestGuards(now time.Time) {
	m.guards.mu.Lock()
	defer m.guards.mu.Unlock()

	cutoff := now.Add(-2 * time.Minute)
	deleteStaleUploadWindows(m.guards.creationServers, cutoff)
	deleteStaleUploadWindows(m.guards.creationUsers, cutoff)
	deleteStaleUploadWindows(m.guards.requestServers, cutoff)
	deleteStaleUploadWindows(m.guards.requestUsers, cutoff)
}

// allow checks every rate scope atomically before consuming the request.
func (g *uploadRequestGuards) allow(
	serverUUID string,
	userUUID string,
	nodeLimit int,
	serverLimit int,
	userLimit int,
	node *uploadRateWindow,
	servers map[string]uploadRateWindow,
	users map[string]uploadRateWindow,
) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := time.Now().UTC()
	serverWindow := currentUploadWindow(servers[serverUUID], now)
	userKey := uploadUserKey(serverUUID, userUUID)
	userWindow := currentUploadWindow(users[userKey], now)
	*node = currentUploadWindow(*node, now)
	if uploadLimitReached(node.count, nodeLimit) ||
		uploadLimitReached(serverWindow.count, serverLimit) ||
		uploadLimitReached(userWindow.count, userLimit) {
		retryAfter := blockedUploadWindowDuration(*node, nodeLimit, now)
		retryAfter = max(retryAfter, blockedUploadWindowDuration(serverWindow, serverLimit, now))
		retryAfter = max(retryAfter, blockedUploadWindowDuration(userWindow, userLimit, now))
		return &UploadRateLimitError{RetryAfter: retryAfter}
	}
	node.count++
	node.lastSeen = now
	serverWindow.count++
	serverWindow.lastSeen = now
	userWindow.count++
	userWindow.lastSeen = now
	servers[serverUUID] = serverWindow
	users[userKey] = userWindow
	return nil
}

// blockedUploadWindowDuration returns the time remaining only when a scope is exhausted.
func blockedUploadWindowDuration(window uploadRateWindow, limit int, now time.Time) time.Duration {
	if !uploadLimitReached(window.count, limit) {
		return 0
	}
	remaining := time.Minute - now.Sub(window.started)
	if remaining < time.Second {
		return time.Second
	}
	return remaining
}

// currentUploadWindow starts a new fixed window once the prior minute has elapsed.
func currentUploadWindow(window uploadRateWindow, now time.Time) uploadRateWindow {
	if window.started.IsZero() || now.Sub(window.started) >= time.Minute {
		return uploadRateWindow{started: now, lastSeen: now}
	}
	return window
}

// uploadLimitReached reports whether a positive limit has no remaining capacity.
func uploadLimitReached(current, maximum int) bool {
	return maximum > 0 && current >= maximum
}

// decrementUploadCounter decrements a keyed counter and removes empty entries.
func decrementUploadCounter(counters map[string]int, key string) {
	counters[key]--
	if counters[key] <= 0 {
		delete(counters, key)
	}
}

// deleteStaleUploadWindows removes rate keys that have been idle beyond the cutoff.
func deleteStaleUploadWindows(windows map[string]uploadRateWindow, cutoff time.Time) {
	for key, window := range windows {
		if window.lastSeen.Before(cutoff) {
			delete(windows, key)
		}
	}
}
