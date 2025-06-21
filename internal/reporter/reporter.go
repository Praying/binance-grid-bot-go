package reporter

import (
	"binance-grid-bot-go/internal/exchange"
	"binance-grid-bot-go/internal/logger"
	"binance-grid-bot-go/internal/models"
	"math"
	"sort"
	"time"
)

// Metrics 存储计算出的所有回测性能指标
type Metrics struct {
	InitialBalance      float64
	FinalEquity         float64 // 更名：最终资金 -> 期末总资产
	TotalPnL            float64 // 更名：总利润 -> 总盈亏 (含浮动)
	RealizedProfit      float64 // 新增：已实现利润
	UnrealizedPnL       float64 // 新增：浮动盈亏
	ProfitPercentage    float64
	TotalTrades         int
	WinningTrades       int
	LosingTrades        int
	WinRate             float64
	AvgProfitLoss       float64
	MaxDrawdown         float64
	SharpeRatio         float64
	AnnualizedReturn    float64       // 新增：年化收益率
	EndingCash          float64       // 新增：期末现金
	EndingAssetValue    float64       // 新增：期末持仓市值
	AvgHoldDuration     time.Duration // 新增：平均持仓时长
	AvgWinHoldDuration  time.Duration // 新增：盈利交易平均持仓时长
	AvgLossHoldDuration time.Duration // 新增：亏损交易平均持仓时长
	TotalSlippage       float64       // 新增：总滑点成本
	TotalAssetQty       float64       // 新增：持有资产的总数量
	TotalFees           float64       // 新增: 累积总手续费
	StartTime           time.Time
	EndTime             time.Time
	MaxWalletExposure   float64 // 新增：最大钱包风险暴露
}

// GenerateReport 根据回测交易所的状态计算并打印性能报告
func GenerateReport(backtestExchange *exchange.BacktestExchange, dataPath string, startTime, endTime time.Time) {
	logger.S().Info("开始生成回测报告...")
	logger.S().Infof("待处理的已完成交易记录数量: %d", len(backtestExchange.TradeLog))

	metrics, symbol := calculateMetrics(backtestExchange, startTime, endTime)

	logger.S().Info("========== 回测结果报告 ==========")
	logger.S().Infof("数据文件:         %s", dataPath)
	logger.S().Infof("交易对:           %s", symbol)
	logger.S().Infof("回测周期:         %s 到 %s", metrics.StartTime.Format("2006-01-02 15:04"), metrics.EndTime.Format("2006-01-02 15:04"))
	logger.S().Info("------------------------------------")
	logger.S().Infof("初始资金:         %.2f USDT", metrics.InitialBalance)
	logger.S().Infof("期末总资产(Equity): %.2f USDT", metrics.FinalEquity)
	logger.S().Infof("总盈亏 (Total PnL): %.2f USDT", metrics.TotalPnL)
	logger.S().Infof("  - 已实现利润:   %.2f USDT", metrics.RealizedProfit)
	logger.S().Infof("  - 浮动盈亏:     %.2f USDT", metrics.UnrealizedPnL)
	logger.S().Infof("总收益率:         %.2f%%", metrics.ProfitPercentage)
	logger.S().Infof("总手续费:         %.4f USDT", metrics.TotalFees)
	logger.S().Infof("总滑点成本:       %.4f USDT", metrics.TotalSlippage)
	logger.S().Info("------------------------------------")
	logger.S().Infof("总交易次数:       %d", metrics.TotalTrades)
	logger.S().Infof("盈利次数:         %d", metrics.WinningTrades)
	logger.S().Infof("亏损次数:         %d", metrics.LosingTrades)
	logger.S().Infof("胜率:             %.2f%%", metrics.WinRate)
	logger.S().Infof("平均盈亏比:       %.2f", metrics.AvgProfitLoss)
	logger.S().Infof("最大回撤:         %.2f%%", metrics.MaxDrawdown)
	logger.S().Infof("最大钱包风险暴露: %.2f%%", metrics.MaxWalletExposure*100)
	logger.S().Info("------------------------------------")
	logger.S().Infof("平均持仓时长:     %s", metrics.AvgHoldDuration)
	logger.S().Infof("盈利交易平均时长: %s", metrics.AvgWinHoldDuration)
	logger.S().Infof("亏损交易平均时长: %s", metrics.AvgLossHoldDuration)
	logger.S().Info("------------------------------------")
	logger.S().Infof("夏普比率:         %.2f", metrics.SharpeRatio)
	if metrics.AnnualizedReturn == 0 && metrics.ProfitPercentage > 1000 {
		logger.S().Info("年化收益率:       N/A (因总收益率异常)")
	} else if metrics.AnnualizedReturn == 0 {
		logger.S().Info("年化收益率:       N/A (因回测周期过短)")
	} else {
		logger.S().Infof("年化收益率:       %.2f%%", metrics.AnnualizedReturn)
	}
	logger.S().Info("--- 期末资产分析 ---")
	logger.S().Infof("期末现金:         %.2f USDT", metrics.EndingCash)
	logger.S().Infof("期末持仓市值:     %.2f USDT (共 %.4f %s)", metrics.EndingAssetValue, metrics.TotalAssetQty, symbol)
	logger.S().Info("===================================")

	// 新增：打印交易分布分析
	logger.S().Info("正在准备打印交易分布分析...")
	printTradeDistributionAnalysis(backtestExchange.TradeLog)
	logger.S().Info("交易分布分析打印完成。")
	logger.S().Info("回测报告生成完毕。")
}

