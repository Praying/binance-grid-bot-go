package bot

import (
	"binance-grid-bot-go/internal/exchange"
	"binance-grid-bot-go/internal/models"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// GridTradingBot 是网格交易机器人的核心结构
type GridTradingBot struct {
	config       *models.Config
	exchange     exchange.Exchange
	wsConn       *websocket.Conn
	gridLevels   []models.GridLevel // 现在代表活动的挂单
	currentPrice float64
	returnPrice  float64
	isRunning    bool
	IsBacktest   bool // 新增：用于区分实盘和回测模式
	mutex        sync.RWMutex
	stopChannel  chan bool
	logger       *log.Logger
}

// NewGridTradingBot 创建一个新的网格交易机器人实例
func NewGridTradingBot(config *models.Config, ex exchange.Exchange, isBacktest bool) *GridTradingBot {
	return &GridTradingBot{
		config:      config,
		exchange:    ex,
		gridLevels:  make([]models.GridLevel, 0),
		isRunning:   false,
		IsBacktest:  isBacktest, // 设置模式
		stopChannel: make(chan bool),
		logger:      log.New(log.Writer(), "[GridBot] ", log.LstdFlags),
	}
}

// initializeGrid 实现新的建仓和挂单策略
func (b *GridTradingBot) initializeGrid() error {
	currentPrice, err := b.exchange.GetPrice(b.config.Symbol)
	if err != nil {
		return fmt.Errorf("获取当前价格失败: %v", err)
	}

	b.mutex.Lock()
	b.currentPrice = currentPrice
	b.returnPrice = currentPrice * (1 + b.config.ReturnRate)
	b.gridLevels = make([]models.GridLevel, 0)
	b.mutex.Unlock()

	b.logger.Printf("当前价格: %.8f, 回归价格: %.8f", b.currentPrice, b.returnPrice)

	if err := b.exchange.SetLeverage(b.config.Symbol, b.config.Leverage); err != nil {
		b.logger.Printf("设置杠杆失败: %v", err)
	}

	b.logger.Println("正在取消所有现有挂单以确保一个干净的开始...")
	if err := b.exchange.CancelAllOpenOrders(b.config.Symbol); err != nil {
		b.logger.Printf("取消订单失败，可能需要手动检查: %v", err)
	}

	// 1. 计算网格数量和总购买量
	gridSpacing := b.config.GridSpacing
	numGrids := int(math.Floor(math.Log(b.returnPrice/b.currentPrice) / math.Log(1+gridSpacing)))
	if numGrids <= 0 {
		numGrids = 1
		b.logger.Println("计算出的网格数小于等于0, 将至少创建1个网格。")
	}

	totalQuantity := 0.0
	for i := 0; i < numGrids; i++ {
		price := b.currentPrice * math.Pow(1+gridSpacing, float64(i))
		totalQuantity += b.calculateQuantity(price)
	}

	b.logger.Printf("计算出当前价格到回归价格之间有 %d 个网格，准备一次性市价买入总数量: %.5f", numGrids, totalQuantity)

	// 2. 一次性市价买入
	marketOrder, err := b.exchange.PlaceOrder(b.config.Symbol, "BUY", "MARKET", totalQuantity, 0, "LONG")
	if err != nil {
		return fmt.Errorf("初始市价买入失败: %v", err)
	}
	b.logger.Printf("初始市价买入成功, 订单ID: %d", marketOrder.OrderId)

	// 3. 在对应位置挂空单
	for i := 0; i < numGrids; i++ {
		sellPrice := b.currentPrice * math.Pow(1+gridSpacing, float64(i+1))
		// 使用 go b.placeNewOrder(...) 以允许它们并发执行
		go b.placeNewOrder("SELL", "LONG", sellPrice)
	}

	// 4. 在当前价格下方挂一个多单
	buyPrice := b.currentPrice * (1 - gridSpacing)
	go b.placeNewOrder("BUY", "LONG", buyPrice)

	b.logger.Println("新的初始化策略执行完毕。")
	return nil
}

// placeNewOrder 是一个辅助函数，用于下单并将其添加到网格级别
func (b *GridTradingBot) placeNewOrder(side, positionSide string, price float64) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	// 确保卖单价格不超过回归价格
	if side == "SELL" && price > b.returnPrice {
		b.logger.Printf("建议的卖出价 %.4f 高于回归价 %.4f。跳过下单。", price, b.returnPrice)
		return
	}

	quantity := b.calculateQuantity(price)

	order, err := b.exchange.PlaceOrder(b.config.Symbol, side, "LIMIT", quantity, price, positionSide)
	if err != nil {
		b.logger.Printf("下 %s 单失败，价格 %.4f: %v", side, price, err)
		return
	}

	b.gridLevels = append(b.gridLevels, models.GridLevel{
		Price:    price,
		Quantity: quantity,
		Side:     side,
		IsActive: true, // All orders in this list are considered active
		OrderID:  order.OrderId,
	})
	b.logger.Printf("成功下 %s 单: ID %d, 价格 %.4f, 数量 %.5f", side, order.OrderId, price, quantity)
}

