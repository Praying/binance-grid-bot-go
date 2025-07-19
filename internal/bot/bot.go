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
	eventChannel            chan NormalizedEvent // The central event queue
	symbolInfo              *models.SymbolInfo
	isHalted                bool
	safeModeReason          string
	idGenerator             *idgenerator.IDGenerator
	storage                 storage.Storage
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

	// 在回测中，市价单被认为是立即成交的。我们直接检查一次状态即可。
	// 这种简化的逻辑消除了ticker轮询，这是之前死锁的根源。
	time.Sleep(10 * time.Millisecond) // 短暂休眠，以防万一模拟交易所有微小的延迟。

	status, err := b.exchange.GetOrderStatus(b.config.Symbol, order.OrderId)
	if err != nil {
		// 在回测中，如果GetOrderStatus找不到订单，我们假设它已经成交并被归档。
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
	b.grid.ConceptualGrid = make([]models.Level, 0)
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
	for price > (b.grid.EntryPrice * 0.5) {
		adjustedPrice := adjustValueToStep(price, tickSize)
		if len(conceptualLevels) == 0 || conceptualLevels[len(conceptualLevels)-1] != adjustedPrice {
			conceptualLevels = append(conceptualLevels, adjustedPrice)
		}
		price *= 1 - b.config.GridSpacing
	}

	for i, p := range conceptualLevels {
		b.grid.ConceptualGrid = append(b.grid.ConceptualGrid, models.Level{
			GridID: i,
			Price:  p,
			State:  models.StateIdle,
		})
	}
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
	for _, level := range b.grid.ConceptualGrid {
		if level.Price > b.grid.EntryPrice {
			sellGridCount++
		}
	}
	// buyGridCount := len(b.grid.ConceptualGrid) - sellGridCount // Currently unused
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
	logger.S().Debugf("[DEBUG] Enter placeAndManageOrder for Level %d, Price %.4f", level.GridID, level.Price)
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
	logger.S().Debugf("[DEBUG] placeNewOrder returned for Level %d. Error: %v", level.GridID, err)

	b.mutex.Lock() // Re-lock to update the final state
	defer b.mutex.Unlock()

	// Another check in case state changed while unlocked (e.g. by a manual intervention or a different process)
	if level.State != models.StatePlacing {
		logger.S().Warnf("State for level %d changed to %s during order placement. Aborting final state update.", level.GridID, level.State)
		// If the order placement itself didn't fail, we might have an orphaned order that needs to be cancelled.
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
				// The read error handler will likely catch the disconnection.
			}
		case <-b.stopChannel:
			logger.S().Info("WebSocket listener loop stopped.")
			b.wsConn.Close()
			return
		}
	}
}

// handleWebSocketMessage parses and handles messages from the WebSocket
func (b *GridTradingBot) handleWebSocketMessage(message []byte) {
	var data map[string]interface{}
	if err := json.Unmarshal(message, &data); err != nil {
		logger.S().Warnf("Could not unmarshal WebSocket message into map: %v, Raw: %s", err, string(message))
		return
	}

	eventType, ok := data["e"].(string)
	if !ok {
		logger.S().Debugf("Received event with non-string or missing event type: %s", string(message))
		return
	}

	switch eventType {
	case "ORDER_TRADE_UPDATE":
		var orderUpdateEvent models.OrderUpdateEvent
		if err := json.Unmarshal(message, &orderUpdateEvent); err != nil {
			logger.S().Warnf("Could not unmarshal order trade update event: %v, Raw: %s", err, string(message))
			return
		}
		// Instead of handling it directly, push it to the event channel
		b.eventChannel <- NormalizedEvent{
			Type:      OrderUpdateEvent,
			Timestamp: time.Now(),
			Data:      orderUpdateEvent,
		}
	case "ACCOUNT_UPDATE":
		// Placeholder for handling account updates if needed in the future.
	case "TRADE_LITE":
		// This is a public trade event, not specific to our orders. We can safely ignore it.
	default:
		// Optionally log unknown event types for future analysis, but avoid spamming.
	}
}

