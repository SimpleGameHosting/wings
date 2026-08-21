package server

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCrashHandlerUptime(t *testing.T) {
	h := CrashHandler{}

	assert.Zero(t, h.LastUptime())

	h.SetLastUptime(90500)
	assert.EqualValues(t, 90500, h.LastUptime())

	// Concurrent access must be race-free; CI runs go test -race.
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(v int64) { defer wg.Done(); h.SetLastUptime(v) }(int64(i))
		go func() { defer wg.Done(); _ = h.LastUptime() }()
	}
	wg.Wait()
}