// calculateQuantity 计算订单数量
func (b *GridTradingBot) calculateQuantity(price float64) float64 {
	usdtValue := b.config.GridValue
	quantity := usdtValue / price

	minQuantity := 0.001
	if quantity < minQuantity {
		quantity = minQuantity
	}
	return math.Floor(quantity*100000) / 100000
}

// checkOrderStatusAndHandleFills 检查活动订单的状态并处理已成交的订单。
// 这个函数现在是原子的：它会锁定机器人状态，执行所有检查、移除、下单和添加操作，然后解锁。
// 这避免了复杂的跨函数锁管理和潜在的死锁。
func (b *GridTradingBot) checkOrderStatusAndHandleFills() {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	// b.logger.Printf("开始检查订单状态，当前活动订单数: %d", len(b.gridLevels))

	if len(b.gridLevels) == 0 {
		// b.logger.Println("没有活动订单。可能是所有订单都已成交且未成功挂新单，或初始订单失败。")
		// 注意：在真实交易中，这里可能需要一个更复杂的重新初始化逻辑。
		// go b.reinitializeGridIfNeeded() // 暂时禁用自动重新初始化
		return
	}

	i := 0
	for i < len(b.gridLevels) {
		grid := b.gridLevels[i]
		// b.logger.Printf("循环 %d: 检查订单 ID: %d", i, grid.OrderID)

		orderStatus, err := b.exchange.GetOrderStatus(b.config.Symbol, grid.OrderID)
		if err != nil {
			b.logger.Printf("获取订单 %d 状态失败: %v", grid.OrderID, err)
			i++ // 获取状态失败，暂时跳过
			continue
		}

		// b.logger.Printf("检查结果 - ID: %d, 状态: '%s'", grid.OrderID, orderStatus.Status)

		if orderStatus.Status == "FILLED" {
			b.logger.Printf("订单 %d (%s) 已成交, 准备挂新单。", grid.OrderID, grid.Side)

			filledPrice, err := strconv.ParseFloat(orderStatus.Price, 64)
			if err != nil || filledPrice == 0 {
				filledPrice = grid.Price // Fallback to grid price
			}

			// 1. 从列表中移除已成交的订单
			b.gridLevels = append(b.gridLevels[:i], b.gridLevels[i+1:]...)

			// 2. 计算新订单的参数 (内联自旧的 handleFilledOrder)
			var newSide string
			var newPrice float64
			if grid.Side == "BUY" {
				newSide = "SELL"
				newPrice = filledPrice * (1 + b.config.GridSpacing)
			} else { // SELL
				newSide = "BUY"
				newPrice = filledPrice * (1 - b.config.GridSpacing)
			}

			// 如果建议的卖单价格高于回归价格，则不挂单
			if newSide == "SELL" && b.returnPrice > 0 && newPrice > b.returnPrice {
				b.logger.Printf("新卖单价格 %.4f 高于回归价格 %.4f，不再挂新单。", newPrice, b.returnPrice)
				// 因为我们移除了一个元素，所以循环不应增加 i
				continue
			}

			quantity := b.calculateQuantity(newPrice)

			// 3. 下一个新订单
			order, err := b.exchange.PlaceOrder(b.config.Symbol, newSide, "LIMIT", quantity, newPrice, "LONG")
			if err != nil {
				b.logger.Printf("挂新 %s 单失败: %v", newSide, err)
				// 即使下单失败，我们已经移除了旧订单，所以只需继续循环
				// 索引 i 不需要增加
				continue
			}

			b.logger.Printf("成功下新 %s 单: ID %d, 价格 %.4f, 数量 %.5f", newSide, order.OrderId, newPrice, quantity)

			// 4. 将新订单加回列表
			newGrid := models.GridLevel{
				Price:    newPrice,
				Quantity: quantity,
				Side:     newSide,
				OrderID:  order.OrderId,
				IsActive: true,
			}
			b.gridLevels = append(b.gridLevels, newGrid)

			// 5. 注意：因为我们移除了旧元素并可能添加了新元素，为了安全地重新评估整个列表，
			// 我们不增加 i，让循环从当前位置（现在是下一个元素）继续。

		} else if orderStatus.Status == "CANCELED" || orderStatus.Status == "EXPIRED" || orderStatus.Status == "REJECTED" {
			b.logger.Printf("订单 %d (%s) 已失效 (状态: %s)。将其从活动列表中移除。", grid.OrderID, grid.Side, orderStatus.Status)
			b.gridLevels = append(b.gridLevels[:i], b.gridLevels[i+1:]...)
			// 注意：因为我们移除了当前元素，所以索引 i 不需要增加
		} else {
			// 订单仍然是 NEW 或 PARTIALLY_FILLED，继续保留并检查下一个
			i++
		}
	}
}

