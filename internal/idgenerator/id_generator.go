package idgenerator

import (
	"binance-grid-bot-go/internal/storage"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// IDGenerator is a persistent, sequential ID generator.
type IDGenerator struct {
	lastID  atomic.Uint64
	storage storage.Storage
}

// NewIDGenerator creates a new persistent IDGenerator.
// It loads the last known ID from the provided storage.
func NewIDGenerator(storage storage.Storage) (*IDGenerator, error) {
	if storage == nil {
		return nil, errors.New("storage cannot be nil")
	}

	lastID, err := storage.LoadIDGeneratorState()
	if err != nil {
		return nil, fmt.Errorf("failed to load id generator state: %w", err)
	}

	gen := &IDGenerator{
		storage: storage,
	}
	gen.lastID.Store(lastID)

	return gen, nil
}

// Generate creates and returns a new unique, persistent ID.
// The format is "grid-timestamp-sequence".
func (g *IDGenerator) Generate() (string, error) {
	// Atomically increment the ID.
	newID := g.lastID.Add(1)

	// Persist the new ID to storage.
	if err := g.storage.SaveIDGeneratorState(newID); err != nil {
		// This is a critical failure. If we can't save the state, we risk reusing IDs on restart.
		// A real-world system might enter a safe mode here.
		// For now, we'll return the ID but also the error to signal the problem.
		return "", fmt.Errorf("generated new ID (%d) but failed to persist state: %w", newID, err)
	}

	// Format the ID with a timestamp for better uniqueness and readability.
	// Example: "grid-1704067200-123"
	timestamp := time.Now().Unix()
	return fmt.Sprintf("grid-%d-%d", timestamp, newID), nil
}
