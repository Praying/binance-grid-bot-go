package persistence

import "binance-grid-bot-go/internal/models"

// StateRepository defines the interface for state persistence.
// It abstracts the underlying storage mechanism (e.g., BadgerDB, in-memory)
// from the rest of the application.
type StateRepository interface {
	// SaveState atomically saves the entire bot state.
	SaveState(state *models.BotState) error

	// LoadState loads the bot state from storage.
	// If no state is found, it should return (nil, nil).
	LoadState() (*models.BotState, error)

	// Close gracefully closes the connection to the database.
	Close() error
}
