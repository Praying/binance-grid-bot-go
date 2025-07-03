package bot

import (
	"binance-grid-bot-go/internal/exchange"
	"binance-grid-bot-go/internal/idgenerator"
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
	symbolInfo              *models.SymbolInfo       // 缓存交易规则
	isHalted                bool                     // 新增：标记机器人是否因无法交易而暂停
	idGenerator             *idgenerator.IDGenerator // 新增：全局唯一的ID生成器
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

	// 初始化ID生成器 (instanceID 0 是临时的，未来可以从配置或服务中获取)
	idGen, err := idgenerator.NewIDGenerator(0)
	if err != nil {
		// 这是一个致命错误，因为没有ID生成器机器人无法工作
		logger.S().Fatalf("无法创建ID生成器: %v", err)
	}
	bot.idGenerator = idGen

	return bot
}

// establishBasePositionAndWait 尝试建立初始底仓并阻塞等待其成交，成功后返回成交价
func (b *GridTradingBot) establishBasePositionAndWait(quantity float64) (float64, error) {
	clientOrderID, err := b.generateClientOrderID()
	if err != nil {
		return 0, fmt.Errorf("无法为初始订单生成ID: %v", err)
	}
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
	clientOrderID, err := b.generateClientOrderID()
	if err != nil {
		return nil, fmt.Errorf("无法为网格订单 (GridID: %d) 生成ID: %v", gridID, err)
	}
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
		step, _ := strconv.ParseFloat(stepSize, 64)
		if step > 0 {
			adjustedQuantity += step
			adjustedQuantity = adjustValueToStep(adjustedQuantity, stepSize) // 再次调整
		}
	}

	// 7. 【关键修复】在所有调整完成后，最后再验证一次名义价值
	if price*adjustedQuantity < minNotionalValue {
		step, _ := strconv.ParseFloat(stepSize, 64)
		if step > 0 {
			// 增加一个步进单位，以确保超过最小名义价值
			adjustedQuantity += step
			// 再次进行精度调整
			adjustedQuantity = adjustValueToStep(adjustedQuantity, stepSize)
		}
	}

	return adjustedQuantity, nil
}

// resetGridAroundPrice 是新的核心函数，用于在订单成交后重置网格
func (b *GridTradingBot) resetGridAroundPrice(pivotGridID int, pivotSide string) {
	// 注意：这里的锁已被移除，以解决死锁问题。锁的管理已下放至各个下单goroutine内部。
	logger.S().Infof("--- 基于GridID %d (%s) 重置网格 ---", pivotGridID, pivotSide)

	// 1. 取消所有现有订单并清空本地列表
	if err := b.exchange.CancelAllOpenOrders(b.config.Symbol); err != nil {
		logger.S().Warnf("重置网格时取消所有订单失败: %v", err)
	}
	time.Sleep(500 * time.Millisecond) // 等待交易所处理

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
				newGrid, err := b.placeNewOrder(o.Side, o.Price, o.GridID)
				if err != nil {
					logger.S().Errorf("重置网格时下单失败 (GridID: %d, Price: %.4f): %v", o.GridID, o.Price, err)
				} else {
					b.mutex.Lock()
					b.gridLevels = append(b.gridLevels, *newGrid)
					b.mutex.Unlock()
				}
			}(order)
		}
		wg.Wait()
		logger.S().Info("所有新网格订单已提交。")
	}
}

// connectWebSocket 连接到币安的用户数据流
func (b *GridTradingBot) connectWebSocket() error {
	// 1. 获取 Listen Key
	listenKey, err := b.exchange.CreateListenKey()
	if err != nil {
		return fmt.Errorf("获取 listen key 失败: %v", err)
	}
	b.listenKey = listenKey
	logger.S().Info("成功获取 Listen Key。")

	// 2. 连接 WebSocket
	conn, err := b.exchange.ConnectWebSocket(b.listenKey)
	if err != nil {
		return fmt.Errorf("连接 WebSocket 失败: %v", err)
	}
	b.wsConn = conn
	logger.S().Info("成功连接到用户数据流 WebSocket。")

	// 3. 启动 Listen Key 的 Keep-Alive
	go func() {
		ticker := time.NewTicker(30 * time.Minute) // 币安建议每30分钟发送一次
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				err := b.exchange.KeepAliveListenKey(b.listenKey)
				if err != nil {
					logger.S().Warnf("保持 Listen Key 活跃失败: %v", err)
				} else {
					logger.S().Info("成功发送 Listen Key Keep-Alive 请求。")
				}
			case <-b.stopChannel:
				return
			}
		}
	}()

	return nil
}

