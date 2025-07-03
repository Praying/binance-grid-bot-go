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
	listenKey               string             // 新增: 用于用户数据流的 listen key
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

// establishBasePositionAndWait 尝试建立初始底仓并阻塞等待其成交，成功后返回成交价
func (b *GridTradingBot) establishBasePositionAndWait(quantity float64) (float64, error) {
	clientOrderID := b.generateClientOrderID("base")
	order, err := b.exchange.PlaceOrder(b.config.Symbol, "BUY", "MARKET", quantity, 0, clientOrderID)
	if err != nil {
		return 0, fmt.Errorf("初始市价买入失败: %v", err)
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
					// 在回测中，我们无法轻易得知市价单的确切成交价，这里使用一个近似值
					// 更好的做法是在 backtest_exchange 中记录下来
					return b.currentPrice, nil
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

				// 获取真实成交价
				trade, err := b.exchange.GetLastTrade(b.config.Symbol, order.OrderId)
				if err != nil {
					return 0, fmt.Errorf("无法获取初始订单 %d 的成交价: %v", order.OrderId, err)
				}
				filledPrice, err := strconv.ParseFloat(trade.Price, 64)
				if err != nil {
					return 0, fmt.Errorf("解析初始订单 %d 的成交价失败: %v", order.OrderId, err)
				}
				return filledPrice, nil // 成功

			case "CANCELED", "REJECTED", "EXPIRED":
				return 0, fmt.Errorf("初始仓位订单 %d 建立失败，状态为: %s", order.OrderId, status.Status)
			default:
				// "NEW" or "PARTIALLY_FILLED", 继续等待
				logger.S().Debugf("初始订单 %d 状态: %s. 等待成交...", order.OrderId, status.Status)
			}
		case <-timeout:
			return 0, fmt.Errorf("等待初始订单 %d 成交超时", order.OrderId)
		case <-b.stopChannel:
			return 0, fmt.Errorf("机器人已停止，中断建立初始仓位流程")
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
	// 获取价格精度 (tickSize)
	var tickSize string
	for _, f := range b.symbolInfo.Filters {
		if f.FilterType == "PRICE_FILTER" {
			tickSize = f.TickSize
		}
	}

	price := b.reversionPrice
	for price > (b.entryPrice * 0.5) {
		adjustedPrice := adjustValueToStep(price, tickSize)
		// 避免重复添加相同的价格
		if len(b.conceptualGrid) == 0 || b.conceptualGrid[len(b.conceptualGrid)-1] != adjustedPrice {
			b.conceptualGrid = append(b.conceptualGrid, adjustedPrice)
		}
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
	// 打印详细的网格价位以供调试
	if len(b.conceptualGrid) > 0 {
		logger.S().Debug("--- 理论天地网格详细价位 ---")
		// 为了避免刷屏，可以考虑只打印部分，但为了调试清晰，暂时全部打印
		for i, p := range b.conceptualGrid {
			logger.S().Debugf("  - 网格 %d: %.4f", i+1, p)
		}
		logger.S().Debug("-----------------------------")
	}

	// 步骤 3: 计算并建立底仓 (部分持仓模式)
	sellGridCount := 0
	for _, price := range b.conceptualGrid {
		if price > b.entryPrice {
			sellGridCount++
		}
	}
	buyGridCount := len(b.conceptualGrid) - sellGridCount
	// 使用新的 calculateQuantity 来确定单网格数量
	singleGridQuantity, err := b.calculateQuantity(b.entryPrice)
	if err != nil {
		return fmt.Errorf("在计算初始底仓时无法确定网格数量: %v", err)
	}

	initialPositionQuantity := float64(sellGridCount) * singleGridQuantity
	reservedCashEquivalent := float64(buyGridCount) * singleGridQuantity * b.entryPrice // 估算所需现金

	logger.S().Infof("理论卖出网格数: %d, 理论买入网格数: %d, 单网格数量: %.8f %s",
		sellGridCount, buyGridCount, singleGridQuantity, strings.Replace(b.config.Symbol, "USDT", "", -1))
	logger.S().Infof("计算得出初始底仓数量: %.8f, 需保留现金约: %.2f USDT.", initialPositionQuantity, reservedCashEquivalent)
	logger.S().Infof("准备市价买入底仓...")

	// 直接使用计算出的精确数量
	adjustedQuantity := initialPositionQuantity

	if !b.isWithinExposureLimit(adjustedQuantity) {
		logger.S().Warnf("初始建仓被阻止：钱包风险暴露将超过限制。")
		b.mutex.Lock()
		b.basePositionEstablished = true // 同样标记为true
		b.mutex.Unlock()
	} else {
		filledPrice, err := b.establishBasePositionAndWait(adjustedQuantity)
		if err != nil {
			// 如果建立底仓失败，这是一个致命错误，应终止机器人
			return fmt.Errorf("建立初始仓位失败，无法继续: %v", err)
		}
		b.entryPrice = filledPrice // 使用真实的成交价作为入场价
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

		// 找到最接近入场价的网格ID作为初始枢轴
		initialPivotGridID := -1
		minDiff := math.MaxFloat64
		for i, p := range b.conceptualGrid {
			diff := math.Abs(p - b.entryPrice)
			if diff < minDiff {
				minDiff = diff
				initialPivotGridID = i
			}
		}

		if initialPivotGridID != -1 {
			logger.S().Infof("根据入场价 %.4f, 找到最接近的初始网格ID: %d (价格: %.4f)", b.entryPrice, initialPivotGridID, b.conceptualGrid[initialPivotGridID])
			// 初始挂单，我们假定这是一个买入事件（因为我们刚建仓），来设置上卖下买的格局
			b.resetGridAroundPrice(initialPivotGridID, "BUY")
			logger.S().Info("--- 新周期初始化挂单完成 ---")
		} else {
			logger.S().Error("严重错误: 无法在天地网格中为初始入场价找到枢轴点。")
		}
	} else {
		// 这种情况理论上不应发生，因为establishBasePositionAndWait会返回错误
		logger.S().Error("严重错误：底仓状态未被正确标记，无法进行挂单。")
	}

	return nil
}

// placeNewOrder 是一个辅助函数，用于下单并将结果返回
func (b *GridTradingBot) placeNewOrder(side string, price float64, gridID int) (*models.GridLevel, error) {
	// 注意：此函数不再管理互斥锁，调用方需要负责线程安全

	// 获取价格和数量的精度规则
	var tickSize string
	for _, f := range b.symbolInfo.Filters {
		if f.FilterType == "PRICE_FILTER" {
			tickSize = f.TickSize
		}
	}

	// 调整价格和数量以符合精度要求
	adjustedPrice := adjustValueToStep(price, tickSize)
	quantity, err := b.calculateQuantity(adjustedPrice)
	if err != nil {
		return nil, fmt.Errorf("在价格 %.4f 计算订单数量失败: %v", adjustedPrice, err)
	}
	adjustedQuantity := quantity // calculateQuantity now returns the final adjusted quantity

	// 在下买单前检查钱包风险暴露
	if side == "BUY" {
		if !b.isWithinExposureLimit(quantity) {
			return nil, fmt.Errorf("下单被阻止：钱包风险暴露将超过限制")
		}
	}

	// --- 下单并轮询确认 ---
	// 1. 下单
	clientOrderID := b.generateClientOrderID(fmt.Sprintf("grid%d", gridID))
	order, err := b.exchange.PlaceOrder(b.config.Symbol, side, "LIMIT", adjustedQuantity, adjustedPrice, clientOrderID)
	if err != nil {
		return nil, fmt.Errorf("下 %s 单失败，价格 %.4f: %v", side, adjustedPrice, err)
	}
	logger.S().Infof("已提交 %s 订单: ID %d, 价格 %.4f, 数量 %.5f, GridID: %d. 等待交易所确认...", side, order.OrderId, adjustedPrice, adjustedQuantity, gridID)

	// 2. 轮询确认
	// 在回测模式下，订单是同步成交的，无需轮询
	if !b.IsBacktest {
		ticker := time.NewTicker(200 * time.Millisecond) // 轮询频率
		defer ticker.Stop()
		timeout := time.After(10 * time.Second) // 设置10秒超时

		for {
			select {
			case <-ticker.C:
				status, err := b.exchange.GetOrderStatus(b.config.Symbol, order.OrderId)
				if err != nil {
					// 如果错误是“未找到”，可能是因为网络延迟，继续轮询
					if strings.Contains(err.Error(), "order does not exist") {
						logger.S().Warnf("订单 %d 暂时未找到，继续轮询...", order.OrderId)
						continue
					}
					return nil, fmt.Errorf("获取订单 %d 状态失败: %v", order.OrderId, err)
				}

				switch status.Status {
				case "NEW", "PARTIALLY_FILLED":
					logger.S().Infof("订单 %d 已被交易所确认，状态: %s", order.OrderId, status.Status)
					goto confirmed
				case "FILLED":
					logger.S().Infof("订单 %d 在确认期间已被完全成交", order.OrderId)
					goto confirmed
				case "CANCELED", "REJECTED", "EXPIRED":
					return nil, fmt.Errorf("订单 %d 确认失败，最终状态为: %s", order.OrderId, status.Status)
				}
			case <-timeout:
				return nil, fmt.Errorf("等待订单 %d 确认超时", order.OrderId)
			case <-b.stopChannel:
				return nil, fmt.Errorf("机器人已停止，中断订单 %d 的确认流程", order.OrderId)
			}
		}
	}

confirmed:
	newGridLevel := &models.GridLevel{
		Price:    adjustedPrice,
		Quantity: adjustedQuantity,
		Side:     side,
		IsActive: true,
		OrderID:  order.OrderId,
		GridID:   gridID,
	}
	logger.S().Infof("成功确认 %s 单: ID %d, 价格 %.4f, 数量 %.5f, GridID: %d", side, order.OrderId, adjustedPrice, adjustedQuantity, gridID)
	return newGridLevel, nil
}

// calculateQuantity 根据配置和交易所规则计算并验证订单数量
func (b *GridTradingBot) calculateQuantity(price float64) (float64, error) {
	var quantity float64
	var minNotional, minQty, stepSize string

	// 1. 从缓存的交易规则中提取过滤器
	for _, f := range b.symbolInfo.Filters {
		switch f.FilterType {
		case "MIN_NOTIONAL":
			minNotional = f.MinNotional
		case "LOT_SIZE":
			minQty = f.MinQty
			stepSize = f.StepSize
		}
	}

	minNotionalValue, _ := strconv.ParseFloat(minNotional, 64)
	minQtyValue, _ := strconv.ParseFloat(minQty, 64)

	// 2. 根据配置确定基础数量
	if b.config.GridQuantity > 0 {
		quantity = b.config.GridQuantity
	} else if b.config.GridValue > 0 {
		quantity = b.config.GridValue / price
	} else {
		return 0, fmt.Errorf("未配置 grid_quantity 或 grid_value")
	}

	// 3. 验证并调整数量以满足最小名义价值
	if price*quantity < minNotionalValue {
		logger.S().Debugf(
			"按当前配置计算的数量 %.8f (价值 %.4f) 低于交易所最小名义价值 %.4f。将自动调整数量以满足最小价值。",
			quantity, price*quantity, minNotionalValue,
		)
		quantity = (minNotionalValue / price) * 1.01 // 增加1%的缓冲，防止因价格波动导致下单失败
	}

	// 4. 验证并调整数量以满足最小订单量
	if quantity < minQtyValue {
		logger.S().Debugf(
			"计算出的数量 %.8f 低于最小订单量 %.8f。将自动调整到最小订单量。",
			quantity, minQtyValue,
		)
		quantity = minQtyValue
	}

	// 5. 调整数量以符合步进精度
	adjustedQuantity := adjustValueToStep(quantity, stepSize)

	// 6. 最后再次检查调整后的数量是否小于最小量 (因为向下取整可能导致此问题)
	if adjustedQuantity < minQtyValue {
		// 如果向下取整后小于最小量，则需要增加一个步进
		step, _ := strconv.ParseFloat(stepSize, 64)
		adjustedQuantity += step
		// 再次调整以防万一
		adjustedQuantity = adjustValueToStep(adjustedQuantity, stepSize)
	}

	return adjustedQuantity, nil
}

// resetGridAroundPrice 是新的核心函数，用于在订单成交后重置网格
func (b *GridTradingBot) resetGridAroundPrice(pivotGridID int, pivotSide string) {
	// 注意：这里的锁已被移除，以解决死锁问题。锁的管理已下放至各个下单goroutine内部。
	logger.S().Infof("--- 基于GridID %d (%s) 重置网格 ---", pivotGridID, pivotSide)

	// 1. 取消所有现有订单并清空本地列表
	go func() {
		if err := b.exchange.CancelAllOpenOrders(b.config.Symbol); err != nil {
			logger.S().Warnf("重置网格时取消所有订单失败: %v", err)
		}
	}()
	b.mutex.Lock()
	b.gridLevels = make([]models.GridLevel, 0)
	b.mutex.Unlock()

	// 2. 直接根据 pivotGridID 和 pivotSide 计算新的挂单索引
	sellStartIndex := -1
	buyStartIndex := -1

	if pivotSide == "BUY" {
		// 如果是买单成交，说明价格下跌触及买单。新的卖单应该挂在成交的这一格，新的买单挂在下一格。
		sellStartIndex = pivotGridID - 1
		buyStartIndex = pivotGridID + 1
	} else { // SELL
		// 如果是卖单成交，说明价格上涨触及卖单。新的卖单应该挂在上一格，新的买单挂在成交的这一格。
		sellStartIndex = pivotGridID - 1
		buyStartIndex = pivotGridID + 1
	}
	logger.S().Debugf(
		"[DEBUG] 定位枢轴 GridID %d (%s). sellStartIndex: %d, buyStartIndex: %d",
		pivotGridID, pivotSide, sellStartIndex, buyStartIndex,
	)

	// 3. 定义需要挂单的列表
	ordersToPlace := make([]struct {
		Side   string
		Price  float64
		GridID int
	}, 0)

	// 4. 准备要挂的卖单
	if sellStartIndex >= 0 {
		for i := 0; i < b.config.ActiveOrdersCount; i++ {
			index := sellStartIndex - i
			if index >= 0 {
				ordersToPlace = append(ordersToPlace, struct {
					Side   string
					Price  float64
					GridID int
				}{"SELL", b.conceptualGrid[index], index})
			} else {
				break
			}
		}
	}

	// 5. 准备要挂的买单
	if buyStartIndex < len(b.conceptualGrid) {
		for i := 0; i < b.config.ActiveOrdersCount; i++ {
			index := buyStartIndex + i
			if index < len(b.conceptualGrid) {
				ordersToPlace = append(ordersToPlace, struct {
					Side   string
					Price  float64
					GridID int
				}{"BUY", b.conceptualGrid[index], index})
			} else {
				break
			}
		}
	}

	// 6. 异步下单
	if len(ordersToPlace) > 0 {
		logger.S().Infof("准备挂 %d 个新订单...", len(ordersToPlace))

		// 使用 WaitGroup 等待所有下单 goroutine 完成
		var wg sync.WaitGroup
		for _, order := range ordersToPlace {
			wg.Add(1)
			go func(o struct {
				Side   string
				Price  float64
				GridID int
			}) {
				defer wg.Done()
				newLevel, err := b.placeNewOrder(o.Side, o.Price, o.GridID)
				if err != nil {
					logger.S().Errorf("挂单失败 (GridID: %d, Price: %.4f): %v", o.GridID, o.Price, err)
				} else if newLevel != nil {
					b.mutex.Lock()
					b.gridLevels = append(b.gridLevels, *newLevel)
					b.mutex.Unlock()
				}
			}(order)
		}
		wg.Wait()
		logger.S().Info("所有新网格订单已提交。")
	}
}

// --- WebSocket 和主循环 ---

// connectWebSocket 建立到币安用户数据流的 WebSocket 连接
func (b *GridTradingBot) connectWebSocket() error {
	var err error
	b.listenKey, err = b.exchange.CreateListenKey()
	if err != nil {
		return fmt.Errorf("无法创建 listenKey: %v", err)
	}
	logger.S().Infof("成功获取 ListenKey: %s", b.listenKey)

	b.wsConn, err = b.exchange.ConnectWebSocket(b.listenKey)
	if err != nil {
		return fmt.Errorf("无法连接到 WebSocket: %v", err)
	}
	logger.S().Info("成功连接到币安用户数据流 WebSocket。")
	return nil
}

// webSocketLoop 维持 WebSocket 连接并处理消息
func (b *GridTradingBot) webSocketLoop() {
	// listenKey 保活定时器 (币安应用层要求)
	keepAliveTicker := time.NewTicker(30 * time.Minute)
	defer keepAliveTicker.Stop()

	// WebSocket Ping 帧心跳定时器 (TCP物理连接层保活)
	pingTicker := time.NewTicker(15 * time.Second)
	defer pingTicker.Stop()

	// messageOrErrChan 用于从读取goroutine接收消息或错误
	type readResult struct {
		message []byte
		err     error
	}
	var messageOrErrChan chan readResult
	var stopReadLoop chan struct{}

	// startReadLoop 是一个辅助函数，用于启动消息读取goroutine
	startReadLoop := func() {
		messageOrErrChan = make(chan readResult, 1)
		stopReadLoop = make(chan struct{})
		go func() {
			for {
				select {
				case <-stopReadLoop:
					return
				default:
					if b.wsConn == nil {
						messageOrErrChan <- readResult{err: fmt.Errorf("wsConn is nil, triggering reconnect")}
						return // 退出 goroutine, 让主循环处理
					}
					_, msg, err := b.wsConn.ReadMessage()
					messageOrErrChan <- readResult{message: msg, err: err}
					if err != nil {
						return // 发生读取错误后退出 goroutine
					}
				}
			}
		}()
	}

	// 初始启动读取循环
	startReadLoop()

	for {
		select {
		case <-b.stopChannel:
			logger.S().Info("WebSocket 循环已停止。")
			if stopReadLoop != nil {
				close(stopReadLoop)
			}
			return

		case <-keepAliveTicker.C:
			if b.listenKey != "" {
				logger.S().Info("正在尝试延长 listenKey 的有效期...")
				if err := b.exchange.KeepAliveListenKey(b.listenKey); err != nil {
					logger.S().Errorf("延长 listenKey 失败: %v", err)
				} else {
					logger.S().Info("成功延长 listenKey 有效期。")
				}
			}

		case <-pingTicker.C:
			if b.wsConn != nil {
				// 为 Ping 消息设置写入超时，防止永久阻塞
				if err := b.wsConn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
					logger.S().Warnf("为 Ping 消息设置写入超时失败: %v", err)
				}
				if err := b.wsConn.WriteMessage(websocket.PingMessage, nil); err != nil {
					// 如果发送ping失败，连接很可能已经断开。
					// 此处只记录日志，读取循环的错误会触发真正的重连逻辑。
					logger.S().Warnf("发送 WebSocket Ping 失败，连接可能已断开: %v", err)
				}
				// 重置写入超时
				if err := b.wsConn.SetWriteDeadline(time.Time{}); err != nil {
					logger.S().Warnf("重置 Ping 消息写入超时失败: %v", err)
				}
			}

		case result := <-messageOrErrChan:
			if result.err != nil {
				logger.S().Errorf("WebSocket 读取错误，正在尝试重连: %v", result.err)
				if b.wsConn != nil {
					b.wsConn.Close()
				}
				if stopReadLoop != nil {
					close(stopReadLoop)
				}

				// 执行重连
				time.Sleep(5 * time.Second) // 等待5秒再重连
				if err := b.connectWebSocket(); err != nil {
					logger.S().Errorf("WebSocket 重连失败: %v。将再次尝试...", err)
				}
				// 为新的连接或下一次尝试重启读取循环
				startReadLoop()
				continue
			}

			if result.message != nil {
				b.handleWebSocketMessage(result.message)
			}
		}
	}
}

