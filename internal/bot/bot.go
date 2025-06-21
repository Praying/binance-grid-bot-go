package bot

import (
	"binance-grid-bot-go/internal/exchange"
	"binance-grid-bot-go/internal/logger"
	"binance-grid-bot-go/internal/models"
	"encoding/json"
	"fmt"
	"io/ioutil"
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
	config                  *models.Config
	exchange                exchange.Exchange
	wsConn                  *websocket.Conn
	gridLevels              []models.GridLevel // 现在代表活动的挂单
	currentPrice            float64
	isRunning               bool
	IsBacktest              bool      // 新增：用于区分实盘和回测模式
	currentTime             time.Time // 新增：用于存储当前时间，主要用于回测日志
	basePositionEstablished bool      // 新增：标记初始底仓是否已建立
	conceptualGrid          []float64 // 新增：存储理论上的“天地网格”所有价位
	entryPrice              float64   // 新增：记录本次周期的初始入场价
	reversionPrice          float64   // 新增：记录本次周期的回归价格（网格上限）
	isReentering            bool      // 新增：防止再入场逻辑并发执行的状态锁
	reentrySignal           chan bool // 新增：用于解耦再入场信号的通道
	mutex                   sync.RWMutex
	stopChannel             chan bool
	symbolInfo              *models.SymbolInfo // 缓存交易规则
	isHalted                bool               // 新增：标记机器人是否因无法交易而暂停
}

// NewGridTradingBot 创建一个新的网格交易机器人实例
func NewGridTradingBot(config *models.Config, ex exchange.Exchange, isBacktest bool) *GridTradingBot {
	bot := &GridTradingBot{
		config:                  config,
		exchange:                ex,
		gridLevels:              make([]models.GridLevel, 0),
		isRunning:               false,
		IsBacktest:              isBacktest, // 设置模式
		basePositionEstablished: false,      // 初始为 false
		stopChannel:             make(chan bool),
		reentrySignal:           make(chan bool, 1), // 带缓冲的channel，防止信号发送阻塞
		isHalted:                false,
	}

	// 获取并缓存交易规则
	symbolInfo, err := ex.GetSymbolInfo(config.Symbol)
	if err != nil {
		logger.S().Fatalf("无法获取交易对 %s 的规则: %v", config.Symbol, err)
	}
	bot.symbolInfo = symbolInfo
	logger.S().Infof("成功获取并缓存了 %s 的交易规则。", config.Symbol)

	return bot
}

// establishBasePositionAndWait 尝试建立初始底仓并阻塞等待其成交
func (b *GridTradingBot) establishBasePositionAndWait(quantity float64) error {
	order, err := b.exchange.PlaceOrder(b.config.Symbol, "BUY", "MARKET", quantity, 0)
	if err != nil {
		return fmt.Errorf("初始市价买入失败: %v", err)
	}
	logger.S().Infof("已提交初始市价买入订单 ID: %d, 数量: %.5f. 等待成交...", order.OrderId, quantity)

	// 轮询检查订单状态
	ticker := time.NewTicker(500 * time.Millisecond) // 每500ms检查一次
	defer ticker.Stop()

	// 设置一个超时以防永久阻塞
	timeout := time.After(2 * time.Minute)

	for {
		select {
		case <-ticker.C:
			status, err := b.exchange.GetOrderStatus(b.config.Symbol, order.OrderId)
			if err != nil {
				// 在回测模式下，GetOrderStatus 可能会因为订单立即成交并从列表中移除而返回 "未找到"
				// 我们需要依赖 exchange 层的逻辑来正确处理。
				// 在我们的 backtest_exchange 中，订单状态会被设置为 FILLED，所以这个错误不应该经常发生。
				// 但作为一种保障，如果错误是 "未找到" 并且在回测中，我们假定它已成交。
				if b.IsBacktest && strings.Contains(err.Error(), "未找到") {
					logger.S().Infof("初始订单 %d 状态检查返回 '未找到'，在回测模式下假定为已成交。", order.OrderId)
					b.mutex.Lock()
					b.basePositionEstablished = true
					b.mutex.Unlock()
					return nil
				}
				logger.S().Warnf("获取初始订单 %d 状态失败: %v. 继续尝试...", order.OrderId, err)
				continue
			}

			switch status.Status {
			case "FILLED":
				logger.S().Infof("初始仓位订单 %d 已成交!", order.OrderId)
				b.mutex.Lock()
				b.basePositionEstablished = true
				b.mutex.Unlock()
				return nil // 成功
			case "CANCELED", "REJECTED", "EXPIRED":
				return fmt.Errorf("初始仓位订单 %d 建立失败，状态为: %s", order.OrderId, status.Status)
			default:
				// "NEW" or "PARTIALLY_FILLED", 继续等待
				logger.S().Debugf("初始订单 %d 状态: %s. 等待成交...", order.OrderId, status.Status)
			}
		case <-timeout:
			return fmt.Errorf("等待初始订单 %d 成交超时", order.OrderId)
		case <-b.stopChannel:
			return fmt.Errorf("机器人已停止，中断建立初始仓位流程")
		}
	}
}

