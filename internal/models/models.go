package models

import "time"

// Config 结构体定义了机器人的所有配置参数
type Config struct {
	IsTestnet           bool      `json:"is_testnet"` // 是否使用测试网
	LiveAPIURL          string    `json:"live_api_url"`
	LiveWSURL           string    `json:"live_ws_url"`
	TestnetAPIURL       string    `json:"testnet_api_url"`
	TestnetWSURL        string    `json:"testnet_ws_url"`
	Symbol              string    `json:"symbol"`                // 交易对，如 "BTCUSDT"
	GridSpacing         float64   `json:"grid_spacing"`          // 网格间距比例
	GridValue           float64   `json:"grid_value"`            // 每个网格的交易价值 (USDT)
	InitialInvestment   float64   `json:"initial_investment"`    // 初始投资额 (USDT), 用于市价买入
	Leverage            int       `json:"leverage"`              // 杠杆倍数
	GridCount           int       `json:"grid_count"`            // 网格数量（对）
	ActiveOrdersCount   int       `json:"active_orders_count"`   // 在价格两侧各挂的订单数量
	ReturnRate          float64   `json:"return_rate"`           // 预期回归价格比例
	WalletExposureLimit float64   `json:"wallet_exposure_limit"` // 新增：钱包风险暴露上限
	LogConfig           LogConfig `json:"log"`                   // 新增：日志配置

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
	UnRealizedProfit string `json:"unRealizedProfit"`
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
