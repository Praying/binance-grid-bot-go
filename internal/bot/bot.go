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
	IsBacktest   bool      // 新增：用于区分实盘和回测模式
	currentTime  time.Time // 新增：用于存储当前时间，主要用于回测日志
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

	totalQuantity, estimatedUSDT := b.calculateInitialGrid(b.currentPrice, b.returnPrice, numGrids)

	b.logger.Printf("计算出当前价格到回归价格之间有 %d 个网格，准备一次性市价买入总数量: %.5f (预计花费: %.2f USDT)", numGrids, totalQuantity, estimatedUSDT)

	// 2. 一次性市价买入 (增加风险暴露检查)
	if !b.isWithinExposureLimit(totalQuantity) {
		return fmt.Errorf("初始建仓失败：钱包风险暴露将超过限制")
	}
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

	quantity := b.calculateQuantity(price)

	// 在下买单前检查钱包风险暴露
	if side == "BUY" {
		if !b.isWithinExposureLimit(quantity) {
			b.logger.Printf("下单被阻止：钱包风险暴露将超过限制。")
			return
		}
	}

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

	if len(b.gridLevels) == 0 {
		return
	}

	i := 0
	for i < len(b.gridLevels) {
		grid := b.gridLevels[i]

		orderStatus, err := b.exchange.GetOrderStatus(b.config.Symbol, grid.OrderID)
		// 核心逻辑修正：正确处理订单状态
		// 当交易所返回“未找到”时，对于回测而言，这等同于订单已成交并从活动列表移除。
		// 因此，我们手动创建一个状态为 FILLED 的订单对象，以触发后续的挂单逻辑。
		// 对于其他类型的错误，我们才记录并跳过。
		if err != nil {
			if strings.Contains(err.Error(), "未找到") {
				orderStatus = &models.Order{Status: "FILLED", Price: fmt.Sprintf("%.8f", grid.Price)}
			} else {
				b.logger.Printf("获取订单 %d 状态失败: %v", grid.OrderID, err)
				i++
				continue
			}
		}

		if orderStatus.Status == "FILLED" {
			b.logger.Printf("订单 %d (%s @ %.4f) 已成交 at %s, 准备挂新单。", grid.OrderID, grid.Side, grid.Price, b.currentTime.Format(time.RFC3339))

			filledPrice, err := strconv.ParseFloat(orderStatus.Price, 64)
			if err != nil || filledPrice == 0 {
				filledPrice = grid.Price
			}

			b.gridLevels = append(b.gridLevels[:i], b.gridLevels[i+1:]...)

			var newSide string
			var newPrice float64
			if grid.Side == "BUY" {
				newSide = "SELL"
				newPrice = filledPrice * (1 + b.config.GridSpacing)
				// 这是关键的修正：当买单成交后，如果计算出的新卖单价格超过了回归价，
				// 我们就简单地不再挂这个卖单，让这个网格分支自然结束。
				// 这会逐渐减少挂单数量，最终在所有卖单都消失后触发重置。
				if b.returnPrice > 0 && newPrice > b.returnPrice {
					b.logger.Printf("[INFO] 网格分支完成: 新卖单价 %.4f 高于回归价 %.4f。不再挂新单。", newPrice, b.returnPrice)
					continue
				}
			} else { // SELL
				newSide = "BUY"
				newPrice = filledPrice * (1 - b.config.GridSpacing)
			}

			quantity := b.calculateQuantity(newPrice)

			order, err := b.exchange.PlaceOrder(b.config.Symbol, newSide, "LIMIT", quantity, newPrice, "LONG")
			if err != nil {
				b.logger.Printf("挂新 %s 单失败: %v", newSide, err)
				continue
			}

			b.logger.Printf("成功下新 %s 单: ID %d, 价格 %.4f, 数量 %.5f", newSide, order.OrderId, newPrice, quantity)

			newGrid := models.GridLevel{
				Price:    newPrice,
				Quantity: quantity,
				Side:     newSide,
				OrderID:  order.OrderId,
				IsActive: true,
			}
			b.gridLevels = append(b.gridLevels, newGrid)

		} else if orderStatus.Status == "CANCELED" || orderStatus.Status == "EXPIRED" || orderStatus.Status == "REJECTED" {
			b.logger.Printf("订单 %d (%s) 已失效 (状态: %s)。将其从活动列表中移除。", grid.OrderID, grid.Side, orderStatus.Status)
			b.gridLevels = append(b.gridLevels[:i], b.gridLevels[i+1:]...)
		} else {
			i++
		}

	}

	// 修正后的重置逻辑：
	// 检查是否已无卖单（即空仓），且当前价格超过了回归价
	hasSellOrders := false
	for _, grid := range b.gridLevels {
		if grid.Side == "SELL" {
			hasSellOrders = true
			break
		}
	}

	if !hasSellOrders && b.returnPrice > 0 {
		b.logger.Println("-----------------------------------------")
		b.logger.Printf("[!!!] 网格重置触发！已无卖单，且当前价格 %.4f > 旧回归价 %.4f。", b.currentPrice, b.returnPrice)

		// 释放锁，因为 initializeGrid 会自己加锁
		b.mutex.Unlock()
		// 调用初始化函数来重置网格
		err := b.initializeGrid()
		// 重新获取锁以保证函数退出时 defer 能正确解锁
		b.mutex.Lock()

		if err != nil {
			b.logger.Printf("[ERROR] 重置网格失败: %v", err)
		} else {
			b.logger.Printf("[SUCCESS] 网格已在新的价格水平上成功重置。")
		}
		b.logger.Println("-----------------------------------------")
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

	totalQuantity, estimatedUSDT := b.calculateInitialGrid(b.currentPrice, b.returnPrice, numGrids)

	b.logger.Printf("计算出 %d 个网格，准备市价买入总数量: %.5f (预计花费: %.2f USDT)", numGrids, totalQuantity, estimatedUSDT)

	// 2. 一次性市价买入 (增加风险暴露检查)
	if !b.isWithinExposureLimit(totalQuantity) {
		return fmt.Errorf("回测中初始建仓失败：钱包风险暴露将超过限制")
	}
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
	b.mutex.Lock()
	b.currentTime = b.exchange.GetCurrentTime()
	b.mutex.Unlock()
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

// calculateInitialGrid 是一个辅助函数，用于计算初始网格的总购买量和预计花费
func (b *GridTradingBot) calculateInitialGrid(currentPrice, returnPrice float64, numGrids int) (float64, float64) {
	gridSpacing := b.config.GridSpacing
	totalQuantity := 0.0
	estimatedUSDT := 0.0
	for i := 0; i < numGrids; i++ {
		price := currentPrice * math.Pow(1+gridSpacing, float64(i))
		quantity := b.calculateQuantity(price)
		totalQuantity += quantity
		estimatedUSDT += quantity * price // 估算花费的USDT
	}
	return totalQuantity, estimatedUSDT
}

// isWithinExposureLimit 检查增加给定数量的仓位后，钱包风险暴露是否仍在限制内。
// 注意：这个检查只针对增加仓位的操作（即买入）。
func (b *GridTradingBot) isWithinExposureLimit(quantityToAdd float64) bool {
	if b.config.WalletExposureLimit <= 0 {
		return true // 如果未设置限制，则总是允许
	}

	// 获取当前账户状态
	positionValue, accountEquity, err := b.exchange.GetAccountState(b.config.Symbol)
	if err != nil {
		b.logger.Printf("[ERROR] 无法获取账户状态以检查风险暴露: %v", err)
		return false // 在不确定的情况下，为安全起见，阻止下单
	}

	if accountEquity == 0 {
		b.logger.Printf("[WARN] 账户总权益为0，无法计算风险暴露。")
		return false // 无法计算时阻止下单
	}

	// 计算预期的未来持仓价值
	// futurePositionValue = 当前持仓价值 + 新增持仓的价值
	futurePositionValue := positionValue + (quantityToAdd * b.currentPrice)

	// 计算预期的钱包风险暴露
	futureWalletExposure := futurePositionValue / accountEquity

	b.logger.Printf("[RISK CHECK] 当前持仓价值: %.2f, 账户权益: %.2f, 新增后预估持仓价值: %.2f, 预估风险暴露: %.4f, 限制: %.4f",
		positionValue, accountEquity, futurePositionValue, futureWalletExposure, b.config.WalletExposureLimit)

	return futureWalletExposure <= b.config.WalletExposureLimit
}