// enterMarketAndSetupGrid 实现全新的“天地网格”和“周期性再入场”逻辑
func (b *GridTradingBot) enterMarketAndSetupGrid() error {
	logger.S().Info("--- 开始新的交易周期 ---")

	// 步骤 1: 定义周期参数
	currentPrice, err := b.exchange.GetPrice(b.config.Symbol)
	if err != nil {
		return fmt.Errorf("获取当前价格失败: %v", err)
	}

	b.mutex.Lock()
	b.currentPrice = currentPrice
	b.entryPrice = currentPrice
	b.reversionPrice = b.entryPrice * (1 + b.config.ReturnRate)
	b.gridLevels = make([]models.GridLevel, 0) // 清空旧的活动订单
	b.conceptualGrid = make([]float64, 0)      // 清空旧的天地网格
	b.isReentering = false                     // 重置再入场状态锁
	b.mutex.Unlock()

	logger.S().Infof("定义新周期: 入场价: %.4f, 回归价 (网格上限): %.4f", b.entryPrice, b.reversionPrice)

	// 步骤 2: 生成天地网格
	b.mutex.Lock()
	price := b.reversionPrice
	for price > (b.entryPrice * 0.5) {
		b.conceptualGrid = append(b.conceptualGrid, price)
		price *= (1 - b.config.GridSpacing)
	}
	b.mutex.Unlock()

	if len(b.conceptualGrid) == 0 {
		logger.S().Warn("计算出的天地网格为空，可能是回归率或网格间距设置不当。跳过建仓和挂单。")
		b.mutex.Lock()
		b.basePositionEstablished = true // 标记为true以允许机器人继续运行，即使是空操作
		b.mutex.Unlock()
		return nil
	}
	logger.S().Infof("成功生成“天地网格”，共 %d 个理论价位。", len(b.conceptualGrid))

	// 步骤 3: 计算并建立底仓 (部分持仓模式)
	sellGridCount := 0
	for _, price := range b.conceptualGrid {
		if price > b.entryPrice {
			sellGridCount++
		}
	}
	buyGridCount := len(b.conceptualGrid) - sellGridCount
	initialInvestmentUSDT := float64(sellGridCount) * b.config.GridValue
	reservedCash := float64(buyGridCount) * b.config.GridValue

	logger.S().Infof("理论卖出网格数: %d, 理论买入网格数: %d, 单网格价值: %.2f USDT", sellGridCount, buyGridCount, b.config.GridValue)
	logger.S().Infof("计算得出初始底仓价值: %.2f USDT, 需保留现金: %.2f USDT.", initialInvestmentUSDT, reservedCash)
	logger.S().Infof("准备市价买入底仓...")

	quantity := initialInvestmentUSDT / b.entryPrice
	var stepSize string
	for _, f := range b.symbolInfo.Filters {
		if f.FilterType == "LOT_SIZE" {
			stepSize = f.StepSize
		}
	}
	adjustedQuantity := adjustValueToStep(quantity, stepSize)

	if !b.isWithinExposureLimit(adjustedQuantity) {
		logger.S().Warnf("初始建仓被阻止：钱包风险暴露将超过限制。")
		b.mutex.Lock()
		b.basePositionEstablished = true // 同样标记为true
		b.mutex.Unlock()
	} else {
		if err := b.establishBasePositionAndWait(adjustedQuantity); err != nil {
			// 如果建立底仓失败，这是一个致命错误，应终止机器人
			return fmt.Errorf("建立初始仓位失败，无法继续: %v", err)
		}
	}

	// 步骤 4: 初始化挂单
	b.mutex.RLock()
	isEstablished := b.basePositionEstablished
	b.mutex.RUnlock()

	if isEstablished {
		logger.S().Info("初始仓位已确认，现在开始设置网格订单...")
		// 取消所有可能存在的旧订单
		if err := b.exchange.CancelAllOpenOrders(b.config.Symbol); err != nil {
			logger.S().Warnf("清理旧订单失败，可能需要手动检查: %v", err)
		}
		// 使用新的维护逻辑来挂单
		b.maintainGridOrders()
		logger.S().Info("--- 新周期初始化挂单完成 ---")
	} else {
		// 这种情况理论上不应发生，因为establishBasePositionAndWait会返回错误
		logger.S().Error("严重错误：底仓状态未被正确标记，无法进行挂单。")
	}

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
			logger.S().Warnf("下单被阻止：钱包风险暴露将超过限制。")
			return
		}
	}

	order, err := b.exchange.PlaceOrder(b.config.Symbol, side, "LIMIT", adjustedQuantity, adjustedPrice)
	if err != nil {
		logger.S().Errorf("下 %s 单失败，价格 %.4f: %v", side, adjustedPrice, err)
		return
	}

	b.gridLevels = append(b.gridLevels, models.GridLevel{
		Price:    adjustedPrice,
		Quantity: adjustedQuantity,
		Side:     side,
		IsActive: true, // All orders in this list are considered active
		OrderID:  order.OrderId,
	})
	logger.S().Infof("成功下 %s 单: ID %d, 价格 %.4f, 数量 %.5f", side, order.OrderId, adjustedPrice, adjustedQuantity)
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

