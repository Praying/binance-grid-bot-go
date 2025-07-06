package bot

import (
	"binance-grid-bot-go/internal/exchange"
	"binance-grid-bot-go/internal/idgenerator"
	"binance-grid-bot-go/internal/logger"
	"binance-grid-bot-go/internal/models"
	"binance-grid-bot-go/internal/persistence"
	"binance-grid-bot-go/internal/statemanager"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// GridTradingBot is the core struct for the grid trading bot
type GridTradingBot struct {
	config         *models.Config
	exchange       exchange.Exchange
	wsConn         *websocket.Conn
	listenKey      string
	repo           persistence.StateRepository // For state persistence
	stateManager   *statemanager.StateManager
	currentPrice   float64 // For real-time price updates, not for state management
	isRunning      bool
	IsBacktest     bool
	currentTime    time.Time
	isReentering   bool
	reentrySignal  chan bool
	stopChannel    chan bool
	symbolInfo     *models.SymbolInfo
	isHalted       bool
	safeModeReason string
	idGenerator    *idgenerator.IDGenerator
	logger         *zap.Logger
}

// NewGridTradingBot creates a new instance of the grid trading bot
func NewGridTradingBot(config *models.Config, ex exchange.Exchange, repo persistence.StateRepository, isBacktest bool, logger *zap.Logger) *GridTradingBot {
	bot := &GridTradingBot{
		config:        config,
		exchange:      ex,
		repo:          repo, // The repo is still needed for the initial state recovery logic.
		isRunning:     false,
		IsBacktest:    isBacktest,
		stopChannel:   make(chan bool),
		reentrySignal: make(chan bool, 1),
		isHalted:      false,
		logger:        logger,
	}

	// The initial state will be properly loaded/initialized in the Start or recoverState methods.
	// For now, we pass an empty state to the StateManager.
	initialState := &models.BotState{}
	// Pass the bot instance itself as the OrderPlacer to break the dependency cycle.
	sm := statemanager.NewStateManager(initialState, repo, bot, logger)
	bot.stateManager = sm

	symbolInfo, err := ex.GetSymbolInfo(config.Symbol)
	if err != nil {
		bot.logger.Sugar().Fatalf("Could not get symbol info for %s: %v", config.Symbol, err)
	}
	bot.symbolInfo = symbolInfo
	bot.logger.Sugar().Infof("Successfully fetched and cached trading rules for %s.", config.Symbol)

	idGen, err := idgenerator.NewIDGenerator(0)
	if err != nil {
		bot.logger.Sugar().Fatalf("Could not create ID generator: %v", err)
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
	b.logger.Sugar().Infof("Submitted initial market buy order ID: %d, Quantity: %.5f. Waiting for fill...", order.OrderId, quantity)

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	timeout := time.After(2 * time.Minute)

	for {
		select {
		case <-ticker.C:
			status, err := b.exchange.GetOrderStatus(b.config.Symbol, order.OrderId)
			if err != nil {
				if b.IsBacktest && strings.Contains(err.Error(), "not found") {
					b.logger.Sugar().Infof("Initial order %d status check returned 'not found', assuming filled in backtest mode.", order.OrderId)
					// This legacy flag is no longer used. The position is tracked in b.state.
					return b.currentPrice, nil
				}
				b.logger.Sugar().Warnf("Failed to get status for initial order %d: %v. Retrying...", order.OrderId, err)
				continue
			}

			switch status.Status {
			case "FILLED":
				b.logger.Sugar().Infof("Initial position order %d has been filled!", order.OrderId)
				// This legacy flag is no longer used. The position is tracked in b.state.

				trade, err := b.exchange.GetLastTrade(b.config.Symbol, order.OrderId)
				if err != nil {
					return 0, fmt.Errorf("could not get trade for initial order %d: %v", order.OrderId, err)
				}
				filledPrice, err := strconv.ParseFloat(trade.Price, 64)
				if err != nil {
					return 0, fmt.Errorf("could not parse fill price for initial order %d: %v", order.OrderId, err)
				}
				return filledPrice, nil

			case "CANCELED", "REJECTED", "EXPIRED":
				return 0, fmt.Errorf("initial position order %d failed with status: %s", order.OrderId, status.Status)
			default:
				b.logger.Sugar().Debugf("Initial order %d status: %s. Waiting for fill...", order.OrderId, status.Status)
			}
		case <-timeout:
			return 0, fmt.Errorf("timeout waiting for initial order %d to fill", order.OrderId)
		case <-b.stopChannel:
			return 0, fmt.Errorf("bot stopped, interrupting initial position establishment")
		}
	}
}

// enterMarketAndSetupGrid implements the logic for entering the market and setting up the grid
// enterMarketAndSetupGrid initializes a new trading cycle, establishes the base position,
// sets up the initial grid, and persists the state. This is now the state-driven entry point for a fresh start.
func (b *GridTradingBot) enterMarketAndSetupGrid() error {
	b.logger.Sugar().Info("--- Starting New Trading Cycle ---")

	// 1. Get current price and define initial parameters
	entryPrice, err := b.exchange.GetPrice(b.config.Symbol)
	if err != nil {
		return fmt.Errorf("failed to get current price: %v", err)
	}
	b.currentPrice = entryPrice
	regressionPrice := entryPrice * (1 + b.config.ReturnRate)
	b.logger.Sugar().Infof("New cycle definition: Entry Price: %.4f, Regression Price (Grid Top): %.4f", entryPrice, regressionPrice)

	// 2. Generate the static grid definitions for this cycle
	var tickSize string
	for _, f := range b.symbolInfo.Filters {
		if f.FilterType == "PRICE_FILTER" {
			tickSize = f.TickSize
		}
	}

	gridDefinitions := make([]models.GridLevelDefinition, 0)
	price := regressionPrice
	gridID := 1
	for price > (entryPrice * 0.5) { // Use a reasonable lower bound
		adjustedPrice := adjustValueToStep(price, tickSize)
		quantity, err := b.calculateQuantity(adjustedPrice)
		if err != nil {
			return fmt.Errorf("could not calculate quantity for grid definition at price %.4f: %v", adjustedPrice, err)
		}

		// Determine side based on price relative to entry
		var side models.Side
		if adjustedPrice > entryPrice {
			side = models.Sell
		} else {
			side = models.Buy
		}

		isDuplicate := false
		for _, def := range gridDefinitions {
			if def.Price == adjustedPrice {
				isDuplicate = true
				break
			}
		}
		if !isDuplicate && adjustedPrice > 0 {
			def := models.GridLevelDefinition{
				ID:       gridID,
				Price:    adjustedPrice,
				Quantity: quantity,
				Side:     side,
			}
			gridDefinitions = append(gridDefinitions, def)
			gridID++
		}
		price *= 1 - b.config.GridSpacing
	}

	if len(gridDefinitions) == 0 {
		return fmt.Errorf("generated grid definition is empty, likely due to misconfiguration")
	}
	b.logger.Sugar().Infof("Successfully generated grid definition with %d levels.", len(gridDefinitions))

	// 3. Calculate and establish initial position
	var initialPositionQuantity float64
	for _, def := range gridDefinitions {
		if def.Side == models.Sell {
			initialPositionQuantity += def.Quantity
		}
	}
	b.logger.Sugar().Infof("Calculated initial position quantity: %.8f", initialPositionQuantity)

	var actualFilledPrice = entryPrice
	if initialPositionQuantity > 0 {
		if !b.isWithinExposureLimit(initialPositionQuantity) {
			return fmt.Errorf("initial position blocked: wallet exposure limit would be exceeded")
		}
		filledPrice, err := b.establishBasePositionAndWait(initialPositionQuantity)
		if err != nil {
			return fmt.Errorf("failed to establish initial position: %v", err)
		}
		actualFilledPrice = filledPrice // Update entry price to actual fill price
	} else {
		b.logger.Sugar().Warn("Initial position quantity is zero. Starting without a base position.")
	}

	// 4. Initialize the full BotState locally
	newState := &models.BotState{
		BotID:   "bot-001", // Or generate a unique ID
		Symbol:  b.config.Symbol,
		Version: 1,
		InitialParams: models.InitialParams{
			EntryPrice:     actualFilledPrice,
			GridDefinition: gridDefinitions,
		},
		CurrentCycle: models.CycleState{
			CycleID:         fmt.Sprintf("cycle-%d", time.Now().Unix()),
			RegressionPrice: regressionPrice,
			GridStates:      make(map[int]*models.GridLevelState),
			PositionSize:    initialPositionQuantity,
		},
		LastUpdateTime: time.Now(),
	}

	// 5. Setup the initial grid orders. This will dispatch events to update the state.
	b.logger.Sugar().Info("Initial position confirmed, setting up grid orders...")
	if err := b.setupInitialGrid(newState); err != nil {
		return fmt.Errorf("initial grid setup failed: %v", err)
	}

	// 6. Dispatch an event to reset the state in the StateManager.
	// The StateManager will then handle persistence.
	b.logger.Sugar().Info("Dispatching StateResetEvent to StateManager...")
	b.stateManager.DispatchEvent(statemanager.NormalizedEvent{
		Type:      statemanager.StateResetEvent,
		Timestamp: time.Now(),
		Data:      newState,
	})

	b.logger.Sugar().Info("--- New Trading Cycle Setup Complete ---")
	return nil
}

// PlaceNewOrder implements the statemanager.OrderPlacer interface.
// It is called by the StateManager to execute an order placement.
// After successfully placing and confirming the order, it dispatches an
// UpdateGridLevelEvent back to the StateManager to record the new state.
func (b *GridTradingBot) PlaceNewOrder(def models.GridLevelDefinition) {
	if def.Side == models.Buy && !b.isWithinExposureLimit(def.Quantity) {
		b.logger.Sugar().Errorf("Order blocked for LevelID %d: wallet exposure limit would be exceeded", def.ID)
		return
	}

	clientOrderID, err := b.generateClientOrderID()
	if err != nil {
		b.logger.Sugar().Errorf("Could not generate client order ID for grid order (LevelID: %d): %v", def.ID, err)
		return
	}

	order, err := b.exchange.PlaceOrder(b.config.Symbol, string(def.Side), "LIMIT", def.Quantity, def.Price, clientOrderID)
	if err != nil {
		b.logger.Sugar().Errorf("Failed to place %s order at price %.4f for LevelID %d: %v", def.Side, def.Price, def.ID, err)
		return
	}
	b.logger.Sugar().Infof("Submitted %s order: OrderID %d, Price %.4f, Quantity %.5f, LevelID: %d. Waiting for confirmation...", def.Side, order.OrderId, def.Price, def.Quantity, def.ID)

	// The confirmation loop ensures the order is on the book before we proceed.
	if !b.IsBacktest {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		timeout := time.After(10 * time.Second)

		for {
			select {
			case <-ticker.C:
				status, err := b.exchange.GetOrderStatus(b.config.Symbol, order.OrderId)
				if err != nil {
					if strings.Contains(err.Error(), "order does not exist") {
						b.logger.Sugar().Warnf("Order %d not found yet, retrying...", order.OrderId)
						continue
					}
					b.logger.Sugar().Errorf("Failed to get status for order %d: %v", order.OrderId, err)
					return
				}

				switch status.Status {
				case "NEW", "PARTIALLY_FILLED":
					b.logger.Sugar().Infof("Order %d confirmed by exchange with status: %s", order.OrderId, status.Status)
					goto confirmed
				case "FILLED":
					b.logger.Sugar().Warnf("Order %d was already FILLED upon confirmation check. This can happen in volatile markets.", order.OrderId)
					goto confirmed
				case "CANCELED", "REJECTED", "EXPIRED":
					b.logger.Sugar().Errorf("Order %d failed confirmation with final status: %s", order.OrderId, status.Status)
					return
				}
			case <-timeout:
				b.logger.Sugar().Errorf("Timeout waiting for order %d confirmation", order.OrderId)
				return
			case <-b.stopChannel:
				b.logger.Sugar().Infof("Bot stopped, interrupting order %d confirmation", order.OrderId)
				return
			}
		}
	}

confirmed:
	newState := &models.GridLevelState{
		LevelID:        def.ID,
		OrderID:        clientOrderID,
		Status:         "OPEN",
		LastUpdateTime: time.Now(),
	}
	b.logger.Sugar().Infof("Successfully confirmed %s order: ClientOrderID %s, Price %.4f, Quantity %.5f, LevelID: %d", def.Side, newState.OrderID, def.Price, def.Quantity, def.ID)

	// Dispatch an event to update the grid level state instead of returning it.
	b.stateManager.DispatchEvent(statemanager.NormalizedEvent{
		Type:      statemanager.UpdateGridLevelEvent,
		Timestamp: time.Now(),
		Data: statemanager.UpdateGridLevelEventData{
			LevelID: def.ID,
			State:   newState,
		},
	})
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
		b.logger.Sugar().Info("Backtest mode, skipping WebSocket connection.")
		return nil
	}

	listenKey, err := b.exchange.CreateListenKey()
	if err != nil {
		return fmt.Errorf("could not create listen key: %v", err)
	}
	b.listenKey = listenKey
	b.logger.Sugar().Infof("Successfully obtained Listen Key: %s", b.listenKey)

	conn, err := b.exchange.ConnectWebSocket(b.listenKey)
	if err != nil {
		return fmt.Errorf("could not connect to WebSocket: %v", err)
	}
	b.wsConn = conn
	b.logger.Sugar().Info("Successfully connected to user data stream WebSocket.")

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
					b.logger.Sugar().Warnf("Failed to keep listen key alive: %v", err)
				} else {
					b.logger.Sugar().Info("Successfully kept listen key alive.")
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
		case <-pingTicker.C:
			if err := b.wsConn.WriteMessage(websocket.PingMessage, nil); err != nil {
				logger.S().Warnf("Failed to send WebSocket ping: %v", err)
			}
		case err := <-errChannel:
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				logger.S().Info("WebSocket connection closed normally.")
			} else {
				logger.S().Errorf("Error reading from WebSocket: %v. Starting reconnection process...", err)
				b.wsConn.Close() // Ensure the old connection is closed

				reconnectAttempts := 0
				for {
					reconnectAttempts++
					waitDuration := time.Duration(math.Min(float64(5*reconnectAttempts), 300)) * time.Second
					logger.S().Infof("Attempting to reconnect (attempt %d)... waiting for %v", reconnectAttempts, waitDuration)

					select {
					case <-time.After(waitDuration):
						if err := b.connectWebSocket(); err != nil {
							logger.S().Errorf("WebSocket reconnection attempt %d failed: %v", reconnectAttempts, err)
						} else {
							logger.S().Info("WebSocket reconnected successfully.")
							go b.webSocketLoop() // Restart the loop
							return               // Exit the old loop
						}
					case <-b.stopChannel:
						logger.S().Info("Stop signal received during reconnection, aborting.")
						return
					}
				}
			}
		case <-b.stopChannel:
			logger.S().Info("Stop signal received, closing WebSocket message loop.")
			b.wsConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return
		}
	}
}