// handleWebSocketMessage 解析并处理来自 WebSocket 的消息
func (b *GridTradingBot) handleWebSocketMessage(message []byte) {
	var event models.UserDataEvent
	if err := json.Unmarshal(message, &event); err != nil {
		logger.S().Warnf("无法解析 WebSocket 消息: %s", string(message))
		return
	}

	if event.EventType == "executionReport" {
		report := event.ExecutionReport
		logger.S().Infof("[WebSocket] 收到订单更新: Symbol=%s, Side=%s, Status=%s, OrderID=%d",
			report.Symbol, report.Side, report.OrderStatus, report.OrderID)

		if report.OrderStatus == "FILLED" {
			// 查找被触发的网格
			b.mutex.Lock()
			var triggeredGrid *models.GridLevel
			var triggeredIndex int
			for i, level := range b.gridLevels {
				if level.OrderID == report.OrderID {
					triggeredGrid = &level
					triggeredIndex = i
					break
				}
			}

			if triggeredGrid != nil {
				logger.S().Infof("网格订单 %d (GridID: %d, Side: %s, Price: %.4f) 已成交!",
					triggeredGrid.OrderID, triggeredGrid.GridID, triggeredGrid.Side, triggeredGrid.Price)

				// 从活动列表中移除已成交的订单
				b.gridLevels = append(b.gridLevels[:triggeredIndex], b.gridLevels[triggeredIndex+1:]...)

				// 在新的 goroutine 中重置网格，以避免阻塞 WebSocket 循环
				go b.resetGridAroundPrice(triggeredGrid.GridID, triggeredGrid.Side)
			}
			b.mutex.Unlock()
		}
	}
}

