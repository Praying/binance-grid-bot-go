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
	Symbol                string // 新增：存储当前回测的交易对
	InitialBalance        float64
	Cash                  float64
	CurrentPrice          float64   // Stores the close price of the current tick
	CurrentTime           time.Time // Stores the timestamp of the current tick
	Positions             map[string]float64
	AvgEntryPrice         map[string]float64
	buyQueue              map[string][]models.BuyTrade // 新增：FIFO买入队列, 替换 positionEntryTime
	orders                map[int64]*models.Order
	TradeLog              []models.CompletedTrade
	EquityCurve           []float64
	dailyEquity           map[string]float64 // 新增：用于记录每日权益
	NextOrderID           int64
	Leverage              int
	mu                    sync.Mutex
	config                *models.Config // 新增：存储配置
	TakerFeeRate          float64        // 吃单手续费率
	MakerFeeRate          float64        // 挂单手续费率
	SlippageRate          float64        // 滑点率
	MaintenanceMarginRate float64        // 维持保证金率
	TotalFees             float64        // 累积总手续费
	MinNotionalValue      float64        // 新增：最小名义价值
	Margin                float64        // 仓位保证金
	UnrealizedPNL         float64        // 未实现盈亏
	LiquidationPrice      float64        // 预估爆仓价
	isLiquidated          bool           // 标记是否已爆仓 (私有)
	MaxWalletExposure     float64        // 新增：记录回测期间最大的钱包风险暴露
}

// NewBacktestExchange 创建一个新的 BacktestExchange 实例。
func NewBacktestExchange(cfg *models.Config) *BacktestExchange {
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
		config:                cfg,
	}
}

// SetPrice 是回测的核心，模拟价格变动并触发订单成交检查。
func (e *BacktestExchange) SetPrice(open, high, low, close float64, timestamp time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.CurrentTime = timestamp

	if e.isLiquidated {
		return
	}

	e.checkLimitOrdersAtPrice(open)
	e.checkLimitOrdersAtPrice(low)
	e.checkLimitOrdersAtPrice(high)
	e.checkLimitOrdersAtPrice(close)

	e.CurrentPrice = close
	e.updateMarginAndPNL()

	if e.Positions[e.Symbol] > 0 && e.LiquidationPrice > 0 && low <= e.LiquidationPrice {
		e.CurrentPrice = e.LiquidationPrice
		e.handleLiquidation()
		e.updateEquity()
		return
	}

	e.updateEquity()
}

func (e *BacktestExchange) checkLimitOrdersAtPrice(price float64) {
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
			if order.Side == "BUY" && price <= limitPrice {
				shouldFill = true
			} else if order.Side == "SELL" && price >= limitPrice {
				shouldFill = true
			}

			if shouldFill {
				e.handleFilledOrder(order)
			}
		}
	}
}

func (e *BacktestExchange) handleFilledOrder(order *models.Order) {
	order.Status = "FILLED"

	limitPrice, _ := strconv.ParseFloat(order.Price, 64)
	quantity, _ := strconv.ParseFloat(order.OrigQty, 64)

	var executionPrice float64
	if order.Side == "BUY" {
		executionPrice = limitPrice * (1 + e.SlippageRate)
	} else { // SELL
		executionPrice = limitPrice * (1 - e.SlippageRate)
	}
	if order.Type == "MARKET" {
		if order.Side == "BUY" {
			executionPrice = e.CurrentPrice * (1 + e.SlippageRate)
		} else {
			executionPrice = e.CurrentPrice * (1 - e.SlippageRate)
		}
	}

	feeRate := e.MakerFeeRate
	if order.Type == "MARKET" {
		feeRate = e.TakerFeeRate
	}
	fee := executionPrice * quantity * feeRate
	e.TotalFees += fee
	e.Cash -= fee

	currentPosition := e.Positions[order.Symbol]
	avgEntry := e.AvgEntryPrice[order.Symbol]

	if order.Side == "BUY" {
		newBuy := models.BuyTrade{
			Timestamp: e.CurrentTime,
			Quantity:  quantity,
			Price:     executionPrice,
		}
		e.buyQueue[order.Symbol] = append(e.buyQueue[order.Symbol], newBuy)

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
				sellQuantity = currentPosition
			}

			var weightedEntryPriceSum float64
			var weightedHoldDurationSum float64
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

			if e.Positions[order.Symbol] < 1e-9 {
				e.buyQueue[order.Symbol] = nil
			}
		}
	}

	e.updateMarginAndPNL()
	if e.Positions[order.Symbol] > 1e-9 {
		e.calculateLiquidationPrice()
	} else {
		e.Positions[order.Symbol] = 0
		e.LiquidationPrice = 0
	}

	equity := e.Cash + e.Margin + e.UnrealizedPNL
	positionValue := e.Positions[order.Symbol] * e.CurrentPrice
	walletExposure := 0.0
	if equity > 0 {
		walletExposure = positionValue / equity
	}
	if walletExposure > e.MaxWalletExposure {
		e.MaxWalletExposure = walletExposure
	}

	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.SetTitle(fmt.Sprintf("[回测] 订单成交快照 (%s)", e.CurrentTime.Format("2006-01-02 15:04:05")))
	t.SetStyle(table.StyleLight)

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