// maintainGridOrdersLocked 是全新的核心维护函数（无锁版），采用“理想网格同步”逻辑
// 调用此函数前必须持有锁
func (b *GridTradingBot) maintainGridOrdersLocked() {

	// 1. 获取当前状态
	currentPrice := b.currentPrice
	conceptualGrid := b.conceptualGrid
	activeOrders := b.gridLevels
	if len(conceptualGrid) == 0 {
		return // 如果没有天地网格，则不执行任何操作
	}

	// 2. 计算理想挂单 (Ideal Orders)
	idealPrices := make(map[float64]string) // 使用map来快速查找理想价格点
	// 找到距离当前价最近的卖单价位
	sellIndex := -1
	for i, p := range conceptualGrid {
		if p > currentPrice { // 修正: 必须严格大于当前价
			sellIndex = i
		} else {
			break // 价格已经低于或等于当前价，后续都是更低的价格
		}
	}
	// 找到距离当前价最近的买单价位 (从高到低找第一个低于当前价的)
	buyIndex := -1
	for i, p := range conceptualGrid {
		if p < currentPrice {
			buyIndex = i
			break
		}
	}

	// 添加理想卖单价位
	if sellIndex != -1 {
		for i := 0; i < b.config.ActiveOrdersCount && (sellIndex-i) >= 0; i++ {
			idealPrices[conceptualGrid[sellIndex-i]] = "SELL"
		}
	}
	// 添加理想买单价位
	if buyIndex != -1 {
		for i := 0; i < b.config.ActiveOrdersCount && (buyIndex+i) < len(conceptualGrid); i++ {
			idealPrices[conceptualGrid[buyIndex+i]] = "BUY"
		}
	}

	// 3. 获取实际挂单 (Actual Orders)
	actualOrders := make(map[float64]models.GridLevel)
	for _, order := range activeOrders {
		actualOrders[order.Price] = order
	}

	// 4. 同步订单 (Diff & Sync)
	// 4.1. 识别需要取消和需要保留的订单
	ordersToCancel := make([]models.GridLevel, 0)
	ordersToKeep := make([]models.GridLevel, 0)
	for price, order := range actualOrders {
		if _, exists := idealPrices[price]; !exists {
			ordersToCancel = append(ordersToCancel, order)
		} else {
			ordersToKeep = append(ordersToKeep, order)
		}
	}
	// 在锁内原子地更新活动订单列表，确保后续逻辑的正确性
	b.gridLevels = ordersToKeep

	// 4.2. 新增缺失的订单
	ordersToPlace := make([]struct {
		Side  string
		Price float64
	}, 0)
	// 获取最小价格精度 (tickSize)
	var tickSize string
	for _, f := range b.symbolInfo.Filters {
		if f.FilterType == "PRICE_FILTER" {
			tickSize = f.TickSize
		}
	}
	tickSizeFloat, _ := strconv.ParseFloat(tickSize, 64)

	for price, side := range idealPrices {
		// 核心修复：如果理想价格与当前中心价过于接近（小于一个tick），则放弃该挂单，防止循环
		if math.Abs(price-currentPrice) < tickSizeFloat {
			logger.S().Warnf("[DIAGNOSTIC] 理想价格 %.8f 与当前中心价 %.8f 过于接近 (TickSize: %.8f)，跳过此挂单以防止循环。", price, currentPrice, tickSizeFloat)
			continue
		}

		if _, exists := actualOrders[price]; !exists {
			ordersToPlace = append(ordersToPlace, struct {
				Side  string
				Price float64
			}{side, price})
		}
	}

	// 更新暂停状态
	if len(idealPrices) == 0 && len(b.gridLevels) == 0 {
		if !b.isHalted { // 仅在状态改变时打印日志
			logger.S().Info("[STRATEGY] 机器人进入暂停状态：无理想订单可挂且无活动订单。")
			b.isHalted = true
		}
	} else {
		if b.isHalted { // 从暂停状态恢复时打印日志
			logger.S().Info("[STRATEGY] 机器人恢复活动状态。")
		}
		b.isHalted = false
	}

	// 在锁外执行网络操作
	go func() {
		for _, order := range ordersToCancel {
			logger.S().Infof("订单价位 %.4f 已偏离，取消订单 ID %d", order.Price, order.OrderID)
			// 在取消订单时，我们不关心错误，因为订单可能已经成交
			b.exchange.CancelOrder(b.config.Symbol, order.OrderID)
		}
	}()
	go func() {
		for _, order := range ordersToPlace {
			b.placeNewOrder(order.Side, order.Price)
		}
	}()
}

