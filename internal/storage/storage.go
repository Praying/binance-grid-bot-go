package storage

import (
	"binance-grid-bot-go/internal/models"
	"database/sql"
	"fmt"
	"strconv"

	_ "github.com/mattn/go-sqlite3" // Import the sqlite3 driver
)

// InitDB initializes the database connection and creates necessary tables.
func InitDB(dataSourceName string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dataSourceName)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	if err = db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	if err = createTables(db); err != nil {
		return nil, fmt.Errorf("failed to create tables: %w", err)
	}

	return db, nil
}

// createTables creates the necessary database tables if they don't exist.
func createTables(db *sql.DB) error {
	// Orders table to store information about every order placed by the bot.
	// This table is crucial for the recovery process.
	createOrdersTableSQL := `
	CREATE TABLE IF NOT EXISTS orders (
		client_order_id TEXT PRIMARY KEY,
		exchange_order_id INTEGER,
		symbol TEXT NOT NULL,
		side TEXT NOT NULL,
		type TEXT NOT NULL,
		price REAL NOT NULL,
		quantity REAL NOT NULL,
		status TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);`

	if _, err := db.Exec(createOrdersTableSQL); err != nil {
		return err
	}

	// BotState table to store the overall state of the bot's current cycle.
	// This table will only ever have one row.
	createBotStateTableSQL := `
	CREATE TABLE IF NOT EXISTS bot_state (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		current_cycle_id INTEGER NOT NULL,
		status TEXT NOT NULL,
		entry_price REAL NOT NULL,
		reversion_price REAL NOT NULL,
		grid_levels TEXT,
		conceptual_grid TEXT,
		base_position_snapshot BOOLEAN,
		last_update_time DATETIME NOT NULL
	);`

	if _, err := db.Exec(createBotStateTableSQL); err != nil {
		return err
	}

	// BotMetadata table to store simple key-value metadata.
	createBotMetadataTableSQL := `
	CREATE TABLE IF NOT EXISTS bot_metadata (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);`
	if _, err := db.Exec(createBotMetadataTableSQL); err != nil {
		return err
	}

	// Initialize the cycle counter if it doesn't exist.
	initCycleCounterSQL := `INSERT OR IGNORE INTO bot_metadata (key, value) VALUES ('cycle_counter', '0');`
	if _, err := db.Exec(initCycleCounterSQL); err != nil {
		return err
	}

	return nil
}

// GetActiveOrders retrieves all orders that are not in a final state (e.g., FILLED, CANCELED).
func GetActiveOrders(db *sql.DB, symbol string) ([]models.Order, error) {
	query := `
	SELECT client_order_id, exchange_order_id, symbol, side, type, price, quantity, status, created_at, updated_at
	FROM orders
	WHERE symbol = ? AND status NOT IN ('FILLED', 'CANCELED', 'EXPIRED', 'REJECTED')`

	rows, err := db.Query(query, symbol)
	if err != nil {
		return nil, fmt.Errorf("failed to query active orders: %w", err)
	}
	defer rows.Close()

	var orders []models.Order
	for rows.Next() {
		var order models.Order
		var price, quantity float64
		if err := rows.Scan(
			&order.ClientOrderId, &order.OrderId, &order.Symbol, &order.Side, &order.Type,
			&price, &quantity, &order.Status, &order.Time, &order.UpdateTime,
		); err != nil {
			return nil, fmt.Errorf("failed to scan order row: %w", err)
		}
		order.Price = fmt.Sprintf("%f", price)
		order.OrigQty = fmt.Sprintf("%f", quantity)
		orders = append(orders, order)
	}
	return orders, nil
}

// UpdateOrder updates an existing order in the database.
func UpdateOrder(db *sql.DB, order *models.Order) error {
	query := `
	UPDATE orders
	SET exchange_order_id = ?, status = ?, updated_at = ?
	WHERE client_order_id = ?`

	_, err := db.Exec(query, order.OrderId, order.Status, order.UpdateTime, order.ClientOrderId)
	if err != nil {
		return fmt.Errorf("failed to update order %s: %w", order.ClientOrderId, err)
	}
	return nil
}