func (e *BacktestExchange) updateMarginAndPNL() {
	positionSize := e.Positions[e.Symbol]
	if positionSize > 0 {
		avgEntryPrice := e.AvgEntryPrice[e.Symbol]
		e.Margin = (avgEntryPrice * positionSize) / float64(e.Leverage)
		e.UnrealizedPNL = (e.CurrentPrice - avgEntryPrice) * positionSize
	} else {
		e.Margin = 0
		e.UnrealizedPNL = 0
		e.AvgEntryPrice[e.Symbol] = 0
	}
}

func (e *BacktestExchange) calculateLiquidationPrice() {
	positionSize := e.Positions[e.Symbol]
	avgEntryPrice := e.AvgEntryPrice[e.Symbol]
	mmr := e.MaintenanceMarginRate
	walletBalance := e.Cash + e.Margin

	if positionSize > 1e-9 {
		numerator := avgEntryPrice*positionSize - walletBalance
		denominator := positionSize * (1 - mmr)

		if denominator != 0 {
			e.LiquidationPrice = numerator / denominator
		} else {
			e.LiquidationPrice = -1
		}
	} else {
		e.LiquidationPrice = 0
	}
}

func (e *BacktestExchange) handleLiquidation() {
	if !e.isLiquidated {
		e.isLiquidated = true

		originalCash := e.Cash
		originalMargin := e.Margin
		originalUnrealizedPNL := e.UnrealizedPNL
		finalEquity := originalCash + originalMargin + originalUnrealizedPNL
		originalPositionSize := e.Positions[e.Symbol]
		originalAvgEntryPrice := e.AvgEntryPrice[e.Symbol]

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
			e.CurrentPrice,
			e.LiquidationPrice,
			originalCash,
			originalMargin,
			originalUnrealizedPNL,
			finalEquity,
			originalPositionSize,
			originalAvgEntryPrice,
			e.Cash,
		)
		e.LiquidationPrice = 0
	}
}

func (e *BacktestExchange) updateEquity() {
	equity := e.Cash + e.Margin + e.UnrealizedPNL
	e.EquityCurve = append(e.EquityCurve, equity)

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
		ClientOrderId: clientOrderID,
		Symbol:        e.Symbol,
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

// GetOrderHistory 在回测中模拟获取订单历史。
// 根据任务要求，我们简单地返回一个已成交的订单，以测试恢复逻辑。
func (e *BacktestExchange) GetOrderHistory(symbol string, clientOrderID string) (*models.Order, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// 尝试在现有订单中查找
	for _, order := range e.orders {
		if order.ClientOrderId == clientOrderID {
			// 如果找到了，返回一个副本
			orderCopy := *order
			// 确保状态是最终状态
			if orderCopy.Status == "NEW" {
				orderCopy.Status = "FILLED" // 模拟它已经被成交
			}
			return &orderCopy, nil
		}
	}

	// 如果在当前订单列表中找不到（这在恢复场景中很常见），
	// 我们创建一个模拟的已成交订单。
	return &models.Order{
		Symbol:        symbol,
		ClientOrderId: clientOrderID,
		Status:        "FILLED",
		Type:          "LIMIT", // 假设是限价单
		Side:          "BUY",   // 假设是买单
		Price:         "1.0",   // 模拟价格
		OrigQty:       "1.0",   // 模拟数量
		ExecutedQty:   "1.0",   // 模拟已执行数量
	}, nil
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
				Symbol:           symbol,
				PositionAmt:      fmt.Sprintf("%f", posSize),
				EntryPrice:       fmt.Sprintf("%f", avgPrice),
				UnrealizedProfit: fmt.Sprintf("%f", e.UnrealizedPNL),
				LiquidationPrice: fmt.Sprintf("%f", e.LiquidationPrice),
				Leverage:         strconv.Itoa(e.Leverage),
				MarginType:       strings.ToLower(e.config.MarginType),
			},
		}, nil
	}
	return []models.Position{}, nil
}

