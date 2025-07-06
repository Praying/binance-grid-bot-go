package statemanager

import (
	"binance-grid-bot-go/internal/models"
	"binance-grid-bot-go/internal/persistence"
	"strconv"
	"time"

	"go.uber.org/zap"
)

// EventType defines the type of a normalized event
type EventType int

const (
	OrderUpdateEvent EventType = iota
	StateResetEvent
	UpdateGridLevelEvent
	PlaceOrderEvent
)

// OrderPlacer defines the interface for placing orders.
// This is used to break the circular dependency between StateManager and GridTradingBot.
type OrderPlacer interface {
	PlaceNewOrder(level models.GridLevelDefinition)
}

// NormalizedEvent is a standardized internal representation of an event
type NormalizedEvent struct {
	Type      EventType
	Timestamp time.Time
	Data      interface{}
}

// UpdateGridLevelEventData is the data structure for updating a single grid level's state.
type UpdateGridLevelEventData struct {
	LevelID int
	State   *models.GridLevelState
}

// PlaceOrderEventData is the data for a request to place a new order.
type PlaceOrderEventData struct {
	Level models.GridLevelDefinition
}

// StateManager is responsible for all state mutations and persistence.
// It ensures that all state changes are processed serially.
type StateManager struct {
	state           *models.BotState
	repo            persistence.StateRepository
	orderPlacer     OrderPlacer // Use interface to break dependency
	eventChannel    chan NormalizedEvent
	persistenceChan chan *models.BotState
	stopChan        chan bool
	logger          *zap.Logger
}

// NewStateManager creates a new StateManager.
func NewStateManager(initialState *models.BotState, repo persistence.StateRepository, orderPlacer OrderPlacer, logger *zap.Logger) *StateManager {
	return &StateManager{
		state:           initialState,
		repo:            repo,
		orderPlacer:     orderPlacer,
		eventChannel:    make(chan NormalizedEvent, 1024), // Buffered channel
		persistenceChan: make(chan *models.BotState, 128), // Buffered channel for state snapshots to be persisted
		stopChan:        make(chan bool),
		logger:          logger,
	}
}

// Start begins the state manager's event processing and persistence loops.
func (sm *StateManager) Start() {
	go sm.eventLoop()
	go sm.persistenceLoop()
	sm.logger.Sugar().Info("StateManager started.")
}

// Stop gracefully shuts down the StateManager.
func (sm *StateManager) Stop() {
	close(sm.stopChan)
	sm.logger.Sugar().Info("StateManager stopped.")
}

// DispatchEvent sends an event to the StateManager for processing.
func (sm *StateManager) DispatchEvent(event NormalizedEvent) {
	sm.eventChannel <- event
}

// GetStateSnapshot returns a deep copy of the current state for safe, concurrent reading.
func (sm *StateManager) GetStateSnapshot() *models.BotState {
	return sm.deepCopy()
}

// deepCopy creates a deep copy of the BotState to prevent data races.
func (sm *StateManager) deepCopy() *models.BotState {
	if sm.state == nil {
		return nil
	}

	// Start with a shallow copy of the state
	stateCopy := *sm.state

	// Deep copy InitialParams.GridDefinition
	if sm.state.InitialParams.GridDefinition != nil {
		stateCopy.InitialParams.GridDefinition = make([]models.GridLevelDefinition, len(sm.state.InitialParams.GridDefinition))
		copy(stateCopy.InitialParams.GridDefinition, sm.state.InitialParams.GridDefinition)
	}

	// Deep copy CurrentCycle.GridStates
	if sm.state.CurrentCycle.GridStates != nil {
		stateCopy.CurrentCycle.GridStates = make(map[int]*models.GridLevelState, len(sm.state.CurrentCycle.GridStates))
		for k, v := range sm.state.CurrentCycle.GridStates {
			if v != nil {
				// Copy the GridLevelState struct
				levelStateCopy := *v
				stateCopy.CurrentCycle.GridStates[k] = &levelStateCopy
			}
		}
	}

	return &stateCopy
}

// eventLoop is the core processing loop that handles all incoming events serially.
func (sm *StateManager) eventLoop() {
	for {
		select {
		case event := <-sm.eventChannel:
			sm.processEvent(event)
		case <-sm.stopChan:
			return
		}
	}
}

// persistenceLoop handles the asynchronous saving of state snapshots.
func (sm *StateManager) persistenceLoop() {
	for {
		select {
		case stateToSave := <-sm.persistenceChan:
			if sm.repo != nil {
				if err := sm.repo.SaveState(stateToSave); err != nil {
					sm.logger.Sugar().Errorf("CRITICAL: Failed to save state: %v", err)
					// In a real-world scenario, you might want to trigger a safe mode or retry logic here.
				}
			}
		case <-sm.stopChan:
			return
		}
	}
}

