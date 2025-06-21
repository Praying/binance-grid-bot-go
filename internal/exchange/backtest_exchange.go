package exchange

import (
	"binance-grid-bot-go/internal/models"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
)

// BacktestExchange 实现了 Exchange 接口，用于模拟交易所行为以进行回测。
type BacktestExchange struct {
	Symbol            string // 新增：存储当前回测的交易对
	InitialBalance    float64
	Cash              float64
	CurrentPrice      float64   // Stores the close price of the current tick
	CurrentTime       time.Time // Stores the timestamp of the current tick
	Positions         map[string]float64
	AvgEntryPrice     map[string]float64
	positionEntryTime map[string]time.Time // 新增：用于跟踪持仓的开仓时间
	orders            map[int64]*models.Order
	TradeLog          []models.CompletedTrade
	EquityCurve       []float64
	dailyEquity       map[string]float64 // 新增：用于记录每日权益
	NextOrderID       int64
	Leverage          int
	mu                sync.Mutex

	// 回测引擎特定配置
	TakerFeeRate          float64 // 吃单手续费率
	MakerFeeRate          float64 // 挂单手续费率
	SlippageRate          float64 // 滑点率
	MaintenanceMarginRate float64 // 维持保证金率
	TotalFees             float64 // 累积总手续费
	MinNotionalValue      float64 // 新增：最小名义价值

	// 合约交易状态
	Margin            float64 // 仓位保证金
	UnrealizedPNL     float64 // 未实现盈亏
	LiquidationPrice  float64 // 预估爆仓价
	isLiquidated      bool    // 标记是否已爆仓 (私有)
	MaxWalletExposure float64 // 新增：记录回测期间最大的钱包风险暴露
}

// NewBacktestExchange 创建一个新的 BacktestExchange 实例。
func NewBacktestExchange(cfg *models.Config) *BacktestExchange {
	// 注意：在回测模式下，我们使用 InitialInvestment 作为起始资金
	return &BacktestExchange{
		Symbol:                cfg.Symbol,
		InitialBalance:        cfg.InitialInvestment,
		Cash:                  cfg.InitialInvestment,
		Positions:             make(map[string]float64),
		AvgEntryPrice:         make(map[string]float64),
		positionEntryTime:     make(map[string]time.Time),
		orders:                make(map[int64]*models.Order),
		TradeLog:              make([]models.CompletedTrade, 0),
		EquityCurve:           make([]float64, 0, 10000),
		dailyEquity:           make(map[string]float64),
		NextOrderID:           1,
		Leverage:              cfg.Leverage,
		Margin:                0,
		UnrealizedPNL:         0,
		LiquidationPrice:      0,
		isLiquidated:          false,
		TakerFeeRate:          cfg.TakerFeeRate,
		MakerFeeRate:          cfg.MakerFeeRate,
		SlippageRate:          cfg.SlippageRate,
		MaintenanceMarginRate: cfg.MaintenanceMarginRate,
		TotalFees:             0,
		MinNotionalValue:      cfg.MinNotionalValue,
		MaxWalletExposure:     0.0,
	}
}

// SetPrice 是回测的核心，模拟价格变动并触发订单成交检查。
// 通过模拟OHLC路径来提高回测精度
func (e *BacktestExchange) SetPrice(open, high, low, close float64, timestamp time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.CurrentTime = timestamp

	// 如果已爆仓，则停止所有活动
	if e.isLiquidated {
		return
	}

	// --- 核心逻辑顺序重构 ---

	// 步骤 1: 首先，按O->L->H->C的路径模拟价格变动并处理所有可能成交的订单
	// 这种方式比仅使用高低点更精确地模拟了K线内部的价格行为
	e.checkLimitOrdersAtPrice(open)
	e.checkLimitOrdersAtPrice(low)
	e.checkLimitOrdersAtPrice(high)
	e.checkLimitOrdersAtPrice(close)

	// 步骤 2: 然后，基于最新的持仓状态，更新当前价格和所有账户指标
	e.CurrentPrice = close
	e.updateMarginAndPNL() // 这将更新保证金、未实现盈亏，并在持仓归零时重置均价

	// 步骤 3: 最后，在所有状态都更新到最新之后，才检查是否触发爆仓
	if e.Positions[e.Symbol] > 0 && e.LiquidationPrice > 0 && low <= e.LiquidationPrice {
		e.CurrentPrice = e.LiquidationPrice
		e.handleLiquidation()
		e.updateEquity() // 爆仓后做最后一次权益更新
		return
	}

	// 如果没有爆仓，则正常更新权益曲线
	e.updateEquity()
}