// Start starts the bot
func (b *GridTradingBot) Start() error {
	if b.isRunning {
		return fmt.Errorf("bot is already running")
	}
	b.isRunning = true

	logger.S().Info("Starting bot...")

	// Start the StateManager's own goroutines
	b.stateManager.Start()

	// Attempt to load and recover state before starting the main loops
	if b.repo != nil {
		loadedState, err := b.repo.LoadState()
		if err != nil {
			// If LoadState returns any error, it's a critical failure.
			return fmt.Errorf("failed to load state: %v", err)
		}

		if loadedState == nil {
			// The repository is configured, but no state was found. This is the "fresh start" case.
			logger.S().Info("No previous state found. Starting fresh.")
			if err := b.enterMarketAndSetupGrid(); err != nil {
				return fmt.Errorf("failed to initialize new trading cycle: %v", err)
			}
		} else {
			// Dispatch a reset event to the state manager to initialize it with loaded state
			b.stateManager.DispatchEvent(statemanager.NormalizedEvent{
				Type:      statemanager.StateResetEvent,
				Timestamp: time.Now(),
				Data:      loadedState,
			})
			logger.S().Info("Successfully loaded previous state. Recovering...")
			if err := b.recoverState(); err != nil {
				return fmt.Errorf("failed to recover state: %v", err)
			}
		}
	} else {
		// If no persistence is configured, start fresh
		logger.S().Info("No persistence repository configured. Starting fresh.")
		if err := b.enterMarketAndSetupGrid(); err != nil {
			return fmt.Errorf("failed to initialize new trading cycle: %v", err)
		}
	}

	if !b.IsBacktest {
		if err := b.connectWebSocket(); err != nil {
			return fmt.Errorf("failed to connect to websocket: %v", err)
		}
		go b.webSocketLoop()
	}

	go b.strategyLoop()
	go b.monitorStatus()

	logger.S().Info("Bot started successfully.")
	return nil
}

