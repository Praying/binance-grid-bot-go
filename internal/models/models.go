package models

import (
	"fmt"
	"time"
)

// Config 结构体定义了机器人的所有配置参数
type Config struct {
	IsTestnet                bool      `json:"is_testnet"` // 是否使用测试网
	DBPath                   string    `json:"db_path"`    // 新增：数据库文件路径
	LiveAPIURL               string    `json:"live_api_url"`
	LiveWSURL                string    `json:"live_ws_url"`
	TestnetAPIURL            string    `json:"testnet_api_url"`
	TestnetWSURL             string    `json:"testnet_ws_url"`
	Symbol                   string    `json:"symbol"`                                // 交易对，如 "BTCUSDT"
	GridSpacing              float64   `json:"grid_spacing"`                          // 网格间距比例
	GridValue                float64   `json:"grid_value,omitempty"`                  // 每个网格的交易价值 (USDT)
	GridQuantity             float64   `json:"grid_quantity,omitempty"`               // 新增：每个网格的交易数量（基础货币）
	MinNotionalValue         float64   `json:"min_notional_value"`                    // 新增: 交易所最小订单名义价值 (例如 5 USDT)
	InitialInvestment        float64   `json:"initial_investment"`                    // 初始投资额 (USDT), 用于市价买入
	Leverage                 int       `json:"leverage"`                              // 杠杆倍数
	MarginType               string    `json:"margin_type"`                           // 保证金模式: CROSSED 或 ISOLATED
	PositionMode             string    `json:"position_mode"`                         // 新增: 持仓模式, "Oneway" 或 "Hedge"
	StopLossRate             float64   `json:"stop_loss_rate,omitempty"`              // 新增: 止损率
	HedgeMode                bool      `json:"hedge_mode"`                            // 是否开启对冲模式 (双向持仓)
	GridCount                int       `json:"grid_count"`                            // 网格数量（对）
	ActiveOrdersCount        int       `json:"active_orders_count"`                   // 在价格两侧各挂的订单数量
	ReturnRate               float64   `json:"return_rate"`                           // 预期回归价格比例
	WalletExposureLimit      float64   `json:"wallet_exposure_limit"`                 // 新增：钱包风险暴露上限
	LogConfig                LogConfig `json:"log"`                                   // 新增：日志配置
	RetryAttempts            int       `json:"retry_attempts"`                        // 新增: 下单失败时的重试次数
	RetryInitialDelayMs      int       `json:"retry_initial_delay_ms"`                // 新增: 重试前的初始延迟毫秒数
	WebSocketPingIntervalSec int       `json:"websocket_ping_interval_sec,omitempty"` // 新增: WebSocket Ping消息发送间隔(秒)
	WebSocketPongTimeoutSec  int       `json:"websocket_pong_timeout_sec,omitempty"`  // 新增: WebSocket Pong消息超时时间(秒)

	// 回测引擎特定配置
	TakerFeeRate          float64 `json:"taker_fee_rate"`          // 吃单手续费率
	MakerFeeRate          float64 `json:"maker_fee_rate"`          // 挂单手续费率
	SlippageRate          float64 `json:"slippage_rate"`           // 滑点率
	MaintenanceMarginRate float64 `json:"maintenance_margin_rate"` // 维持保证金率

	BaseURL   string `json:"base_url"`    // REST API基础地址 (将由程序动态设置)
	WSBaseURL string `json:"ws_base_url"` // WebSocket基础地址 (将由程序动态设置)
}

// LogConfig 定义了日志相关的配置
type LogConfig struct {
	Level      string `json:"level"`       // 日志级别, e.g., "debug", "info", "warn", "error"
	Output     string `json:"output"`      // 输出模式: "console", "file", "both"
	File       string `json:"file"`        // 日志文件路径
	MaxSize    int    `json:"max_size"`    // 单个日志文件的最大大小 (MB)
	MaxBackups int    `json:"max_backups"` // 保留的旧日志文件最大数量
	MaxAge     int    `json:"max_age"`     // 旧日志文件的最大保留天数
	Compress   bool   `json:"compress"`    // 是否压缩旧日志文件
}