// checkLimitOrdersAtPrice 遍历所有订单，检查是否有挂单可以在指定价格点成交。
func (e *BacktestExchange) checkLimitOrdersAtPrice(price float64) {
	// 注意：此函数现在应在已持有锁的情况下被调用
	var orderedIDs []int64
	for id := range e.orders {
		orderedIDs = append(orderedIDs, id)
	}
	sort.Slice(orderedIDs, func(i, j int) bool { return orderedIDs[i] < orderedIDs[j] })

	for _, orderID := range orderedIDs {
		order := e.orders[orderID]
		if order.Status == "NEW" && order.Type == "LIMIT" {
			limitPrice, _ := strconv.ParseFloat(order.Price, 64)
			shouldFill := false
			// 核心逻辑: 使用传入的单一价格点判断成交
			if order.Side == "BUY" && price <= limitPrice {
				shouldFill = true
			} else if order.Side == "SELL" && price >= limitPrice {
				shouldFill = true
			}

			if shouldFill {
				// 成交价应为挂单价，但为了模拟滑点，handleFilledOrder内部会处理
				e.handleFilledOrder(order)
			}
		}
	}
}

// handleFilledOrder 处理一个已成交的订单，更新账户状态。必须在持有锁的情况下调用。
func (e *BacktestExchange) handleFilledOrder(order *models.Order) {
	order.Status = "FILLED"

	limitPrice, _ := strconv.ParseFloat(order.Price, 64)
	quantity, _ := strconv.ParseFloat(order.OrigQty, 64)

	// --- 1. 计算包含滑点的成交价 ---
	var executionPrice float64
	if order.Side == "BUY" {
		executionPrice = limitPrice * (1 + e.SlippageRate)
	} else { // SELL
		executionPrice = limitPrice * (1 - e.SlippageRate)
	}
	// 市价单使用当前价作为基础
	if order.Type == "MARKET" {
		if order.Side == "BUY" {
			executionPrice = e.CurrentPrice * (1 + e.SlippageRate)
		} else {
			executionPrice = e.CurrentPrice * (1 - e.SlippageRate)
		}
	}

	// --- 2. 计算手续费 ---
	// 假设: LIMIT 单是 Maker, MARKET 单是 Taker
	feeRate := e.MakerFeeRate
	if order.Type == "MARKET" {
		feeRate = e.TakerFeeRate
	}
	fee := executionPrice * quantity * feeRate
	e.TotalFees += fee // 累加手续费
	e.Cash -= fee      // 无论开仓平仓，手续费总是支出

	// --- 3. 更新仓位和平均开仓价 ---
	currentPosition := e.Positions[order.Symbol]
	avgEntry := e.AvgEntryPrice[order.Symbol]

	if order.Side == "BUY" { // 开多仓或加仓
		// 如果这是第一笔仓位，记录开仓时间
		if currentPosition <= 1e-9 {
			e.positionEntryTime[order.Symbol] = e.CurrentTime
		}
		newTotalQty := currentPosition + quantity
		// 关键：新的开仓价值现在基于滑点后的成交价
		newTotalValue := (avgEntry * currentPosition) + (executionPrice * quantity)
		if newTotalQty > 0 {
			e.AvgEntryPrice[order.Symbol] = newTotalValue / newTotalQty
		}
		e.Positions[order.Symbol] = newTotalQty
	} else { // SELL (平多仓)
		// 关键修复: 只有在实际持有多仓时，平仓才有意义并能计算盈亏
		if currentPosition > 1e-9 { // 使用一个极小值来避免浮点数精度问题
			// 确保平仓数量不超过当前持仓量
			sellQuantity := quantity
			if sellQuantity > currentPosition {
				// 在当前策略中，这通常意味着一个逻辑错误(例如，尝试平掉比所拥有更多的仓位)
				// 我们将只平掉所有可用的仓位。
				// 未来可以考虑在这里添加日志记录。
				sellQuantity = currentPosition
			}

			// 只有在平仓时才计算已实现盈亏
			realizedPNL := (executionPrice - avgEntry) * sellQuantity
			e.Cash += realizedPNL // 已实现盈亏直接计入现金

			e.Positions[order.Symbol] -= sellQuantity

			entryTime := e.positionEntryTime[order.Symbol]
			holdDuration := e.CurrentTime.Sub(entryTime)
			slippageCost := (executionPrice - limitPrice) * sellQuantity

			e.TradeLog = append(e.TradeLog, models.CompletedTrade{
				Symbol:       e.Symbol,
				Quantity:     sellQuantity,
				EntryTime:    entryTime,
				ExitTime:     e.CurrentTime,
				HoldDuration: holdDuration,
				EntryPrice:   avgEntry,
				ExitPrice:    executionPrice,
				Profit:       realizedPNL - fee,
				Fee:          fee,
				Slippage:     slippageCost,
			})

			// 如果仓位已完全平掉，重置开仓时间
			if e.Positions[order.Symbol] <= 1e-9 {
				delete(e.positionEntryTime, order.Symbol)
			}
		}
		// 如果 currentPosition <= 0，我们不执行任何操作。
		// 这是为了防止在没有持仓的情况下（avgEntry 为 0），
		// 错误地将 `executionPrice * quantity` 记为利润。
	}

	// --- 4. 更新保证金、PNL和爆仓价 ---
	e.updateMarginAndPNL()
	if e.Positions[order.Symbol] > 1e-9 { // 使用一个极小值来避免浮点数精度问题
		e.calculateLiquidationPrice()
	} else {
		e.Positions[order.Symbol] = 0 // 仓位归零
		e.LiquidationPrice = 0        // 确保无持仓时爆仓价为0
	}

	// 打印详细的成交后状态
	equity := e.Cash + e.Margin + e.UnrealizedPNL

	// 计算 wallet exposure
	positionValue := e.Positions[order.Symbol] * e.CurrentPrice
	walletExposure := 0.0
	if equity > 0 {
		walletExposure = positionValue / equity
	}
	if walletExposure > e.MaxWalletExposure {
		e.MaxWalletExposure = walletExposure
	}

	fmt.Printf(`
--- [回测] 订单成交快照 ---
时间: %s
成交订单: %s %s @ %.4f, 数量: %.5f
--- 持仓状态 ---
新持仓量: %.5f %s
平均持仓成本: %.4f
钱包风险暴露: %.2f%%
--- 账户状态 ---
现金余额: %.4f
保证金: %.4f
未实现盈亏: %.4f
账户总权益: %.4f
--------------------------
`,
		e.CurrentTime.Format("2006-01-02 15:04:05"),
		order.Side, order.Type, executionPrice, quantity,
		e.Positions[order.Symbol], order.Symbol,
		e.AvgEntryPrice[order.Symbol],
		walletExposure*100, // 打印百分比
		e.Cash,
		e.Margin,
		e.UnrealizedPNL,
		equity,
	)
}