// recoverState is the new function to handle state reconciliation on startup.
func (b *GridTradingBot) recoverState() error {
	logger.S().Info("--- Starting State Recovery and Exchange Synchronization ---")

	// Synchronize our loaded state with the reality on the exchange.
	if err := b.syncWithExchange(); err != nil {
		// If sync fails, it's safer to halt than to proceed with potentially inconsistent state.
		b.enterSafeMode(fmt.Sprintf("State synchronization failed: %v", err))
		return fmt.Errorf("CRITICAL: could not synchronize state with exchange: %w", err)
	}

	// After sync, the state is considered canonical. Now, set up the grid orders.
	// We get the fresh, synced state from the manager.
	syncedState := b.stateManager.GetStateSnapshot()
	if err := b.setupInitialGrid(syncedState); err != nil {
		return fmt.Errorf("failed to re-establish grid from recovered state: %v", err)
	}

	logger.S().Info("--- State Recovery and Synchronization Complete ---")
	return nil
}

// strategyLoop is the main strategy loop
func (b *GridTradingBot) strategyLoop() {
	for {
		select {
		case <-b.reentrySignal:
			// This logic for cycle restart remains.
			// The grid rebuild is now implicitly handled by the bot's reaction to state changes,
			// which will be implemented in a subsequent step. The direct channel trigger is removed.
			logger.S().Info("Re-entry signal received. Attempting to restart trading cycle.")
			b.isReentering = true
			b.cancelAllActiveOrders()
			// Wait a bit for cancellations to be processed
			time.Sleep(5 * time.Second)
			if err := b.enterMarketAndSetupGrid(); err != nil {
				b.enterSafeMode(fmt.Sprintf("Failed to re-enter market: %v", err))
				return
			}
			b.isReentering = false
		case <-b.stopChannel:
			logger.S().Info("Strategy loop stopped.")
			return
		}
	}
}

