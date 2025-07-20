package bot

import (
	"binance-grid-bot-go/internal/exchange"
	"binance-grid-bot-go/internal/idgenerator"
	"binance-grid-bot-go/internal/logger"
	"binance-grid-bot-go/internal/models"
	"binance-grid-bot-go/internal/storage"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// EventType defines the type of a normalized event
type EventType int

const (
	OrderUpdateEvent EventType = iota
	// Add other event types here in the future, e.g., PriceTickEvent
)

// NormalizedEvent is a standardized internal representation of an event from any source
type NormalizedEvent struct {
	Type      EventType
	Timestamp time.Time
	Data      interface{} // Can be models.OrderUpdateEvent or other event data structs
}

// GridTradingBot is the core struct for the grid trading bot
type GridTradingBot struct {
	config                  *models.Config
	exchange                exchange.Exchange
	wsConn                  *websocket.Conn
	listenKey               string
	grid                    *models.Grid // Core data structure for grid state
	currentPrice            float64
	isRunning               bool
	IsBacktest              bool
	currentTime             time.Time
	basePositionEstablished bool
	isReentering            bool
	reentrySignal           chan bool
	mutex                   sync.RWMutex
	stopChannel             chan bool
	stopOnce                sync.Once
	eventChannel            chan NormalizedEvent // The central event queue
	symbolInfo              *models.SymbolInfo
	isHalted                bool
	safeModeReason          string
	idGenerator             *idgenerator.IDGenerator
	storage                 storage.Storage
	logger                  *zap.Logger
}

// NewGridTradingBot creates a new instance of the grid trading bot
func NewGridTradingBot(config *models.Config, ex exchange.Exchange, isBacktest bool) *GridTradingBot {
	bot := &GridTradingBot{
		config:                  config,
		exchange:                ex,
		grid:                    &models.Grid{Config: config}, // Initialize the grid
		isRunning:               false,
		IsBacktest:              isBacktest,
		basePositionEstablished: false,
		stopChannel:             make(chan bool),
		eventChannel:            make(chan NormalizedEvent, 1024), // Buffered channel
		reentrySignal:           make(chan bool, 1),
		isHalted:                false,
		logger:                  logger.L(),
	}

	symbolInfo, err := ex.GetSymbolInfo(config.Symbol)
	if err != nil {
		logger.S().Fatalf("Could not get symbol info for %s: %v", config.Symbol, err)
	}
	bot.symbolInfo = symbolInfo
	logger.S().Infof("Successfully fetched and cached trading rules for %s.", config.Symbol)

	storage, err := storage.NewSQLiteStorage("grid_state.db")
	if err != nil {
		logger.S().Fatalf("Could not create storage: %v", err)
	}
	bot.storage = storage
	logger.S().Info("Successfully initialized SQLite storage.")

	idGen, err := idgenerator.NewIDGenerator(bot.storage)
	if err != nil {
		logger.S().Fatalf("Could not create persistent ID generator: %v", err)
	}
	bot.idGenerator = idGen

	return bot
}

// establishBasePositionAndWait tries to establish the initial base position and waits for it to be filled
func (b *GridTradingBot) establishBasePositionAndWait(quantity float64) (float64, error) {
	clientOrderID, err := b.generateClientOrderID()
	if err != nil {
		return 0, fmt.Errorf("could not generate ID for initial order: %v", err)
	}
	order, err := b.exchange.PlaceOrder(b.config.Symbol, "BUY", "MARKET", quantity, 0, clientOrderID)
	if err != nil {
		return 0, fmt.Errorf("initial market buy failed: %v", err)
	}
	logger.S().Infof("Submitted initial market buy order ID: %d, Quantity: %.5f. Waiting for fill...", order.OrderId, quantity)

	// In backtesting, market orders are assumed to fill instantly. We check status once.
	// This simplified logic removes the ticker polling that was a source of deadlocks.
	time.Sleep(10 * time.Millisecond) // Brief sleep just in case of minor simulated exchange latency.

	status, err := b.exchange.GetOrderStatus(b.config.Symbol, order.OrderId)
	if err != nil {
		// In backtesting, if GetOrderStatus can't find the order, we assume it filled and was archived.
		if b.IsBacktest && strings.Contains(err.Error(), "not found") {
			logger.S().Infof("Initial order %d status check returned 'not found', assuming filled in backtest mode.", order.OrderId)
			b.mutex.Lock()
			b.basePositionEstablished = true
			b.mutex.Unlock()
			return b.currentPrice, nil
		}
		return 0, fmt.Errorf("failed to get status for initial order %d: %v", order.OrderId, err)
	}

	if status.Status == "FILLED" {
		logger.S().Infof("Initial position order %d has been filled!", order.OrderId)
		b.mutex.Lock()
		b.basePositionEstablished = true
		b.mutex.Unlock()

		trade, err := b.exchange.GetLastTrade(b.config.Symbol, order.OrderId)
		if err != nil {
			return 0, fmt.Errorf("could not get trade for initial order %d: %v", order.OrderId, err)
		}
		filledPrice, err := strconv.ParseFloat(trade.Price, 64)
		if err != nil {
			return 0, fmt.Errorf("could not parse fill price for initial order %d: %v", order.OrderId, err)
		}
		return filledPrice, nil
	}

	return 0, fmt.Errorf("initial position order %d did not fill immediately. Status: %s", order.OrderId, status.Status)
}

// enterMarketAndSetupGrid implements the logic for entering the market and setting up the grid
func (b *GridTradingBot) enterMarketAndSetupGrid() error {
	logger.S().Info("--- Starting new trading cycle ---")

	currentPrice, err := b.exchange.GetPrice(b.config.Symbol)
	if err != nil {
		return fmt.Errorf("failed to get current price: %v", err)
	}

	b.mutex.Lock()
	b.currentPrice = currentPrice
	b.grid.EntryPrice = currentPrice
	b.grid.ReversionPrice = b.grid.EntryPrice * (1 + b.config.ReturnRate)
	b.grid.ConceptualGrid = make([]float64, 0)
	b.grid.GridLevels = make([]models.Level, 0)
	b.isReentering = false
	b.mutex.Unlock()

	logger.S().Infof("New cycle defined: Entry Price: %.4f, Reversion Price (Grid Top): %.4f", b.grid.EntryPrice, b.grid.ReversionPrice)

	b.mutex.Lock()
	var tickSize string
	for _, f := range b.symbolInfo.Filters {
		if f.FilterType == "PRICE_FILTER" {
			tickSize = f.TickSize
		}
	}

	conceptualLevels := make([]float64, 0)
	price := b.grid.ReversionPrice
	for price > (b.grid.EntryPrice * 0.5) { // Define a reasonable floor for the grid
		adjustedPrice := adjustValueToStep(price, tickSize)
		if len(conceptualLevels) == 0 || conceptualLevels[len(conceptualLevels)-1] != adjustedPrice {
			conceptualLevels = append(conceptualLevels, adjustedPrice)
		}
		price *= 1 - b.config.GridSpacing
	}

	b.grid.ConceptualGrid = conceptualLevels
	b.mutex.Unlock()

	if len(b.grid.ConceptualGrid) == 0 {
		logger.S().Warn("Conceptual grid is empty, likely due to misconfiguration of return rate or grid spacing. Skipping position and orders.")
		b.mutex.Lock()
		b.basePositionEstablished = true
		b.mutex.Unlock()
		return nil
	}
	logger.S().Infof("Successfully generated conceptual grid with %d levels.", len(b.grid.ConceptualGrid))

	sellGridCount := 0
	for _, price := range b.grid.ConceptualGrid {
		if price > b.grid.EntryPrice {
			sellGridCount++
		}
	}
	singleGridQuantity, err := b.calculateQuantity(b.grid.EntryPrice)
	if err != nil {
		return fmt.Errorf("could not determine grid quantity for initial position: %v", err)
	}

	initialPositionQuantity := float64(sellGridCount) * singleGridQuantity
	logger.S().Infof("Calculated initial position quantity: %.8f", initialPositionQuantity)

	if !b.isWithinExposureLimit(initialPositionQuantity) {
		logger.S().Warnf("Initial position blocked: wallet exposure limit would be exceeded.")
		b.mutex.Lock()
		b.basePositionEstablished = true
		b.mutex.Unlock()
	} else {
		filledPrice, err := b.establishBasePositionAndWait(initialPositionQuantity)
		if err != nil {
			return fmt.Errorf("failed to establish initial position, cannot continue: %v", err)
		}
		b.grid.EntryPrice = filledPrice
	}

	b.mutex.RLock()
	isEstablished := b.basePositionEstablished
	b.mutex.RUnlock()

	if isEstablished {
		logger.S().Info("Initial position confirmed, setting up grid orders...")
		err := b.setupInitialGrid(b.grid.EntryPrice)
		if err != nil {
			return fmt.Errorf("initial grid setup failed: %v", err)
		}
		logger.S().Info("--- New cycle grid setup complete ---")
	} else {
		logger.S().Error("CRITICAL: Base position not marked as established, cannot place grid orders.")
	}

	return nil
}

// placeNewOrder is a helper function to place an order and return the result
func (b *GridTradingBot) placeNewOrder(side models.OrderSide, price float64, gridID int) (*models.Order, error) {
	var tickSize string
	for _, f := range b.symbolInfo.Filters {
		if f.FilterType == "PRICE_FILTER" {
			tickSize = f.TickSize
		}
	}

	adjustedPrice := adjustValueToStep(price, tickSize)
	quantity, err := b.calculateQuantity(adjustedPrice)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate order quantity at price %.4f: %v", adjustedPrice, err)
	}

	if side == models.Buy && !b.isWithinExposureLimit(quantity) {
		return nil, fmt.Errorf("order blocked: wallet exposure limit would be exceeded")
	}

	clientOrderID, err := b.generateClientOrderID()
	if err != nil {
		return nil, fmt.Errorf("could not generate client order ID for grid order (GridID: %d): %v", gridID, err)
	}

	order, err := b.exchange.PlaceOrder(b.config.Symbol, string(side), "LIMIT", quantity, adjustedPrice, clientOrderID)
	if err != nil {
		return nil, fmt.Errorf("failed to place %s order at price %.4f: %v", side, adjustedPrice, err)
	}

	logger.S().Infof("Submitted %s order: ClientID %s, Price %.4f, Quantity %.5f, GridID: %d. Waiting for confirmation...", side, clientOrderID, adjustedPrice, quantity, gridID)
	return order, nil
}

