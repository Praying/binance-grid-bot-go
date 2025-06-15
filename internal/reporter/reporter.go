package reporter

import (
	"binance-grid-bot-go/internal/exchange"
	"log"
	"math"
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
	StartTime        time.Time
	EndTime          time.Time
}

// GenerateReport 根据回测交易所的状态计算并打印性能报告
func GenerateReport(backtestExchange *exchange.BacktestExchange, dataPath string, startTime, endTime time.Time) {
	metrics, symbol := calculateMetrics(backtestExchange)
	metrics.StartTime = startTime
	metrics.EndTime = endTime

	log.Println("========== 回测结果报告 ==========")
	log.Printf("数据文件:         %s", dataPath)
	log.Printf("交易对:           %s", symbol)
	log.Printf("回测周期:         %s 到 %s", metrics.StartTime.Format("2006-01-02 15:04"), metrics.EndTime.Format("2006-01-02 15:04"))
	log.Printf("------------------------------------")
	log.Printf("初始资金:         %.2f USDT", metrics.InitialBalance)
	log.Printf("最终资金:         %.2f USDT", metrics.FinalBalance)
	log.Printf("总利润:           %.2f USDT", metrics.TotalProfit)
	log.Printf("收益率:           %.2f%%", metrics.ProfitPercentage)
	log.Printf("------------------------------------")
	log.Printf("总交易次数:       %d", metrics.TotalTrades)
	log.Printf("盈利次数:         %d", metrics.WinningTrades)
	log.Printf("亏损次数:         %d", metrics.LosingTrades)
	log.Printf("胜率:             %.2f%%", metrics.WinRate)
	log.Printf("平均盈亏比:       %.2f", metrics.AvgProfitLoss)
	log.Printf("最大回撤:         %.2f%%", metrics.MaxDrawdown)
	log.Printf("夏普比率:         %.2f (暂未实现)", metrics.SharpeRatio)
	log.Printf("--- 期末资产分析 ---")
	log.Printf("期末现金:         %.2f USDT", metrics.EndingCash)
	log.Printf("期末持仓市值:     %.2f USDT (共 %.4f %s)", metrics.EndingAssetValue, metrics.TotalAssetQty, symbol)
	log.Println("===================================")
}

func calculateMetrics(be *exchange.BacktestExchange) (*Metrics, string) {
	m := &Metrics{}
	var symbol string

	// 从持仓或交易日志中动态推断交易对
	for s := range be.Positions {
		symbol = s
		break
	}
	if symbol == "" && len(be.TradeLog) > 0 {
		symbol = be.TradeLog[0].Symbol
	}

	m.InitialBalance = be.InitialBalance
	// 最终资金现在从更精确的来源计算
	// m.FinalBalance = be.EquityCurve[len(be.EquityCurve)-1]
	m.TotalTrades = len(be.TradeLog)

	var totalProfit, totalLoss float64
	for _, trade := range be.TradeLog {
		if trade.Profit > 0 {
			m.WinningTrades++
			totalProfit += trade.Profit
		} else {
			m.LosingTrades++
			totalLoss += trade.Profit
		}
	}

	if m.TotalTrades > 0 {
		m.WinRate = float64(m.WinningTrades) / float64(m.TotalTrades) * 100
	}
	if m.LosingTrades > 0 && m.WinningTrades > 0 {
		avgWin := totalProfit / float64(m.WinningTrades)
		avgLoss := math.Abs(totalLoss / float64(m.LosingTrades))
		m.AvgProfitLoss = avgWin / avgLoss
	}

	// 计算期末资产详情
	m.EndingCash = be.Cash
	for _, posQty := range be.Positions {
		m.TotalAssetQty += posQty
	}
	m.EndingAssetValue = m.TotalAssetQty * be.CurrentPrice
	m.FinalBalance = m.EndingCash + m.EndingAssetValue

	// 基于更精确的FinalBalance重新计算总利润和收益率
	m.TotalProfit = m.FinalBalance - m.InitialBalance
	if m.InitialBalance != 0 {
		m.ProfitPercentage = (m.TotalProfit / m.InitialBalance) * 100
	}

	m.MaxDrawdown = calculateMaxDrawdown(be.EquityCurve) * 100

	return m, symbol
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
