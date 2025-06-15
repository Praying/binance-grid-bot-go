package exchange

import (
	"binance-grid-bot-go/internal/models"
	"fmt"
	"sort"
	"strconv"
	"sync"
)

// BacktestExchange 实现了 Exchange 接口，用于模拟交易所行为以进行回测。
type BacktestExchange struct {
	Symbol         string // 新增：存储当前回测的交易对
	InitialBalance float64
	Cash           float64
	CurrentPrice   float64 // Stores the close price of the current tick
	Positions      map[string]float64
	AvgEntryPrice  map[string]float64
	orders         map[int64]*models.Order
	TradeLog       []models.CompletedTrade
	EquityCurve    []float64
	NextOrderID    int64
	Leverage       int
	mu             sync.Mutex
}

// NewBacktestExchange 创建一个新的 BacktestExchange 实例。
func NewBacktestExchange(symbol string, initialBalance float64, leverage int) *BacktestExchange {
	return &BacktestExchange{
		Symbol:         symbol, // 设置交易对
		InitialBalance: initialBalance,
		Cash:           initialBalance,
		Positions:      make(map[string]float64),
		AvgEntryPrice:  make(map[string]float64),
		orders:         make(map[int64]*models.Order),
		TradeLog:       make([]models.CompletedTrade, 0),
		EquityCurve:    make([]float64, 0, 10000),
		NextOrderID:    1,
		Leverage:       leverage,
	}
}

// SetPrice 是回测的核心，模拟价格变动并触发订单成交检查。
// 使用高低价来决定是否成交，收盘价用于记录和更新权益。
func (e *BacktestExchange) SetPrice(high, low, close float64) {
	e.checkLimitOrders(high, low)

	e.mu.Lock()
	e.CurrentPrice = close
	e.updateEquity()
	e.mu.Unlock()
}

// checkLimitOrders 遍历所有订单，检查是否有挂起的限价单可以被当前价格成交。
func (e *BacktestExchange) checkLimitOrders(high, low float64) {
	e.mu.Lock()
	defer e.mu.Unlock()

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

	// log.Printf("[Backtest] Order Filled: %s %s %.5f at %.4f", order.Side, order.Symbol, quantity, price)

	currentPosition := e.Positions[order.Symbol]
	avgEntry := e.AvgEntryPrice[order.Symbol]

	if order.Side == "BUY" {
		newTotalQty := currentPosition + quantity
		newTotalValue := (avgEntry * currentPosition) + (price * quantity)
		if newTotalQty > 0 {
			e.AvgEntryPrice[order.Symbol] = newTotalValue / newTotalQty
		}
		e.Positions[order.Symbol] = newTotalQty
	} else { // SELL
		profit := (price - avgEntry) * quantity
		e.Cash += profit
		e.Positions[order.Symbol] -= quantity

		e.TradeLog = append(e.TradeLog, models.CompletedTrade{
			Symbol: e.Symbol, // 强制使用交易所内部的 symbol
			Profit: profit,
		})
	}
}

// updateEquity 计算并记录当前权益。必须在持有锁的情况下调用。
func (e *BacktestExchange) updateEquity() {
	equity := e.Cash
	for _, qty := range e.Positions {
		equity += qty * e.CurrentPrice
	}
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