func calculateMetrics(be *exchange.BacktestExchange, startTime, endTime time.Time) (*Metrics, string) {
	m := &Metrics{
		StartTime: startTime,
		EndTime:   endTime,
	}

	m.InitialBalance = be.InitialBalance

	// 1. 计算已实现利润和相关交易统计
	m.TotalTrades = len(be.TradeLog)
	var totalProfitOnWins, totalLossOnLosses float64
	var totalHoldDuration, winHoldDuration, lossHoldDuration time.Duration
	for _, trade := range be.TradeLog {
		totalHoldDuration += trade.HoldDuration
		m.TotalSlippage += trade.Slippage
		m.RealizedProfit += trade.Profit // 累加每笔交易的净利润

		if trade.Profit > 0 {
			m.WinningTrades++
			totalProfitOnWins += trade.Profit
			winHoldDuration += trade.HoldDuration
		} else {
			m.LosingTrades++
			totalLossOnLosses += trade.Profit
			lossHoldDuration += trade.HoldDuration
		}
	}
	m.TotalFees = be.TotalFees // 直接从 exchange 获取准确的总手续费

	// 2. 计算期末资产详情
	m.EndingCash = be.Cash
	for _, posQty := range be.Positions {
		m.TotalAssetQty += posQty
	}
	m.EndingAssetValue = m.TotalAssetQty * be.CurrentPrice
	m.FinalEquity = m.EndingCash + m.EndingAssetValue

	// 3. 计算总盈亏和浮动盈亏
	m.TotalPnL = m.FinalEquity - m.InitialBalance
	m.UnrealizedPnL = m.TotalPnL - m.RealizedProfit // 总盈亏 = 已实现 + 未实现

	// 4. 计算收益率和其他比率
	if m.InitialBalance != 0 {
		m.ProfitPercentage = (m.TotalPnL / m.InitialBalance) * 100
	}

	if m.TotalTrades > 0 {
		m.WinRate = float64(m.WinningTrades) / float64(m.TotalTrades) * 100
		m.AvgHoldDuration = totalHoldDuration / time.Duration(m.TotalTrades)
	}

	if m.WinningTrades > 0 {
		m.AvgWinHoldDuration = winHoldDuration / time.Duration(m.WinningTrades)
	}

	if m.LosingTrades > 0 {
		m.AvgLossHoldDuration = lossHoldDuration / time.Duration(m.LosingTrades)
	}

	if m.WinningTrades > 0 && m.LosingTrades > 0 {
		avgWin := totalProfitOnWins / float64(m.WinningTrades)
		avgLoss := math.Abs(totalLossOnLosses / float64(m.LosingTrades))
		if avgLoss > 0 {
			m.AvgProfitLoss = avgWin / avgLoss
		}
	}

	// 从权益曲线计算最大回撤
	m.MaxDrawdown = calculateMaxDrawdown(be.EquityCurve) * 100

	// 计算年化收益率和夏普比率
	days := endTime.Sub(startTime).Hours() / 24
	// 增加鲁棒性：当周期过短(小于30天)或总收益率过高(大于1000%)时，认为年化指标无意义。
	isProfitAbnormal := m.ProfitPercentage > 1000
	if days >= 30 && m.InitialBalance > 0 && !isProfitAbnormal {
		m.AnnualizedReturn = (math.Pow(1+m.TotalPnL/m.InitialBalance, 365.0/days) - 1) * 100
	} else {
		m.AnnualizedReturn = 0 // 周期太短或利润异常，不计算
	}
	m.SharpeRatio = calculateSharpeRatio(be.GetDailyEquity(), 0.0)
	m.MaxWalletExposure = be.GetMaxWalletExposure()

	return m, be.Symbol
}

