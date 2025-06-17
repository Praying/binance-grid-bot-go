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

const (
	ActiveOrdersCount = 3 // 在价格两侧各挂的订单数量
)

// GridTradingBot 是网格交易机器人的核心结构
type GridTradingBot struct {
	config       *models.Config
	exchange     exchange.Exchange
	wsConn       *websocket.Conn
	gridLevels   []models.GridLevel // 现在代表活动的挂单
	currentPrice float64
	isRunning    bool
	IsBacktest   bool      // 新增：用于区分实盘和回测模式
	currentTime  time.Time // 新增：用于存储当前时间，主要用于回测日志
	mutex        sync.RWMutex
	stopChannel  chan bool
	logger       *log.Logger
	symbolInfo   *models.SymbolInfo // 缓存交易规则
}

// NewGridTradingBot 创建一个新的网格交易机器人实例
func NewGridTradingBot(config *models.Config, ex exchange.Exchange, isBacktest bool) *GridTradingBot {
	bot := &GridTradingBot{
		config:      config,
		exchange:    ex,
		gridLevels:  make([]models.GridLevel, 0),
		isRunning:   false,
		IsBacktest:  isBacktest, // 设置模式
		stopChannel: make(chan bool),
		logger:      log.New(log.Writer(), "[GridBot] ", log.LstdFlags),
	}

	// 获取并缓存交易规则
	symbolInfo, err := ex.GetSymbolInfo(config.Symbol)
	if err != nil {
		bot.logger.Fatalf("无法获取交易对 %s 的规则: %v", config.Symbol, err)
	}
	bot.symbolInfo = symbolInfo
	bot.logger.Printf("成功获取并缓存了 %s 的交易规则。", config.Symbol)

	return bot
}

// initializeGrid 实现新的动态挂单策略
func (b *GridTradingBot) initializeGrid() error {
	currentPrice, err := b.exchange.GetPrice(b.config.Symbol)
	if err != nil {
		return fmt.Errorf("获取当前价格失败: %v", err)
	}

	b.mutex.Lock()
	b.currentPrice = currentPrice
	b.gridLevels = make([]models.GridLevel, 0)
	b.mutex.Unlock()

	b.logger.Printf("初始化网格，当前价格: %.4f", currentPrice)

	if err := b.exchange.SetLeverage(b.config.Symbol, b.config.Leverage); err != nil {
		b.logger.Printf("设置杠杆失败: %v", err)
	}

	// 初始建仓逻辑（仅在全新启动时执行）
	// 1. 计算回归价格和理论网格数
	returnPrice := currentPrice * (1 + b.config.ReturnRate)
	priceDiff := returnPrice - currentPrice
	priceStep := currentPrice * b.config.GridSpacing

	var conceptualGridCount float64
	if priceStep > 0 {
		conceptualGridCount = math.Floor(priceDiff / priceStep)
	}

	b.logger.Printf("计算初始仓位：当前价: %.4f, 回归率: %.2f, 回归价格上限: %.4f", currentPrice, b.config.ReturnRate, returnPrice)

	if conceptualGridCount > 0 {
		// 2. 计算总投资额
		initialInvestmentUSDT := conceptualGridCount * b.config.GridValue
		b.logger.Printf("理论网格数: %.0f, 单网格价值: %.2f USDT", conceptualGridCount, b.config.GridValue)
		b.logger.Printf("计算得出初始投资总额: %.2f USDT, 准备市价买入底仓...", initialInvestmentUSDT)

		// 3. 执行市价买入
		quantity := initialInvestmentUSDT / currentPrice
		var stepSize string
		for _, f := range b.symbolInfo.Filters {
			if f.FilterType == "LOT_SIZE" {
				stepSize = f.StepSize
			}
		}
		adjustedQuantity := adjustValueToStep(quantity, stepSize)

		if !b.isWithinExposureLimit(adjustedQuantity) {
			b.logger.Printf("初始建仓被阻止：钱包风险暴露将超过限制。")
		} else {
			order, err := b.exchange.PlaceOrder(b.config.Symbol, "BUY", "MARKET", adjustedQuantity, 0) // 市价单价格为0
			if err != nil {
				b.logger.Printf("初始市价买入失败: %v", err)
			} else {
				b.logger.Printf("成功建立初始仓位: 市价买入 %.5f %s, 订单ID: %d", adjustedQuantity, b.config.Symbol, order.OrderId)
			}
		}
	} else {
		b.logger.Println("回归价格低于或等于当前价格，跳过初始建仓。")
	}

	b.logger.Println("正在取消所有现有挂单以确保一个干净的开始...")
	if err := b.exchange.CancelAllOpenOrders(b.config.Symbol); err != nil {
		b.logger.Printf("取消订单失败，可能需要手动检查: %v", err)
	}

	gridSpacing := b.config.GridSpacing

	// 以当前价格为中心，在两侧挂单
	for i := 1; i <= ActiveOrdersCount; i++ {
		// 挂卖单
		sellPrice := currentPrice * (1 + float64(i)*gridSpacing)
		go b.placeNewOrder("SELL", sellPrice)

		// 挂买单
		buyPrice := currentPrice * (1 - float64(i)*gridSpacing)
		go b.placeNewOrder("BUY", buyPrice)
	}

	b.logger.Println("初始化挂单完成。")
	return nil
}

