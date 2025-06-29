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
	"sort"
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

		// 新增：排序和打印挂单详情
		// 按价格从高到低排序
		sort.Slice(ordersToPlace, func(i, j int) bool {
			return ordersToPlace[i].Price > ordersToPlace[j].Price
		})

		logger.S().Info("--- 计划挂单列表 ---")
		for _, order := range ordersToPlace {
			logger.S().Infof("  - %s @ %.4f (GridID: %d)", order.Side, order.Price, order.GridID)
		}
		logger.S().Info("--------------------")

		var wg sync.WaitGroup
		var newLevels []*models.GridLevel
		var newLevelsMutex sync.Mutex

		for _, order := range ordersToPlace {
			wg.Add(1)
			go func(order struct {
				Side   string
				Price  float64
				GridID int
			}) {
				defer wg.Done()
				newLevel, err := b.placeNewOrder(order.Side, order.Price, order.GridID)
				if err != nil {
					logger.S().Errorf("重置网格时下单失败: %v", err)
				} else if newLevel != nil {
					newLevelsMutex.Lock()
					newLevels = append(newLevels, newLevel)
					newLevelsMutex.Unlock()
				}
			}(order)
		}
		wg.Wait()

		b.mutex.Lock()
		for _, level := range newLevels {
			b.gridLevels = append(b.gridLevels, *level)
		}
		b.mutex.Unlock()
	}
}

// checkAndHandleFills 检查并处理已成交的订单
func (b *GridTradingBot) checkAndHandleFills() {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	var filledLevels []models.GridLevel
	var activeLevels []models.GridLevel

	// 遍历所有活动订单，检查状态
	for _, level := range b.gridLevels {
		if !level.IsActive {
			continue
		}
		orderStatus, err := b.exchange.GetOrderStatus(b.config.Symbol, level.OrderID)
		if err != nil {
			logger.S().Warnf("无法获取订单 %d 的状态: %v", level.OrderID, err)
			activeLevels = append(activeLevels, level) // 保留订单，下次再检查
			continue
		}

		if orderStatus.Status == "FILLED" {
			filledLevels = append(filledLevels, level)
			logger.S().Infof("检测到订单成交: %s @ %.4f, ID: %d, GridID: %d", level.Side, level.Price, level.OrderID, level.GridID)
		} else {
			activeLevels = append(activeLevels, level)
		}
	}

	// 更新活动订单列表
	b.gridLevels = activeLevels

	// 如果有成交的订单，则重置网格
	if len(filledLevels) > 0 {
		// 为了简化逻辑，我们只基于第一个检测到的成交订单来重置网格
		// 在快速变化的市场中，可能会有多个订单几乎同时成交，但这种简化是可接受的
		pivotLevel := filledLevels[0]
		go b.resetGridAroundPrice(pivotLevel.GridID, pivotLevel.Side)
	}

	// 检查是否达到回归价格
	if b.currentPrice >= b.reversionPrice {
		logger.S().Infof("价格 %.4f 已达到或超过回归价 %.4f。", b.currentPrice, b.reversionPrice)
		// 检查是否正在再入场，防止重复触发
		if !b.isReentering {
			b.isReentering = true // 设置状态锁
			logger.S().Info("触发周期性再入场信号...")
			// 使用非阻塞发送，如果通道已满（说明已有信号在处理），则忽略本次信号
			select {
			case b.reentrySignal <- true:
			default:
				logger.S().Warn("再入场信号通道已满，忽略本次触发。")
			}
		}
	}
}

// checkForReentryLocked 是一个在持有锁的情况下检查再入场条件的辅助函数
func (b *GridTradingBot) checkForReentryLocked() {
	if b.currentPrice >= b.reversionPrice && !b.isReentering {
		b.isReentering = true
		logger.S().Info("触发周期性再入场信号...")
		b.reentrySignal <- true
	}
}

// checkForReentry 是一个独立的函数，用于在不持有锁的情况下检查再入场
func (b *GridTradingBot) checkForReentry() {
	b.mutex.RLock()
	defer b.mutex.RUnlock()
	b.checkForReentryLocked()
}

// connectWebSocket 连接到币安的 WebSocket API
func (b *GridTradingBot) connectWebSocket() error {
	if b.IsBacktest {
		return nil // 回测模式下不连接 WebSocket
	}
	// ... WebSocket 连接逻辑 ...
	return nil
}