// webSocketLoop 是处理WebSocket消息的主循环
func (b *GridTradingBot) webSocketLoop() {
	if b.wsConn == nil {
		logger.S().Error("WebSocket 连接为空，无法启动消息循环。")
		return
	}
	defer b.wsConn.Close()

	// 定义一个带结果的结构体，用于从goroutine中接收消息和错误
	type readResult struct {
		message []byte
		err     error
	}
	readChannel := make(chan readResult)

	// 启动一个goroutine来读取消息，这样我们可以非阻塞地检查停止信号
	startReadLoop := func() {
		go func() {
			for {
				_, message, err := b.wsConn.ReadMessage()
				select {
				case readChannel <- readResult{message, err}:
					if err != nil {
						return // 如果有错误，发送后退出goroutine
					}
				case <-b.stopChannel:
					return // 如果机器人停止，也退出goroutine
				}
			}
		}()
	}

	startReadLoop() // 初始启动

	logger.S().Info("WebSocket 消息监听循环已启动。")

	for {
		select {
		case result := <-readChannel:
			if result.err != nil {
				// 检查是否是正常的关闭错误
				if websocket.IsCloseError(result.err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					logger.S().Info("WebSocket 连接正常关闭。")
				} else {
					logger.S().Errorf("读取 WebSocket 消息失败: %v。尝试在5秒后重连...", result.err)
					time.Sleep(5 * time.Second)
					// 尝试重连
					if err := b.connectWebSocket(); err != nil {
						logger.S().Errorf("WebSocket 重连失败: %v", err)
					} else {
						startReadLoop() // 重连成功后，重新启动读取循环
					}
				}
				continue
			}
			// 处理收到的消息
			b.handleWebSocketMessage(result.message)

		case <-b.stopChannel:
			logger.S().Info("收到停止信号，正在关闭 WebSocket 消息循环。")
			// 优雅地关闭连接
			err := b.wsConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			if err != nil {
				logger.S().Warnf("发送 WebSocket 关闭帧失败: %v", err)
			}
			return
		}
	}
}