// placeAndManageOrder is a new helper function that encapsulates the full order placement and state management logic.
// It's designed to be called both synchronously and asynchronously (in a goroutine).
// It ensures atomicity by checking the level's state inside the lock.
func (b *GridTradingBot) placeAndManageOrder(side models.OrderSide, level *models.Level, wg *sync.WaitGroup) {
	if wg != nil {
		defer wg.Done()
	}
	b.mutex.Lock()
	// ATOMIC CHECK: Ensure the level is still idle before placing an order.
	if level.State != models.StateIdle {
		logger.S().Warnf("Aborting order placement for Level %d. Expected state Idle, but found %s.", level.GridID, level.State)
		b.mutex.Unlock()
		return
	}

	// Pre-write state to Placing
	level.State = models.StatePlacing
	level.Side = side
	level.UpdatedAt = time.Now().Unix()
	b.saveGridState()
	b.mutex.Unlock() // Unlock before the blocking network call

	order, err := b.placeNewOrder(side, level.Price, level.GridID)

	b.mutex.Lock() // Re-lock to update the final state
	defer b.mutex.Unlock()

	// Another check in case state changed while unlocked
	if level.State != models.StatePlacing {
		logger.S().Warnf("State for level %d changed to %s during order placement. Aborting final state update.", level.GridID, level.State)
		if err == nil {
			logger.S().Warnf("Cancelling potentially orphaned order %d for level %d", order.OrderId, level.GridID)
			cancelErr := b.exchange.CancelOrder(b.config.Symbol, order.OrderId)
			if cancelErr != nil {
				logger.S().Errorf("CRITICAL: Failed to cancel orphaned order %d: %v. Manual intervention may be required.", order.OrderId, cancelErr)
			}
		}
		return
	}

	if err != nil {
		logger.S().Errorf("Failed to place order for Level %d: %v. Rolling back state to Idle.", level.GridID, err)
		level.State = models.StateIdle // Rollback state
		level.Side = ""
		b.saveGridState()
		return
	}

	// Update state to Active with the confirmed OrderID
	level.State = models.StateActive
	level.OrderID = order.OrderId
	level.ClientOrderID = order.ClientOrderId
	level.UpdatedAt = time.Now().Unix()
	b.saveGridState()

	logger.S().Infof("Successfully confirmed %s order: ID %d, Price %.4f, GridID: %d", side, order.OrderId, level.Price, level.GridID)
}