// webSocketLoop 是处理 WebSocket 消息的主循环
func (b *GridTradingBot) webSocketLoop() {
	for {
		select {
		case <-b.stopChannel:
			return
		default:
			if b.wsConn == nil {
				if err := b.connectWebSocket(); err != nil {
					logger.S().Errorf("WebSocket 连接失败: %v", err)
					time.Sleep(5 * time.Second)
					continue
				}
			}
			if err := b.handleWebSocketMessages(); err != nil {
				logger.S().Errorf("处理 WebSocket 消息时出错: %v", err)
				b.wsConn.Close()
				b.wsConn = nil
			}
		}
	}
}

// handleWebSocketMessages 读取和处理来自 WebSocket 的消息
func (b *GridTradingBot) handleWebSocketMessages() error {
	if b.IsBacktest || b.wsConn == nil {
		// 在回测模式下，我们通过 strategyLoop 模拟价格变动，而不是通过ws
		// 所以这里我们创建一个假的循环来让机器人保持运行
		for {
			select {
			case <-b.stopChannel:
				return nil
			default:
				time.Sleep(100 * time.Millisecond) // 避免空转
			}
		}
	}

	// 实盘逻辑
	for {
		_, message, err := b.wsConn.ReadMessage()
		if err != nil {
			return err
		}

		var event models.TradeEvent
		if err := json.Unmarshal(message, &event); err != nil {
			logger.S().Warnf("无法解析 WebSocket 消息: %v", err)
			continue
		}

		if event.EventType == "aggTrade" {
			price, err := strconv.ParseFloat(event.Price, 64)
			if err != nil {
				logger.S().Warnf("无法解析价格: %v", err)
				continue
			}
			b.mutex.Lock()
			b.currentPrice = price
			b.currentTime = time.Now() // 更新当前时间
			b.mutex.Unlock()
		}
	}
}

// Start 启动机器人
func (b *GridTradingBot) Start() error {
	b.mutex.Lock()
	if b.isRunning {
		b.mutex.Unlock()
		return fmt.Errorf("机器人已在运行")
	}
	b.isRunning = true
	b.mutex.Unlock()

	logger.S().Info("机器人正在启动...")

	// 启动时设置杠杆和保证金模式
	if !b.IsBacktest {
		if err := b.exchange.SetLeverage(b.config.Symbol, b.config.Leverage); err != nil {
			return fmt.Errorf("设置杠杆失败: %v", err)
		}
		logger.S().Infof("成功设置杠杆为 %dx", b.config.Leverage)

		if err := b.exchange.SetMarginType(b.config.Symbol, b.config.MarginType); err != nil {
			return fmt.Errorf("设置保证金模式失败: %v", err)
		}
		logger.S().Infof("成功设置保证金模式为 %s", b.config.MarginType)

		if err := b.exchange.SetPositionMode(b.config.PositionMode == "Hedge"); err != nil {
			return fmt.Errorf("设置持仓模式失败: %v", err)
		}
		logger.S().Infof("成功设置持仓模式为 %s", b.config.PositionMode)
	}

	// 尝试加载状态
	if err := b.LoadState("grid_state.json"); err != nil {
		logger.S().Warnf("加载状态失败或无状态文件，将开始新周期: %v", err)
		if err := b.enterMarketAndSetupGrid(); err != nil {
			return fmt.Errorf("启动时无法建立初始网格: %v", err)
		}
	} else {
		logger.S().Info("成功从 grid_state.json 加载机器人状态。")
	}

	go b.strategyLoop()
	go b.monitorStatus()

	logger.S().Info("机器人已成功启动。")
	return nil
}

