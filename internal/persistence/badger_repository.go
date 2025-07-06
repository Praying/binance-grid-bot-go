package persistence

import (
	"binance-grid-bot-go/internal/models"
	"encoding/json"
	"errors"

	"github.com/dgraph-io/badger/v3"
)

// badgerRepository is the BadgerDB implementation of the StateRepository.
type badgerRepository struct {
	db       *badger.DB
	stateKey []byte
}

// NewBadgerRepository creates and returns a new repository instance connected to a BadgerDB database.
func NewBadgerRepository(dbPath string) (StateRepository, error) {
	opts := badger.DefaultOptions(dbPath)
	// For this use case, we can disable Badger's own logging to keep our app's logs clean.
	// Errors will still be returned from DB operations.
	opts.Logger = nil

	db, err := badger.Open(opts)
	if err != nil {
		return nil, err
	}

	return &badgerRepository{
		db:       db,
		stateKey: []byte("bot_state"), // Define a constant key for our single state object
	}, nil
}

// SaveState atomically saves the entire bot state.
// It marshals the state struct into JSON and saves it under a predefined key.
func (r *badgerRepository) SaveState(state *models.BotState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}

	return r.db.Update(func(txn *badger.Txn) error {
		return txn.Set(r.stateKey, data)
	})
}

// LoadState loads the bot state from storage.
// If the state key is not found, it returns (nil, nil) to indicate no state is present.
func (r *badgerRepository) LoadState() (*models.BotState, error) {
	var state models.BotState

	err := r.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(r.stateKey)
		if err != nil {
			// This is the correct way to handle key not found.
			// We return the specific error to check it outside the transaction.
			return err
		}

		return item.Value(func(val []byte) error {
			if len(val) == 0 {
				return errors.New("state value is empty in database")
			}
			return json.Unmarshal(val, &state)
		})
	})

	// After the transaction, check for the specific "key not found" error.
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, nil // This is the expected "no state found" case.
	}

	// If there was another type of error, return it.
	if err != nil {
		return nil, err
	}

	// If everything went well, return the loaded state.
	return &state, nil
}

// Close gracefully closes the connection to the database.
func (r *badgerRepository) Close() error {
	return r.db.Close()
}