func calculateMaxDrawdown(equityCurve []float64) float64 {
	if len(equityCurve) < 2 {
		return 0.0
	}
	peak := equityCurve[0]
	maxDrawdown := 0.0

	for _, equity := range equityCurve {
		if equity > peak {
			peak = equity
		}
		drawdown := (peak - equity) / peak
		if drawdown > maxDrawdown {
			maxDrawdown = drawdown
		}
	}
	return maxDrawdown
}

func calculateSharpeRatio(dailyEquity map[string]float64, riskFreeRate float64) float64 {
	var dailyReturns []float64
	// 为了计算收益率，我们需要对日期进行排序
	days := make([]string, 0, len(dailyEquity))
	for day := range dailyEquity {
		days = append(days, day)
	}
	sort.Strings(days)

	var lastEquity float64 = -1
	for _, day := range days {
		equity := dailyEquity[day]
		if lastEquity != -1 {
			dailyReturn := (equity - lastEquity) / lastEquity
			dailyReturns = append(dailyReturns, dailyReturn)
		}
		lastEquity = equity
	}

	if len(dailyReturns) < 2 {
		return 0.0
	}

	// 计算平均日收益率和标准差
	var sumReturns float64
	for _, r := range dailyReturns {
		sumReturns += r
	}
	meanReturn := sumReturns / float64(len(dailyReturns))

	var sumSqDiff float64
	for _, r := range dailyReturns {
		sumSqDiff += math.Pow(r-meanReturn, 2)
	}
	stdDev := math.Sqrt(sumSqDiff / float64(len(dailyReturns)))

	if stdDev == 0 {
		return 0.0
	}

	// 年化夏普比率 (假设一年有252个交易日)
	sharpeRatio := (meanReturn - riskFreeRate/252) / stdDev * math.Sqrt(252)
	return sharpeRatio
}

// --- 新增的交易分布分析函数 ---

func printTradeDistributionAnalysis(trades []models.CompletedTrade) {
	if len(trades) == 0 {
		return
	}

	logger.S().Info("--- 交易分布分析 ---")
	analyzeTradeDistributionByDay(trades)
	analyzeTradeDistributionByPrice(trades)
	logger.S().Info("===================================")
}

// analyzeTradeDistributionByDay 分析每日的交易次数
func analyzeTradeDistributionByDay(trades []models.CompletedTrade) {
	tradesByDay := make(map[string]int)
	for _, trade := range trades {
		day := trade.ExitTime.Format("2006-01-02")
		tradesByDay[day]++
	}

	// 为了有序输出，我们对日期进行排序
	days := make([]string, 0, len(tradesByDay))
	for day := range tradesByDay {
		days = append(days, day)
	}
	sort.Strings(days)

	logger.S().Info("\n[每日交易次数分布]")
	for _, day := range days {
		logger.S().Infof("%s: %d 次", day, tradesByDay[day])
	}
}

// analyzeTradeDistributionByPrice 分析价格区间的交易次数
func analyzeTradeDistributionByPrice(trades []models.CompletedTrade) {
	tradesByPrice := make(map[int]int)
	minPrice, maxPrice := trades[0].ExitPrice, trades[0].ExitPrice
	for _, trade := range trades {
		if trade.ExitPrice < minPrice {
			minPrice = trade.ExitPrice
		}
		if trade.ExitPrice > maxPrice {
			maxPrice = trade.ExitPrice
		}
	}

	// 动态确定价格步长，目标是分成大约20个区间
	priceRange := maxPrice - minPrice
	if priceRange == 0 {
		logger.S().Info("\n[价格区间交易次数分布]: 所有交易都在同一价格完成。")
		return
	}
	step := math.Pow(10, math.Floor(math.Log10(priceRange/20)))
	step = math.Max(step, 0.0001) // 避免步长为0

	for _, trade := range trades {
		bucket := int(math.Floor(trade.ExitPrice/step)) * int(step)
		tradesByPrice[bucket]++
	}

	buckets := make([]int, 0, len(tradesByPrice))
	for bucket := range tradesByPrice {
		buckets = append(buckets, bucket)
	}
	sort.Ints(buckets)

	logger.S().Info("\n[价格区间交易次数分布]")
	for _, bucket := range buckets {
		logger.S().Infof("~%.4f USDT: %d 次", float64(bucket), tradesByPrice[bucket])
	}
}