// calculateQuantity calculates and validates the order quantity based on configuration and exchange rules
func (b *GridTradingBot) calculateQuantity(price float64) (float64, error) {
	var quantity float64
	var minNotional, minQty, stepSize string

	for _, f := range b.symbolInfo.Filters {
		switch f.FilterType {
		case "MIN_NOTIONAL":
			minNotional = f.MinNotional
		case "LOT_SIZE":
			minQty = f.MinQty
			stepSize = f.StepSize
		}
	}

	minNotionalValue, _ := strconv.ParseFloat(minNotional, 64)
	minQtyValue, _ := strconv.ParseFloat(minQty, 64)

	if b.config.GridQuantity > 0 {
		quantity = b.config.GridQuantity
	} else if b.config.GridValue > 0 {
		quantity = b.config.GridValue / price
	} else {
		return 0, fmt.Errorf("neither grid_quantity nor grid_value is configured")
	}

	if price*quantity < minNotionalValue {
		quantity = (minNotionalValue / price) * 1.01
	}

	if quantity < minQtyValue {
		quantity = minQtyValue
	}

	adjustedQuantity := adjustValueToStep(quantity, stepSize)

	if adjustedQuantity < minQtyValue {
		step, _ := strconv.ParseFloat(stepSize, 64)
		if step > 0 {
			adjustedQuantity += step
			adjustedQuantity = adjustValueToStep(adjustedQuantity, stepSize)
		}
	}

	if price*adjustedQuantity < minNotionalValue {
		step, _ := strconv.ParseFloat(stepSize, 64)
		if step > 0 {
			adjustedQuantity += step
			adjustedQuantity = adjustValueToStep(adjustedQuantity, stepSize)
		}
	}

	return adjustedQuantity, nil
}