// strategyLoop 是机器人的主策略循环
func (b *GridTradingBot) strategyLoop() {
	// 在实盘模式下，我们依赖 WebSocket 来更新价格和触发检查
	// 在回测模式下，这个循环是空的，因为所有逻辑都在 ProcessBacktestTick 中
	if b.IsBacktest {
		return
	}

	// 主循环，监听再入场信号
	for {
		select {
		case <-b.stopChannel:
			logger.S().Info("策略循环收到停止信号，正在退出...")
			return
		case <-b.reentrySignal:
			logger.S().Info("收到再入场信号，开始执行平仓和再入场...")

			// 1. 取消所有挂单
			b.cancelAllActiveOrders()

			// 2. 市价平掉所有仓位
			positions, err := b.exchange.GetPositions(b.config.Symbol)
			if err != nil {
				logger.S().Errorf("再入场时获取持仓失败: %v", err)
				continue // 跳过本次再入场
			}
			if len(positions) > 0 {
				positionAmt, _ := strconv.ParseFloat(positions[0].PositionAmt, 64)
				if positionAmt > 0 {
					logger.S().Infof("检测到持仓 %.8f，将市价平仓...", positionAmt)
					clientOrderID := b.generateClientOrderID("reentry_close")
					_, err := b.exchange.PlaceOrder(b.config.Symbol, "SELL", "MARKET", positionAmt, 0, clientOrderID)
					if err != nil {
						logger.S().Errorf("再入场时市价平仓失败: %v", err)
						// 即使平仓失败，也继续尝试再入场
					}
				}
			}

			// 3. 延时以确保平仓完成
			time.Sleep(2 * time.Second)

			// 4. 开始新的周期
			if err := b.enterMarketAndSetupGrid(); err != nil {
				logger.S().Errorf("再入场时无法建立新网格: %v", err)
				// 如果失败，机器人将处于无挂单状态，等待下一次价格达到回归价
			}
		}
	}
}

// StartForBacktest 为回测启动机器人
func (b *GridTradingBot) StartForBacktest() error {
	b.mutex.Lock()
	if b.isRunning {
		b.mutex.Unlock()
		return fmt.Errorf("机器人已在运行")
	}
	b.isRunning = true
	b.mutex.Unlock()

	logger.S().Info("机器人正在为回测启动...")

	// 回测直接开始新周期
	if err := b.enterMarketAndSetupGrid(); err != nil {
		return fmt.Errorf("回测启动时无法建立初始网格: %v", err)
	}

	logger.S().Info("回测机器人已成功启动。")
	return nil
}

// ProcessBacktestTick 在回测模式下处理单个时间点的数据
func (b *GridTradingBot) ProcessBacktestTick() {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	if !b.isRunning {
		return
	}

	// 在回测中，我们直接调用 checkAndHandleFills
	// 因为 SetPrice 已经模拟了价格变动和订单成交
	b.checkAndHandleFills()

	// 检查止损
	if b.config.StopLossRate > 0 {
		positions, err := b.exchange.GetPositions(b.config.Symbol)
		if err != nil {
			logger.S().With("time", b.currentTime.Format(time.RFC3339)).Errorf("[回测] 获取持仓失败: %v", err)
			return
		}
		if len(positions) > 0 {
			positionAmt, _ := strconv.ParseFloat(positions[0].PositionAmt, 64)
			entryPrice, _ := strconv.ParseFloat(positions[0].EntryPrice, 64)
			if positionAmt > 0 {
				stopLossPrice := entryPrice * (1 - b.config.StopLossRate)
				if b.currentPrice <= stopLossPrice {
					logger.S().With("time", b.currentTime.Format(time.RFC3339)).Infof("[回测] 触发止损！当前价 %.4f <= 止损价 %.4f", b.currentPrice, stopLossPrice)
					clientOrderID := b.generateClientOrderID("backtest_stop")
					b.exchange.PlaceOrder(b.config.Symbol, "SELL", "MARKET", positionAmt, 0, clientOrderID)
					logger.S().With("time", b.currentTime.Format(time.RFC3339)).Infof("[回测] 模拟市价卖出所有持仓: %.8f", positionAmt)
					b.isRunning = false // 停止机器人
				}
			}
		}
	}
}

// SetCurrentPrice 在回测中设置当前价格
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
	close(b.stopChannel)
	b.mutex.Unlock()

	if err := b.SaveState("grid_state.json"); err != nil {
		logger.S().Errorf("保存状态失败: %v", err)
	}

	logger.S().Info("正在取消所有活动订单...")
	b.cancelAllActiveOrders()
}

// cancelAllActiveOrders 取消所有活动的网格订单
func (b *GridTradingBot) cancelAllActiveOrders() {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	logger.S().Infof("开始取消 %d 个活动订单...", len(b.gridLevels))
	for i := range b.gridLevels {
		b.gridLevels[i].IsActive = false
	}
	if err := b.exchange.CancelAllOpenOrders(b.config.Symbol); err != nil {
		logger.S().Warnf("取消所有订单时出错: %v", err)
	}
	b.gridLevels = make([]models.GridLevel, 0) // 清空列表
}

// monitorStatus 定期打印机器人状态
func (b *GridTradingBot) monitorStatus() {
	if b.IsBacktest {
		return
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			b.printStatus()
		case <-b.stopChannel:
			return
		}
	}
}