// StartForBacktest starts the bot for backtesting
func (b *GridTradingBot) StartForBacktest() error {
	if b.isRunning {
		return fmt.Errorf("bot is already running")
	}
	b.isRunning = true

	logger.S().Info("Starting backtest bot...")
	if err := b.enterMarketAndSetupGrid(); err != nil {
		return fmt.Errorf("backtest initialization failed: %v", err)
	}
	logger.S().Info("Backtest bot initialized successfully.")
	return nil
}

// ProcessBacktestTick processes a single tick in backtest mode
func (b *GridTradingBot) ProcessBacktestTick() {
	if b.isHalted {
		return
	}
}

// SetCurrentPrice sets the current price for backtesting
func (b *GridTradingBot) SetCurrentPrice(price float64) {
	b.currentPrice = price
}

// Stop stops the bot
func (b *GridTradingBot) Stop() {
	if !b.isRunning {
		return
	}
	b.isRunning = false
	close(b.stopChannel)

	b.stateManager.Stop()

	logger.S().Info("Stopping bot...")
	b.cancelAllActiveOrders()
	if b.wsConn != nil {
		b.wsConn.Close()
	}
	logger.S().Info("Bot stopped.")
}

// cancelAllActiveOrders cancels all active orders
func (b *GridTradingBot) cancelAllActiveOrders() {
	logger.S().Info("Cancelling all active orders...")
	if err := b.exchange.CancelAllOpenOrders(b.config.Symbol); err != nil {
		logger.S().Warnf("Failed to cancel all orders: %v", err)
	} else {
		logger.S().Info("Successfully sent request to cancel all orders.")
	}
}