// --- 合约交易核心计算方法 --
// --- 合约交易核心计算方法 ---

// updateMarginAndPNL 根据当前价格、持仓和均价更新保证金和未实现盈亏。
// 必须在持有锁的情况下调用。
func (e *BacktestExchange) updateMarginAndPNL() {
	positionSize := e.Positions[e.Symbol]
	if positionSize > 0 {
		avgEntryPrice := e.AvgEntryPrice[e.Symbol]
		e.Margin = (avgEntryPrice * positionSize) / float64(e.Leverage)
		e.UnrealizedPNL = (e.CurrentPrice - avgEntryPrice) * positionSize
	} else {
		e.Margin = 0
		e.UnrealizedPNL = 0
		e.AvgEntryPrice[e.Symbol] = 0 // 关键修复：当持仓归零时，重置平均开仓价
	}
}

// calculateLiquidationPrice 计算并更新预估的爆仓价。
// 必须在持有锁的情况下调用。
func (e *BacktestExchange) calculateLiquidationPrice() {
	positionSize := e.Positions[e.Symbol]
	avgEntryPrice := e.AvgEntryPrice[e.Symbol]
	mmr := e.MaintenanceMarginRate
	walletBalance := e.Cash + e.Margin // 使用钱包余额，即初始保证金+已实现盈亏

	if positionSize > 1e-9 {
		// 币安官方爆仓价格公式:
		// LiqPrice = (EntryPrice * PositionSize - WalletBalance) / (PositionSize * (1 - MaintenanceMarginRate))
		// 这个公式适用于多头持仓。
		numerator := avgEntryPrice*positionSize - walletBalance
		denominator := positionSize * (1 - mmr)

		if denominator != 0 {
			e.LiquidationPrice = numerator / denominator
		} else {
			e.LiquidationPrice = -1 // 防止除以零
		}
	} else {
		e.LiquidationPrice = 0
	}
}