// connectWebSocket establishes a connection to the WebSocket
func (b *GridTradingBot) connectWebSocket() error {
	if b.IsBacktest {
		logger.S().Info("Backtest mode, skipping WebSocket connection.")
		return nil
	}

	listenKey, err := b.exchange.CreateListenKey()
	if err != nil {
		return fmt.Errorf("could not create listen key: %v", err)
	}
	b.listenKey = listenKey
	logger.S().Infof("Successfully obtained Listen Key: %s", b.listenKey)

	conn, err := b.exchange.ConnectWebSocket(b.listenKey)
	if err != nil {
		return fmt.Errorf("could not connect to WebSocket: %v", err)
	}
	b.wsConn = conn
	logger.S().Info("Successfully connected to user data stream WebSocket.")

	// Setup Pong Handler
	pongTimeout := time.Duration(b.config.WebSocketPongTimeoutSec) * time.Second
	if pongTimeout == 0 {
		pongTimeout = 75 * time.Second // Default value
	}
	if err = b.wsConn.SetReadDeadline(time.Now().Add(pongTimeout)); err != nil {
		return err
	}

	b.wsConn.SetPongHandler(func(string) error {
		if err := b.wsConn.SetReadDeadline(time.Now().Add(pongTimeout)); err != nil {
			return err
		}
		return nil
	})

	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := b.exchange.KeepAliveListenKey(b.listenKey); err != nil {
					logger.S().Warnf("Failed to keep listen key alive: %v", err)
				} else {
					logger.S().Info("Successfully kept listen key alive.")
				}
			case <-b.stopChannel:
				return
			}
		}
	}()

	return nil
}

// webSocketLoop listens for messages from the WebSocket
func (b *GridTradingBot) webSocketLoop() {
	if b.IsBacktest || b.wsConn == nil {
		return
	}

	readChannel := make(chan []byte)
	errChannel := make(chan error)

	go func() {
		for {
			_, message, err := b.wsConn.ReadMessage()
			if err != nil {
				errChannel <- err
				return
			}
			// Reset read deadline on successful message read
			pongTimeout := time.Duration(b.config.WebSocketPongTimeoutSec) * time.Second
			if pongTimeout == 0 {
				pongTimeout = 75 * time.Second // Default value
			}
			b.wsConn.SetReadDeadline(time.Now().Add(pongTimeout))
			readChannel <- message
		}
	}()

	// Ping Ticker
	pingInterval := time.Duration(b.config.WebSocketPingIntervalSec) * time.Second
	if pingInterval == 0 {
		pingInterval = 30 * time.Second // Default value
	}
	pingTicker := time.NewTicker(pingInterval)
	defer pingTicker.Stop()

	logger.S().Info("WebSocket message listener loop started.")

	for {
		select {
		case message := <-readChannel:
			b.handleWebSocketMessage(message)
		case err := <-errChannel:
			logger.S().Errorf("WebSocket read error: %v. Reconnecting...", err)
			b.wsConn.Close()
			time.Sleep(5 * time.Second) // Wait before reconnecting
			if err := b.connectWebSocket(); err != nil {
				logger.S().Fatalf("Failed to reconnect WebSocket: %v", err)
			} else {
				go b.webSocketLoop() // Restart the loop
				return               // Exit the old loop
			}
		case <-pingTicker.C:
			if err := b.wsConn.WriteMessage(websocket.PingMessage, nil); err != nil {
				logger.S().Warnf("Failed to send ping: %v", err)
				// Don't necessarily need to reconnect on ping failure,
				// the read deadline will catch a dead connection.
			}
		case <-b.stopChannel:
			logger.S().Info("WebSocket listener stopping.")
			b.wsConn.Close()
			return
		}
	}
}

// handleWebSocketMessage parses the incoming message and dispatches it to the event channel
func (b *GridTradingBot) handleWebSocketMessage(message []byte) {
	var baseEvent struct {
		EventType string `json:"e"`
	}
	if err := json.Unmarshal(message, &baseEvent); err != nil {
		logger.S().Warnf("Could not unmarshal base event: %v", err)
		return
	}

	switch baseEvent.EventType {
	case "executionReport", "ORDER_TRADE_UPDATE":
		var orderUpdate models.OrderUpdateEvent
		if err := json.Unmarshal(message, &orderUpdate); err != nil {
			logger.S().Warnf("Could not unmarshal order update event: %v", err)
			return
		}
		// Push the parsed event onto the central channel
		b.eventChannel <- NormalizedEvent{
			Type:      OrderUpdateEvent,
			Timestamp: time.Now(),
			Data:      orderUpdate,
		}
	default:
		// logger.S().Debugf("Ignoring WebSocket event type: %s", baseEvent.EventType)
	}
}