// Start 启动机器人
func (b *GridTradingBot) Start() error {
	b.mutex.Lock()
	if b.isRunning {
		b.mutex.Unlock()
		return fmt.Errorf("机器人已在运行中")
	}
	b.isRunning = true
	b.mutex.Unlock()

	logger.S().Info("--- 机器人启动 ---")

	// 如果不是回测模式，则连接 WebSocket
	if !b.IsBacktest {
		if err := b.connectWebSocket(); err != nil {
			return fmt.Errorf("启动失败，无法连接 WebSocket: %v", err)
		}
		go b.webSocketLoop()
	}

	// 检查是否需要建立初始仓位
	b.mutex.RLock()
	isEstablished := b.basePositionEstablished
	b.mutex.RUnlock()

	if !isEstablished {
		if err := b.enterMarketAndSetupGrid(); err != nil {
			return fmt.Errorf("初始化网格和仓位失败: %v", err)
		}
	}

	// 启动主策略循环（如果需要的话）
	// go b.strategyLoop() // 当前版本，主要逻辑由 WebSocket 事件驱动

	// 启动状态监控
	go b.monitorStatus()

	return nil
}

// strategyLoop 是机器人的主循环（在当前事件驱动模型中可能被简化）
func (b *GridTradingBot) strategyLoop() {
	logger.S().Info("策略循环已启动。")
	ticker := time.NewTicker(10 * time.Second) // 例如，每10秒检查一次状态
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			b.mutex.RLock()
			// 在这里可以添加一些周期性的检查逻辑，
			// 例如检查与交易所的连接状态，或打印状态摘要。
			// logger.S().Debug("策略循环心跳...")
			b.mutex.RUnlock()

		case <-b.stopChannel:
			logger.S().Info("策略循环已停止。")
			return
		}
	}
}