// placeNewOrder 是一个辅助函数，用于下单并将其添加到网格级别
func (b *GridTradingBot) placeNewOrder(side string, price float64) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	// 获取价格和数量的精度规则
	var tickSize, stepSize string
	for _, f := range b.symbolInfo.Filters {
		if f.FilterType == "PRICE_FILTER" {
			tickSize = f.TickSize
		} else if f.FilterType == "LOT_SIZE" {
			stepSize = f.StepSize
		}
	}

	// 调整价格和数量以符合精度要求
	adjustedPrice := adjustValueToStep(price, tickSize)
	quantity := b.calculateQuantity(adjustedPrice)
	adjustedQuantity := adjustValueToStep(quantity, stepSize)

	// 在下买单前检查钱包风险暴露
	if side == "BUY" {
		if !b.isWithinExposureLimit(quantity) {
			b.logger.Printf("下单被阻止：钱包风险暴露将超过限制。")
			return
		}
	}

	order, err := b.exchange.PlaceOrder(b.config.Symbol, side, "LIMIT", adjustedQuantity, adjustedPrice)
	if err != nil {
		b.logger.Printf("下 %s 单失败，价格 %.4f: %v", side, adjustedPrice, err)
		return
	}

	b.gridLevels = append(b.gridLevels, models.GridLevel{
		Price:    adjustedPrice,
		Quantity: adjustedQuantity,
		Side:     side,
		IsActive: true, // All orders in this list are considered active
		OrderID:  order.OrderId,
	})
	b.logger.Printf("成功下 %s 单: ID %d, 价格 %.4f, 数量 %.5f", side, order.OrderId, adjustedPrice, adjustedQuantity)
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

// checkOrderStatusAndHandleFills 检查并处理订单，维持动态网格
func (b *GridTradingBot) checkOrderStatusAndHandleFills() {
	// 复制一份订单列表以在无锁的情况下进行网络请求
	b.mutex.RLock()
	if len(b.gridLevels) == 0 {
		b.mutex.RUnlock()
		b.logger.Println("无活动订单，检查是否需要重新初始化...")
		return
	}
	gridsToCheck := make([]models.GridLevel, len(b.gridLevels))
	copy(gridsToCheck, b.gridLevels)
	b.mutex.RUnlock()

	type filledOrderInfo struct {
		grid   models.GridLevel
		status *models.Order
	}
	filledOrders := make(chan filledOrderInfo, len(gridsToCheck))
	invalidOrderIDs := make(chan int64, len(gridsToCheck))

	var wg sync.WaitGroup
	for _, grid := range gridsToCheck {
		wg.Add(1)
		go func(g models.GridLevel) {
			defer wg.Done()
			orderStatus, err := b.exchange.GetOrderStatus(b.config.Symbol, g.OrderID)
			if err != nil {
				if b.IsBacktest && strings.Contains(err.Error(), "未找到") {
					orderStatus = &models.Order{Status: "FILLED", Price: fmt.Sprintf("%.8f", g.Price)}
				} else {
					b.logger.Printf("获取订单 %d 状态失败: %v", g.OrderID, err)
					return
				}
			}

			switch orderStatus.Status {
			case "FILLED":
				filledOrders <- filledOrderInfo{grid: g, status: orderStatus}
			case "CANCELED", "EXPIRED", "REJECTED":
				b.logger.Printf("订单 %d (%s) 已失效 (状态: %s)。准备将其从活动列表中移除。", g.OrderID, g.Side, orderStatus.Status)
				invalidOrderIDs <- g.OrderID
			}
		}(grid)
	}

	wg.Wait()
	close(filledOrders)
	close(invalidOrderIDs)

	// 检查是否有任何订单状态发生了变化
	if len(filledOrders) == 0 && len(invalidOrderIDs) == 0 {
		return // 没有任何成交或失效的订单，直接返回，不做任何操作
	}

	// 现在，一次性获取写锁来处理所有状态变更
	b.mutex.Lock()
	defer b.mutex.Unlock()

	// 只要有任何订单成交或失效，就认为网格需要重新评估
	didStateChange := false

	if len(filledOrders) > 0 {
		didStateChange = true
		for info := range filledOrders {
			b.handleFilledOrder(info.grid, info.status)
		}
	}

	if len(invalidOrderIDs) > 0 {
		didStateChange = true
		for orderID := range invalidOrderIDs {
			b.removeGridLevel(orderID)
		}
	}

	// 只有在订单状态真实发生变化后，才重塑网格
	if didStateChange {
		b.logger.Println("检测到订单成交或失效，开始重塑网格...")
		b.maintainGridLevels()
	}
}