// handleOrderUpdate is now called sequentially by the event processor.
func (b *GridTradingBot) handleOrderUpdate(event models.OrderUpdateEvent) {
	if event.Order.ExecutionType != "TRADE" || event.Order.Status != "FILLED" {
		return
	}

	o := event.Order
	logger.S().Infof("--- Processing Order Fill Event ---")
	logger.S().Infof("Order ID: %d, Symbol: %s, Side: %s, Price: %s, Quantity: %s, TradeID: %d",
		o.OrderID, o.Symbol, o.Side, o.Price, o.ExecutedQty, o.TradeID)

	b.mutex.Lock()
	defer b.mutex.Unlock()

	var triggeredLevel *models.Level
	for i := range b.grid.ConceptualGrid {
		if b.grid.ConceptualGrid[i].OrderID == o.OrderID {
			triggeredLevel = &b.grid.ConceptualGrid[i]
			break
		}
	}

	if triggeredLevel == nil {
		logger.S().Warnf("Received a fill for an unknown order ID: %d. Ignoring.", o.OrderID)
		return
	}

	tradeIDStr := strconv.FormatInt(o.TradeID, 10)

	// Idempotency Check: If we've already processed this trade, ignore.
	if triggeredLevel.LastTradeID == tradeIDStr {
		logger.S().Warnf("Received duplicate trade event for OrderID %d, TradeID %s. Ignoring.", o.OrderID, tradeIDStr)
		return
	}

	// State Transition Check: Only process fills for active orders.
	if triggeredLevel.State != models.StateActive {
		logger.S().Warnf("Received fill for order %d which is not in Active state (current: %s). Ignoring.", o.OrderID, triggeredLevel.State)
		return
	}

	logger.S().Infof(">>> Order Filled: %s at %.4f, GridID: %d <<<", triggeredLevel.Side, triggeredLevel.Price, triggeredLevel.GridID)

	filledQty, err := strconv.ParseFloat(o.ExecutedQty, 64)
	if err != nil {
		logger.S().Errorf("Could not parse last filled quantity for order %d: %v", o.OrderID, err)
		return
	}

	triggeredLevel.State = models.StateFilled
	triggeredLevel.FilledQuantity += filledQty
	triggeredLevel.LastTradeID = tradeIDStr
	triggeredLevel.UpdatedAt = time.Now().Unix()
	b.saveGridState()

	// --- State Machine Transition Logic ---
	var triggeredLevelIndex int = -1
	for i := range b.grid.ConceptualGrid {
		if &b.grid.ConceptualGrid[i] == triggeredLevel {
			triggeredLevelIndex = i
			break
		}
	}

	if triggeredLevelIndex == -1 {
		logger.S().Error("Internal inconsistency: triggeredLevel found but its index was not. Aborting state transition.")
		return
	}

	// Check if the filled order is at the boundary of the grid
	isTopHit := triggeredLevel.Side == models.Sell && triggeredLevelIndex == 0
	isBottomHit := triggeredLevel.Side == models.Buy && triggeredLevelIndex == len(b.grid.ConceptualGrid)-1

	if isTopHit || isBottomHit {
		if isTopHit {
			logger.S().Warnf(">>> TOP OF GRID HIT <<< Sell at Level %d filled. Triggering hard reset.", triggeredLevel.GridID)
		}
		if isBottomHit {
			logger.S().Warnf(">>> BOTTOM OF GRID HIT <<< Buy at Level %d filled. Triggering hard reset.", triggeredLevel.GridID)
		}
		go b.hardReset()
	} else {
		// Standard operation: place the opposing order
		if triggeredLevel.Side == models.Buy {
			// Buy filled, place a sell one level higher (at a higher price, hence lower index)
			targetIndex := triggeredLevelIndex - 1
			if targetIndex >= 0 {
				targetLevel := &b.grid.ConceptualGrid[targetIndex]
				if targetLevel.State == models.StateIdle {
					logger.S().Infof("Buy at Level %d filled. Placing Sell at adjacent Level %d.", triggeredLevel.GridID, targetLevel.GridID)
					go b.placeAndManageOrder(models.Sell, targetLevel, nil)
				}
			}
		} else { // Sell filled
			// Sell filled, place a buy one level lower (at a lower price, hence higher index)
			targetIndex := triggeredLevelIndex + 1
			if targetIndex < len(b.grid.ConceptualGrid) {
				targetLevel := &b.grid.ConceptualGrid[targetIndex]
				if targetLevel.State == models.StateIdle {
					logger.S().Infof("Sell at Level %d filled. Placing Buy at adjacent Level %d.", triggeredLevel.GridID, targetLevel.GridID)
					go b.placeAndManageOrder(models.Buy, targetLevel, nil)
				}
			}
		}
	}
}