// handleLiquidation 处理爆仓事件。
// 必须在持有锁的情况下调用。
func (e *BacktestExchange) handleLiquidation() {
	if !e.isLiquidated {
		e.isLiquidated = true

		// 记录爆仓前的状态
		originalCash := e.Cash
		originalMargin := e.Margin
		originalUnrealizedPNL := e.UnrealizedPNL
		finalEquity := originalCash + originalMargin + originalUnrealizedPNL
		originalPositionSize := e.Positions[e.Symbol]
		originalAvgEntryPrice := e.AvgEntryPrice[e.Symbol]

		// 爆仓：所有资产清零
		e.Cash = 0
		e.Positions[e.Symbol] = 0
		e.UnrealizedPNL = 0
		e.Margin = 0

		fmt.Printf(`
!----- 爆仓事件发生 -----!
时间: %s
触发价格(成交价): %.4f
计算出的爆仓价: %.4f
--- 爆仓前状态快照 ---
可用现金 (Cash): %.4f
保证金 (Margin): %.4f
未实现盈亏 (UnrealizedPNL): %.4f
账户总权益 (Equity): %.4f
持仓量 (Position Size): %.8f
平均开仓价 (Avg Entry Price): %.4f
--- 爆仓后 ---
账户剩余权益: %.4f
!--------------------------!
`,
			e.CurrentTime.Format(time.RFC3339),
			e.CurrentPrice,     // 实际触发爆仓的价格
			e.LiquidationPrice, // 爆仓前计算出的理论价格
			originalCash,
			originalMargin,
			originalUnrealizedPNL,
			finalEquity,
			originalPositionSize,
			originalAvgEntryPrice,
			e.Cash, // 爆仓后现金 (应为 0)
		)
		e.LiquidationPrice = 0 // 爆仓后重置
	}
}

// updateEquity 计算并记录当前权益。必须在持有锁的情况下调用。
func (e *BacktestExchange) updateEquity() {
	// 合约账户总权益 = 可用现金 + 占用保证金 + 未实现盈亏
	equity := e.Cash + e.Margin + e.UnrealizedPNL
	e.EquityCurve = append(e.EquityCurve, equity)

	// 记录每日结束时的权益
	dayKey := e.CurrentTime.Format("2006-01-02")
	e.dailyEquity[dayKey] = equity
}

// --- Exchange 接口实现 ---

func (e *BacktestExchange) GetPrice(symbol string) (float64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.CurrentPrice, nil
}

func (e *BacktestExchange) PlaceOrder(symbol, side, orderType string, quantity, price float64) (*models.Order, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	order := &models.Order{
		OrderId: e.NextOrderID,
		Symbol:  e.Symbol, // 强制使用交易所内部的 symbol
		Side:    side,
		Type:    orderType,
		OrigQty: fmt.Sprintf("%.8f", quantity),
		Price:   fmt.Sprintf("%.8f", price),
		Status:  "NEW",
	}
	e.orders[order.OrderId] = order
	e.NextOrderID++

	if orderType == "MARKET" {
		order.Price = fmt.Sprintf("%.8f", e.CurrentPrice)
		e.handleFilledOrder(order)
	}

	return order, nil
}

func (e *BacktestExchange) GetOrderStatus(symbol string, orderID int64) (*models.Order, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if order, ok := e.orders[orderID]; ok {
		orderCopy := *order
		return &orderCopy, nil
	}
	return nil, fmt.Errorf("订单 ID %d 在回测中未找到", orderID)
}

func (e *BacktestExchange) CancelAllOpenOrders(symbol string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, order := range e.orders {
		if order.Symbol == symbol && order.Status == "NEW" {
			order.Status = "CANCELED"
		}
	}
	return nil
}