// checkAndHandleFills 检查并处理已成交的订单（串行版）
func (b *GridTradingBot) checkAndHandleFills() {
	b.mutex.RLock()
	gridsToCheck := make([]models.GridLevel, len(b.gridLevels))
	copy(gridsToCheck, b.gridLevels)
	b.mutex.RUnlock()

	if len(gridsToCheck) == 0 {
		return
	}

	// 改为串行处理，避免在持有读锁的主循环中创建需要写锁的goroutine，从而解决死锁
	for _, grid := range gridsToCheck {
		status, err := b.exchange.GetOrderStatus(b.config.Symbol, grid.OrderID)
		if err != nil {
			// 在回测中，立即成交的订单可能查询不到，这被认为是已成交
			if b.IsBacktest && strings.Contains(err.Error(), "未找到") {
				status = &models.Order{Status: "FILLED"}
			} else {
				logger.S().Warnf("获取订单 %d 状态失败: %v", grid.OrderID, err)
				continue // 继续检查下一个订单
			}
		}

		if status.Status == "FILLED" {
			b.handleFilledOrder(grid) // 直接调用，串行执行
		}
	}
}

// handleFilledOrder 处理单个成交订单，并包含再入场和网格移动逻辑
func (b *GridTradingBot) handleFilledOrder(grid models.GridLevel) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	// 确认该订单是否还在活动列表中，防止重复处理
	isOrderActive := false
	for _, o := range b.gridLevels {
		if o.OrderID == grid.OrderID {
			isOrderActive = true
			break
		}
	}
	if !isOrderActive {
		return // 订单已被处理，直接返回
	}

	logger.S().Infof("订单 %d (%s @ %.4f) 已成交。", grid.OrderID, grid.Side, grid.Price)
	b.removeGridLevel(grid.OrderID) // 从活动列表中移除

	// 订单成交后，以成交价为新中心，重新部署网格
	logger.S().Infof("以成交价 %.4f 为新中心，重新部署网格...", grid.Price)
	b.currentPrice = grid.Price  // 以成交价为新的参考价格
	b.maintainGridOrdersLocked() // 重新部署网格

	// 如果是卖单成交，检查是否需要再入场
	// 暂时禁用此功能，因为它在当前策略组合下过于敏感，会导致意外的重启循环。
	// 我们需要重新定义一个更明确的周期结束条件。
	// if grid.Side == "SELL" {
	// 	b.checkForReentryLocked()
	// }
}