// handleOrderUpdate is the new central logic for processing order updates from the event channel.
func (b *GridTradingBot) handleOrderUpdate(event models.OrderUpdateEvent) {
	b.mutex.Lock()

	var level *models.Level
	var levelFound bool

	// Find the corresponding level in our active grid
	for i := range b.grid.GridLevels {
		if b.grid.GridLevels[i].OrderID == event.Order.OrderID {
			level = &b.grid.GridLevels[i]
			levelFound = true
			break
		}
	}

	if !levelFound {
		// This can happen for orders not part of our grid (e.g., initial position)
		logger.S().Debugf("Received order update for order ID %d which is not in our active grid. Ignoring.", event.Order.OrderID)
		b.mutex.Unlock()
		return
	}

	logger.S().Infof("Processing update for GridID %d, OrderID %d. New Status: %s", level.GridID, level.OrderID, event.Order.ExecutionType)

	switch event.Order.ExecutionType {
	case "FILLED":
		level.State = models.StateFilled
		level.UpdatedAt = time.Now().Unix()
		filledPrice, err := strconv.ParseFloat(event.Order.Price, 64)
		if err != nil {
			logger.S().Errorf("Could not parse fill price '%s' for order %d. Using last known price.", event.Order.Price, event.Order.OrderID)
			filledPrice = b.grid.LastPrice // Fallback
		}
		b.grid.LastPrice = filledPrice
		logger.S().Infof("✅ GRID-EVENT: %s FILLED at %.4f. GridID: %d", level.Side, filledPrice, level.GridID)

		b.saveGridState()

		// This is the core logic trigger for the moving grid. A fill requires a full grid rebuild.
		b.mutex.Unlock() // IMPORTANT: Release lock before calling rebuild to prevent deadlock.
		go func() {
			if err := b.rebuildGrid(filledPrice); err != nil {
				logger.S().Errorf("CRITICAL: Grid rebuild failed after fill: %v", err)
				// The bot will enter safe mode inside rebuildGrid if it fails.
			}
		}()

	case "CANCELED":
		level.State = models.StateCancelled
		level.UpdatedAt = time.Now().Unix()
		logger.S().Infof("Order %d (GridID %d) confirmed as cancelled.", level.OrderID, level.GridID)
		b.saveGridState()
		b.mutex.Unlock()

	case "NEW":
		level.State = models.StateActive
		level.UpdatedAt = time.Now().Unix()
		logger.S().Infof("Order %d (GridID %d) confirmed as active.", level.OrderID, level.GridID)
		b.saveGridState()
		b.mutex.Unlock()

	case "REJECTED":
		level.State = models.StateError
		level.UpdatedAt = time.Now().Unix()
		logger.S().Errorf("Order %d (GridID %d) was REJECTED. Status: %s. Setting level to Error state.", level.OrderID, level.GridID, event.Order.Status)
		b.saveGridState()
		b.mutex.Unlock()

	default:
		// For statuses like "PARTIALLY_FILLED", "PENDING_CANCEL", etc., we just log and don't change state yet.
		// The final state (FILLED, CANCELED) is what matters for our logic.
		logger.S().Infof("Ignoring intermediate order status '%s' for order %d.", event.Order.ExecutionType, event.Order.OrderID)
		b.mutex.Unlock()
	}
}

// closeCurrentPosition is called when the reversion price is hit, to close the entire position.
func (b *GridTradingBot) closeCurrentPosition() error {
	logger.S().Warn("--- Reversion price hit! Closing current position. ---")
	if err := b.cancelAllActiveOrders(); err != nil {
		// Log the error but proceed to try and sell the position anyway
		logger.S().Errorf("Failed to cancel all orders during position close: %v", err)
	}

	// Wait a moment for cancellations to process
	time.Sleep(2 * time.Second)

	b.mutex.Lock()
	defer b.mutex.Unlock()

	// Calculate total held assets (base currency)
	// This is a simplified calculation. A more robust system would track this precisely.
	totalQuantity, err := b.calculateTotalAssetQuantity()
	if err != nil {
		return fmt.Errorf("could not calculate total asset quantity for closing position: %v", err)
	}

	if totalQuantity > 0 {
		logger.S().Infof("Attempting to sell remaining %.8f of %s", totalQuantity, b.config.Symbol)
		clientOrderID, err := b.generateClientOrderID()
		if err != nil {
			return fmt.Errorf("could not generate ID for closing order: %v", err)
		}
		_, err = b.exchange.PlaceOrder(b.config.Symbol, "SELL", "MARKET", totalQuantity, 0, clientOrderID)
		if err != nil {
			return fmt.Errorf("market sell to close position failed: %v", err)
		}
		logger.S().Info("Market sell order submitted to close position.")
	} else {
		logger.S().Info("No assets to sell, position already closed.")
	}

	// Mark the cycle as complete, ready for re-entry
	b.basePositionEstablished = false
	b.isReentering = true
	b.grid = &models.Grid{Config: b.config} // Reset the grid
	b.saveGridState()

	logger.S().Info("--- Position closed. Bot is now in re-entry mode. ---")
	return nil
}

// The final 'Cancelled' state is confirmed by the websocket event handler.
func (b *GridTradingBot) cancelAllActiveOrders() error {
	b.mutex.Lock()
	logger.S().Info("Attempting to cancel all active orders...")

	var levelsToCancel []*models.Level
	for i := range b.grid.GridLevels {
		if b.grid.GridLevels[i].State == models.StateActive || b.grid.GridLevels[i].State == models.StatePlacing {
			levelsToCancel = append(levelsToCancel, &b.grid.GridLevels[i])
		}
	}

	if len(levelsToCancel) == 0 {
		logger.S().Info("No active orders found to cancel.")
		b.mutex.Unlock()
		return nil
	}

	logger.S().Infof("Found %d orders to cancel.", len(levelsToCancel))
	b.mutex.Unlock() // Unlock before making network calls

	var wg sync.WaitGroup
	for _, level := range levelsToCancel {
		wg.Add(1)
		go func(l *models.Level) {
			defer wg.Done()
			logger.S().Infof("Cancelling order %d for Level %d...", l.OrderID, l.GridID)
			err := b.exchange.CancelOrder(b.config.Symbol, l.OrderID)

			b.mutex.Lock()
			defer b.mutex.Unlock()

			if err != nil {
				// If cancellation fails, log it and set the state to Error.
				// This requires manual intervention.
				logger.S().Errorf("Failed to cancel order %d for Level %d: %v. Setting state to Error.", l.OrderID, l.GridID, err)
				l.State = models.StateError
			} else {
				// The state is set to Cancelling. The final state (Cancelled) will be set
				// by the order update event from the websocket.
				l.State = models.StateCancelling
				logger.S().Infof("Cancellation request for order %d (Level %d) sent successfully.", l.OrderID, l.GridID)
			}
		}(level)
	}

	wg.Wait() // Wait for all cancellation requests to be sent
	logger.S().Info("All cancellation requests have been sent.")

	b.mutex.Lock()
	b.saveGridState()
	b.mutex.Unlock()
	return nil
}