// handleWebSocketMessage 解析并处理来自WebSocket的消息
func (b *GridTradingBot) handleWebSocketMessage(message []byte) {
	// 首先，尝试解析为通用的事件类型，以获取事件类型 "e"
	var genericEvent struct {
		EventType interface{} `json:"e"`
	}
	if err := json.Unmarshal(message, &genericEvent); err != nil {
		// 这个错误理论上不应该再发生，除非JSON本身格式错误
		logger.S().Warnf("无法初步解析WebSocket消息: %v, 原始消息: %s", err, string(message))
		return
	}

	// 使用类型断言来安全地处理事件类型
	eventTypeString, ok := genericEvent.EventType.(string)
	if !ok {
		// 如果事件类型不是字符串，我们就记录下来，以便分析是什么我们没预料到的事件
		logger.S().Infof("收到非字符串类型的事件, 类型: %T, 值: %v, 原始消息: %s", genericEvent.EventType, genericEvent.EventType, string(message))
		return
	}

	// 只处理我们关心的 'ORDER_TRADE_UPDATE' 事件
	if eventTypeString == "ORDER_TRADE_UPDATE" {
		var event models.OrderUpdateEvent
		if err := json.Unmarshal(message, &event); err != nil {
			// 即使 e 是字符串，整个消息的结构也可能不匹配 OrderUpdateEvent
			logger.S().Warnf("无法完全解析 'ORDER_TRADE_UPDATE' 事件: %v, 原始消息: %s", err, string(message))
			return
		}

		// 我们只关心订单完全成交(FILLED)的事件
		if event.Order.ExecutionType == "TRADE" && event.Order.Status == "FILLED" {
			logger.S().Infof("--- WebSocket 收到订单成交事件 ---")
			logger.S().Infof("订单ID: %d, 交易对: %s, 方向: %s, 价格: %s, 数量: %s",
				event.Order.OrderID, event.Order.Symbol, event.Order.Side, event.Order.Price, event.Order.OrigQty)

			// 在一个单独的goroutine中处理，以避免阻塞WebSocket循环
			go func(o models.OrderUpdateInfo) {
				b.mutex.Lock()
				defer b.mutex.Unlock()

				var triggeredGrid *models.GridLevel
				var triggeredIndex int = -1

				// 找到被触发的网格
				for i, grid := range b.gridLevels {
					if grid.OrderID == o.OrderID {
						triggeredGrid = &b.gridLevels[i]
						triggeredIndex = i
						break
					}
				}

				if triggeredGrid != nil {
					logger.S().Infof("匹配到活动的网格订单: GridID %d, 价格 %.4f", triggeredGrid.GridID, triggeredGrid.Price)
					// 从活动列表中移除已成交的订单
					b.gridLevels = append(b.gridLevels[:triggeredIndex], b.gridLevels[triggeredIndex+1:]...)

					// 检查价格是否触及回归价
					filledPrice, _ := strconv.ParseFloat(o.Price, 64)
					if filledPrice >= b.reversionPrice {
						logger.S().Infof("价格 %.4f 已达到或超过回归价 %.4f，准备重启周期。", filledPrice, b.reversionPrice)
						// 发送信号以安全地重启周期
						b.reentrySignal <- true
					} else {
						// 在新的goroutine中重置网格，以避免死锁
						go b.resetGridAroundPrice(triggeredGrid.GridID, triggeredGrid.Side)
					}
				} else {
					logger.S().Warnf("收到已成交订单 %d 的更新，但在活动网格中未找到匹配项。可能是在重置过程中成交的。", o.OrderID)
				}
			}(event.Order)
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

	logger.S().Info("--- 机器人正在启动 ---")

	// 在实盘模式下，连接WebSocket
	if !b.IsBacktest {
		if err := b.connectWebSocket(); err != nil {
			return fmt.Errorf("启动时连接WebSocket失败: %v", err)
		}
		go b.webSocketLoop() // 启动WebSocket消息处理循环
	}

	// 尝试加载状态，如果失败则初始化
	if err := b.LoadState("grid_state.json"); err != nil {
		logger.S().Warnf("加载状态失败: %v. 将执行市场入场和网格设置。", err)
		if err := b.enterMarketAndSetupGrid(); err != nil {
			return fmt.Errorf("机器人初始化失败: %v", err)
		}
	} else {
		logger.S().Info("成功从文件加载状态，机器人已恢复。")
		// 即使加载成功，也要确保底仓标记是正确的
		b.mutex.Lock()
		b.basePositionEstablished = true
		b.mutex.Unlock()
	}

	go b.strategyLoop()  // 启动主要的策略循环（用于处理再入场等）
	go b.monitorStatus() // 启动状态监控

	logger.S().Info("--- 机器人已成功启动 ---")
	return nil
}

// strategyLoop 是一个独立的循环，用于处理需要解耦的策略逻辑，如再入场
func (b *GridTradingBot) strategyLoop() {
	logger.S().Info("策略循环已启动，等待信号...")
	for {
		select {
		case <-b.reentrySignal:
			b.mutex.Lock()
			if b.isReentering {
				b.mutex.Unlock()
				logger.S().Info("已在再入场流程中，忽略新的信号。")
				continue
			}
			b.isReentering = true
			b.mutex.Unlock()

			logger.S().Info("收到再入场信号，开始执行...")
			// 取消所有订单
			if err := b.exchange.CancelAllOpenOrders(b.config.Symbol); err != nil {
				logger.S().Errorf("再入场时取消所有订单失败: %v", err)
			}
			// 重新进入市场
			if err := b.enterMarketAndSetupGrid(); err != nil {
				logger.S().Errorf("再入场时执行 enterMarketAndSetupGrid 失败: %v", err)
			}
			b.mutex.Lock()
			b.isReentering = false
			b.mutex.Unlock()
			logger.S().Info("再入场流程完成。")

		case <-b.stopChannel:
			logger.S().Info("收到停止信号，正在关闭策略循环。")
			return
		}
	}
}

// StartForBacktest 为回测模式准备机器人，但不启动循环
func (b *GridTradingBot) StartForBacktest() error {
	b.mutex.Lock()
	if b.isRunning {
		b.mutex.Unlock()
		return fmt.Errorf("机器人已在运行中")
	}
	b.isRunning = true
	b.mutex.Unlock()

	// 在回测中，我们不加载状态，总是从新开始
	if err := b.enterMarketAndSetupGrid(); err != nil {
		return fmt.Errorf("回测机器人初始化失败: %v", err)
	}

	logger.S().Info("--- 回测机器人已初始化 ---")
	return nil
}

// ProcessBacktestTick 在回测模式下处理单个时间点的数据
func (b *GridTradingBot) ProcessBacktestTick() {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	if !b.isRunning || b.isHalted {
		return
	}

	// 模拟交易所检查订单成交
	var filledGrids []models.GridLevel
	var remainingGrids []models.GridLevel

	for _, grid := range b.gridLevels {
		if (grid.Side == "BUY" && b.currentPrice <= grid.Price) || (grid.Side == "SELL" && b.currentPrice >= grid.Price) {
			filledGrids = append(filledGrids, grid)
			logger.S().Infof("[回测] 订单成交: %s @ %.4f, GridID: %d", grid.Side, grid.Price, grid.GridID)
		} else {
			remainingGrids = append(remainingGrids, grid)
		}
	}

	b.gridLevels = remainingGrids

	// 检查是否触及回归价
	if b.currentPrice >= b.reversionPrice {
		logger.S().Infof("[回测] 价格 %.4f 已达到或超过回归价 %.4f，准备重启周期。", b.currentPrice, b.reversionPrice)
		// 在回测中直接调用，因为是单线程
		if err := b.enterMarketAndSetupGrid(); err != nil {
			logger.S().Errorf("[回测] 再入场失败: %v", err)
		}
	} else {
		// 如果有订单成交，则重置网格
		for _, filledGrid := range filledGrids {
			// 在回测中，我们直接调用 resetGrid，因为它会同步执行
			b.resetGridAroundPrice(filledGrid.GridID, filledGrid.Side)
		}
	}
}

// SetCurrentPrice 设置当前价格，主要用于回测
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
		logger.S().Info("机器人已经停止。")
		return
	}
	b.isRunning = false
	close(b.stopChannel) // 关闭通道以广播停止信号
	b.mutex.Unlock()

	logger.S().Info("--- 机器人正在停止 ---")

	// 在实盘模式下，取消所有挂单
	if !b.IsBacktest {
		logger.S().Info("正在取消所有未结订单...")
		b.cancelAllActiveOrders()
	}

	// 等待一小段时间确保所有goroutine都已收到信号
	time.Sleep(1 * time.Second)

	logger.S().Info("--- 机器人已停止 ---")
}