// checkForReentryLocked 在已持有锁的情况下，检查是否满足再入场条件。
// 这是为了防止在已持有锁的函数（如 handleFilledOrder）中再次请求同一个锁导致的死锁。
func (b *GridTradingBot) checkForReentryLocked() {
	// 注意：此函数期望调用者已经持有锁

	positions, err := b.exchange.GetPositions(b.config.Symbol)
	if err != nil {
		logger.S().Errorf("获取持仓以检查再入场条件失败: %v", err)
		return
	}

	positionAmt := 0.0
	if len(positions) > 0 {
		amt, err := strconv.ParseFloat(positions[0].PositionAmt, 64)
		if err == nil {
			positionAmt = amt
		}
	}

	// 如果持仓价值小于半个网格，则满足再入场条件
	currentPrice, _ := b.exchange.GetPrice(b.config.Symbol)
	if positionAmt*currentPrice < (b.config.GridValue / 2) {
		// 检查状态锁，如果已在再入场流程中，则直接返回
		if b.isReentering {
			return
		}
		// 设置状态锁，防止其他goroutine重复触发
		b.isReentering = true

		// 发送一个再入场信号，而不是直接启动goroutine
		select {
		case b.reentrySignal <- true:
			logger.S().Info("!!! 满足再入场条件，已发送重启信号 !!!")
		default:
			// 如果信号已在队列中，则什么都不做
		}
	}
}

// checkForReentry 是一个带锁的公共方法，用于在未持锁的情况下安全地检查再入场条件
func (b *GridTradingBot) checkForReentry() {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.checkForReentryLocked()
}

// removeGridLevel 安全地从 gridLevels 中移除一个订单
func (b *GridTradingBot) removeGridLevel(orderID int64) {
	// 该函数期望调用者持有锁
	newGridLevels := make([]models.GridLevel, 0)
	for _, g := range b.gridLevels {
		if g.OrderID != orderID {
			newGridLevels = append(newGridLevels, g)
		}
	}
	b.gridLevels = newGridLevels
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
			logger.S().Info("WebSocket循环已停止。")
			return
		default:
			if err := b.connectWebSocket(); err != nil {
				logger.S().Errorf("WebSocket连接失败: %v。5秒后重试...", err)
				time.Sleep(5 * time.Second)
				continue
			}

			logger.S().Info("WebSocket连接成功。")
			// handleWebSocketMessages现在会阻塞直到连接断开
			if err := b.handleWebSocketMessages(); err != nil {
				logger.S().Warnf("WebSocket处理时发生错误: %v", err)
			}
			// 连接断开后，关闭连接，循环会再次尝试重连
			if b.wsConn != nil {
				b.wsConn.Close()
			}
			logger.S().Info("WebSocket连接已断开，准备重连...")
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
					logger.S().Warnf("发送Ping失败: %v", err)
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
				logger.S().Warnf("解析价格信息失败: %v", err)
				continue // 继续接收下一条消息
			}

			price, err := ticker.Price.Float64()
			if err != nil {
				logger.S().Warnf("转换价格失败: %v", err)
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
	b.mutex.Unlock()

	// 1. 检查时间同步
	serverTime, err := b.exchange.GetServerTime()
	if err != nil {
		return fmt.Errorf("获取币安服务器时间失败: %v。请检查网络连接和API配置。", err)
	}
	localTime := time.Now().UnixMilli()
	timeDiff := serverTime - localTime
	if timeDiff > 1000 || timeDiff < -1000 {
		logger.S().Fatalf("!!! CRITICAL: 系统时间与币安服务器时间不同步! 偏差: %d ms。请立即同步您的系统时钟(NTP)!", timeDiff)
	}
	logger.S().Infof("时间同步检查通过。与服务器时间偏差: %d ms。", timeDiff)

	// 2. 正式开始，锁定状态
	b.mutex.Lock()
	b.isRunning = true
	b.stopChannel = make(chan bool)
	b.mutex.Unlock()

	// 3. 设置杠杆
	if err := b.exchange.SetLeverage(b.config.Symbol, b.config.Leverage); err != nil {
		// 在这里只打印警告，因为杠杆可能已经设置正确
		logger.S().Warnf("设置杠杆失败: %v", err)
	}

	// 4. 清理旧订单并开始新周期
	logger.S().Info("正在取消所有历史挂单，以确保全新状态...")
	if err := b.exchange.CancelAllOpenOrders(b.config.Symbol); err != nil {
		logger.S().Warnf("清理旧订单时出错 (可能无需关注): %v", err)
	}

	if err := b.enterMarketAndSetupGrid(); err != nil {
		return fmt.Errorf("启动新周期失败: %v", err)
	}

	// 5. 启动后台服务
	go b.webSocketLoop()
	go b.strategyLoop()
	go b.monitorStatus()

	logger.S().Info("网格交易机器人已启动。")
	return nil
}