func (e *BacktestExchange) SetLeverage(symbol string, leverage int) error {
	e.Leverage = leverage
	return nil
}

func (e *BacktestExchange) SetMarginType(symbol string, marginType string) error {
	// 在回测中，我们假设这个操作总是成功的
	return nil
}

func (e *BacktestExchange) SetPositionMode(isHedgeMode bool) error {
	// 在回测中，我们假设这个操作总是成功的
	return nil
}

func (e *BacktestExchange) GetPositionMode() (bool, error) {
	return e.config.HedgeMode, nil
}

func (e *BacktestExchange) GetMarginType(symbol string) (string, error) {
	return e.config.MarginType, nil
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

func (e *BacktestExchange) GetAccountState(symbol string) (positionValue float64, accountEquity float64, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	equity := e.Cash + e.Margin + e.UnrealizedPNL
	positionValue = e.Positions[symbol] * e.CurrentPrice
	return positionValue, equity, nil
}

func (e *BacktestExchange) GetSymbolInfo(symbol string) (*models.SymbolInfo, error) {
	return &models.SymbolInfo{
		Symbol: symbol,
		Filters: []models.Filter{
			{FilterType: "PRICE_FILTER", TickSize: "0.01"},
			{FilterType: "LOT_SIZE", StepSize: "0.001", MinQty: "0.001"},
			{FilterType: "MIN_NOTIONAL", MinNotional: "5.0"},
		},
	}, nil
}

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

func (e *BacktestExchange) GetServerTime() (int64, error) {
	return time.Now().UnixMilli(), nil
}

func (e *BacktestExchange) GetDailyEquity() map[string]float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	// 返回一个副本以避免外部修改
	dailyEquityCopy := make(map[string]float64)
	for k, v := range e.dailyEquity {
		dailyEquityCopy[k] = v
	}
	return dailyEquityCopy
}

func (e *BacktestExchange) GetMaxWalletExposure() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.MaxWalletExposure
}

func (e *BacktestExchange) GetLastTrade(symbol string, orderID int64) (*models.Trade, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.TradeLog) > 0 {
		// 在回测中，我们简单地返回最后一笔交易作为模拟
		lastTrade := e.TradeLog[len(e.TradeLog)-1]
		return &models.Trade{
			Symbol: lastTrade.Symbol,
			Price:  strconv.FormatFloat(lastTrade.ExitPrice, 'f', -1, 64),
			Qty:    strconv.FormatFloat(lastTrade.Quantity, 'f', -1, 64),
		}, nil
	}
	return nil, fmt.Errorf("回测中没有可用的成交记录")
}

func (e *BacktestExchange) CancelOrder(symbol string, orderID int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if order, ok := e.orders[orderID]; ok {
		order.Status = "CANCELED"
		return nil
	}
	return fmt.Errorf("订单 ID %d 在回测中未找到", orderID)
}

func (e *BacktestExchange) GetBalance() (float64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.Cash, nil
}

func (e *BacktestExchange) GetAccountInfo() (*models.AccountInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	equity := e.Cash + e.Margin + e.UnrealizedPNL
	return &models.AccountInfo{
		TotalWalletBalance: fmt.Sprintf("%f", equity),
		AvailableBalance:   fmt.Sprintf("%f", e.Cash),
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
				Asset:            "USDT",
				WalletBalance:    fmt.Sprintf("%f", equity),
				UnrealizedProfit: fmt.Sprintf("%f", e.UnrealizedPNL),
			},
		},
	}, nil
}

func (e *BacktestExchange) CreateListenKey() (string, error) {
	return "mock-listen-key", nil
}

func (e *BacktestExchange) KeepAliveListenKey(listenKey string) error {
	return nil
}

func (e *BacktestExchange) ConnectWebSocket(listenKey string) (*websocket.Conn, error) {
	// 回测模式下不建立真实的 WebSocket 连接
	return nil, nil
}