// monitorStatus prints the bot's status periodically
func (b *GridTradingBot) monitorStatus() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			//  b.printStatus()
		case <-b.stopChannel:
			logger.S().Info("Status monitor received stop signal, exiting.")
			return
		}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
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
	if b.config.WalletExposureLimit <= 0 {
		return true // No limit set
	}

	state := b.stateManager.GetStateSnapshot()
	if state == nil {
		logger.S().Warn("Could not check exposure limit: state is not available.")
		return true // Cannot determine, fail open
	}

	currentPositionValue := state.CurrentCycle.PositionSize * b.currentPrice
	orderValue := quantityToAdd * b.currentPrice
	potentialTotalValue := currentPositionValue + orderValue

	walletBalance, err := b.exchange.GetBalance()
	if err != nil {
		logger.S().Warnf("Could not get balance to check exposure limit: %v", err)
		return true // Fail open if we can't get the balance
	}

	if walletBalance <= 0 {
		logger.S().Warn("Cannot place order: wallet balance for quote asset is zero or negative.")
		return false
	}

	exposure := potentialTotalValue / walletBalance
	if exposure > b.config.WalletExposureLimit {
		logger.S().Warnf("Order blocked: potential wallet exposure of %.2f%% would exceed the limit of %.2f%%.", exposure*100, b.config.WalletExposureLimit*100)
		return false
	}

	return true
}