// AccountInfo 定义了币安账户信息
type AccountInfo struct {
	TotalWalletBalance string `json:"totalWalletBalance"`
	AvailableBalance   string `json:"availableBalance"`
	Assets             []struct {
		Asset                  string `json:"asset"`
		WalletBalance          string `json:"walletBalance"`
		UnrealizedProfit       string `json:"unrealizedProfit"`
		MarginBalance          string `json:"marginBalance"`
		MaintMargin            string `json:"maintMargin"`
		InitialMargin          string `json:"initialMargin"`
		PositionInitialMargin  string `json:"positionInitialMargin"`
		OpenOrderInitialMargin string `json:"openOrderInitialMargin"`
		MaxWithdrawAmount      string `json:"maxWithdrawAmount"`
	} `json:"assets"`
}

// Position 定义了持仓信息
type Position struct {
	Symbol           string `json:"symbol"`
	PositionAmt      string `json:"positionAmt"`
	EntryPrice       string `json:"entryPrice"`
	MarkPrice        string `json:"markPrice"`
	UnrealizedProfit string `json:"unRealizedProfit"`
	LiquidationPrice string `json:"liquidationPrice"`
	Leverage         string `json:"leverage"`
	MaxNotionalValue string `json:"maxNotionalValue"`
	MarginType       string `json:"marginType"`
	IsolatedMargin   string `json:"isolatedMargin"`
	IsAutoAddMargin  string `json:"isAutoAddMargin"`
	PositionSide     string `json:"positionSide"`
	Notional         string `json:"notional"`
	IsolatedWallet   string `json:"isolatedWallet"`
	UpdateTime       int64  `json:"updateTime"`
}

// Order 定义了订单信息
type Order struct {
	Symbol        string `json:"symbol"`
	OrderId       int64  `json:"orderId"`
	ClientOrderId string `json:"clientOrderId"`
	Price         string `json:"price"`
	OrigQty       string `json:"origQty"`
	ExecutedQty   string `json:"executedQty"`
	CumQuote      string `json:"cumQuote"`
	Status        string `json:"status"`
	TimeInForce   string `json:"timeInForce"`
	Type          string `json:"type"`
	Side          string `json:"side"`
	StopPrice     string `json:"stopPrice"`
	IcebergQty    string `json:"icebergQty"`
	Time          int64  `json:"time"`
	UpdateTime    int64  `json:"updateTime"`
	IsWorking     bool   `json:"isWorking"`
	WorkingType   string `json:"workingType"`
	OrigType      string `json:"origType"`
	PositionSide  string `json:"positionSide"`
	ActivatePrice string `json:"activatePrice"`
	PriceRate     string `json:"priceRate"`
	ReduceOnly    bool   `json:"reduceOnly"`
	ClosePosition bool   `json:"closePosition"`
	PriceProtect  bool   `json:"priceProtect"`
}

// GridLevel 代表网格中的一个价格档位
type GridLevel struct {
	Price           float64 `json:"price"`
	Quantity        float64 `json:"quantity"`
	Side            string  `json:"side"`
	IsActive        bool    `json:"is_active"`
	OrderID         int64   `json:"order_id"`
	GridID          int     `json:"grid_id"`                     // 新增：记录该订单关联的理论网格ID (conceptualGrid的索引)
	PairID          int     `json:"pair_id"`                     // 用于配对买单和卖单
	PairedSellPrice float64 `json:"paired_sell_price,omitempty"` // 仅在买单中使用，记录其对应的卖出价
}

// CompletedTrade 记录一笔完成的交易（买入和卖出）
type CompletedTrade struct {
	Symbol       string
	Quantity     float64
	EntryTime    time.Time
	ExitTime     time.Time
	HoldDuration time.Duration // 新增：持仓时长
	EntryPrice   float64
	ExitPrice    float64 // 新增：记录卖出价格
	Profit       float64
	Fee          float64 // 新增：单笔交易手续费
	Slippage     float64 // 新增：单笔交易滑点成本
}

// BuyTrade records the details of a single buy-in for FIFO accounting.
type BuyTrade struct {
	Timestamp time.Time
	Quantity  float64
	Price     float64
}

// ExchangeInfo holds the full exchange information response
type ExchangeInfo struct {
	Symbols []SymbolInfo `json:"symbols"`
}

// SymbolInfo holds trading rules for a single symbol
type SymbolInfo struct {
	Symbol  string   `json:"symbol"`
	Filters []Filter `json:"filters"`
}

