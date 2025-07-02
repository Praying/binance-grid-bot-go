package exchange

import (
	"binance-grid-bot-go/internal/models"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jedib0t/go-pretty/v6/table"
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
	buyQueue       map[string][]models.BuyTrade // 新增：FIFO买入队列, 替换 positionEntryTime
	orders         map[int64]*models.Order
	TradeLog       []models.CompletedTrade
	EquityCurve    []float64
	dailyEquity    map[string]float64 // 新增：用于记录每日权益
	NextOrderID    int64
	Leverage       int
	mu             sync.Mutex

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
		buyQueue:              make(map[string][]models.BuyTrade),
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

	if order.Side == "BUY" {
		// FIFO Logic: Add buy trade to the queue
		newBuy := models.BuyTrade{
			Timestamp: e.CurrentTime,
			Quantity:  quantity,
			Price:     executionPrice,
		}
		e.buyQueue[order.Symbol] = append(e.buyQueue[order.Symbol], newBuy)

		// Update position and average entry price (existing logic is correct for overall avg price)
		newTotalQty := currentPosition + quantity
		newTotalValue := (avgEntry * currentPosition) + (executionPrice * quantity)
		if newTotalQty > 0 {
			e.AvgEntryPrice[order.Symbol] = newTotalValue / newTotalQty
		}
		e.Positions[order.Symbol] = newTotalQty

	} else { // SELL (平多仓) with FIFO logic
		if currentPosition > 1e-9 {
			sellQuantity := quantity
			if sellQuantity > currentPosition {
				sellQuantity = currentPosition // Do not sell more than we have
			}

			var weightedEntryPriceSum float64
			var weightedHoldDurationSum float64 // in nanoseconds * quantity
			var quantityToMatch = sellQuantity
			var consumedCount = 0

			queue := e.buyQueue[order.Symbol]

			for i := range queue {
				if quantityToMatch < 1e-9 {
					break
				}
				oldestBuy := &queue[i]

				var consumedQty float64
				if oldestBuy.Quantity <= quantityToMatch {
					consumedQty = oldestBuy.Quantity
				} else {
					consumedQty = quantityToMatch
				}

				weightedEntryPriceSum += oldestBuy.Price * consumedQty
				durationNs := float64(e.CurrentTime.Sub(oldestBuy.Timestamp))
				weightedHoldDurationSum += durationNs * consumedQty

				oldestBuy.Quantity -= consumedQty
				quantityToMatch -= consumedQty

				if oldestBuy.Quantity < 1e-9 {
					consumedCount++
				}
			}

			// Clean up fully consumed buy orders from the front of the queue
			if consumedCount > 0 {
				e.buyQueue[order.Symbol] = queue[consumedCount:]
			} else {
				e.buyQueue[order.Symbol] = queue
			}

			if sellQuantity > 1e-9 {
				avgEntryForThisSell := weightedEntryPriceSum / sellQuantity
				avgHoldDurationNs := weightedHoldDurationSum / sellQuantity
				avgHoldDuration := time.Duration(avgHoldDurationNs)
				entryTimeApproximation := e.CurrentTime.Add(-avgHoldDuration)

				realizedPNL := (executionPrice - avgEntryForThisSell) * sellQuantity
				e.Cash += realizedPNL
				e.Positions[order.Symbol] -= sellQuantity

				slippageCost := (executionPrice - limitPrice) * sellQuantity

				e.TradeLog = append(e.TradeLog, models.CompletedTrade{
					Symbol:       e.Symbol,
					Quantity:     sellQuantity,
					EntryTime:    entryTimeApproximation,
					ExitTime:     e.CurrentTime,
					HoldDuration: avgHoldDuration,
					EntryPrice:   avgEntryForThisSell,
					ExitPrice:    executionPrice,
					Profit:       realizedPNL - fee,
					Fee:          fee,
					Slippage:     slippageCost,
				})
			}

			// If position is now fully closed, clear the queue to prevent floating point dust
			if e.Positions[order.Symbol] < 1e-9 {
				e.buyQueue[order.Symbol] = nil
			}
		}
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

	// 使用 go-pretty 库来确保完美的表格对齐
	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.SetTitle(fmt.Sprintf("[回测] 订单成交快照 (%s)", e.CurrentTime.Format("2006-01-02 15:04:05")))
	t.SetStyle(table.StyleLight) // 使用一个更简洁的样式

	t.AppendRow(table.Row{"成交详情", fmt.Sprintf("%s %s @ %.4f, 数量: %.5f", order.Side, order.Type, executionPrice, quantity)})
	t.AppendSeparator()
	t.AppendRow(table.Row{"持仓状态"})
	t.AppendRow(table.Row{"  新持仓量", fmt.Sprintf("%.5f %s", e.Positions[order.Symbol], strings.Replace(order.Symbol, "USDT", "", -1))})
	t.AppendRow(table.Row{"  平均持仓成本", fmt.Sprintf("%.4f", e.AvgEntryPrice[order.Symbol])})
	t.AppendRow(table.Row{"  钱包风险暴露", fmt.Sprintf("%.2f%%", walletExposure*100)})
	t.AppendSeparator()
	t.AppendRow(table.Row{"账户状态"})
	t.AppendRow(table.Row{"  现金余额", fmt.Sprintf("%.4f", e.Cash)})
	t.AppendRow(table.Row{"  保证金", fmt.Sprintf("%.4f", e.Margin)})
	t.AppendRow(table.Row{"  未实现盈亏", fmt.Sprintf("%.4f", e.UnrealizedPNL)})
	t.AppendRow(table.Row{"  账户总权益", fmt.Sprintf("%.4f", equity)})

	t.Render()
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

func (e *BacktestExchange) PlaceOrder(symbol, side, orderType string, quantity, price float64, clientOrderID string) (*models.Order, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	order := &models.Order{
		OrderId:       e.NextOrderID,
		ClientOrderId: clientOrderID, // 存储 clientOrderID
		Symbol:        e.Symbol,      // 强制使用交易所内部的 symbol
		Side:          side,
		Type:          orderType,
		OrigQty:       fmt.Sprintf("%.8f", quantity),
		Price:         fmt.Sprintf("%.8f", price),
		Status:        "NEW",
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
func (e *BacktestExchange) GetPositions(symbol string) ([]models.Position, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if posSize, ok := e.Positions[symbol]; ok && posSize > 0 {
		avgPrice := e.AvgEntryPrice[symbol]
		return []models.Position{
			{
				Symbol:      symbol,
				PositionAmt: fmt.Sprintf("%f", posSize),
				EntryPrice:  fmt.Sprintf("%f", avgPrice),
			},
		}, nil
	}
	return []models.Position{}, nil
}
func (e *BacktestExchange) CancelOrder(symbol string, orderID int64) error { return nil }
func (e *BacktestExchange) SetLeverage(symbol string, leverage int) error {
	e.Leverage = leverage
	return nil
}
func (e *BacktestExchange) SetMarginType(symbol string, marginType string) error {
	// 在回测中, 我们假设保证金模式已经根据配置设置好了, 此处无需操作
	return nil
}
func (e *BacktestExchange) SetPositionMode(isHedgeMode bool) error {
	// 在回测中, 我们假设持仓模式已经根据配置设置好了, 此处无需操作
	return nil
}

// GetPositionMode 在回测中返回一个固定的值，因为这通常不影响回测逻辑。
func (e *BacktestExchange) GetPositionMode() (bool, error) {
	// 假设回测环境总是使用双向持仓模式，或者可以从配置中读取
	return true, nil
}

func (e *BacktestExchange) GetAllOrders() []*models.Order {
	e.mu.Lock()
	defer e.mu.Unlock()
	var allOrders []*models.Order
	for _, order := range e.orders {
		allOrders = append(allOrders, order)
	}
	return allOrders
}
func (e *BacktestExchange) GetCurrentTime() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.CurrentTime
}

// GetAccountState 获取回测账户的状态
func (e *BacktestExchange) GetAccountState(symbol string) (positionValue float64, accountEquity float64, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	positionSize := e.Positions[symbol]
	positionValue = positionSize * e.CurrentPrice
	accountEquity = e.Cash + e.Margin + e.UnrealizedPNL

	return positionValue, accountEquity, nil
}

// GetSymbolInfo 返回一个模拟的交易规则，以满足机器人启动时的需求
func (e *BacktestExchange) GetSymbolInfo(symbol string) (*models.SymbolInfo, error) {
	// 返回一个合理的默认值，特别是 tickSize 和 stepSize
	return &models.SymbolInfo{
		Symbol: symbol,
		Filters: []models.Filter{
			{
				FilterType: "PRICE_FILTER",
				TickSize:   "0.01", // 假设价格精度为 0.01
			},
			{
				FilterType: "LOT_SIZE",
				StepSize:   "0.001", // 假设数量精度为 0.001
				MinQty:     "0.001",
			},
			{
				FilterType:  "MIN_NOTIONAL",
				MinNotional: fmt.Sprintf("%.2f", e.MinNotionalValue),
			},
		},
	}, nil
}

// GetOpenOrders 获取所有未成交的订单
func (e *BacktestExchange) GetOpenOrders(symbol string) ([]models.Order, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var openOrders []models.Order
	for _, order := range e.orders {
		if order.Symbol == symbol && order.Status == "NEW" {
			openOrders = append(openOrders, *order)
		}
	}
	return openOrders, nil
}

// GetServerTime 模拟获取服务器时间
func (e *BacktestExchange) GetServerTime() (int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.CurrentTime.UnixMilli(), nil
}

// GetDailyEquity 返回每日权益记录
func (e *BacktestExchange) GetDailyEquity() map[string]float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	// 返回一个副本以确保线程安全
	dailyEquityCopy := make(map[string]float64)
	for k, v := range e.dailyEquity {
		dailyEquityCopy[k] = v
	}
	return dailyEquityCopy
}

// GetMaxWalletExposure 返回回测期间记录的最大钱包风险暴露
func (e *BacktestExchange) GetMaxWalletExposure() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.MaxWalletExposure
}

// GetLastTrade 模拟获取最新成交
func (e *BacktestExchange) GetLastTrade(symbol string, orderID int64) (*models.Trade, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// 在回测中，我们没有真实的成交列表。
	// 我们需要从订单信息中构建一个模拟的成交回报。
	order, ok := e.orders[orderID]
	if !ok || order.Status != "FILLED" {
		return nil, fmt.Errorf("未找到已成交的订单 %d", orderID)
	}

	// 模拟一个成交记录
	return &models.Trade{
		Symbol: order.Symbol,
		Price:  order.Price, // 使用订单价格作为成交价
		Qty:    order.OrigQty,
		Time:   e.CurrentTime.UnixMilli(),
	}, nil
}

// CreateListenKey 是一个虚拟实现，以满足 Exchange 接口的要求。
func (e *BacktestExchange) CreateListenKey() (string, error) {
	return "dummy-listen-key-for-backtest", nil
}

// KeepAliveListenKey 是一个虚拟实现，以满足 Exchange 接口的要求。
func (e *BacktestExchange) KeepAliveListenKey(listenKey string) error {
	return nil
}

// GetBalance 返回回测环境中的现金余额
func (e *BacktestExchange) GetBalance() (float64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.Cash, nil
}

// GetAccountInfo 模拟获取账户信息
func (e *BacktestExchange) GetAccountInfo() (*models.AccountInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	equity := e.Cash + e.Margin + e.UnrealizedPNL

	// 创建一个模拟的 AccountInfo 结构
	// 我们只填充那些在回测中有意义且可用的字段
	info := &models.AccountInfo{
		TotalWalletBalance: fmt.Sprintf("%f", equity),
		AvailableBalance:   fmt.Sprintf("%f", e.Cash), // 可用余额等于现金
		Assets: []struct {
			Asset                  string `json:"asset"`
			WalletBalance          string `json:"walletBalance"`
			UnrealizedProfit       string `json:"unrealizedProfit"`
			MarginBalance          string `json:"marginBalance"`
			MaintMargin            string `json:"maintMargin"`
			InitialMargin          string `json:"initialMargin"`
			PositionInitialMargin  string `json:"positionInitialMargin"`
			OpenOrderInitialMargin string `json:"openOrderInitialMargin"`
			MaxWithdrawAmount      string `json:"maxWithdrawAmount"`
		}{
			{
				Asset:                  "USDT", // 假设主要资产是 USDT
				WalletBalance:          fmt.Sprintf("%f", equity),
				UnrealizedProfit:       fmt.Sprintf("%f", e.UnrealizedPNL),
				MarginBalance:          fmt.Sprintf("%f", e.Cash+e.Margin), // 保证金余额
				MaintMargin:            "0",                                // 简化，不计算维持保证金
				InitialMargin:          fmt.Sprintf("%f", e.Margin),
				PositionInitialMargin:  fmt.Sprintf("%f", e.Margin),
				OpenOrderInitialMargin: "0", // 简化，不计算挂单保证金
				MaxWithdrawAmount:      fmt.Sprintf("%f", e.Cash),
			},
		},
	}

	return info, nil
}

// ConnectWebSocket 在回测中是一个空操作
func (e *BacktestExchange) ConnectWebSocket(listenKey string) (*websocket.Conn, error) {
	// 回测模式下不建立真实的 WebSocket 连接
	return nil, nil
}
