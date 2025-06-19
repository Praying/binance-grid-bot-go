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
	InitialBalance   float64
	FinalBalance     float64
	TotalProfit      float64
	ProfitPercentage float64
	TotalTrades      int
	WinningTrades    int
	LosingTrades     int
	WinRate          float64
	AvgProfitLoss    float64
	MaxDrawdown      float64
	SharpeRatio      float64 // (暂未实现)
	EndingCash       float64 // 新增：期末现金
	EndingAssetValue float64 // 新增：期末持仓市值
	TotalAssetQty    float64 // 新增：持有资产的总数量
	TotalFees        float64 // 新增: 累积总手续费
	StartTime        time.Time
	EndTime          time.Time
}

// GenerateReport 根据回测交易所的状态计算并打印性能报告
func GenerateReport(backtestExchange *exchange.BacktestExchange, dataPath string, startTime, endTime time.Time) {
	logger.S().Info("开始生成回测报告...")
	logger.S().Infof("待处理的已完成交易记录数量: %d", len(backtestExchange.TradeLog))

	metrics, symbol := calculateMetrics(backtestExchange)
	metrics.StartTime = startTime
	metrics.EndTime = endTime

	logger.S().Info("========== 回测结果报告 ==========")
	logger.S().Infof("数据文件:         %s", dataPath)
	logger.S().Infof("交易对:           %s", symbol)
	logger.S().Infof("回测周期:         %s 到 %s", metrics.StartTime.Format("2006-01-02 15:04"), metrics.EndTime.Format("2006-01-02 15:04"))
	logger.S().Info("------------------------------------")
	logger.S().Infof("初始资金:         %.2f USDT", metrics.InitialBalance)
	logger.S().Infof("最终资金:         %.2f USDT", metrics.FinalBalance)
	logger.S().Infof("总利润:           %.2f USDT", metrics.TotalProfit)
	logger.S().Infof("收益率:           %.2f%%", metrics.ProfitPercentage)
	logger.S().Infof("总手续费:         %.4f USDT", metrics.TotalFees)
	logger.S().Info("------------------------------------")
	logger.S().Infof("总交易次数:       %d", metrics.TotalTrades)
	logger.S().Infof("盈利次数:         %d", metrics.WinningTrades)
	logger.S().Infof("亏损次数:         %d", metrics.LosingTrades)
	logger.S().Infof("胜率:             %.2f%%", metrics.WinRate)
	logger.S().Infof("平均盈亏比:       %.2f", metrics.AvgProfitLoss)
	logger.S().Infof("最大回撤:         %.2f%%", metrics.MaxDrawdown)
	logger.S().Infof("夏普比率:         %.2f (暂未实现)", metrics.SharpeRatio)
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

func calculateMetrics(be *exchange.BacktestExchange) (*Metrics, string) {
	m := &Metrics{}

	m.InitialBalance = be.InitialBalance

	if len(be.EquityCurve) > 0 {
		m.FinalBalance = be.EquityCurve[len(be.EquityCurve)-1]
	} else {
		m.FinalBalance = be.InitialBalance
	}
	m.TotalProfit = m.FinalBalance - m.InitialBalance
	if m.InitialBalance != 0 {
		m.ProfitPercentage = (m.TotalProfit / m.InitialBalance) * 100
	}

	m.TotalTrades = len(be.TradeLog)
	var totalProfit, totalLoss float64
	for _, trade := range be.TradeLog {
		// Profit现在是净利润（已实现盈亏 - 手续费）
		if trade.Profit > 0 {
			m.WinningTrades++
			totalProfit += trade.Profit
		} else {
			m.LosingTrades++
			totalLoss += trade.Profit
		}
		// 累加手续费
		// 注意: trade.Profit = realizedPNL - fee, 所以 fee = realizedPNL - trade.Profit
		// 这个计算是间接的。更直接的方式是在BacktestExchange中累加。
		// 但为了简单起见，我们暂时接受这种方式。
		// realizedPNL = (executionPrice - avgEntry) * quantity
		// 我们没有在这里记录avgEntry, 所以无法直接计算。
		// ***PLAN***: 必须在 BacktestExchange 中添加一个字段来累积总费用。
		// ***临时修复***: 暂时无法精确计算总费用，但在下一次迭代中修复。
	}
	m.TotalFees = be.TotalFees // 直接从 exchange 获取准确的总手续费

	if m.TotalTrades > 0 {
		m.WinRate = float64(m.WinningTrades) / float64(m.TotalTrades) * 100
	}

	if m.WinningTrades > 0 && m.LosingTrades > 0 {
		avgWin := totalProfit / float64(m.WinningTrades)
		avgLoss := math.Abs(totalLoss / float64(m.LosingTrades))
		if avgLoss > 0 {
			m.AvgProfitLoss = avgWin / avgLoss
		}
	}

	// 期末资产详情
	m.EndingCash = be.Cash
	for _, posQty := range be.Positions {
		m.TotalAssetQty += posQty
	}
	m.EndingAssetValue = m.TotalAssetQty * be.CurrentPrice

	// 从权益曲线计算最大回撤
	m.MaxDrawdown = calculateMaxDrawdown(be.EquityCurve) * 100

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