// Filter holds filter data, we are interested in PRICE_FILTER and LOT_SIZE
type Filter struct {
	FilterType  string `json:"filterType"`
	TickSize    string `json:"tickSize,omitempty"`    // For PRICE_FILTER
	StepSize    string `json:"stepSize,omitempty"`    // For LOT_SIZE
	MinQty      string `json:"minQty,omitempty"`      // For LOT_SIZE
	MaxQty      string `json:"maxQty,omitempty"`      // For LOT_SIZE
	MinNotional string `json:"minNotional,omitempty"` // For MIN_NOTIONAL
}

// Trade 定义了单次成交的信息
type Trade struct {
	Symbol          string `json:"symbol"`
	ID              int64  `json:"id"`
	OrderID         int64  `json:"orderId"`
	Side            string `json:"side"`
	Price           string `json:"price"`
	Qty             string `json:"qty"`
	RealizedPnl     string `json:"realizedPnl"`
	MarginAsset     string `json:"marginAsset"`
	QuoteQty        string `json:"quoteQty"`
	Commission      string `json:"commission"`
	CommissionAsset string `json:"commissionAsset"`
	Time            int64  `json:"time"`
	PositionSide    string `json:"positionSide"`
	Buyer           bool   `json:"buyer"`
	Maker           bool   `json:"maker"`
}

// TradeEvent 定义了来自 WebSocket 的交易事件
type TradeEvent struct {
	EventType string `json:"e"` // Event type
	EventTime int64  `json:"E"` // Event time
	Symbol    string `json:"s"` // Symbol
	TradeID   int64  `json:"a"` // Aggregate trade ID
	Price     string `json:"p"` // Price
	Quantity  string `json:"q"` // Quantity
	FirstID   int64  `json:"f"` // First trade ID
	LastID    int64  `json:"l"` // Last trade ID
	TradeTime int64  `json:"T"` // Trade time
	IsMaker   bool   `json:"m"` // Is the buyer the market maker?
}

// UserDataEvent 是从用户数据流接收到的通用事件结构
type UserDataEvent struct {
	EventType string `json:"e"` // Event type, e.g., "executionReport"
	EventTime int64  `json:"E"` // Event time
	// 根据事件类型，这里可以包含不同的负载
	ExecutionReport ExecutionReport `json:"o"`
}

// ExecutionReport 包含了订单更新的详细信息
type ExecutionReport struct {
	Symbol        string `json:"s"`  // Symbol
	ClientOrderID string `json:"c"`  // Client Order ID
	Side          string `json:"S"`  // Side
	OrderType     string `json:"o"`  // Order Type
	TimeInForce   string `json:"f"`  // Time in Force
	OrigQty       string `json:"q"`  // Original Quantity
	Price         string `json:"p"`  // Price
	AvgPrice      string `json:"ap"` // Average Price
	StopPrice     string `json:"sp"` // Stop Price
	ExecType      string `json:"x"`  // Execution Type
	OrderStatus   string `json:"X"`  // Order Status
	OrderID       int64  `json:"i"`  // Order ID
	ExecutedQty   string `json:"l"`  // Last Executed Quantity
	CumQty        string `json:"z"`  // Cumulative Filled Quantity
	ExecutedPrice string `json:"L"`  // Last Executed Price
	CommissionAmt string `json:"n"`  // Commission Amount
	CommissionAs  string `json:"N"`  // Commission Asset
	TradeTime     int64  `json:"T"`  // Trade Time
	TradeID       int64  `json:"t"`  // Trade ID
}

// BotState represents the persisted state of the trading bot's current cycle.
type BotState struct {
	ID                   int       `json:"id"`
	CurrentCycleID       int64     `json:"current_cycle_id"`
	Status               string    `json:"status"`
	EntryPrice           float64   `json:"entry_price"`
	ReversionPrice       float64   `json:"reversion_price"`
	GridLevels           string    `json:"grid_levels"`
	ConceptualGrid       string    `json:"conceptual_grid"`
	BasePositionSnapshot bool      `json:"base_position_snapshot"`
	LastUpdateTime       time.Time `json:"last_update_time"`
}

// Balance 定义了账户中特定资产的余额信息
type Balance struct {
	Asset              string `json:"asset"`
	Balance            string `json:"balance"`
	CrossWalletBalance string `json:"crossWalletBalance"`
	AvailableBalance   string `json:"availableBalance"`
}

// Error 定义了币安API返回的错误信息结构
type Error struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// Error 方法使得 BinanceError 实现了 error 接口
func (e *Error) Error() string {
	return fmt.Sprintf("API Error: code=%d, msg=%s", e.Code, e.Msg)
}