// strategyLoop 是机器人的主循环，定期检查订单成交状态
func (b *GridTradingBot) strategyLoop() {
	checkTicker := time.NewTicker(5 * time.Second)
	defer checkTicker.Stop()

	for {
		select {
		case <-b.stopChannel:
			return
		case <-b.reentrySignal:
			logger.S().Info("接收到再入场信号，开始重启周期...")
			b.Stop()
			// 在真实交易中，不需要暂停，因为API调用有延迟
			if err := b.enterMarketAndSetupGrid(); err != nil {
				logger.S().Fatalf("重启周期失败: %v", err)
			}
		case <-checkTicker.C:
			b.checkAndHandleFills()
		}
	}
}

// StartForBacktest 为回测初始化机器人
func (b *GridTradingBot) StartForBacktest() error {
	b.mutex.Lock()
	b.isRunning = true
	if b.currentPrice == 0 {
		// 在回测模式下，如果外部没有设置初始价格，我们尝试从交易所获取一个
		price, err := b.exchange.GetPrice(b.config.Symbol)
		if err != nil {
			b.mutex.Unlock()
			return fmt.Errorf("回测开始前无法获取初始价格: %w", err)
		}
		b.currentPrice = price
	}
	b.gridLevels = make([]models.GridLevel, 0)
	b.mutex.Unlock()

	logger.S().Infof("回测模式启动。初始价格: %.4f", b.currentPrice)

	// 在回测中，直接调用市场准入函数来设置初始网格
	return b.enterMarketAndSetupGrid()
}

// ProcessBacktestTick 在回测期间的每个价格点被调用
func (b *GridTradingBot) ProcessBacktestTick() {
	// 检查再入场信号
	select {
	case <-b.reentrySignal:
		logger.S().Info("--- 回测: 检测到再入场信号，执行重启 ---")
		b.Stop()
		// 注意：在回测中，我们不sleep，因为时间是由数据驱动的
		b.enterMarketAndSetupGrid()
		// 重启后，这个tick的处理就结束了
		return
	default:
		// 没有信号，继续正常处理
	}

	b.mutex.Lock()
	// 在回测中，时间是由回测引擎通过 exchange 模块驱动的
	b.currentTime = b.exchange.GetCurrentTime()
	currentPrice, _ := b.exchange.GetPrice(b.config.Symbol)
	b.currentPrice = currentPrice
	b.mutex.Unlock()

	//logger.S().Infof("--- 回测 Tick: %s, 价格: %.4f ---", b.currentTime.Format("2006-01-02 15:04:05"), b.currentPrice)

	// 模拟主循环的逻辑: 只检查成交。网格维护由成交事件触发
	b.checkAndHandleFills()
}