// hardReset orchestrates the full cycle of closing the position and re-entering the market.
func (b *GridTradingBot) hardReset() {
	logger.S().Warn("--- HARD RESET TRIGGERED ---")

	b.mutex.Lock()
	if b.isReentering {
		logger.S().Warn("Hard reset is already in progress. Ignoring trigger.")
		b.mutex.Unlock()
		return
	}
	b.isReentering = true
	b.mutex.Unlock()

	defer func() {
		b.mutex.Lock()
		b.isReentering = false
		b.mutex.Unlock()
	}()

	logger.S().Info("Hard Reset Step 1/3: Cancelling all active orders...")
	if err := b.cancelAllActiveOrders(); err != nil {
		b.enterSafeMode(fmt.Sprintf("Failed to cancel all orders during hard reset: %v", err))
		return
	}

	time.Sleep(2 * time.Second)

	logger.S().Info("Hard Reset Step 2/3: Closing current position...")
	if err := b.closeCurrentPosition(); err != nil {
		b.enterSafeMode(fmt.Sprintf("Failed to close position during hard reset: %v", err))
		return
	}

	time.Sleep(5 * time.Second)

	logger.S().Info("Hard Reset Step 3/3: Re-entering market with a new grid...")
	if err := b.enterMarketAndSetupGrid(); err != nil {
		b.enterSafeMode(fmt.Sprintf("Failed to re-enter market during hard reset: %v", err))
		return
	}

	logger.S().Warn("--- HARD RESET COMPLETE ---")
}

// closeCurrentPosition closes the bot's current open position on the exchange by placing a market order.
func (b *GridTradingBot) closeCurrentPosition() error {
	logger.S().Info("--- Attempting to close current position ---")

	positions, err := b.exchange.GetPositions(b.config.Symbol)
	if err != nil {
		return fmt.Errorf("could not get positions to close position: %v", err)
	}

	if len(positions) == 0 || positions[0].PositionAmt == "0" {
		logger.S().Info("No open positions found or position is zero. Nothing to close.")
		return nil
	}

	currentPositionSize, err := strconv.ParseFloat(positions[0].PositionAmt, 64)
	if err != nil {
		return fmt.Errorf("could not parse position amount '%s': %v", positions[0].PositionAmt, err)
	}

	if math.Abs(currentPositionSize) < 1e-9 {
		logger.S().Info("Position size is effectively zero. Nothing to close.")
		return nil
	}

	var side models.OrderSide
	quantityToClose := math.Abs(currentPositionSize)

	if currentPositionSize > 0 {
		side = models.Sell
	} else {
		side = models.Buy
	}

	var stepSize string
	for _, f := range b.symbolInfo.Filters {
		if f.FilterType == "LOT_SIZE" {
			stepSize = f.StepSize
		}
	}

	adjustedQuantity := adjustValueToStep(quantityToClose, stepSize)
	if adjustedQuantity == 0 && quantityToClose > 0 {
		logger.S().Warnf("Position size %.8f is smaller than step size %s. Cannot place closing order.", quantityToClose, stepSize)
		return nil
	}

	clientOrderID, err := b.generateClientOrderID()
	if err != nil {
		return fmt.Errorf("could not generate client order ID for closing order: %v", err)
	}

	logger.S().Infof("Placing MARKET %s order to close position of size %.8f.", side, adjustedQuantity)

	_, err = b.exchange.PlaceOrder(b.config.Symbol, string(side), "MARKET", adjustedQuantity, 0, clientOrderID)
	if err != nil {
		return fmt.Errorf("failed to place market order to close position: %v", err)
	}

	logger.S().Info("Market order to close position has been submitted. Assuming it will fill shortly.")
	return nil
}