// cancelAllActiveOrders 取消所有活动的网格订单
func (b *GridTradingBot) cancelAllActiveOrders() {
	b.mutex.RLock()
	defer b.mutex.RUnlock()

	logger.S().Infof("准备取消 %d 个活动订单...", len(b.gridLevels))
	if err := b.exchange.CancelAllOpenOrders(b.config.Symbol); err != nil {
		logger.S().Errorf("停止机器人时取消所有订单失败: %v", err)
	} else {
		logger.S().Info("已成功发送取消所有订单的请求。")
	}
}

// monitorStatus 定期打印机器人状态
func (b *GridTradingBot) monitorStatus() {
	if b.IsBacktest {
		return // 回测模式下不打印状态
	}
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

// SaveState 将机器人的当前状态保存到文件
func (b *GridTradingBot) SaveState(path string) error {
	b.mutex.RLock()
	defer b.mutex.RUnlock()

	state := &models.GridState{
		GridLevels:     b.gridLevels,
		EntryPrice:     b.entryPrice,
		ReversionPrice: b.reversionPrice,
		ConceptualGrid: b.conceptualGrid,
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化状态失败: %v", err)
	}

	if err := ioutil.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("写入状态文件失败: %v", err)
	}

	logger.S().Infof("机器人状态已成功保存到 %s", path)
	return nil
}

// LoadState 从文件加载机器人的状态
func (b *GridTradingBot) LoadState(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("状态文件 %s 不存在", path)
	}

	data, err := ioutil.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取状态文件失败: %v", err)
	}

	var state models.GridState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("反序列化状态失败: %v", err)
	}

	b.mutex.Lock()
	defer b.mutex.Unlock()

	b.gridLevels = state.GridLevels
	b.entryPrice = state.EntryPrice
	b.reversionPrice = state.ReversionPrice
	b.conceptualGrid = state.ConceptualGrid

	logger.S().Infof("机器人状态已成功从 %s 加载", path)
	return nil
}