// maintainGridOrders 是 maintainGridOrdersLocked 的公共包装器
func (b *GridTradingBot) maintainGridOrders() {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.maintainGridOrdersLocked()
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
		logger.S().Errorf("保存状态失败: %v", err)
	}

	logger.S().Info("正在取消所有活动订单...")
	b.cancelAllActiveOrders()
	logger.S().Info("网格交易机器人已停止。")
}

// cancelAllActiveOrders 取消所有活动订单
func (b *GridTradingBot) cancelAllActiveOrders() {
	b.mutex.RLock()
	gridsToCancel := make([]models.GridLevel, len(b.gridLevels))
	copy(gridsToCancel, b.gridLevels)
	b.mutex.RUnlock()

	for _, grid := range gridsToCancel {
		if err := b.exchange.CancelOrder(b.config.Symbol, grid.OrderID); err != nil {
			logger.S().Warnf("取消订单 %d 失败: %v", grid.OrderID, err)
		} else {
			logger.S().Infof("成功取消订单 %d", grid.OrderID)
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
			logger.S().Info("状态文件不存在，将创建新状态。")
			return nil
		}
		return fmt.Errorf("无法读取状态文件: %v", err)
	}

	if len(data) == 0 {
		logger.S().Info("状态文件为空，将创建新状态。")
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

	logger.S().Infof("成功从 %s 加载了 %d 个活动订单状态。", path, len(b.gridLevels))
	return nil
}

// printStatus 打印机器人当前状态
func (b *GridTradingBot) printStatus() {
	b.mutex.RLock()
	defer b.mutex.RUnlock()

	logger.S().Info("========== 机器人状态 ==========")
	logger.S().Infof("时间: %s", time.Now().Format("2006-01-02 15:04:05"))
	logger.S().Infof("状态: %s", map[bool]string{true: "运行中", false: "已停止"}[b.isRunning])
	logger.S().Infof("当前价格: %.8f", b.currentPrice)

	logger.S().Infof("当前挂单数量: %d", len(b.gridLevels))
	for _, grid := range b.gridLevels {
		logger.S().Infof("  - [ID: %d] %s %.5f @ %.4f", grid.OrderID, grid.Side, grid.Quantity, grid.Price)
	}

	positions, err := b.exchange.GetPositions(b.config.Symbol)
	if err != nil {
		logger.S().Errorf("获取持仓失败: %v", err)
	} else {
		hasPosition := false
		for _, pos := range positions {
			posAmt, _ := strconv.ParseFloat(pos.PositionAmt, 64)
			if math.Abs(posAmt) > 0 {
				entryPrice, _ := strconv.ParseFloat(pos.EntryPrice, 64)
				unrealizedProfit, _ := strconv.ParseFloat(pos.UnRealizedProfit, 64)
				logger.S().Infof("持仓: %.5f %s, 开仓均价: %.8f, 未实现盈亏: %.8f",
					posAmt, b.config.Symbol, entryPrice, unrealizedProfit)
				hasPosition = true
			}
		}
		if !hasPosition {
			logger.S().Info("当前无持仓。")
		}
	}
	logger.S().Info("================================")
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
		logger.S().Errorf("[ERROR] 无法获取账户状态以检查风险暴露: %v", err)
		return false // 在不确定的情况下，为安全起见，阻止下单
	}

	if accountEquity == 0 {
		logger.S().Warnf("[WARN] 账户总权益为0，无法计算风险暴露。")
		return false // 无法计算时阻止下单
	}

	// 计算预期的未来持仓价值
	// futurePositionValue = 当前持仓价值 + 新增持仓的价值
	futurePositionValue := positionValue + (quantityToAdd * b.currentPrice)

	// 计算预期的钱包风险暴露
	futureWalletExposure := futurePositionValue / accountEquity

	logger.S().Debugf("[RISK CHECK] 当前持仓价值: %.2f, 账户权益: %.2f, 新增后预估持仓价值: %.2f, 预估风险暴露: %.4f, 限制: %.4f",
		positionValue, accountEquity, futurePositionValue, futureWalletExposure, b.config.WalletExposureLimit)

	return futureWalletExposure <= b.config.WalletExposureLimit
}

// IsHalted 返回机器人是否处于暂停状态
func (b *GridTradingBot) IsHalted() bool {
	b.mutex.RLock()
	defer b.mutex.RUnlock()
	return b.isHalted
}