// cancelAllActiveOrders iterates through the grid and cancels all orders in 'Active' state.
func (b *GridTradingBot) cancelAllActiveOrders() error {
	logger.S().Info("Attempting to cancel all active orders...")

	var ordersToCancel []*models.Level
	b.mutex.RLock()
	for i := range b.grid.ConceptualGrid {
		level := &b.grid.ConceptualGrid[i]
		if level.State == models.StateActive {
			ordersToCancel = append(ordersToCancel, level)
		}
	}
	b.mutex.RUnlock()

	if len(ordersToCancel) == 0 {
		logger.S().Info("No active orders found to cancel.")
		return nil
	}

	logger.S().Infof("Found %d active orders to cancel.", len(ordersToCancel))

	var firstError error
	for _, level := range ordersToCancel {
		logger.S().Infof("Cancelling order %d for Level %d", level.OrderID, level.GridID)
		err := b.exchange.CancelOrder(b.config.Symbol, level.OrderID)
		if err != nil {
			logger.S().Errorf("Failed to cancel order %d for Level %d: %v.", level.OrderID, level.GridID, err)
			if firstError == nil {
				firstError = err
			}
		}
	}

	if firstError != nil {
		return fmt.Errorf("encountered one or more errors while cancelling orders: %w", firstError)
	}

	logger.S().Info("All active orders have been requested for cancellation.")
	return nil
}