// StartForBacktest 为回测模式启动机器人
func (b *GridTradingBot) StartForBacktest() error {
	b.mutex.Lock()
	if b.isRunning {
		b.mutex.Unlock()
		return fmt.Errorf("机器人已在运行中")
	}
	b.isRunning = true
	b.mutex.Unlock()

	logger.S().Info("--- 回测机器人启动 ---")

	// 在回测中，我们不连接 WebSocket，而是直接设置网格
	if err := b.enterMarketAndSetupGrid(); err != nil {
		return fmt.Errorf("回测初始化网格失败: %v", err)
	}

	return nil
}

// ProcessBacktestTick 处理回测中的每个时间点
func (b *GridTradingBot) ProcessBacktestTick() {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	if !b.isRunning {
		return
	}

	// 在回测模式下，订单成交逻辑已在 exchange.SetPrice 中同步处理。
	// 此函数现在主要负责处理每个时间点（tick）的其他逻辑，例如检查止损。

	// 检查止损
	if b.config.StopLossRate > 0 {
		positions, err := b.exchange.GetPositions(b.config.Symbol)
		if err != nil {
			logger.S().Errorf("回测检查止损时获取持仓失败: %v", err)
			return
		}
		if len(positions) > 0 {
			entryPrice, _ := strconv.ParseFloat(positions[0].EntryPrice, 64)
			positionAmt, _ := strconv.ParseFloat(positions[0].PositionAmt, 64)
			stopLossPrice := entryPrice * (1 - b.config.StopLossRate)
			if positionAmt > 0 && b.currentPrice <= stopLossPrice {
				logger.S().Infof("触发止损：当前价格 %.4f <= 止损价格 %.4f", b.currentPrice, stopLossPrice)
				b.Stop()
			}
		}
	}
}