// handleFilledOrder 处理已成交的订单
func (b *GridTradingBot) handleFilledOrder(grid models.GridLevel, orderStatus *models.Order) {
	// 该函数现在期望调用者已经持有锁

	b.logger.Printf("订单 %d (%s @ %.4f) 已成交, 准备挂新单。", grid.OrderID, grid.Side, grid.Price)

	// 从活动列表中移除已成交的订单
	b.removeGridLevel(grid.OrderID)

	filledPrice, err := strconv.ParseFloat(orderStatus.Price, 64)
	if err != nil || filledPrice == 0 {
		filledPrice = grid.Price
	}

	// 移除旧的挂单逻辑。
	// 新的逻辑是，在移除成交订单后，由 maintainGridLevels 统一负责根据最新价格重塑整个网格，
	// 而不是在这里简单地挂一个方向相反的订单。
}

// removeGridLevel 安全地从 gridLevels 中移除一个订单
func (b *GridTradingBot) removeGridLevel(orderID int64) {
	newGridLevels := make([]models.GridLevel, 0)
	for _, g := range b.gridLevels {
		if g.OrderID != orderID {
			newGridLevels = append(newGridLevels, g)
		}
	}
	b.gridLevels = newGridLevels
}

// maintainGridLevels 检查并维持两侧挂单数量
func (b *GridTradingBot) maintainGridLevels() {
	// 该函数期望调用者已经持有锁
	// 1. 以最新的当前价格为中心，计算出理论上应该存在的3x3网格
	theoreticalPrices := make(map[float64]string) // price -> side
	gridSpacing := b.config.GridSpacing
	currentPrice := b.currentPrice

	var tickSize string
	for _, f := range b.symbolInfo.Filters {
		if f.FilterType == "PRICE_FILTER" {
			tickSize = f.TickSize
		}
	}

	for i := 1; i <= ActiveOrdersCount; i++ {
		// 理论卖单价
		sellPrice := adjustValueToStep(currentPrice*(1+float64(i)*gridSpacing), tickSize)
		theoreticalPrices[sellPrice] = "SELL"
		// 理论买单价
		buyPrice := adjustValueToStep(currentPrice*(1-float64(i)*gridSpacing), tickSize)
		theoreticalPrices[buyPrice] = "BUY"
	}

	// 2. 找出当前实际挂单中，哪些是不在理论网格里的（需要取消）
	existingPrices := make(map[float64]bool)
	ordersToCancel := make([]models.GridLevel, 0)
	for _, grid := range b.gridLevels {
		if _, exists := theoreticalPrices[grid.Price]; !exists {
			ordersToCancel = append(ordersToCancel, grid)
		}
		existingPrices[grid.Price] = true
	}

	// 3. 取消多余的订单
	if len(ordersToCancel) > 0 {
		b.logger.Println("检测到盘口变化，正在取消不再符合网格布局的旧订单...")
		for _, grid := range ordersToCancel {
			b.logger.Printf("正在同步取消订单 ID %d @ %.4f...", grid.OrderID, grid.Price)
			// 将并发调用改为同步阻塞调用，以避免因时间戳重复导致API错误
			// 这是解决-1007错误的最终手段
			if err := b.exchange.CancelOrder(b.config.Symbol, grid.OrderID); err != nil {
				// 即使取消失败，也打印错误并继续，因为这个订单可能已经不存在了
				b.logger.Printf("取消订单 ID %d 失败: %v", grid.OrderID, err)
			}
			// 从活动列表中移除
			b.removeGridLevel(grid.OrderID)
		}
	}

	// 4. 找出理论网格中，哪些是当前盘口没有的（需要补充）
	for price, side := range theoreticalPrices {
		if _, exists := existingPrices[price]; !exists {
			// 这个价格的订单需要被挂上
			b.logger.Printf("补充缺失的 %s 订单 @ %.4f", side, price)
			go b.placeNewOrder(side, price)
		}
	}
}

