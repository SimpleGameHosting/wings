package transfer

import (
	"sync"
)

var (
	incomingTransfers = NewManager()
	outgoingTransfers = NewManager()
)

// Incoming returns a transfer manager for incoming transfers.
func Incoming() *Manager {
	return incomingTransfers
}

// Outgoing returns a transfer manager for outgoing transfers.
func Outgoing() *Manager {
	return outgoingTransfers
}

// Manager manages transfers.
type Manager struct {
	mu        sync.RWMutex
	transfers map[string]*Transfer
	reserved  map[string]struct{}
}

// NewManager returns a new transfer manager.
func NewManager() *Manager {
	return &Manager{
		transfers: make(map[string]*Transfer),
		reserved:  make(map[string]struct{}),
	}
}

// TryReserve atomically claims the exclusive right to drive a transfer for
// the given server ID. It fails while another request holds the reservation
// or while a registered transfer for that server exists, and the winner must
// call Release once the transfer has been fully settled.
func (m *Manager) TryReserve(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.reserved[id]; exists {
		return false
	}
	if _, exists := m.transfers[id]; exists {
		return false
	}

	m.reserved[id] = struct{}{}
	return true
}

// Release frees a reservation taken with TryReserve. Releasing an ID that is
// not reserved is a no-op so callers can release unconditionally on exit.
func (m *Manager) Release(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.reserved, id)
}

// Add adds a transfer to the manager.
func (m *Manager) Add(transfer *Transfer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.transfers[transfer.Server.ID()] = transfer
}

// Remove removes a transfer from the manager.
func (m *Manager) Remove(transfer *Transfer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.transfers, transfer.Server.ID())
}

// Get gets a transfer from the manager using a server ID.
func (m *Manager) Get(id string) *Transfer {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.transfers[id]
}