// SetCurrentPrice 设置当前价格（主要用于回测）
func (b *GridTradingBot) SetCurrentPrice(price float64) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.currentPrice = price
}

// Stop 停止机器人
func (b *GridTradingBot) Stop() {
	b.mutex.Lock()
	if !b.isRunning {
		b.mutex.Unlock()
		return
	}
	b.isRunning = false
	b.mutex.Unlock()

	logger.S().Info("--- 正在停止机器人 ---")
	close(b.stopChannel)

	// 关闭 WebSocket 连接
	if b.wsConn != nil {
		err := b.wsConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		if err != nil {
			logger.S().Warnf("发送 WebSocket 关闭帧失败: %v", err)
		}
		b.wsConn.Close()
		logger.S().Info("WebSocket 连接已关闭。")
	}

	// 在停止时取消所有挂单
	b.cancelAllActiveOrders()
	logger.S().Info("所有活动订单已取消。")
}

// cancelAllActiveOrders 取消所有活动的网格订单
func (b *GridTradingBot) cancelAllActiveOrders() {
	b.mutex.RLock()
	defer b.mutex.RUnlock()
	logger.S().Infof("正在取消 %d 个活动订单...", len(b.gridLevels))
	if err := b.exchange.CancelAllOpenOrders(b.config.Symbol); err != nil {
		logger.S().Errorf("取消所有订单失败: %v", err)
	}
}