// reinitializeGridIfNeeded is a safety net to restart the grid if all orders are gone
func (b *GridTradingBot) reinitializeGridIfNeeded() {
	b.mutex.Lock()
	if len(b.gridLevels) == 0 {
		b.logger.Println("检测到无活动订单，将在一分钟后尝试重新初始化...")
		b.mutex.Unlock()
		time.Sleep(1 * time.Minute)

		b.mutex.Lock()
		if len(b.gridLevels) == 0 {
			b.logger.Println("重新初始化网格...")
			currentPrice := b.currentPrice
			initialBuyPrice := currentPrice * (1 - b.config.GridSpacing)
			initialSellPrice := currentPrice * (1 + b.config.GridSpacing)
			go b.placeNewOrder("BUY", "LONG", initialBuyPrice)
			go b.placeNewOrder("SELL", "LONG", initialSellPrice)
		} else {
			b.logger.Println("在等待期间已有新订单，取消重新初始化。")
		}
		b.mutex.Unlock()
	} else {
		b.mutex.Unlock()
	}
}

// connectWebSocket 连接WebSocket获取实时价格
func (b *GridTradingBot) connectWebSocket() error {
	wsURL := fmt.Sprintf("%s/ws/%s@ticker", b.config.WSBaseURL, strings.ToLower(b.config.Symbol))
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("WebSocket连接失败: %v", err)
	}
	b.wsConn = conn
	return nil
}

// handleWebSocketMessages 处理WebSocket消息
func (b *GridTradingBot) handleWebSocketMessages() {
	defer b.wsConn.Close()
	for {
		select {
		case <-b.stopChannel:
			return
		default:
			_, message, err := b.wsConn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					b.logger.Printf("WebSocket意外关闭: %v", err)
				}
				b.logger.Println("WebSocket读取错误，将在10秒后尝试重连...")
				time.Sleep(10 * time.Second)
				if err_recon := b.connectWebSocket(); err_recon != nil {
					b.logger.Printf("WebSocket重连失败: %v", err_recon)
					return
				}
				continue
			}

			var ticker struct {
				LastPrice json.Number `json:"c"`
			}
			if err := json.Unmarshal(message, &ticker); err != nil {
				b.logger.Printf("解析价格信息失败: %v", err)
				continue
			}

			price, err := ticker.LastPrice.Float64()
			if err != nil {
				b.logger.Printf("转换价格失败: %v", err)
				continue
			}

			b.mutex.Lock()
			b.currentPrice = price
			b.mutex.Unlock()
		}
	}
}