func (b *GridTradingBot) rebuildGrid(pivotLevelID int) error {
	logger.S().Infof("--- Starting grid rebuild, pivot Level.GridID: %d ---", pivotLevelID)

	b.mutex.Lock()

	// Step 1: Cancel all active orders
	logger.S().Info("Step 1/3: Cancelling all active orders...")
	for i := range b.grid.ConceptualGrid {
		level := &b.grid.ConceptualGrid[i]
		if level.State == models.StateActive {
			logger.S().Infof("Cancelling order %d for Level %d", level.OrderID, level.GridID)
			err := b.exchange.CancelOrder(b.config.Symbol, level.OrderID)
			if err != nil {
				logger.S().Errorf("Failed to cancel order %d for Level %d: %v. Setting state to Error.", level.OrderID, level.GridID, err)
				level.State = models.StateError
			} else {
				level.State = models.StateCancelling
				// In a real system, we'd wait for a websocket event to confirm cancellation.
				// For this refactoring step, we'll assume it gets cancelled and update the state directly.
				level.State = models.StateCancelled
			}
		}
	}

	// Step 2: Determine the new center price for the grid
	logger.S().Info("Step 2/3: Determining new center price...")
	var newCenterPrice float64
	var pivotFound bool
	for _, level := range b.grid.ConceptualGrid {
		if level.GridID == pivotLevelID {
			newCenterPrice = level.Price
			pivotFound = true
			break
		}
	}

	if !pivotFound {
		var err error
		newCenterPrice, err = b.exchange.GetPrice(b.config.Symbol)
		if err != nil {
			b.mutex.Unlock() // Unlock before entering safe mode
			reason := fmt.Sprintf("failed to get current price for full rebuild: %v", err)
			b.enterSafeMode(reason)
			return errors.New(reason)
		}
		logger.S().Infof("Pivot Level ID %d not found, using current market price %.4f as new center.", pivotLevelID, newCenterPrice)
	} else {
		logger.S().Infof("New center price will be based on pivot Level %d's price: %.4f", pivotLevelID, newCenterPrice)
	}

	// Step 3: Reset grid states and place new orders
	logger.S().Info("Step 3/3: Resetting grid and placing new orders...")
	for i := range b.grid.ConceptualGrid {
		level := &b.grid.ConceptualGrid[i]
		if level.State != models.StateActive && level.State != models.StatePlacing {
			level.State = models.StateIdle
			level.OrderID = 0
			level.ClientOrderID = ""
		}
	}

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

// setupInitialGrid places the initial set of orders based on the established entry price.
// It now only places sell orders above the entry price, as per the refined strategy.
// It identifies which levels need orders and then places them sequentially using the thread-safe helper.
func (b *GridTradingBot) setupInitialGrid(entryPrice float64) error {
	logger.S().Info("--- Setting up initial grid orders ---")

	// Step 1: Identify levels that need sell orders, under a read lock.
	b.mutex.RLock()
	var levelsToOrder []*models.Level
	for i := range b.grid.ConceptualGrid {
		level := &b.grid.ConceptualGrid[i]
		// Only place SELL orders above the final entry price.
		if level.Price > entryPrice && level.State == models.StateIdle {
			levelsToOrder = append(levelsToOrder, level)
		}
	}
	b.mutex.RUnlock()

	// Step 2: Place orders sequentially for the identified levels.
	// This is safer for initialization and easier to debug than concurrent placement.
	// placeAndManageOrder is a blocking call, so this loop will execute them one by one.
	ordersTriggered := 0
	for _, level := range levelsToOrder {
		// We are only placing Sells during the initial setup.
		b.placeAndManageOrder(models.Sell, level, nil)
		ordersTriggered++
	}

	if ordersTriggered == 0 {
		logger.S().Warn("No initial sell orders were placed. This might be expected if the entry price is above all grid levels.")
	} else {
		logger.S().Infof("--- Initial grid setup: %d sell orders triggered for placement ---", ordersTriggered)
	}
	return nil
}

// enterSafeMode puts the bot into a safe mode where it stops trading
func (b *GridTradingBot) enterSafeMode(reason string) {
	b.mutex.Lock()
	if b.isHalted {
		b.mutex.Unlock()
		return
	}
	b.isHalted = true
	b.safeModeReason = reason
	b.mutex.Unlock() // Unlock before logging and launching goroutine to avoid deadlocks

	logger.S().Errorf("--- Entering Safe Mode ---")
	logger.S().Errorf("Reason: %s", reason)
	logger.S().Errorf("Bot has stopped all trading activity. Manual intervention required.")

	go func() {
		if err := b.cancelAllActiveOrders(); err != nil {
			logger.S().Errorf("Error during safe mode order cancellation: %v", err)
		}
	}()
}

// eventProcessor is the heart of the bot, processing all events sequentially from a single channel.
// This architectural choice eliminates race conditions for state modifications.
func (b *GridTradingBot) eventProcessor() {
	logger.S().Info("Core event processor started.")
	for {
		select {
		case event := <-b.eventChannel:
			b.processSingleEvent(event)
		case <-b.stopChannel:
			logger.S().Info("Core event processor stopped.")
			return
		}
	}
}

// processSingleEvent handles a single normalized event.
// All state-modifying logic should be called from here.
func (b *GridTradingBot) processSingleEvent(event NormalizedEvent) {
	switch event.Type {
	case OrderUpdateEvent:
		if orderUpdate, ok := event.Data.(models.OrderUpdateEvent); ok {
			b.handleOrderUpdate(orderUpdate)
		} else {
			logger.S().Warnf("Received OrderUpdateEvent with unexpected data type: %T", event.Data)
		}
		// Future event types can be handled here
		// case PriceTickEvent:
		// ...
	}
}

// saveGridState is a helper function to persist the current grid state to the database.
// It's designed to be called after any state-modifying operation.
func (b *GridTradingBot) saveGridState() {
	// The lock should already be held by the calling function, but a RLock is safe.
	b.mutex.RLock()
	defer b.mutex.RUnlock()

	if err := b.storage.SaveGrid(b.grid); err != nil {
		logger.S().Errorf("--- FAILED TO SAVE GRID STATE: %v ---", err)
		// In a real-world scenario, this might trigger a more drastic safety mechanism.
	} else {
		logger.S().Debug("Successfully saved grid state.")
	}
}

// reconcileStateWithExchange is called on startup to ensure the bot's internal state
// matches the reality on the exchange.
func (b *GridTradingBot) reconcileStateWithExchange() error {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	logger.S().Info("Step 1/3: Fetching all open orders from exchange...")
	openOrders, err := b.exchange.GetOpenOrders(b.config.Symbol)
	if err != nil {
		return fmt.Errorf("could not get open orders from exchange: %w", err)
	}
	logger.S().Infof("Found %d open orders on the exchange.", len(openOrders))

	// Create a map of open orders for efficient lookup by ClientOrderID.
	ordersByClientOrderID := make(map[string]models.Order)
	for _, order := range openOrders {
		ordersByClientOrderID[order.ClientOrderId] = order
	}

	logger.S().Info("Step 2/3: Reconciling local grid levels with exchange orders...")
	for i := range b.grid.ConceptualGrid {
		level := &b.grid.ConceptualGrid[i]

		// We only care about levels that we believe should have an active order.
		if level.State != models.StateActive && level.State != models.StatePlacing {
			continue
		}

		logger.S().Debugf("Reconciling Level %d (State: %s, ClientID: %s)", level.GridID, level.State, level.ClientOrderID)

		order, found := ordersByClientOrderID[level.ClientOrderID]
		if found {
			// GOOD: The order exists on the exchange. Our state is likely consistent.
			// Let's ensure our OrderID is aligned with the exchange's official ID.
			if level.OrderID != order.OrderId {
				logger.S().Warnf("Aligning OrderID for Level %d. Local: %d, Exchange: %d", level.GridID, level.OrderID, order.OrderId)
				level.OrderID = order.OrderId
			}
			level.State = models.StateActive // Ensure state is Active, not Placing.
			// Remove the order from the map so we can identify orphans later.
			delete(ordersByClientOrderID, level.ClientOrderID)
		} else {
			// BAD: We think there's an order, but it's not on the exchange.
			// It was likely filled or cancelled while we were offline.
			logger.S().Warnf("DISCREPANCY: Level %d is %s locally, but no corresponding open order found on exchange (ClientID: %s).", level.GridID, level.State, level.ClientOrderID)

			// To resolve, we should check the trade history for this order.
			// This is a simplified approach for now: we reset the level to Idle.
			// A more advanced implementation would query the order's final status.
			level.State = models.StateIdle
			level.OrderID = 0
			// ClientOrderID is kept for historical reference, but the level is now available.
		}
	}

	// Step 3/3: Handle any remaining "orphaned" orders on the exchange.
	if len(ordersByClientOrderID) > 0 {
		logger.S().Warnf("Found %d orphaned orders on the exchange that are not tracked locally. Cancelling them now...", len(ordersByClientOrderID))
		for _, order := range ordersByClientOrderID {
			logger.S().Infof("Cancelling orphaned order ID %d (ClientID: %s)...", order.OrderId, order.ClientOrderId)
			if err := b.exchange.CancelOrder(b.config.Symbol, order.OrderId); err != nil {
				// This is serious, as it could leave unwanted orders active.
				logger.S().Errorf("CRITICAL: FAILED TO CANCEL ORPHANED ORDER ID %d: %v", order.OrderId, err)
				// We might want to enter safe mode here.
			}
		}
	} else {
		logger.S().Info("No orphaned orders found on the exchange.")
	}

	// Finally, save the reconciled state.
	b.saveGridState()

	return nil
}

// adjustValueToStep adjusts a value to the given step size
func adjustValueToStep(value float64, step string) float64 {
	if step == "" || step == "0" {
		return value
	}
	stepFloat, err := strconv.ParseFloat(step, 64)
	if err != nil || stepFloat == 0 {
		return value
	}
	multiplier := 1.0 / stepFloat
	return math.Floor(value*multiplier) / multiplier
}

// generateClientOrderID generates a new client order ID
func (b *GridTradingBot) generateClientOrderID() (string, error) {
	id, err := b.idGenerator.Generate()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("x-grid-%s", id), nil
}

// isWithinExposureLimit checks if adding a trade would exceed the wallet exposure limit
func (b *GridTradingBot) isWithinExposureLimit(quantityToAdd float64) bool {
	b.mutex.RLock()
	defer b.mutex.RUnlock()

	if b.config.WalletExposureLimit <= 0 {
		return true
	}

	positions, err := b.exchange.GetPositions(b.config.Symbol)
	if err != nil {
		logger.S().Warnf("Could not get positions to check exposure limit: %v", err)
		return false
	}

	var currentPositionSize float64
	if len(positions) > 0 {
		currentPositionSize, _ = strconv.ParseFloat(positions[0].PositionAmt, 64)
	}

	_, accountEquity, err := b.exchange.GetAccountState(b.config.Symbol)
	if err != nil {
		logger.S().Warnf("Could not get account state to check exposure limit: %v", err)
		return false
	}

	if accountEquity <= 0 {
		return false
	}

	futurePositionValue := (currentPositionSize + quantityToAdd) * b.currentPrice
	expectedExposure := futurePositionValue / accountEquity

	if expectedExposure > b.config.WalletExposureLimit {
		logger.S().Warnf(
			"Wallet exposure check failed: Expected exposure %.2f%% would exceed limit of %.2f%%.",
			expectedExposure*100, b.config.WalletExposureLimit*100,
		)
		return false
	}
	return true
}

// Start is the main entry point for the live trading bot.
func (b *GridTradingBot) Start() error {
	logger.S().Info("--- Starting Grid Trading Bot ---")

	// Step 1: Load grid state from storage
	loadedGrid, err := b.storage.LoadGrid()
	if err != nil {
		if errors.Is(err, models.ErrStateNotFound) {
			logger.S().Info("No previous state found. Starting with a fresh grid.")
			// This is not an error, we just start fresh.
		} else {
			return fmt.Errorf("failed to load grid state: %w", err)
		}
	} else if loadedGrid != nil {
		b.grid = loadedGrid
		logger.S().Info("Successfully loaded grid state from database.")

		// Step 2: Reconcile state with the exchange
		if err := b.reconcileStateWithExchange(); err != nil {
			b.enterSafeMode(fmt.Sprintf("Failed to reconcile state with exchange: %v", err))
			return err
		}
	}

	// Step 3: If the grid is empty (fresh start), set it up.
	if len(b.grid.ConceptualGrid) == 0 {
		if err := b.enterMarketAndSetupGrid(); err != nil {
			return fmt.Errorf("failed to perform initial market entry and grid setup: %w", err)
		}
	}

	// Step 4: Connect to WebSocket and start listening for events.
	if err := b.connectWebSocket(); err != nil {
		return fmt.Errorf("failed to connect to WebSocket: %w", err)
	}

	b.isRunning = true
	go b.webSocketLoop()
	go b.eventProcessor()

	logger.S().Info("--- Grid Trading Bot is now running ---")
	return nil
}

// Stop gracefully shuts down the bot.
func (b *GridTradingBot) Stop() {
	logger.S().Info("--- Stopping Grid Trading Bot ---")
	b.mutex.Lock()
	if !b.isRunning {
		b.mutex.Unlock()
		logger.S().Info("Bot is not running.")
		return
	}
	b.isRunning = false
	b.mutex.Unlock()

	close(b.stopChannel) // Signal all goroutines to stop

	if b.wsConn != nil {
		b.wsConn.Close()
	}
	if b.storage != nil {
		b.storage.Close()
	}
	logger.S().Info("--- Bot has been stopped ---")
}

// StartForBacktest prepares the bot for a backtest run.
func (b *GridTradingBot) StartForBacktest() error {
	logger.S().Info("--- Initializing Bot for Backtest ---")
	// In backtesting, we always start with a fresh grid.
	if err := b.enterMarketAndSetupGrid(); err != nil {
		return fmt.Errorf("failed to perform initial market entry for backtest: %w", err)
	}
	b.isRunning = true
	logger.S().Info("--- Backtest Bot Initialized ---")
	return nil
}

// SetCurrentPrice updates the bot's current price view, for backtesting purposes.
func (b *GridTradingBot) SetCurrentPrice(price float64) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.currentPrice = price
}

// ProcessBacktestTick simulates a single tick of market data for backtesting.
func (b *GridTradingBot) ProcessBacktestTick() {
	// In a real backtest, this would trigger the same logic as the live event processor.
	// For this refactoring, we assume the backtest exchange will create and push events.
	// The core logic is now unified in the event processor.
	// We can simulate price-crossing checks here if needed, but for now, we rely on the exchange mock.
}

// IsHalted returns true if the bot is in a safe mode.
func (b *GridTradingBot) IsHalted() bool {
	b.mutex.RLock()
	defer b.mutex.RUnlock()
	return b.isHalted
}
