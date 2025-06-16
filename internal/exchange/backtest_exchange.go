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
	Symbol         string // 新增：存储当前回测的交易对
	InitialBalance float64
	Cash           float64
	CurrentPrice   float64   // Stores the close price of the current tick
	CurrentTime    time.Time // Stores the timestamp of the current tick
	Positions      map[string]float64
	AvgEntryPrice  map[string]float64
	orders         map[int64]*models.Order
	TradeLog       []models.CompletedTrade
	EquityCurve    []float64
	NextOrderID    int64
	Leverage       int
	mu             sync.Mutex

	// 合约交易状态
	Margin           float64 // 仓位保证金
	UnrealizedPNL    float64 // 未实现盈亏
	LiquidationPrice float64 // 预估爆仓价
	isLiquidated     bool    // 标记是否已爆仓 (私有)
}

// NewBacktestExchange 创建一个新的 BacktestExchange 实例。
func NewBacktestExchange(symbol string, initialBalance float64, leverage int) *BacktestExchange {
	return &BacktestExchange{
		Symbol:           symbol, // 设置交易对
		InitialBalance:   initialBalance,
		Cash:             initialBalance,
		Positions:        make(map[string]float64),
		AvgEntryPrice:    make(map[string]float64),
		orders:           make(map[int64]*models.Order),
		TradeLog:         make([]models.CompletedTrade, 0),
		EquityCurve:      make([]float64, 0, 10000),
		NextOrderID:      1,
		Leverage:         leverage,
		Margin:           0,
		UnrealizedPNL:    0,
		LiquidationPrice: 0,
		isLiquidated:     false,
	}
}

