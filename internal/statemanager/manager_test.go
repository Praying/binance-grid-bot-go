package statemanager

import (
	"binance-grid-bot-go/internal/models"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// mockStateRepository is a mock implementation of the StateRepository interface for testing.
type mockStateRepository struct {
	sync.Mutex
	savedState   *models.BotState
	saveCalled   bool
	loadState    *models.BotState
	loadError    error
	saveError    error
	saveDoneChan chan bool // Channel to signal when SaveState is done
}

func newMockStateRepository() *mockStateRepository {
	return &mockStateRepository{
		saveDoneChan: make(chan bool, 1),
	}
}

func (m *mockStateRepository) SaveState(state *models.BotState) error {
	m.Lock()
	defer m.Unlock()

	// Deep copy the state to simulate real persistence and avoid race conditions in tests
	copiedState := *state
	if state.InitialParams.GridDefinition != nil {
		copiedState.InitialParams.GridDefinition = make([]models.GridLevelDefinition, len(state.InitialParams.GridDefinition))
		copy(copiedState.InitialParams.GridDefinition, state.InitialParams.GridDefinition)
	}
	if state.CurrentCycle.GridStates != nil {
		copiedState.CurrentCycle.GridStates = make(map[int]*models.GridLevelState, len(state.CurrentCycle.GridStates))
		for k, v := range state.CurrentCycle.GridStates {
			if v != nil {
				levelStateCopy := *v
				copiedState.CurrentCycle.GridStates[k] = &levelStateCopy
			}
		}
	}

	m.saveCalled = true
	m.savedState = &copiedState

	// Signal that save is complete
	m.saveDoneChan <- true

	return m.saveError
}

func (m *mockStateRepository) LoadState() (*models.BotState, error) {
	m.Lock()
	defer m.Unlock()
	return m.loadState, m.loadError
}

func (m *mockStateRepository) Close() error {
	return nil
}

func (m *mockStateRepository) getSavedState() *models.BotState {
	m.Lock()
	defer m.Unlock()
	return m.savedState
}

func (m *mockStateRepository) wasSaveCalled() bool {
	m.Lock()
	defer m.Unlock()
	return m.saveCalled
}

// mockOrderPlacer is a mock implementation of the OrderPlacer interface.
type mockOrderPlacer struct{}

func (m *mockOrderPlacer) PlaceNewOrder(level models.GridLevelDefinition) {
	// Mock implementation, does nothing.
}

// TestNewStateManager verifies that the StateManager is initialized correctly.
func TestNewStateManager(t *testing.T) {
	initialState := &models.BotState{BotID: "test-bot"}
	repo := newMockStateRepository()
	orderPlacer := &mockOrderPlacer{}
	logger := zap.NewNop()

	sm := NewStateManager(initialState, repo, orderPlacer, logger)
	require.NotNil(t, sm, "StateManager should not be nil")

	// Check if the initial state is set correctly
	snapshot := sm.GetStateSnapshot()
	require.NotNil(t, snapshot, "Initial state snapshot should not be nil")
	assert.Equal(t, "test-bot", snapshot.BotID, "Initial BotID should match")

	// Check if channels are created
	assert.NotNil(t, sm.eventChannel, "eventChannel should be created")
	assert.NotNil(t, sm.persistenceChan, "persistenceChan should be created")
	assert.NotNil(t, sm.stopChan, "stopChan should be created")
}

// TestStateResetEvent tests the handling of a StateResetEvent.
func TestStateResetEvent(t *testing.T) {
	initialState := &models.BotState{BotID: "initial-bot"}
	repo := newMockStateRepository()
	orderPlacer := &mockOrderPlacer{}
	logger := zap.NewNop()

	sm := NewStateManager(initialState, repo, orderPlacer, logger)
	sm.Start()
	defer sm.Stop()

	// Create a new state to reset to
	newState := &models.BotState{
		BotID:   "reset-bot",
		Version: 2,
		CurrentCycle: models.CycleState{
			PositionSize: 1.23,
		},
	}

	// Dispatch the reset event
	sm.DispatchEvent(NormalizedEvent{
		Type:      StateResetEvent,
		Timestamp: time.Now(),
		Data:      newState,
	})

	// Wait for the persistence to complete
	select {
	case <-repo.saveDoneChan:
		// Save completed
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for state to be saved")
	}

	// Verify the state was updated
	snapshot := sm.GetStateSnapshot()
	require.NotNil(t, snapshot)
	assert.Equal(t, "reset-bot", snapshot.BotID)
	assert.Equal(t, 2, snapshot.Version)
	assert.Equal(t, 1.23, snapshot.CurrentCycle.PositionSize)

	// Verify that the state was persisted
	assert.True(t, repo.wasSaveCalled(), "SaveState should have been called after a state reset")
	saved := repo.getSavedState()
	require.NotNil(t, saved)
	assert.Equal(t, "reset-bot", saved.BotID)
}

// TestUpdateGridLevelEvent tests the handling of an UpdateGridLevelEvent.
func TestUpdateGridLevelEvent(t *testing.T) {
	initialState := &models.BotState{
		BotID: "grid-test-bot",
		CurrentCycle: models.CycleState{
			GridStates: make(map[int]*models.GridLevelState),
		},
	}
	repo := newMockStateRepository()
	orderPlacer := &mockOrderPlacer{}
	logger := zap.NewNop()

	sm := NewStateManager(initialState, repo, orderPlacer, logger)
	sm.Start()
	defer sm.Stop()

	// Data for the new grid level
	levelID := 10
	newLevelState := &models.GridLevelState{
		LevelID: levelID,
		OrderID: "order-123",
		Status:  "OPEN",
	}

	// Dispatch the update event
	sm.DispatchEvent(NormalizedEvent{
		Type:      UpdateGridLevelEvent,
		Timestamp: time.Now(),
		Data: UpdateGridLevelEventData{
			LevelID: levelID,
			State:   newLevelState,
		},
	})

	// Wait for the persistence to complete
	select {
	case <-repo.saveDoneChan:
		// Save completed
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for state to be saved")
	}

	// Verify the grid state was updated
	snapshot := sm.GetStateSnapshot()
	require.NotNil(t, snapshot)
	require.Contains(t, snapshot.CurrentCycle.GridStates, levelID)
	assert.Equal(t, "order-123", snapshot.CurrentCycle.GridStates[levelID].OrderID)
	assert.Equal(t, "OPEN", snapshot.CurrentCycle.GridStates[levelID].Status)

	// Verify that the state was persisted
	assert.True(t, repo.wasSaveCalled(), "SaveState should have been called after a grid level update")
	saved := repo.getSavedState()
	require.NotNil(t, saved)
	require.Contains(t, saved.CurrentCycle.GridStates, levelID)
	assert.Equal(t, "order-123", saved.CurrentCycle.GridStates[levelID].OrderID)
}

// TestAsyncPersistence verifies that state persistence happens asynchronously.
func TestAsyncPersistence(t *testing.T) {
	initialState := &models.BotState{BotID: "async-test"}
	repo := newMockStateRepository()
	orderPlacer := &mockOrderPlacer{}
	logger := zap.NewNop()

	sm := NewStateManager(initialState, repo, orderPlacer, logger)
	sm.Start()
	defer sm.Stop()

	// Dispatch an event that will trigger persistence
	sm.DispatchEvent(NormalizedEvent{
		Type:      StateResetEvent,
		Timestamp: time.Now(),
		Data:      &models.BotState{BotID: "new-state"},
	})

	// --- Verification Point 1: Check that Save is NOT called synchronously ---
	// Immediately after dispatching, the save function should not have been called yet.
	assert.False(t, repo.wasSaveCalled(), "SaveState should not be called synchronously with DispatchEvent")

	// --- Verification Point 2: Wait for the async save to complete ---
	select {
	case <-repo.saveDoneChan:
		// This confirms that SaveState was eventually called by the persistenceLoop.
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for async SaveState call")
	}

	// --- Verification Point 3: Check that the correct state was saved ---
	assert.True(t, repo.wasSaveCalled(), "SaveState should have been called asynchronously")
	savedState := repo.getSavedState()
	require.NotNil(t, savedState)
	assert.Equal(t, "new-state", savedState.BotID)
}