// IsHalted returns whether the bot is halted
func (b *GridTradingBot) IsHalted() bool {
	return b.isHalted
}

// syncWithExchange reconciles the bot's in-memory state with the exchange's actual state.
func (b *GridTradingBot) syncWithExchange() error {
	logger.S().Info("--- Synchronizing with Exchange ---")

	state := b.stateManager.GetStateSnapshot()
	if state == nil {
		return fmt.Errorf("cannot sync with exchange: state is not available")
	}

	// 1. Fetch all open orders from the exchange for the symbol.
	remoteOrders, err := b.exchange.GetOpenOrders(b.config.Symbol)
	if err != nil {
		return fmt.Errorf("could not get open orders from exchange: %w", err)
	}
	logger.S().Infof("Found %d open orders on the exchange for %s.", len(remoteOrders), b.config.Symbol)

	remoteOrdersMap := make(map[string]models.Order)
	for _, order := range remoteOrders {
		remoteOrdersMap[order.ClientOrderId] = order
	}

	// 2. Compare local state with remote state.
	reconciledGridStates := make(map[int]*models.GridLevelState)
	for levelID, localState := range state.CurrentCycle.GridStates {
		if localState.Status != "OPEN" {
			reconciledGridStates[levelID] = localState
			continue
		}

		if _, exists := remoteOrdersMap[localState.OrderID]; exists {
			logger.S().Infof("Order %s (LevelID: %d) confirmed on exchange.", localState.OrderID, levelID)
			reconciledGridStates[levelID] = localState
			delete(remoteOrdersMap, localState.OrderID)
		} else {
			// 本地订单在交易所的当前挂单中未找到，需要查询历史记录来确定其最终状态。
			logger.S().Infof("核对本地 OPEN 订单: LevelID: %d, ClientOrderID: %s. 在交易所当前挂单中未找到，正在查询历史记录...", levelID, localState.OrderID)

			historyOrder, err := b.exchange.GetOrderHistory(b.config.Symbol, localState.OrderID)
			if err != nil {
				// 如果查询出错（例如，订单在历史上也不存在），则记录警告并将其从状态中移除。
				logger.S().Warnf("无法获取订单 %s 的历史记录，将从本地状态中移除。LevelID: %d. 错误: %v", localState.OrderID, levelID, err)
				delete(reconciledGridStates, levelID) // 从映射中移除
				continue
			}

			switch historyOrder.Status {
			case "FILLED":
				logger.S().Infof("历史记录确认订单 %s (LevelID: %d) 已成交 (FILLED)。正在派发更新事件...", localState.OrderID, levelID)
				// 不要直接修改状态，而是派发一个事件，让 StateManager 来处理。
				// StateManager 的现有逻辑会处理 FILLED 事件并触发网格重建。
				updateEvent := statemanager.NormalizedEvent{
					Type:      statemanager.OrderUpdateEvent,
					Timestamp: time.Now(),
					Data: models.OrderUpdateEvent{
						Order: models.OrderUpdateInfo{
							Symbol:        historyOrder.Symbol,
							ClientOrderID: historyOrder.ClientOrderId,
							Status:        historyOrder.Status,
							ExecutedQty:   historyOrder.ExecutedQty,
						},
					},
				}
				b.stateManager.DispatchEvent(updateEvent)
				// 在同步逻辑的这个点，我们让它保持原样，等待事件处理循环生效。
				// 从 reconciledGridStates 中删除它，因为它不再是“活跃”的挂单。
				// StateManager 将根据事件重建正确的状态。
				delete(reconciledGridStates, levelID)

			case "CANCELED", "EXPIRED":
				logger.S().Warnf("历史记录显示订单 %s (LevelID: %d) 已被取消或过期 (%s)。将从本地状态中移除。", localState.OrderID, levelID, historyOrder.Status)
				delete(reconciledGridStates, levelID) // 从映射中移除

			default:
				logger.S().Warnf("订单 %s (LevelID: %d) 的历史状态为非最终状态 '%s'，这不符合预期。暂时将其从本地状态中移除以确保安全。", localState.OrderID, levelID, historyOrder.Status)
				delete(reconciledGridStates, levelID) // 从映射中移除
			}
		}
	}

	// 3. Cancel any "rogue" orders found on the exchange but not in our state.
	if len(remoteOrdersMap) > 0 {
		logger.S().Warnf("Found %d rogue orders on the exchange that are not in our state. Cancelling them...", len(remoteOrdersMap))
		for _, rogueOrder := range remoteOrdersMap {
			logger.S().Warnf("Cancelling rogue order ID %d (Client ID: %s)...", rogueOrder.OrderId, rogueOrder.ClientOrderId)
			if err := b.exchange.CancelOrder(b.config.Symbol, rogueOrder.OrderId); err != nil {
				logger.S().Errorf("Failed to cancel rogue order %d: %v", rogueOrder.OrderId, err)
			}
		}
	}

	// 4. Sync position size
	positions, err := b.exchange.GetPositions(b.config.Symbol)
	if err != nil {
		return fmt.Errorf("could not get position information from exchange: %v", err)
	}
	var exchangePositionSize float64
	if len(positions) > 0 {
		// Assuming we are not in hedge mode and only have one position.
		posSize, err := strconv.ParseFloat(positions[0].PositionAmt, 64)
		if err != nil {
			return fmt.Errorf("could not parse exchange position size: %v", err)
		}
		exchangePositionSize = math.Abs(posSize)
	}

	if math.Abs(state.CurrentCycle.PositionSize-exchangePositionSize) > 0.00001 {
		logger.S().Warnf("Position size mismatch. State: %.8f, Exchange: %.8f. Adjusting state.", state.CurrentCycle.PositionSize, exchangePositionSize)
		state.CurrentCycle.PositionSize = exchangePositionSize
	}

	// 5. Update the bot's state with the reconciled data by dispatching an event
	state.CurrentCycle.GridStates = reconciledGridStates
	b.stateManager.DispatchEvent(statemanager.NormalizedEvent{
		Type:      statemanager.StateResetEvent,
		Timestamp: time.Now(),
		Data:      state,
	})

	logger.S().Info("--- Exchange Synchronization Complete ---")
	return nil
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
		// Dispatch the event directly to the StateManager
		b.stateManager.DispatchEvent(statemanager.NormalizedEvent{
			Type:      statemanager.OrderUpdateEvent,
			Timestamp: time.Now(),
			Data:      orderUpdateEvent,
		})
	case "ACCOUNT_UPDATE":
		// Placeholder for handling account updates if needed in the future.
	case "TRADE_LITE":
		// This is a public trade event, not specific to our orders. We can safely ignore it.
	default:
		// Optionally log unknown event types for future analysis.
	}
}