// rebuildGrid is the core logic for the "moving grid". It's triggered after a fill.
// It cancels all orders, determines a new center price, and sets up a new grid.
// For now, it re-uses the existing conceptual grid definition.
func (b *GridTradingBot) rebuildGrid(pivotPrice float64) error {
	logger.S().Info("--- Starting grid rebuild process ---")

	// Step 1: Cancel all currently active orders.
	logger.S().Info("Step 1/3: Cancelling all active orders...")
	if err := b.cancelAllActiveOrders(); err != nil {
		reason := fmt.Sprintf("failed to cancel all orders during rebuild: %v", err)
		b.enterSafeMode(reason)
		return errors.New(reason)
	}
	logger.S().Info("All active orders have been requested for cancellation. Waiting for confirmation via websocket...")
	// In a real scenario, we'd wait for all CANCELED events. For simplicity here, we'll just sleep.
	time.Sleep(2 * time.Second)

	b.mutex.Lock()

	// Step 2: Determine the new center price for the grid
	logger.S().Info("Step 2/3: Using fill price as new center price...")
	newCenterPrice := pivotPrice
	logger.S().Infof("New center price will be the pivot fill price: %.4f", newCenterPrice)

	// Step 3: Reset grid and place new orders
	logger.S().Info("Step 3/3: Resetting grid and placing new orders...")
	// The old grid levels are now all cancelled or cancelling. We can clear them.
	b.grid.GridLevels = []models.Level{}
	b.grid.LastPrice = newCenterPrice // Update last price

	// IMPORTANT: Release the lock before calling setupInitialGrid to prevent deadlock,
	// as setupInitialGrid will acquire its own lock.
	b.mutex.Unlock()
	err := b.setupInitialGrid(newCenterPrice)
	// No need to re-acquire the lock as the function is ending.

	if err != nil {
		reason := fmt.Sprintf("failed to setup new grid during rebuild: %v", err)
		b.enterSafeMode(reason)
		return errors.New(reason)
	}

	logger.S().Info("--- Grid rebuild process finished ---")
	return nil
}

// setupInitialGrid creates and places the initial set of orders based on the conceptual grid.
// In this new version, it creates the `Level` objects from the `ConceptualGrid` prices
// and places both BUY and SELL orders around the given entry/center price.
func (b *GridTradingBot) setupInitialGrid(centerPrice float64) error {
	logger.S().Info("--- Setting up grid orders around new center price ---")

	b.mutex.Lock()

	// Ensure GridLevels is empty before setup. This is crucial for rebuilds.
	b.grid.GridLevels = []models.Level{}

	// Step 1: Create Level objects from the conceptual grid prices.
	for i, price := range b.grid.ConceptualGrid {
		level := models.Level{
			GridID:    i, // Use index as a simple unique ID for the level
			Price:     price,
			State:     models.StateIdle,
			UpdatedAt: time.Now().Unix(),
		}
		b.grid.GridLevels = append(b.grid.GridLevels, level)
	}

	// Step 2: Identify which orders to place initially based on the new algorithm.
	var wg sync.WaitGroup
	var firstBuyLevelFound bool

	// Unlock before spawning goroutines that will make blocking network calls.
	// The goroutines themselves will handle locking for state changes.
	b.mutex.Unlock()

	// Place all sell orders for levels above the center price.
	for i := range b.grid.GridLevels {
		// We need to pass a copy of the level pointer to the goroutine
		// to avoid race conditions on the loop variable.
		level := &b.grid.GridLevels[i]
		if level.Price > centerPrice {
			wg.Add(1)
			go b.placeAndManageOrder(models.Sell, level, &wg)
		}
	}

	// Place only the first buy order immediately below the center price.
	// We iterate from the top (highest price) downwards to find the first suitable level.
	for i := 0; i < len(b.grid.GridLevels); i++ {
		level := &b.grid.GridLevels[i]
		if level.Price < centerPrice && !firstBuyLevelFound {
			wg.Add(1)
			go b.placeAndManageOrder(models.Buy, level, &wg)
			firstBuyLevelFound = true
			break // Found the one and only buy order to place, so we can stop.
		}
	}

	if !firstBuyLevelFound {
		logger.S().Warn("No suitable buy level found below the center price to place an initial order.")
	}

	logger.S().Info("Waiting for initial grid orders to be placed...")
	wg.Wait() // Wait for all the spawned goroutines to complete.

	b.mutex.Lock()
	defer b.mutex.Unlock()

	b.saveGridState() // Save the newly populated grid state
	logger.S().Info("Grid setup process finished.")
	return nil
}