// monitorStatus 定期打印机器人状态
func (b *GridTradingBot) monitorStatus() {
	ticker := time.NewTicker(30 * time.Second) // 每30秒打印一次
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			b.printStatus()
		case <-b.stopChannel:
			logger.S().Info("状态监控已停止。")
			return
		}
	}
}

// SaveState 保存机器人状态到文件
func (b *GridTradingBot) SaveState(path string) error {
	b.mutex.RLock()
	defer b.mutex.RUnlock()

	state := models.BotState{
		GridLevels:              b.gridLevels,
		BasePositionEstablished: b.basePositionEstablished,
		ConceptualGrid:          b.conceptualGrid,
		EntryPrice:              b.entryPrice,
		ReversionPrice:          b.reversionPrice,
		IsReentering:            b.isReentering,
		CurrentPrice:            b.currentPrice,
		CurrentTime:             b.exchange.GetCurrentTime(),
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化状态失败: %v", err)
	}

	return ioutil.WriteFile(path, data, 0644)
}

// LoadState 从文件加载机器人状态
func (b *GridTradingBot) LoadState(path string) error {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("状态文件不存在: %s", path)
		}
		return fmt.Errorf("读取状态文件失败: %v", err)
	}

	var state models.BotState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("反序列化状态失败: %v", err)
	}

	b.mutex.Lock()
	defer b.mutex.Unlock()

	b.gridLevels = state.GridLevels
	b.basePositionEstablished = state.BasePositionEstablished
	b.conceptualGrid = state.ConceptualGrid
	b.entryPrice = state.EntryPrice
	b.reversionPrice = state.ReversionPrice
	b.isReentering = state.IsReentering
	b.currentPrice = state.CurrentPrice
	b.currentTime = state.CurrentTime

	logger.S().Infof("成功从 %s 加载机器人状态。", path)
	return nil
}