// handleOrderUpdate is now called sequentially by the event processor.
// It no longer needs to manage its own concurrency with goroutines or the isRebuilding flag.

// rebuildGrid cancels old orders and creates a new set of orders around a new center price.
func (b *GridTradingBot) rebuildGrid(triggeredDef models.GridLevelDefinition) error {
	logger.S().Infof("--- Rebuilding grid, triggered by LevelID: %d ---", triggeredDef.ID)

	// 1. Cancel all existing open orders.
	b.cancelAllActiveOrders()
	time.Sleep(2 * time.Second) // Wait for cancellations to propagate.

	// 2. Get a fresh snapshot of the state.
	state := b.stateManager.GetStateSnapshot()
	if state == nil {
		return errors.New("cannot rebuild grid, state is not available")
	}

	// 3. The new center price is the price of the level that was just filled.
	newCenterPrice := triggeredDef.Price
	logger.S().Infof("New center price for grid rebuild: %.4f", newCenterPrice)

	// 4. Create the new set of orders.
	definitions := state.InitialParams.GridDefinition

	// Find the index of the grid level closest to the new center price
	closestIndex := -1
	minDiff := math.MaxFloat64
	for i, def := range definitions {
		diff := math.Abs(def.Price - newCenterPrice)
		if diff < minDiff {
			minDiff = diff
			closestIndex = i
		}
	}
	if closestIndex == -1 {
		return errors.New("could not find a center grid level for rebuild")
	}

	// Determine the range of orders to place
	startIndex := max(0, closestIndex-b.config.ActiveOrdersCount)
	endIndex := min(len(definitions)-1, closestIndex+b.config.ActiveOrdersCount)

	logger.S().Infof("Rebuilding orders from index %d to %d (center index: %d, active count: %d)", startIndex, endIndex, closestIndex, b.config.ActiveOrdersCount)

	for i := startIndex; i <= endIndex; i++ {
		def := definitions[i]
		if i == closestIndex {
			logger.S().Infof("Skipping grid level at index %d (price %.4f) as it's the new center.", i, def.Price)
			continue
		}

		// Dispatch an event to request placing an order, instead of calling it directly.
		b.stateManager.DispatchEvent(statemanager.NormalizedEvent{
			Type:      statemanager.PlaceOrderEvent,
			Timestamp: time.Now(),
			Data: statemanager.PlaceOrderEventData{
				Level: def,
			},
		})
	}

	logger.S().Info("--- Grid rebuild complete, new order requests dispatched ---")
	return nil
}