// Run starts the main loop of the bot
func (b *GridTradingBot) Run() {
	logger.S().Info("Starting Grid Trading Bot...")
	b.isRunning = true

	if err := b.loadGridState(); err != nil {
		logger.S().Warnf("Could not load previous state: %v. Starting fresh.", err)
		if err := b.enterMarketAndSetupGrid(); err != nil {
			logger.S().Fatalf("Failed to perform initial market entry and grid setup: %v", err)
		}
	} else {
		logger.S().Info("Successfully loaded previous state. Reconciling with exchange...")
		if err := b.reconcileStateWithExchange(); err != nil {
			b.enterSafeMode(fmt.Sprintf("Failed to reconcile state with exchange: %v", err))
		}
	}

	if err := b.connectWebSocket(); err != nil {
		logger.S().Fatalf("Failed to connect to WebSocket: %v", err)
	}

	if !b.IsBacktest {
		go b.webSocketLoop()
	}

	// The main event processing loop
	for {
		select {
		case event := <-b.eventChannel:
			if b.isHalted {
				logger.S().Warnf("Bot is halted. Ignoring event type %d.", event.Type)
				continue
			}
			switch event.Type {
			case OrderUpdateEvent:
				if orderUpdate, ok := event.Data.(models.OrderUpdateEvent); ok {
					b.handleOrderUpdate(orderUpdate)
				}
			}
		case <-b.stopChannel:
			logger.S().Info("Bot shutting down.")
			b.isRunning = false
			return
		}
	}
}

// Stop gracefully stops the bot
func (b *GridTradingBot) Stop() {
	logger.S().Info("Stopping bot...")
	if b.isRunning {
		b.stopChannel <- true
		if b.listenKey != "" {
			b.exchange.CloseListenKey(b.listenKey)
		}
		if b.wsConn != nil {
			b.wsConn.Close()
		}
		b.saveGridState()
		b.storage.Close()
	}
}

// saveGridState saves the current grid state to the persistent storage
func (b *GridTradingBot) saveGridState() {
	// This function is called from within locked sections, so no need to lock here.
	if err := b.storage.SaveGrid(b.grid); err != nil {
		logger.S().Errorf("Failed to save grid state: %v", err)
	}
}

// loadGridState loads the grid state from the persistent storage
func (b *GridTradingBot) loadGridState() error {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	grid, err := b.storage.LoadGrid()
	if err != nil {
		return fmt.Errorf("could not load grid from storage: %v", err)
	}
	if grid == nil {
		return errors.New("no saved grid found")
	}

	b.grid = grid
	b.grid.Config = b.config // Re-link the config
	logger.S().Info("Successfully loaded grid state.")
	return nil
}

// reconcileStateWithExchange compares the bot's state with the actual orders on the exchange
func (b *GridTradingBot) reconcileStateWithExchange() error {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	logger.S().Info("--- Starting state reconciliation with exchange ---")

	openOrders, err := b.exchange.GetOpenOrders(b.config.Symbol)
	if err != nil {
		return fmt.Errorf("could not get open orders from exchange: %v", err)
	}

	exchangeOrders := make(map[int64]models.Order)
	for _, order := range openOrders {
		exchangeOrders[order.OrderId] = order
	}

	// We only reconcile levels that are supposed to be active
	for i := range b.grid.GridLevels {
		level := &b.grid.GridLevels[i]
		if level.State == models.StateActive || level.State == models.StatePlacing {
			if _, ok := exchangeOrders[level.OrderID]; ok {
				// Order exists on both sides. Check for inconsistencies.
				// For now, we assume the exchange is the source of truth.
				// A more complex reconciliation could handle price/qty mismatches.
				logger.S().Infof("Level %d (Order %d) is consistent with exchange.", level.GridID, level.OrderID)
				level.State = models.StateActive // Ensure state is active
				delete(exchangeOrders, level.OrderID)
			} else {
				// Order exists in our state but not on the exchange. It might have been filled or cancelled.
				logger.S().Warnf("Order %d for Level %d exists in state but not on exchange. Assuming filled/cancelled.", level.OrderID, level.GridID)
				level.State = models.StateFilled // A safe assumption to trigger rebuild or be ignored
			}
		}
	}

	// Any remaining orders in exchangeOrders are "orphaned" - they exist on the exchange but not in our state.
	if len(exchangeOrders) > 0 {
		logger.S().Warnf("Found %d orphaned orders on the exchange. Attempting to cancel them.", len(exchangeOrders))
		for _, order := range exchangeOrders {
			logger.S().Warnf("Cancelling orphaned order ID %d", order.OrderId)
			b.exchange.CancelOrder(b.config.Symbol, order.OrderId)
		}
	}

	b.saveGridState()
	logger.S().Info("--- Reconciliation finished ---")
	return nil
}

// enterSafeMode halts all trading activity due to a critical error
func (b *GridTradingBot) enterSafeMode(reason string) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	if b.isHalted {
		return
	}
	b.isHalted = true
	b.safeModeReason = reason
	logger.S().Errorf("CRITICAL ERROR: Entering safe mode. Reason: %s", reason)
	logger.S().Error("All trading activities are halted. Manual intervention is required.")
	// Optionally, cancel all orders when entering safe mode
	// b.cancelAllActiveOrders() // Be careful with locking if you enable this
}