// SetPrice 是回测的核心，模拟价格变动并触发订单成交检查。
// 使用高低价来决定是否成交，收盘价用于记录和更新权益。
func (e *BacktestExchange) SetPrice(high, low, close float64, timestamp time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.CurrentTime = timestamp

	// 如果已爆仓，则停止所有活动
	if e.isLiquidated {
		return
	}

	// --- 核心逻辑顺序重构 ---

	// 步骤 1: 首先，处理所有可能因当前价格(高/低)而成交的订单
	e.checkLimitOrders(high, low)

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

// checkLimitOrders 遍历所有订单，检查是否有挂起的限价单可以被当前价格成交。
func (e *BacktestExchange) checkLimitOrders(high, low float64) {
	// 注意：此函数现在应在已持有锁的情况下被调用
	var orderedIDs []int64
	for id := range e.orders {
		orderedIDs = append(orderedIDs, id)
	}
	sort.Slice(orderedIDs, func(i, j int) bool { return orderedIDs[i] < orderedIDs[j] })

	for _, orderID := range orderedIDs {
		order := e.orders[orderID]
		if order.Status == "NEW" && order.Type == "LIMIT" {
			price, _ := strconv.ParseFloat(order.Price, 64)
			shouldFill := false
			// 核心逻辑: 使用高低价判断成交
			if order.Side == "BUY" && low <= price {
				shouldFill = true
			} else if order.Side == "SELL" && high >= price {
				shouldFill = true
			}

			if shouldFill {
				e.handleFilledOrder(order)
			}
		}
	}
}

// handleFilledOrder 处理一个已成交的订单，更新账户状态。必须在持有锁的情况下调用。
func (e *BacktestExchange) handleFilledOrder(order *models.Order) {
	order.Status = "FILLED"

	price, _ := strconv.ParseFloat(order.Price, 64)
	quantity, _ := strconv.ParseFloat(order.OrigQty, 64)

	currentPosition := e.Positions[order.Symbol]
	avgEntry := e.AvgEntryPrice[order.Symbol]

	if order.Side == "BUY" { // 开仓或加仓
		newTotalQty := currentPosition + quantity
		newTotalValue := (avgEntry * currentPosition) + (price * quantity)
		if newTotalQty > 0 {
			e.AvgEntryPrice[order.Symbol] = newTotalValue / newTotalQty
		}
		e.Positions[order.Symbol] = newTotalQty
	} else { // SELL (平仓)
		// 修正：在回测中，卖出操作的已实现盈亏是在权益中体现的，而不是直接增加到现金中。
		// 我们将在这里记录这笔交易的理论利润，但不再错误地修改 e.Cash。
		realizedPNL := (price - avgEntry) * quantity
		e.Positions[order.Symbol] -= quantity

		e.TradeLog = append(e.TradeLog, models.CompletedTrade{
			Symbol:    e.Symbol,
			Profit:    realizedPNL, // Profit 字段仍然用于报告，但不再影响资金计算
			ExitTime:  e.CurrentTime,
			ExitPrice: price,
		})
	}

	// 每次成交后，都重新计算保证金、未实现盈亏
	e.updateMarginAndPNL()
	// 关键修复：仅在仍有持仓时才计算爆仓价
	if e.Positions[order.Symbol] > 0.00000001 { // 使用一个极小值来避免浮点数精度问题
		e.calculateLiquidationPrice()
	} else {
		e.LiquidationPrice = 0 // 确保无持仓时爆仓价为0
	}
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
	if positionSize > 0.00000001 { // 使用极小值以避免浮点精度问题
		// **最终修复：引入正确的、考虑现金缓冲的爆仓价模型**
		// 简化模型：爆仓发生在总浮亏约等于账户可用现金时。
		// LiquidationPrice = EntryPrice - (Cash / PositionSize)
		// 这是一个更真实的简化模型，因为它承认了可用现金是抵御亏损的第一道防线。
		avgEntryPrice := e.AvgEntryPrice[e.Symbol]
		// 为避免除以零的错误，我们再次确认仓位大小
		e.LiquidationPrice = avgEntryPrice - (e.Cash / positionSize)
	} else {
		e.LiquidationPrice = 0
	}
}

// handleLiquidation 处理爆仓事件。
// 必须在持有锁的情况下调用。
func (e *BacktestExchange) handleLiquidation() {
	if !e.isLiquidated {
		e.isLiquidated = true
		e.Cash -= e.Margin // 扣除保证金
		e.Positions[e.Symbol] = 0
		e.UnrealizedPNL = 0
		e.Margin = 0
		// 可以在此处添加日志记录
		equity := e.Cash + e.Margin + e.UnrealizedPNL
		fmt.Printf(`
!----- 爆仓事件发生 -----!
时间: %s
触发价格: %.4f
计算出的爆仓价: %.4f
--- 状态快照 ---
可用现金 (Cash): %.4f
保证金 (Margin): %.4f
未实现盈亏 (UnrealizedPNL): %.4f
账户总权益 (Equity): %.4f
持仓量 (Position Size): %.8f
平均开仓价 (Avg Entry Price): %.4f
!--------------------------!
`,
			e.CurrentTime.Format(time.RFC3339),
			e.CurrentPrice,
			e.LiquidationPrice,
			e.Cash,
			e.Margin,
			e.UnrealizedPNL,
			equity,
			e.Positions[e.Symbol],
			e.AvgEntryPrice[e.Symbol],
		)
	}
}

// updateEquity 计算并记录当前权益。必须在持有锁的情况下调用。
func (e *BacktestExchange) updateEquity() {
	// 合约账户总权益 = 可用现金 + 占用保证金 + 未实现盈亏
	equity := e.Cash + e.Margin + e.UnrealizedPNL
	e.EquityCurve = append(e.EquityCurve, equity)
}

// --- Exchange 接口实现 ---

func (e *BacktestExchange) GetPrice(symbol string) (float64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.CurrentPrice, nil
}

func (e *BacktestExchange) PlaceOrder(symbol, side, orderType string, quantity, price float64, positionSide string) (*models.Order, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	order := &models.Order{
		OrderId:      e.NextOrderID,
		Symbol:       e.Symbol, // 强制使用交易所内部的 symbol
		Side:         side,
		Type:         orderType,
		OrigQty:      fmt.Sprintf("%.8f", quantity),
		Price:        fmt.Sprintf("%.8f", price),
		PositionSide: positionSide,
		Status:       "NEW",
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