// printStatus 打印当前机器人状态的摘要
func (b *GridTradingBot) printStatus() {
	b.mutex.RLock()
	defer b.mutex.RUnlock()

	if !b.isRunning {
		logger.S().Info("[状态] 机器人已停止。")
		return
	}

	positionValue, accountEquity, err := b.exchange.GetAccountState(b.config.Symbol)
	if err != nil {
		logger.S().Warnf("无法获取账户状态: %v", err)
		return
	}

	walletExposure := 0.0
	if accountEquity > 0 {
		walletExposure = (positionValue / accountEquity) * 100
	}

	logger.S().Infof(
		"[状态摘要] 运行中: %v | 活动订单: %d | 当前价格: %.4f | 钱包暴露: %.2f%% | 账户权益: %.4f",
		b.isRunning,
		len(b.gridLevels),
		b.currentPrice,
		walletExposure,
		accountEquity,
	)
}

// adjustValueToStep 使用更健壮的方法来调整数值以符合交易所的精度要求（步长）。
// 它通过计算步长的小数位数，然后对数值进行截断，以避免浮点数精度问题。
func adjustValueToStep(value float64, step string) float64 {
	if step == "" || step == "0" {
		return value
	}
	stepFloat, err := strconv.ParseFloat(step, 64)
	if err != nil || stepFloat <= 0 {
		return value
	}

	// 计算步长的小数位数
	// 例如 "0.01" -> 2, "0.0001" -> 4, "1" -> 0
	var decimals int
	if strings.Contains(step, ".") {
		// 去除尾部的0，例如 "0.0100" -> "0.01"
		parts := strings.Split(step, ".")
		trimmed := strings.TrimRight(parts[1], "0")
		decimals = len(trimmed)
	} else {
		decimals = 0
	}

	// 核心逻辑：向下取整到步长的倍数
	// 例如 value=668.675, step=0.01 -> 668.67
	// 这种方法比直接用浮点数除法更稳定
	truncatedValue := math.Floor(value/stepFloat) * stepFloat

	// 将截断后的值再次根据小数位数进行格式化，以消除潜在的浮点数噪音
	// 例如，结果可能是 668.670000000001，这步会将其修正为 668.67
	formattedString := fmt.Sprintf("%.*f", decimals, truncatedValue)
	finalValue, _ := strconv.ParseFloat(formattedString, 64)

	return finalValue
}