// connectWebSocket 连接WebSocket获取实时价格
func (b *GridTradingBot) connectWebSocket() error {
	wsURL := fmt.Sprintf("%s/ws/%s@aggTrade", b.config.WSBaseURL, strings.ToLower(b.config.Symbol))
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("WebSocket连接失败: %v", err)
	}
	b.wsConn = conn
	return nil
}

// webSocketLoop 是一个守护进程，负责维持WebSocket的连接和重连
func (b *GridTradingBot) webSocketLoop() {
	for {
		select {
		case <-b.stopChannel:
			b.logger.Println("WebSocket循环已停止。")
			return
		default:
			if err := b.connectWebSocket(); err != nil {
				b.logger.Printf("WebSocket连接失败: %v。5秒后重试...", err)
				time.Sleep(5 * time.Second)
				continue
			}

			b.logger.Println("WebSocket连接成功。")
			// handleWebSocketMessages现在会阻塞直到连接断开
			if err := b.handleWebSocketMessages(); err != nil {
				b.logger.Printf("WebSocket处理时发生错误: %v", err)
			}
			// 连接断开后，关闭连接，循环会再次尝试重连
			if b.wsConn != nil {
				b.wsConn.Close()
			}
			b.logger.Println("WebSocket连接已断开，准备重连...")
			time.Sleep(5 * time.Second) // 等待5秒再重连
		}
	}
}