// OrderUpdateEvent 是从用户数据流接收到的订单更新事件的完整结构
type OrderUpdateEvent struct {
	EventType       string          `json:"e"` // Event type, e.g., "ORDER_TRADE_UPDATE"
	EventTime       int64           `json:"E"` // Event time
	TransactionTime int64           `json:"T"` // Transaction time
	Order           OrderUpdateInfo `json:"o"` // Order information
}

// OrderUpdateInfo 包含了订单更新的具体信息
type OrderUpdateInfo struct {
	Symbol          string `json:"s"`  // Symbol
	ClientOrderID   string `json:"c"`  // Client Order ID
	Side            string `json:"S"`  // Side
	OrderType       string `json:"o"`  // Order Type
	TimeInForce     string `json:"f"`  // Time in Force
	OrigQty         string `json:"q"`  // Original Quantity
	Price           string `json:"p"`  // Price
	AvgPrice        string `json:"ap"` // Average Price
	StopPrice       string `json:"sp"` // Stop Price
	ExecutionType   string `json:"x"`  // Execution Type
	Status          string `json:"X"`  // Order Status
	OrderID         int64  `json:"i"`  // Order ID
	ExecutedQty     string `json:"l"`  // Last Executed Quantity
	CumQty          string `json:"z"`  // Cumulative Filled Quantity
	ExecutedPrice   string `json:"L"`  // Last Executed Price
	CommissionAmt   string `json:"n"`  // Commission Amount
	CommissionAsset string `json:"N"`  // Commission Asset, will be null if not traded
	TradeTime       int64  `json:"T"`  // Trade Time
	TradeID         int64  `json:"t"`  // Trade ID
	BidsNotional    string `json:"b"`  // Bids Notional
	AsksNotional    string `json:"a"`  // Asks Notional
	IsMaker         bool   `json:"m"`  // Is the trade a maker trade?
	IsReduceOnly    bool   `json:"R"`  // Is this a reduce only order?
	WorkingType     string `json:"wt"` // Stop Price Working Type
	OrigType        string `json:"ot"` // Original Order Type
	PositionSide    string `json:"ps"` // Position Side
	ClosePosition   bool   `json:"cp"` // If conditional order, is it close position?
	ActivationPrice string `json:"AP"` // Activation Price, only available for TRAILING_STOP_MARKET order
	CallbackRate    string `json:"cr"` // Callback Rate, only available for TRAILING_STOP_MARKET order
	RealizedProfit  string `json:"rp"` // Realized Profit of the trade
}

// AccountUpdateEvent 代表了 ACCOUNT_UPDATE WebSocket 事件的完整结构。
type AccountUpdateEvent struct {
	EventType       string            `json:"e"` // 事件类型
	EventTime       int64             `json:"E"` // 事件时间
	TransactionTime int64             `json:"T"` // 撮合引擎交易时间
	UpdateData      AccountUpdateData `json:"a"` // 账户更新的具体数据
}

// AccountUpdateData 包含账户更新中的余额和仓位信息。
type AccountUpdateData struct {
	Reason    string           `json:"m"` // 事件发生的原因，例如 "ORDER"
	Balances  []BalanceUpdate  `json:"B"` // 余额更新列表
	Positions []PositionUpdate `json:"P"` // 仓位更新列表
}

// BalanceUpdate 代表单个资产的余额更新。
type BalanceUpdate struct {
	Asset              string `json:"a"`  // 资产名称
	WalletBalance      string `json:"wb"` // 钱包余额
	CrossWalletBalance string `json:"cw"` // 全仓账户钱包余额
	BalanceChange      string `json:"bc"` // 余额变化（不含盈亏和手续费）
}

// PositionUpdate 代表单个仓位的更新。
type PositionUpdate struct {
	Symbol              string `json:"s"`   // 交易对
	PositionAmount      string `json:"pa"`  // 仓位数量
	EntryPrice          string `json:"ep"`  // 开仓均价
	AccumulatedRealized string `json:"cr"`  // 累计已实现盈亏
	UnrealizedPnl       string `json:"up"`  // 未实现盈亏
	MarginType          string `json:"mt"`  // 保证金模式 (cross/isolated)
	IsolatedWallet      string `json:"iw"`  // 逐仓钱包余额
	PositionSide        string `json:"ps"`  // 持仓方向 (BOTH, LONG, SHORT)
	BreakEvenPrice      string `json:"bep"` // 盈亏平衡价
}