// generateClientOrderID 生成一个唯一的客户端订单ID
func (b *GridTradingBot) generateClientOrderID(prefix string) string {
	// 币安要求 clientOrderId 长度在 1 到 36 之间，并且只能包含字母、数字和'-' '_'
	// 我们使用 "prefix-timestamp" 的格式
	timestamp := time.Now().UnixNano() / int64(time.Millisecond)
	return fmt.Sprintf("%s-%d", prefix, timestamp)
}

// isWithinExposureLimit 检查增加一笔交易后是否会超过钱包风险暴露上限
func (b *GridTradingBot) isWithinExposureLimit(quantityToAdd float64) bool {
	b.mutex.RLock()
	defer b.mutex.RUnlock()

	// 获取当前持仓
	positions, err := b.exchange.GetPositions(b.config.Symbol)
	if err != nil {
		logger.S().Warnf("无法获取持仓以检查风险暴露: %v", err)
		return false // 保守起见，获取失败则不允许下单
	}

	var currentPositionSize float64
	if len(positions) > 0 {
		currentPositionSize, _ = strconv.ParseFloat(positions[0].PositionAmt, 64)
	}

	// 获取账户权益
	_, accountEquity, err := b.exchange.GetAccountState(b.config.Symbol)
	if err != nil {
		logger.S().Warnf("无法获取账户权益以检查风险暴露: %v", err)
		return false // 保守起见，获取失败则不允许下单
	}

	if accountEquity <= 0 {
		return false // 避免除以零
	}

	// 计算预期的风险暴露
	futurePositionSize := currentPositionSize + quantityToAdd
	futurePositionValue := futurePositionSize * b.currentPrice
	expectedExposure := futurePositionValue / accountEquity

	if expectedExposure > b.config.WalletExposureLimit {
		logger.S().Warnf(
			"钱包风险暴露检查失败: 预期暴露 %.2f%% (持仓价值 %.2f / 账户权益 %.2f) 将超过 %.2f%% 的限制。",
			expectedExposure*100, futurePositionValue, accountEquity, b.config.WalletExposureLimit*100,
		)
		return false
	}

	return true
}

// IsHalted 返回机器人是否处于暂停状态
func (b *GridTradingBot) IsHalted() bool {
	b.mutex.RLock()
	defer b.mutex.RUnlock()
	return b.isHalted
}