// handleWebSocketMessages 为一个已建立的连接处理消息，并实现心跳机制
func (b *GridTradingBot) handleWebSocketMessages() error {
	const (
		pongWait   = 60 * time.Second
		pingPeriod = (pongWait * 9) / 10 // Must be less than pongWait
	)

	// 设置Pong处理器来延长读取超时
	b.wsConn.SetReadDeadline(time.Now().Add(pongWait))
	b.wsConn.SetPongHandler(func(string) error {
		b.wsConn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	// 启动一个goroutine来定期发送Ping
	pingTicker := time.NewTicker(pingPeriod)
	defer pingTicker.Stop()

	// 使用一个单独的channel来停止ping goroutine
	pingStop := make(chan struct{})
	defer close(pingStop)

	go func() {
		for {
			select {
			case <-pingTicker.C:
				if err := b.wsConn.WriteMessage(websocket.PingMessage, nil); err != nil {
					b.logger.Printf("发送Ping失败: %v", err)
					return // exit goroutine
				}
			case <-pingStop:
				return
			case <-b.stopChannel:
				return
			}
		}
	}()

	// 主循环，用于读取数据消息
	for {
		select {
		case <-b.stopChannel:
			// 优雅关闭
			err := b.wsConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			if err != nil {
				return fmt.Errorf("发送WebSocket关闭帧失败: %v", err)
			}
			return nil
		default:
			_, message, err := b.wsConn.ReadMessage()
			if err != nil {
				// 任何读取错误都意味着连接已损坏，返回错误让 webSocketLoop 处理重连
				return fmt.Errorf("读取消息失败: %v", err)
			}

			var ticker struct {
				Price json.Number `json:"p"` // "p"代表价格
			}
			if err := json.Unmarshal(message, &ticker); err != nil {
				b.logger.Printf("解析价格信息失败: %v", err)
				continue // 继续接收下一条消息
			}

			price, err := ticker.Price.Float64()
			if err != nil {
				b.logger.Printf("转换价格失败: %v", err)
				continue // 继续接收下一条消息
			}

			b.mutex.Lock()
			b.currentPrice = price
			b.mutex.Unlock()
		}
	}
}

// Start 启动机器人进行实时交易
// Start 启动机器人进行实时交易
func (b *GridTradingBot) Start() error {
	b.mutex.Lock()
	if b.isRunning {
		b.mutex.Unlock()
		return fmt.Errorf("机器人已在运行")
	}
	b.mutex.Unlock() // 暂时解锁以执行网络操作

	// 1. 检查时间同步
	serverTime, err := b.exchange.GetServerTime()
	if err != nil {
		return fmt.Errorf("获取币安服务器时间失败: %v。请检查网络连接和API配置。", err)
	}
	localTime := time.Now().UnixMilli()
	timeDiff := serverTime - localTime
	if timeDiff > 1000 || timeDiff < -1000 {
		b.logger.Fatalf("!!! CRITICAL: 系统时间与币安服务器时间不同步! 偏差: %d ms。请立即同步您的系统时钟(NTP)!", timeDiff)
	}
	b.logger.Printf("时间同步检查通过。与服务器时间偏差: %d ms。", timeDiff)

	// 2. 正式开始，锁定状态
	b.mutex.Lock()
	b.isRunning = true
	b.stopChannel = make(chan bool)
	b.mutex.Unlock()

	// 3. 从交易所获取真实的挂单情况
	b.logger.Println("正在从交易所获取当前挂单...")
	openOrders, err := b.exchange.GetOpenOrders(b.config.Symbol)
	if err != nil {
		return fmt.Errorf("从交易所获取挂单列表失败: %v", err)
	}

	b.mutex.Lock()
	b.gridLevels = make([]models.GridLevel, 0)
	for _, order := range openOrders {
		price, _ := strconv.ParseFloat(order.Price, 64)
		qty, _ := strconv.ParseFloat(order.OrigQty, 64)
		b.gridLevels = append(b.gridLevels, models.GridLevel{
			Price:    price,
			Quantity: qty,
			Side:     order.Side,
			IsActive: true,
			OrderID:  order.OrderId,
		})
	}
	b.mutex.Unlock()

	// 4. 根据是否存在挂单决定是初始化还是恢复
	if len(openOrders) == 0 {
		b.logger.Println("交易所无此交易对的挂单，执行全新初始化...")
		if err := b.initializeGrid(); err != nil {
			return fmt.Errorf("初始化网格失败: %v", err)
		}
	} else {
		b.logger.Printf("从交易所成功恢复 %d 个挂单。", len(openOrders))
		// 获取当前价格，然后让 maintainGridLevels 来决定是否需要调整
		currentPrice, err := b.exchange.GetPrice(b.config.Symbol)
		if err != nil {
			return fmt.Errorf("启动时获取当前价格失败: %v", err)
		}
		b.mutex.Lock()
		b.currentPrice = currentPrice
		b.mutex.Unlock()

		if err := b.exchange.SetLeverage(b.config.Symbol, b.config.Leverage); err != nil {
			b.logger.Printf("设置杠杆失败: %v", err)
		}

		b.logger.Println("状态恢复完成，开始根据当前价格调整网格...")
		// 加锁调用 maintainGridLevels
		b.mutex.Lock()
		b.maintainGridLevels()
		b.mutex.Unlock()
	}

	// 5. 启动后台服务
	go b.webSocketLoop()
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

// StartForBacktest 为回测初始化机器人
func (b *GridTradingBot) StartForBacktest() error {
	b.mutex.Lock()
	b.isRunning = true
	if b.currentPrice == 0 {
		b.mutex.Unlock()
		return fmt.Errorf("回测开始前必须先设置一个初始价格")
	}
	b.gridLevels = make([]models.GridLevel, 0)
	b.mutex.Unlock()

	b.logger.Printf("回测模式启动。初始价格: %.4f", b.currentPrice)

	// 在回测中，直接调用初始化函数来设置初始网格
	return b.initializeGrid()
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

// adjustValueToStep 通过字符串操作确保精度，避免浮点数计算误差
func adjustValueToStep(value float64, step string) float64 {
	// 找到步长的小数位数
	if !strings.Contains(step, ".") {
		// 如果步长是 "1", "10" 等整数，直接取整
		return math.Floor(value)
	}
	decimalPlaces := len(step) - strings.Index(step, ".") - 1

	// 使用 FormatFloat 将 value 转换为具有正确小数位数的字符串
	// 'f' 表示常规格式, -1 表示尽可能少的数字, 但这里我们用 decimalPlaces
	// 乘以一个因子再取整，然后再除以这个因子，是处理浮点数精度的常用方法
	factor := math.Pow(10, float64(decimalPlaces))
	adjustedValue := math.Floor(value*factor) / factor

	// 最终再用 strconv 确保转换的正确性
	finalValue, _ := strconv.ParseFloat(fmt.Sprintf("%.*f", decimalPlaces, adjustedValue), 64)
	return finalValue
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
