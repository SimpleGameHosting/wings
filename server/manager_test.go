package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestManagerAllReturnsSnapshot ensures callers cannot mutate the manager's
// backing collection after its read lock has been released.
func TestManagerAllReturnsSnapshot(t *testing.T) {
	first := &Server{}
	second := &Server{}
	manager := NewEmptyManager(nil)
	input := []*Server{first, second}
	manager.Put(input)
	input[0] = nil
	assert.Same(t, first, manager.All()[0])

	servers := manager.All()
	servers[0] = nil

	assert.Same(t, first, manager.All()[0])
}