// Start 启动机器人进行实时交易
func (b *GridTradingBot) Start() error {
	b.mutex.Lock()
	if b.isRunning {
		b.mutex.Unlock()
		return fmt.Errorf("机器人已在运行")
	}
	b.isRunning = true
	b.stopChannel = make(chan bool)
	b.mutex.Unlock()

	if err := b.LoadState("grid_state.json"); err != nil || len(b.gridLevels) == 0 {
		b.logger.Printf("加载状态失败或状态为空，将进行全新初始化: %v", err)
		if err := b.initializeGrid(); err != nil {
			return fmt.Errorf("初始化网格失败: %v", err)
		}
	} else {
		b.logger.Println("从文件成功恢复状态。")
		currentPrice, err := b.exchange.GetPrice(b.config.Symbol)
		if err != nil {
			return fmt.Errorf("启动时获取当前价格失败: %v", err)
		}
		b.mutex.Lock()
		b.currentPrice = currentPrice
		b.returnPrice = currentPrice * (1 + b.config.ReturnRate)
		b.mutex.Unlock()
		if err := b.exchange.SetLeverage(b.config.Symbol, b.config.Leverage); err != nil {
			b.logger.Printf("设置杠杆失败: %v", err)
		}
	}

	if err := b.connectWebSocket(); err != nil {
		return err
	}
	go b.handleWebSocketMessages()

	go b.strategyLoop()

	go b.monitorStatus()

	b.logger.Println("网格交易机器人已启动。")
	return nil
}

// strategyLoop 是机器人的主循环，定期检查订单状态
func (b *GridTradingBot) strategyLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-b.stopChannel:
			return
		case <-ticker.C:
			b.checkOrderStatusAndHandleFills()
		}
	}
}

// StartForBacktest 为回测初始化机器人，应用新的建仓策略
func (b *GridTradingBot) StartForBacktest() error {
	b.mutex.Lock()
	b.isRunning = true
	if b.currentPrice == 0 {
		b.mutex.Unlock()
		return fmt.Errorf("回测开始前必须先设置一个初始价格")
	}
	b.returnPrice = b.currentPrice * (1 + b.config.ReturnRate)
	b.gridLevels = make([]models.GridLevel, 0)
	b.mutex.Unlock()

	b.logger.Printf("回测模式启动。初始价格: %.4f, 回归价格: %.4f", b.currentPrice, b.returnPrice)

	// 1. 计算网格数量和总购买量
	gridSpacing := b.config.GridSpacing
	numGrids := int(math.Floor(math.Log(b.returnPrice/b.currentPrice) / math.Log(1+gridSpacing)))
	if numGrids <= 0 {
		numGrids = 1
		b.logger.Println("计算出的网格数小于等于0, 将至少创建1个网格。")
	}

	totalQuantity := 0.0
	for i := 0; i < numGrids; i++ {
		price := b.currentPrice * math.Pow(1+gridSpacing, float64(i))
		totalQuantity += b.calculateQuantity(price)
	}

	b.logger.Printf("计算出 %d 个网格，准备市价买入总数量: %.5f", numGrids, totalQuantity)

	// 2. 一次性市价买入
	_, err := b.exchange.PlaceOrder(b.config.Symbol, "BUY", "MARKET", totalQuantity, 0, "LONG")
	if err != nil {
		return fmt.Errorf("回测中初始市价买入失败: %v", err)
	}

	// 3. 在对应位置挂空单
	for i := 0; i < numGrids; i++ {
		sellPrice := b.currentPrice * math.Pow(1+gridSpacing, float64(i+1))
		b.placeNewOrder("SELL", "LONG", sellPrice)
	}

	// 4. 在当前价格下方挂一个多单
	buyPrice := b.currentPrice * (1 - gridSpacing)
	b.placeNewOrder("BUY", "LONG", buyPrice)

	b.logger.Println("机器人已为回测初始化。")
	return nil
}

// ProcessBacktestTick 在回测期间的每个价格点被调用
func (b *GridTradingBot) ProcessBacktestTick() {
	b.checkOrderStatusAndHandleFills()
}

// SetCurrentPrice is used for backtesting
func (b *GridTradingBot) SetCurrentPrice(price float64) {
	b.mutex.Lock()
	b.currentPrice = price
	b.mutex.Unlock()
}