// CreateOrder inserts a new order into the database.
func CreateOrder(db *sql.DB, order *models.Order) error {
	price, _ := strconv.ParseFloat(order.Price, 64)
	quantity, _ := strconv.ParseFloat(order.OrigQty, 64)

	query := `
	INSERT INTO orders (client_order_id, exchange_order_id, symbol, side, type, price, quantity, status, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	_, err := db.Exec(query,
		order.ClientOrderId, order.OrderId, order.Symbol, order.Side, order.Type,
		price, quantity, order.Status, order.Time, order.UpdateTime,
	)
	if err != nil {
		return fmt.Errorf("failed to insert order %s: %w", order.ClientOrderId, err)
	}
	return nil
}

// SaveBotState creates or updates the single row in the bot_state table.
func SaveBotState(db *sql.DB, state *models.BotState) error {
	query := `
	INSERT INTO bot_state (id, current_cycle_id, status, entry_price, reversion_price, grid_levels, conceptual_grid, base_position_snapshot, last_update_time)
	VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		current_cycle_id = excluded.current_cycle_id,
		status = excluded.status,
		entry_price = excluded.entry_price,
		reversion_price = excluded.reversion_price,
		grid_levels = excluded.grid_levels,
		conceptual_grid = excluded.conceptual_grid,
		base_position_snapshot = excluded.base_position_snapshot,
		last_update_time = excluded.last_update_time;`

	_, err := db.Exec(query,
		state.CurrentCycleID,
		state.Status,
		state.EntryPrice,
		state.ReversionPrice,
		state.GridLevels,
		state.ConceptualGrid,
		state.BasePositionSnapshot,
		state.LastUpdateTime,
	)

	if err != nil {
		return fmt.Errorf("failed to save bot state: %w", err)
	}
	return nil
}

// LoadBotState retrieves the bot's state from the database.
// It will return sql.ErrNoRows if no state is found.
func LoadBotState(db *sql.DB) (*models.BotState, error) {
	query := `
	SELECT id, current_cycle_id, status, entry_price, reversion_price, grid_levels, conceptual_grid, base_position_snapshot, last_update_time
	FROM bot_state WHERE id = 1;`

	row := db.QueryRow(query)

	var state models.BotState
	err := row.Scan(
		&state.ID,
		&state.CurrentCycleID,
		&state.Status,
		&state.EntryPrice,
		&state.ReversionPrice,
		&state.GridLevels,
		&state.ConceptualGrid,
		&state.BasePositionSnapshot,
		&state.LastUpdateTime,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // Return nil, nil if no state is found, this is not an application error.
		}
		return nil, err
	}

	return &state, nil
}

// ClearBotState removes the state from the database, typically after a cycle is complete.
func ClearBotState(db *sql.DB) error {
	query := "DELETE FROM bot_state WHERE id = 1;"
	_, err := db.Exec(query)
	if err != nil {
		return fmt.Errorf("failed to clear bot state: %w", err)
	}
	return nil
}

// GetNextCycleID atomically retrieves and increments the cycle counter from the database.
func GetNextCycleID(db *sql.DB) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction for cycle ID: %w", err)
	}
	defer tx.Rollback() // Rollback on any error

	var counterStr string
	err = tx.QueryRow("SELECT value FROM bot_metadata WHERE key = 'cycle_counter'").Scan(&counterStr)
	if err != nil {
		return 0, fmt.Errorf("failed to read cycle_counter: %w", err)
	}

	counter, err := strconv.ParseInt(counterStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse cycle_counter value '%s': %w", counterStr, err)
	}

	nextCounter := counter + 1

	_, err = tx.Exec("UPDATE bot_metadata SET value = ? WHERE key = 'cycle_counter'", strconv.FormatInt(nextCounter, 10))
	if err != nil {
		return 0, fmt.Errorf("failed to update cycle_counter: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit cycle_counter transaction: %w", err)
	}

	return nextCounter, nil
}