// printStatus 打印当前机器人状态的摘要
func (b *GridTradingBot) printStatus() {
	b.mutex.RLock()
	defer b.mutex.RUnlock()

	logger.S().Info("--- 机器人状态报告 ---")
	logger.S().Infof("交易对: %s", b.config.Symbol)
	logger.S().Infof("当前价格: %.4f", b.currentPrice)
	logger.S().Infof("运行状态: %v", b.isRunning)
	logger.S().Infof("本周期入场价: %.4f", b.entryPrice)
	logger.S().Infof("本周期回归价: %.4f", b.reversionPrice)
	logger.S().Infof("活动挂单数: %d", len(b.gridLevels))

	// 打印活动订单详情
	for _, level := range b.gridLevels {
		logger.S().Infof("  - %s 单: 价格 %.4f, 数量 %.5f, GridID: %d, OrderID: %d",
			level.Side, level.Price, level.Quantity, level.GridID, level.OrderID)
	}

	// 打印持仓信息
	positions, err := b.exchange.GetPositions(b.config.Symbol)
	if err != nil {
		logger.S().Warnf("无法获取持仓信息: %v", err)
	} else if len(positions) > 0 {
		pos := positions[0]
		logger.S().Infof("当前持仓: 数量: %s, 入场价: %s, 未实现盈亏: %s",
			pos.PositionAmt, pos.EntryPrice, pos.UnrealizedProfit)
	} else {
		logger.S().Info("当前无持仓。")
	}
	logger.S().Info("----------------------")
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
func (b *GridTradingBot) generateClientOrderID() (string, error) {
	return b.idGenerator.Generate()
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

// IsHalted 返回机器人是否已暂停
func (b *GridTradingBot) IsHalted() bool {
	b.mutex.RLock()
	defer b.mutex.RUnlock()
	return b.isHalted
}

// syncWithExchange 是一个新的函数，用于在从状态文件恢复后，将本地概念与交易所的实际情况同步
func (b *GridTradingBot) syncWithExchange() error {
	logger.S().Info("开始与交易所同步状态...")

	// 1. 获取所有远程的开放订单
	remoteOrders, err := b.exchange.GetOpenOrders(b.config.Symbol)
	if err != nil {
		return fmt.Errorf("无法从交易所获取开放订单: %v", err)
	}
	logger.S().Infof("从交易所获取了 %d 个开放订单。", len(remoteOrders))

	// 2. 清空本地的活动订单列表，准备重建
	b.mutex.Lock()
	b.gridLevels = make([]models.GridLevel, 0)
	b.mutex.Unlock()

	// 3. 遍历远程订单，尝试将其与本地的“天地网格”匹配
	for _, remoteOrder := range remoteOrders {
		price, _ := strconv.ParseFloat(remoteOrder.Price, 64)
		quantity, _ := strconv.ParseFloat(remoteOrder.OrigQty, 64)

		// 寻找这个价格是否在我们理论网格的价位上
		matchedGridID := -1
		for id, conceptualPrice := range b.conceptualGrid {
			// 使用一个小的容差来比较浮点数
			if math.Abs(price-conceptualPrice) < 1e-8 {
				matchedGridID = id
				break
			}
		}

		if matchedGridID != -1 {
			// 这是一个我们认识的订单，将其添加到本地活动订单列表
			newGridLevel := models.GridLevel{
				Price:    price,
				Quantity: quantity,
				Side:     remoteOrder.Side,
				IsActive: true,
				OrderID:  remoteOrder.OrderId,
				GridID:   matchedGridID,
			}
			b.mutex.Lock()
			b.gridLevels = append(b.gridLevels, newGridLevel)
			b.mutex.Unlock()
			logger.S().Infof("成功匹配并恢复订单: ID %d, GridID %d, Side %s, Price %.4f",
				remoteOrder.OrderId, matchedGridID, remoteOrder.Side, price)
		} else {
			// 这是一个我们不认识的订单，可能是手动下的，或者属于上一个周期的残留订单。取消它。
			logger.S().Warnf("发现一个无法匹配到当前天地网格的订单: ID %d, Price %.4f. 将其取消...",
				remoteOrder.OrderId, price)
			if err := b.exchange.CancelOrder(b.config.Symbol, remoteOrder.OrderId); err != nil {
				logger.S().Errorf("取消无法识别的订单 %d 失败: %v", remoteOrder.OrderId, err)
			}
		}
	}

	logger.S().Info("与交易所状态同步完成。")
	return nil
}