// SaveState 保存机器人的当前状态到文件
func (b *GridTradingBot) SaveState(path string) error {
	b.mutex.RLock()
	defer b.mutex.RUnlock()

	state := models.BotState{
		GridLevels:     b.gridLevels,
		EntryPrice:     b.entryPrice,
		ReversionPrice: b.reversionPrice,
		ConceptualGrid: b.conceptualGrid,
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return ioutil.WriteFile(path, data, 0644)
}

// LoadState 从文件加载机器人的状态
func (b *GridTradingBot) LoadState(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("状态文件 %s 不存在", path)
	}

	data, err := ioutil.ReadFile(path)
	if err != nil {
		return err
	}

	var state models.BotState
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}

	b.mutex.Lock()
	defer b.mutex.Unlock()

	b.gridLevels = state.GridLevels
	b.entryPrice = state.EntryPrice
	b.reversionPrice = state.ReversionPrice
	b.conceptualGrid = state.ConceptualGrid
	b.basePositionEstablished = true // 如果能加载状态，说明底仓已建立

	// 状态恢复后，需要验证并可能取消交易所上仍然存在的旧订单
	// 这是一个好习惯，以防状态文件和交易所的实际情况不一致
	logger.S().Info("状态已加载，正在同步交易所的挂单...")
	openOrders, err := b.exchange.GetOpenOrders(b.config.Symbol)
	if err != nil {
		logger.S().Warnf("无法获取当前挂单来同步状态: %v", err)
		return nil // 不中断启动
	}

	// 创建一个本地活动订单ID的map以便快速查找
	localOrderIDs := make(map[int64]bool)
	for _, level := range b.gridLevels {
		localOrderIDs[level.OrderID] = true
	}

	// 遍历交易所的挂单
	for _, exchangeOrder := range openOrders {
		if _, exists := localOrderIDs[exchangeOrder.OrderId]; !exists {
			logger.S().Infof("发现一个在状态文件中不存在的交易所挂单 (ID: %d)，正在取消...", exchangeOrder.OrderId)
			if err := b.exchange.CancelOrder(b.config.Symbol, exchangeOrder.OrderId); err != nil {
				logger.S().Warnf("取消多余的挂单 %d 失败: %v", exchangeOrder.OrderId, err)
			}
		}
	}

	return nil
}

// printStatus 打印机器人的当前状态
func (b *GridTradingBot) printStatus() {
	b.mutex.RLock()
	defer b.mutex.RUnlock()

	logger.S().Infof("--- 机器人状态 ---")
	logger.S().Infof("当前价格: %.4f", b.currentPrice)
	logger.S().Infof("入场价: %.4f, 回归价: %.4f", b.entryPrice, b.reversionPrice)
	logger.S().Infof("活动挂单数: %d", len(b.gridLevels))
	for _, level := range b.gridLevels {
		logger.S().Infof("  - %s @ %.4f (ID: %d, GridID: %d)", level.Side, level.Price, level.OrderID, level.GridID)
	}
	logger.S().Infof("-------------------")
}

// adjustValueToStep 根据步进精度调整数值
func adjustValueToStep(value float64, step string) float64 {
	stepFloat, err := strconv.ParseFloat(step, 64)
	if err != nil || stepFloat == 0 {
		return value // 如果步进无效，则返回原值
	}
	// 计算小数位数
	precision := 0
	if strings.Contains(step, ".") {
		precision = len(strings.Split(step, ".")[1])
	}

	// 向下取整到步进的倍数
	adjusted := math.Floor(value/stepFloat) * stepFloat

	// 格式化到正确的小数位数
	format := fmt.Sprintf("%%.%df", precision)
	formatted, _ := strconv.ParseFloat(fmt.Sprintf(format, adjusted), 64)
	return formatted
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

	logger.S().Infof("[RISK CHECK] 预估风险暴露: %.2f%% (限制: %.2f%%). 当前持仓价值: %.2f, 新增后预估持仓价值: %.2f, 账户权益: %.2f.",
		futureWalletExposure*100, b.config.WalletExposureLimit*100, positionValue, futurePositionValue, accountEquity)

	// 根据新需求，此函数不再阻止下单，仅作为日志参考。
	return true
}

// IsHalted 返回机器人是否处于暂停状态
func (b *GridTradingBot) IsHalted() bool {
	b.mutex.RLock()
	defer b.mutex.RUnlock()
	return b.isHalted
}

// generateClientOrderID 创建一个唯一的客户端订单ID
func (b *GridTradingBot) generateClientOrderID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}