// isWithinExposureLimit checks if adding a certain quantity would exceed the wallet exposure limit
func (b *GridTradingBot) isWithinExposureLimit(quantityToAdd float64) bool {
	if b.config.WalletExposureLimit <= 0 {
		return true // Limit is disabled
	}

	// This is a simplified calculation. A real implementation would need to
	// fetch the current balance of the base asset.
	// For now, we estimate the current exposure based on our grid.
	currentPosition, err := b.calculateTotalAssetQuantity()
	if err != nil {
		logger.S().Errorf("Could not calculate current position for exposure check: %v", err)
		return false // Fail safe
	}

	return (currentPosition + quantityToAdd) <= b.config.WalletExposureLimit
}

// calculateTotalAssetQuantity estimates the total quantity of the base asset held.
// This is a simplified estimation based on the number of filled buy vs sell orders.
func (b *GridTradingBot) calculateTotalAssetQuantity() (float64, error) {
	// This function is called from within locked sections.

	// Start with the initial position
	sellGridCount := 0
	for _, price := range b.grid.ConceptualGrid {
		if price > b.grid.EntryPrice {
			sellGridCount++
		}
	}
	singleGridQuantity, err := b.calculateQuantity(b.grid.EntryPrice)
	if err != nil {
		return 0, err
	}
	totalQuantity := float64(sellGridCount) * singleGridQuantity

	// Adjust based on filled grid orders
	for _, level := range b.grid.GridLevels {
		if level.State == models.StateFilled {
			if level.Side == models.Buy {
				totalQuantity += singleGridQuantity
			} else if level.Side == models.Sell {
				totalQuantity -= singleGridQuantity
			}
		}
	}

	return totalQuantity, nil
}

// generateClientOrderID creates a new unique client order ID
func (b *GridTradingBot) generateClientOrderID() (string, error) {
	// The new Generate method returns a formatted string directly.
	id, err := b.idGenerator.Generate()
	if err != nil {
		return "", err
	}
	return id, nil
}

// adjustValueToStep adjusts a value to match the required step size for the exchange
func adjustValueToStep(value float64, step string) float64 {
	stepFloat, err := strconv.ParseFloat(step, 64)
	if err != nil || stepFloat == 0 {
		return value // Cannot adjust if step is invalid
	}
	return math.Floor(value/stepFloat) * stepFloat
}

// BacktestTick simulates a single tick of time in backtesting mode
func (b *GridTradingBot) BacktestTick(price float64, timestamp time.Time) {
	if !b.IsBacktest {
		return
	}

	b.mutex.Lock()
	b.currentPrice = price
	b.currentTime = timestamp
	b.mutex.Unlock()

	// Simulate order fills
	b.checkPriceCrossings(price)
}

// checkPriceCrossings simulates order fills by checking if the current price has crossed any grid levels.
func (b *GridTradingBot) checkPriceCrossings(currentPrice float64) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	for i := range b.grid.GridLevels {
		level := &b.grid.GridLevels[i]
		if level.State != models.StateActive {
			continue
		}

		crossed := false
		if level.Side == models.Buy && currentPrice <= level.Price {
			crossed = true
		} else if level.Side == models.Sell && currentPrice >= level.Price {
			crossed = true
		}

		if crossed {
			logger.S().Infof("[BACKTEST] Price of %.4f crossed level %d (%s @ %.4f). Simulating fill.", currentPrice, level.GridID, level.Side, level.Price)
			// Simulate a fill event and push it to the channel
			// In a real backtest, you'd generate a more complete event object.
			orderUpdate := models.OrderUpdateEvent{
				EventType: "executionReport",
				Order: models.OrderUpdateInfo{
					ExecutionType: "FILLED",
					OrderID:       level.OrderID,
					ClientOrderID: level.ClientOrderID,
					Symbol:        b.config.Symbol,
					Side:          string(level.Side),
					Price:         strconv.FormatFloat(level.Price, 'f', -1, 64),
					Status:        "FILLED",
				},
			}
			// Use a goroutine to avoid deadlock on the event channel if it's full
			go func() {
				b.eventChannel <- NormalizedEvent{
					Type:      OrderUpdateEvent,
					Timestamp: b.currentTime,
					Data:      orderUpdate,
				}
			}()
		}
	}
}

// Start 在一个新的 goroutine 中启动机器人。
func (b *GridTradingBot) Start() error {
	b.logger.Info("启动实时交易机器人...")
	go b.Run()
	return nil
}

// StartForBacktest 在当前 goroutine 中启动机器人，用于回测。
func (b *GridTradingBot) StartForBacktest() error {
	b.logger.Info("启动回测机器人...")
	// 在回测模式下，我们直接在当前 goroutine 运行，以便按顺序处理历史数据
	b.Run()
	return nil
}

// IsHalted 返回机器人是否已暂停。
func (b *GridTradingBot) IsHalted() bool {
	b.mutex.RLock()
	defer b.mutex.RUnlock()
	return b.isHalted
}