// setupInitialGrid places the initial grid orders around a center price without cancelling existing orders.
// This is intended for the very first grid setup after the initial position is established.
// setupInitialGrid places the initial set of buy and sell limit orders based on the state.
// It now reads directly from b.state and places orders concurrently.
func (b *GridTradingBot) setupInitialGrid(state *models.BotState) error {
	// This function now reads from the passed-in state and dispatches events
	// instead of modifying state directly.
	gridDefinitions := state.InitialParams.GridDefinition
	currentPrice := b.currentPrice

	if len(gridDefinitions) == 0 {
		return fmt.Errorf("grid definition is empty, cannot set up grid")
	}

	// Find the index of the grid level closest to the current price
	closestIndex := -1
	minDiff := math.MaxFloat64
	for i, level := range gridDefinitions {
		diff := math.Abs(level.Price - currentPrice)
		if diff < minDiff {
			minDiff = diff
			closestIndex = i
		}
	}

	if closestIndex == -1 {
		return fmt.Errorf("could not find closest grid level to current price %.4f", currentPrice)
	}

	// Place orders for the levels around the current price
	startIndex := max(0, closestIndex-b.config.GridCount/2)
	endIndex := min(len(gridDefinitions)-1, closestIndex+b.config.GridCount/2)

	for i := startIndex; i <= endIndex; i++ {
		def := gridDefinitions[i]

		// Skip placing orders that are on the "wrong" side of the current price
		if (def.Side == models.Buy && def.Price >= currentPrice) || (def.Side == models.Sell && def.Price <= currentPrice) {
			logger.S().Debugf("Skipping order for LevelID %d (Price: %.4f, Side: %s) as it's on the wrong side of current price %.4f", def.ID, def.Price, def.Side, currentPrice)
			continue
		}

		// Dispatch an event to request placing an order, instead of calling it directly.
		b.stateManager.DispatchEvent(statemanager.NormalizedEvent{
			Type:      statemanager.PlaceOrderEvent,
			Timestamp: time.Now(),
			Data: statemanager.PlaceOrderEventData{
				Level: def,
			},
		})
	}

	logger.S().Info("Finished dispatching initial grid order requests.")
	return nil
}

// enterSafeMode puts the bot into a safe mode where it stops trading
func (b *GridTradingBot) enterSafeMode(reason string) {
	if b.isHalted {
		return
	}
	b.isHalted = true
	b.safeModeReason = reason
	logger.S().Errorf("--- Entering Safe Mode ---")
	logger.S().Errorf("Reason: %s", reason)
	logger.S().Errorf("Bot has stopped all trading activity. Manual intervention required.")
	go b.cancelAllActiveOrders()
}