// Stop 停止机器人
func (b *GridTradingBot) Stop() {
	b.mutex.Lock()
	if !b.isRunning {
		b.mutex.Unlock()
		return
	}
	b.isRunning = false
	close(b.stopChannel)
	b.mutex.Unlock()

	if err := b.SaveState("grid_state.json"); err != nil {
		b.logger.Printf("保存状态失败: %v", err)
	}

	b.logger.Println("正在取消所有活动订单...")
	b.cancelAllActiveOrders()
	b.logger.Println("网格交易机器人已停止。")
}

// cancelAllActiveOrders 取消所有活动订单
func (b *GridTradingBot) cancelAllActiveOrders() {
	b.mutex.RLock()
	gridsToCancel := make([]models.GridLevel, len(b.gridLevels))
	copy(gridsToCancel, b.gridLevels)
	b.mutex.RUnlock()

	for _, grid := range gridsToCancel {
		if err := b.exchange.CancelOrder(b.config.Symbol, grid.OrderID); err != nil {
			b.logger.Printf("取消订单 %d 失败: %v", grid.OrderID, err)
		} else {
			b.logger.Printf("成功取消订单 %d", grid.OrderID)
		}
	}
}

// monitorStatus 定期打印状态
func (b *GridTradingBot) monitorStatus() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-b.stopChannel:
			return
		case <-ticker.C:
			b.printStatus()
		}
	}
}

// SaveState 将机器人的当前状态（活动订单）保存到文件
func (b *GridTradingBot) SaveState(path string) error {
	b.mutex.RLock()
	defer b.mutex.RUnlock()

	data, err := json.MarshalIndent(b.gridLevels, "", "  ")
	if err != nil {
		return fmt.Errorf("无法序列化状态: %v", err)
	}

	return ioutil.WriteFile(path, data, 0644)
}

// LoadState 从文件加载机器人状态
func (b *GridTradingBot) LoadState(path string) error {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	data, err := ioutil.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			b.logger.Println("状态文件不存在，将创建新状态。")
			return nil
		}
		return fmt.Errorf("无法读取状态文件: %v", err)
	}

	if len(data) == 0 {
		b.logger.Println("状态文件为空，将创建新状态。")
		return nil
	}

	err = json.Unmarshal(data, &b.gridLevels)
	if err != nil {
		return fmt.Errorf("无法反序列化状态: %v", err)
	}

	activeGrids := []models.GridLevel{}
	for _, grid := range b.gridLevels {
		if grid.IsActive && grid.OrderID != 0 {
			activeGrids = append(activeGrids, grid)
		}
	}
	b.gridLevels = activeGrids

	b.logger.Printf("成功从 %s 加载了 %d 个活动订单状态。", path, len(b.gridLevels))
	return nil
}

// printStatus 打印机器人当前状态
func (b *GridTradingBot) printStatus() {
	b.mutex.RLock()
	defer b.mutex.RUnlock()

	b.logger.Println("========== 机器人状态 ==========")
	b.logger.Printf("时间: %s", time.Now().Format("2006-01-02 15:04:05"))
	b.logger.Printf("状态: %s", map[bool]string{true: "运行中", false: "已停止"}[b.isRunning])
	b.logger.Printf("当前价格: %.8f", b.currentPrice)
	b.logger.Printf("回归价格: %.8f", b.returnPrice)

	b.logger.Printf("当前挂单数量: %d", len(b.gridLevels))
	for _, grid := range b.gridLevels {
		b.logger.Printf("  - [ID: %d] %s %.5f @ %.4f", grid.OrderID, grid.Side, grid.Quantity, grid.Price)
	}

	positions, err := b.exchange.GetPositions(b.config.Symbol)
	if err != nil {
		b.logger.Printf("获取持仓失败: %v", err)
	} else {
		hasPosition := false
		for _, pos := range positions {
			posAmt, _ := strconv.ParseFloat(pos.PositionAmt, 64)
			if math.Abs(posAmt) > 0 {
				entryPrice, _ := strconv.ParseFloat(pos.EntryPrice, 64)
				unrealizedProfit, _ := strconv.ParseFloat(pos.UnRealizedProfit, 64)
				b.logger.Printf("持仓: %.5f %s, 开仓均价: %.8f, 未实现盈亏: %.8f",
					posAmt, b.config.Symbol, entryPrice, unrealizedProfit)
				hasPosition = true
			}
		}
		if !hasPosition {
			b.logger.Println("当前无持仓。")
		}
	}
	b.logger.Println("================================")
}