// processEvent contains the logic to mutate the state based on an event.
func (sm *StateManager) processEvent(event NormalizedEvent) {
	switch event.Type {
	case OrderUpdateEvent:
		if orderUpdate, ok := event.Data.(models.OrderUpdateEvent); ok {
			sm.handleOrderUpdate(orderUpdate)
		} else {
			sm.logger.Sugar().Warnf("Received OrderUpdateEvent with unexpected data type: %T", event.Data)
		}
	case StateResetEvent:
		if newState, ok := event.Data.(*models.BotState); ok {
			sm.state = newState
			sm.logger.Sugar().Info("State has been reset.")
		} else {
			sm.logger.Sugar().Warnf("Received StateResetEvent with unexpected data type: %T", event.Data)
		}
	case UpdateGridLevelEvent:
		if data, ok := event.Data.(UpdateGridLevelEventData); ok {
			if sm.state != nil && sm.state.CurrentCycle.GridStates != nil {
				sm.state.CurrentCycle.GridStates[data.LevelID] = data.State
			}
		} else {
			sm.logger.Sugar().Warnf("Received UpdateGridLevelEvent with unexpected data type: %T", event.Data)
		}
	case PlaceOrderEvent:
		if data, ok := event.Data.(PlaceOrderEventData); ok {
			// The StateManager now tells the bot to place an order.
			sm.orderPlacer.PlaceNewOrder(data.Level)
		} else {
			sm.logger.Sugar().Warnf("Received PlaceOrderEvent with unexpected data type: %T", event.Data)
		}
	}

	sm.state.LastUpdateTime = time.Now()

	// After processing, send a deep copy of the new state to the persistence channel.
	if stateCopy := sm.deepCopy(); stateCopy != nil {
		sm.persistenceChan <- stateCopy
	}
}

func (sm *StateManager) handleOrderUpdate(event models.OrderUpdateEvent) {
	if event.Order.ExecutionType != "TRADE" || event.Order.Status != "FILLED" {
		return
	}

	o := event.Order
	sm.logger.Sugar().Info("--- Processing Order Fill Event in StateManager ---")
	sm.logger.Sugar().Infof("Client Order ID: %s, Symbol: %s, Side: %s, Price: %s, Quantity: %s",
		o.ClientOrderID, o.Symbol, o.Side, o.Price, o.OrigQty)

	var matchedState *models.GridLevelState
	var matchedDef *models.GridLevelDefinition

	// Find the triggered grid level state by ClientOrderID
	for _, state := range sm.state.CurrentCycle.GridStates {
		if state.OrderID == o.ClientOrderID {
			matchedState = state
			break
		}
	}

	if matchedState == nil {
		sm.logger.Sugar().Warnf("Received fill for order %s, but no matching active grid state found.", o.ClientOrderID)
		return
	}

	// Find the corresponding definition
	for i := range sm.state.InitialParams.GridDefinition {
		if sm.state.InitialParams.GridDefinition[i].ID == matchedState.LevelID {
			def := sm.state.InitialParams.GridDefinition[i]
			matchedDef = &def
			break
		}
	}

	if matchedDef == nil {
		sm.logger.Sugar().Errorf("CRITICAL: Found state for LevelID %d but no corresponding definition.", matchedState.LevelID)
		return
	}

	sm.logger.Sugar().Infof("Matched active grid order: LevelID %d, Price %.4f", matchedDef.ID, matchedDef.Price)

	// Update state
	matchedState.Status = "FILLED"
	matchedState.LastUpdateTime = time.Now()

	filledPrice, _ := strconv.ParseFloat(o.Price, 64)
	filledQty, _ := strconv.ParseFloat(o.OrigQty, 64)

	if models.Side(o.Side) == models.Buy {
		sm.state.CurrentCycle.PositionSize += filledQty
	} else {
		sm.state.CurrentCycle.PositionSize -= filledQty
	}

	reversionPrice := sm.state.CurrentCycle.RegressionPrice

	// Cycle completion check
	if filledPrice >= reversionPrice && models.Side(o.Side) == models.Sell {
		sm.logger.Sugar().Infof("Price %.4f has reached or exceeded reversion price %.4f. Triggering cycle restart.", filledPrice, reversionPrice)
		// This logic will be handled by the bot's main strategy loop.
		// For now, we just log it. A more robust implementation might send a specific event.
	} else {
		// The bot's strategy loop will handle this by observing the state.
		// No direct signal is needed here anymore.
	}
}
