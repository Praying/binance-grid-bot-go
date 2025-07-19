package storage

import (
	"binance-grid-bot-go/internal/models"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3" // SQLite driver
)

// Storage defines the interface for persistence operations.
type Storage interface {
	SaveGrid(grid *models.Grid) error
	LoadGrid() (*models.Grid, error)
	SaveIDGeneratorState(lastID uint64) error
	LoadIDGeneratorState() (uint64, error)
	Close() error
}

// SQLiteStorage implements the Storage interface using SQLite.
type SQLiteStorage struct {
	db *sql.DB
}

// NewSQLiteStorage creates and initializes a new SQLite storage backend.
func NewSQLiteStorage(dbPath string) (*SQLiteStorage, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	// Create the table for storing the grid state if it doesn't exist.
	// We store the entire grid state as a single JSON blob for flexibility.
	// This avoids complex schema migrations if the Grid struct changes.
	createTableSQL := `
	CREATE TABLE IF NOT EXISTS grid_state (
		id TEXT PRIMARY KEY,
		state_json TEXT NOT NULL,
		updated_at INTEGER NOT NULL
	);`

	if _, err := db.Exec(createTableSQL); err != nil {
		return nil, fmt.Errorf("failed to create grid_state table: %w", err)
	}

	// Create the table for the ID generator state.
	createIDTableSQL := `
	CREATE TABLE IF NOT EXISTS id_generator_state (
		id TEXT PRIMARY KEY,
		last_id INTEGER NOT NULL
	);`
	if _, err := db.Exec(createIDTableSQL); err != nil {
		return nil, fmt.Errorf("failed to create id_generator_state table: %w", err)
	}

	return &SQLiteStorage{db: db}, nil
}

// SaveGrid serializes the entire Grid object to JSON and saves it to the database.
// It uses a fixed ID "current_grid" to always overwrite the latest state.
func (s *SQLiteStorage) SaveGrid(grid *models.Grid) error {
	stateJSON, err := json.Marshal(grid)
	if err != nil {
		return fmt.Errorf("failed to marshal grid state to JSON: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	// Use "INSERT OR REPLACE" (UPSERT) to either create the row or update it if it exists.
	stmt, err := tx.Prepare("INSERT OR REPLACE INTO grid_state (id, state_json, updated_at) VALUES (?, ?, ?)")
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to prepare statement: %w", err)
	}
	defer stmt.Close()

	_, err = stmt.Exec("current_grid", string(stateJSON), time.Now().Unix())
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to execute insert/replace: %w", err)
	}

	return tx.Commit()
}

// LoadGrid retrieves the JSON blob from the database and deserializes it into a Grid object.
func (s *SQLiteStorage) LoadGrid() (*models.Grid, error) {
	row := s.db.QueryRow("SELECT state_json FROM grid_state WHERE id = 'current_grid'")

	var stateJSON string
	err := row.Scan(&stateJSON)
	if err != nil {
		if err == sql.ErrNoRows {
			// This is not an error, it just means there's no saved state yet.
			return nil, nil
		}
		return nil, fmt.Errorf("failed to scan grid state from database: %w", err)
	}

	var grid models.Grid
	if err := json.Unmarshal([]byte(stateJSON), &grid); err != nil {
		return nil, fmt.Errorf("failed to unmarshal grid state from JSON: %w", err)
	}

	return &grid, nil
}

// Close closes the database connection.
func (s *SQLiteStorage) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// SaveIDGeneratorState saves the last generated ID to the database.
func (s *SQLiteStorage) SaveIDGeneratorState(lastID uint64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction for id generator: %w", err)
	}

	stmt, err := tx.Prepare("INSERT OR REPLACE INTO id_generator_state (id, last_id) VALUES (?, ?)")
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to prepare statement for id generator: %w", err)
	}
	defer stmt.Close()

	_, err = stmt.Exec("singleton", lastID)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to execute insert/replace for id generator: %w", err)
	}

	return tx.Commit()
}

// LoadIDGeneratorState retrieves the last saved ID from the database.
func (s *SQLiteStorage) LoadIDGeneratorState() (uint64, error) {
	row := s.db.QueryRow("SELECT last_id FROM id_generator_state WHERE id = 'singleton'")

	var lastID uint64
	err := row.Scan(&lastID)
	if err != nil {
		if err == sql.ErrNoRows {
			// No state saved yet, return 0 as the initial value.
			return 0, nil
		}
		return 0, fmt.Errorf("failed to scan id generator state from database: %w", err)
	}

	return lastID, nil
}
