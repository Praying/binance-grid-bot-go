package models

import "time"

// BotState 定义了需要持久化的所有关键数据
type BotState struct {
	BotID          string        `json:"bot_id"`           // Bot的唯一标识符
	Symbol         string        `json:"symbol"`           // 交易对, e.g., "BNBUSDT"
	Version        int           `json:"version"`          // 状态模型的版本号，用于未来迁移
	InitialParams  InitialParams `json:"initial_params"`   // Bot首次创建时的核心参数 (生命周期内不变)
	CurrentCycle   CycleState    `json:"current_cycle"`    // 当前交易周期的状态 (随交易活动变化)
	LastUpdateTime time.Time     `json:"last_update_time"` // 状态最后更新的时间戳
}

// InitialParams 存储Bot创建时的初始配置，是【不可变】的
type InitialParams struct {
	EntryPrice     float64               `json:"entry_price"`     // 初始入场价格
	GridDefinition []GridLevelDefinition `json:"grid_definition"` // 初始网格的静态定义
}

// GridLevelDefinition 定义了一个理论上的网格线。
// 它是策略的一部分，在一个交易周期内是【不可变】的。
type GridLevelDefinition struct {
	ID       int     `json:"id"`       // 网格线的唯一ID, e.g., 0, 1, 2...
	Price    float64 `json:"price"`    // 理论挂单价格
	Quantity float64 `json:"quantity"` // 理论挂单数量
	Side     Side    `json:"side"`     // 交易方向 (Buy/Sell)
}

// CycleState 代表一个交易周期的完整状态，是【高度可变】的
type CycleState struct {
	CycleID         string                  `json:"cycle_id"`         // 周期ID (e.g., UUID or Timestamp)
	RegressionPrice float64                 `json:"regression_price"` // 当前周期的回归价格/基准价格
	GridStates      map[int]*GridLevelState `json:"grid_states"`      // 【核心】存储所有网格线动态状态的Map
	PositionSize    float64                 `json:"position_size"`    // 当前持有的基础资产数量
	QuoteBalance    float64                 `json:"quote_balance"`    // 当前持有的计价货币余额
}

// GridLevelState 追踪一个理论网格线在运行时的【动态状态】。
type GridLevelState struct {
	LevelID        int       `json:"level_id"`           // 关联到 GridLevelDefinition 的 ID
	Status         string    `json:"status"`             // 状态: INACTIVE, PENDING, OPEN, FILLED, FAILED
	OrderID        string    `json:"order_id,omitempty"` // 关联的交易所订单ID
	LastUpdateTime time.Time `json:"last_update_time"`   // 状态最后更新时间
	RetryCount     int       `json:"retry_count"`        // 失败重试次数
}

// Side 定义了交易方向的类型
type Side string

const (
	Buy  Side = "BUY"
	Sell Side = "SELL"
)