// --- 其他接口方法 (简化或保持不变) ---
// IsLiquidated 返回交易所是否已经历爆仓。
func (e *BacktestExchange) IsLiquidated() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.isLiquidated
}
func (e *BacktestExchange) GetPositions(symbol string) ([]models.Position, error) { return nil, nil }
func (e *BacktestExchange) CancelOrder(symbol string, orderID int64) error        { return nil }
func (e *BacktestExchange) SetLeverage(symbol string, leverage int) error {
	e.Leverage = leverage
	return nil
}
func (e *BacktestExchange) GetAccountInfo() (*models.AccountInfo, error) { return nil, nil }
func (e *BacktestExchange) GetAllOrders() []*models.Order {
	e.mu.Lock()
	defer e.mu.Unlock()

	orders := make([]*models.Order, 0, len(e.orders))
	for _, order := range e.orders {
		orderCopy := *order
		orders = append(orders, &orderCopy)
	}
	return orders
}

func (e *BacktestExchange) GetCurrentTime() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.CurrentTime
}

// GetAccountState 获取回测环境中的账户状态
func (e *BacktestExchange) GetAccountState(symbol string) (positionValue float64, accountEquity float64, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	positionSize := e.Positions[symbol]
	positionValue = positionSize * e.CurrentPrice

	if len(e.EquityCurve) > 0 {
		accountEquity = e.EquityCurve[len(e.EquityCurve)-1]
	} else {
		accountEquity = e.InitialBalance
	}

	return positionValue, accountEquity, nil
}

// GetSymbolInfo 为回测提供一个模拟的交易规则
func (e *BacktestExchange) GetSymbolInfo(symbol string) (*models.SymbolInfo, error) {
	// 对于回测，我们返回一个包含合理默认值的模拟 SymbolInfo
	// 这避免了在回测模式下进行网络调用
	return &models.SymbolInfo{
		Symbol: symbol,
		Filters: []models.Filter{
			{
				FilterType: "PRICE_FILTER",
				TickSize:   "0.01", // 假设价格精度为2位小数
			},
			{
				FilterType: "LOT_SIZE",
				StepSize:   "0.001", // 假设数量精度为3位小数
			},
			{
				FilterType:  "MIN_NOTIONAL",
				MinNotional: fmt.Sprintf("%.2f", e.MinNotionalValue),
			},
		},
	}, nil
}

// GetOpenOrders 为回测提供一个模拟的实现
func (e *BacktestExchange) GetOpenOrders(symbol string) ([]models.Order, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// 在回测中，我们自己管理订单状态，可以直接从业已存在的订单map中筛选
	openOrders := make([]models.Order, 0)
	for _, order := range e.orders {
		if order.Symbol == symbol && order.Status == "NEW" {
			openOrders = append(openOrders, *order)
		}
	}
	return openOrders, nil
}

// GetServerTime 为回测提供一个模拟的实现
func (e *BacktestExchange) GetServerTime() (int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// 在回测中，服务器时间就是当前数据点的时间
	return e.CurrentTime.UnixMilli(), nil
}

// GetDailyEquity 返回每日权益的只读副本
func (e *BacktestExchange) GetDailyEquity() map[string]float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	// 返回一个副本以防止外部修改
	cpy := make(map[string]float64)
	for k, v := range e.dailyEquity {
		cpy[k] = v
	}
	return cpy
}

// GetMaxWalletExposure 返回回测期间记录的最大钱包风险暴露。
func (e *BacktestExchange) GetMaxWalletExposure() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.MaxWalletExposure
}

// GetLastTrade 在回测中模拟获取最新成交记录。
func (e *BacktestExchange) GetLastTrade(symbol string, orderID int64) (*models.Trade, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	order, ok := e.orders[orderID]
	if !ok {
		return nil, fmt.Errorf("在回测中未找到订单 ID %d", orderID)
	}

	// 在回测模式下，我们假设成交价就是订单的挂单价
	// 我们可以创建一个模拟的 Trade 对象返回
	return &models.Trade{
		Symbol:  order.Symbol,
		OrderID: order.OrderId,
		Side:    order.Side,
		Price:   order.Price, // 使用订单价格作为成交价
		Qty:     order.OrigQty,
		Time:    e.CurrentTime.UnixMilli(),
	}, nil
}
