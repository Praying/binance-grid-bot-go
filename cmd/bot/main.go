package main

import (
	"binance-grid-bot-go/internal/bot"
	"binance-grid-bot-go/internal/config"
	"binance-grid-bot-go/internal/downloader"
	"binance-grid-bot-go/internal/exchange"
	"binance-grid-bot-go/internal/models"
	"binance-grid-bot-go/internal/reporter"
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// extractSymbolFromPath 从数据文件路径中提取交易对名称
// 例如: "data/BNBUSDT-2025-03-15-2025-06-15.csv" -> "BNBUSDT"
func extractSymbolFromPath(path string) string {
	// 移除目录和 .csv 后缀
	name := strings.TrimSuffix(path, ".csv")
	parts := strings.Split(name, "/")
	fileName := parts[len(parts)-1]

	// 按 "-" 分割并取第一部分
	symbolParts := strings.Split(fileName, "-")
	if len(symbolParts) > 0 {
		return symbolParts[0]
	}
	return ""
}

func main() {
	// --- 命令行参数定义 ---
	configPath := flag.String("config", "config.json", "path to the config file")
	mode := flag.String("mode", "live", "running mode: live or backtest")
	dataPath := flag.String("data", "", "path to historical data file for backtesting")
	symbol := flag.String("symbol", "", "symbol to backtest (e.g., BNBUSDT)")
	startDate := flag.String("start", "", "start date for backtesting (YYYY-MM-DD)")
	endDate := flag.String("end", "", "end date for backtesting (YYYY-MM-DD)")
	flag.Parse()

	// --- 加载配置 ---
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("无法加载配置文件: %v", err)
	}

	// --- 根据模式执行 ---
	if *mode == "live" {
		runLiveMode(cfg)
	} else if *mode == "backtest" {
		// 自动下载数据逻辑
		if *symbol != "" && *startDate != "" && *endDate != "" {
			startTime, err1 := time.Parse("2006-01-02", *startDate)
			endTime, err2 := time.Parse("2006-01-02", *endDate)
			if err1 != nil || err2 != nil {
				log.Fatal("日期格式错误，请使用 YYYY-MM-DD 格式。")
			}

			downloader := downloader.NewKlineDownloader()
			fileName := fmt.Sprintf("data/%s-%s-%s.csv", *symbol, *startDate, *endDate)
			log.Printf("开始下载 %s 从 %s 到 %s 的K线数据...", *symbol, *startDate, *endDate)

			if err := downloader.DownloadKlines(*symbol, fileName, startTime, endTime); err != nil {
				log.Fatalf("下载数据失败: %v", err)
			}
			*dataPath = fileName // 将下载的文件路径设置为回测数据路径
		}

		if *dataPath == "" {
			log.Fatal("回测模式需要通过 --data 或 --symbol/start/end 参数指定数据源。")
		}
		runBacktestMode(cfg, *dataPath)
	} else {
		log.Fatalf("未知的运行模式: %s。请选择 'live' 或 'backtest'。", *mode)
	}
}

// runLiveMode 运行实时交易机器人
func runLiveMode(cfg *models.Config) {
	log.Println("--- 启动实时交易模式 ---")
	stateFilePath := "grid_state.json"

	// 根据配置设置API URL
	var baseURL, wsBaseURL string
	if cfg.IsTestnet {
		baseURL = "https://testnet.binancefuture.com"
		wsBaseURL = "wss://stream.binancefuture.com"
		log.Println("正在使用币安测试网...")
	} else {
		baseURL = "https://fapi.binance.com"
		wsBaseURL = "wss://fstream.binance.com"
		log.Println("正在使用币安生产网...")
	}
	cfg.BaseURL = baseURL
	cfg.WSBaseURL = wsBaseURL

	// 初始化交易所
	liveExchange := exchange.NewLiveExchange(cfg.APIKey, cfg.SecretKey, cfg.BaseURL)

	// 初始化机器人
	gridBot := bot.NewGridTradingBot(cfg, liveExchange, false)

	// 加载状态
	if err := gridBot.LoadState(stateFilePath); err != nil {
		log.Printf("无法加载状态: %v，将以全新状态启动。", err)
	}

	// 启动机器人
	if err := gridBot.Start(); err != nil {
		log.Fatalf("机器人启动失败: %v", err)
	}

	// 等待中断信号以实现优雅退出
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	// 停止机器人并保存状态
	gridBot.Stop()
	if err := gridBot.SaveState(stateFilePath); err != nil {
		log.Fatalf("保存状态失败: %v", err)
	}
	log.Println("机器人已成功停止，状态已保存。")
}

// runBacktestMode 运行回测模式
func runBacktestMode(cfg *models.Config, dataPath string) {
	log.Println("--- 启动回测模式 ---")
	cfg.WSBaseURL = "ws://localhost" // 在回测中，我们不需要真实的ws连接

	initialBalance := 10000.0
	// 从数据路径中提取 symbol，并用它来覆盖 config 中的值
	backtestSymbol := extractSymbolFromPath(dataPath)
	if backtestSymbol == "" {
		log.Fatalf("无法从数据文件路径 %s 中提取交易对", dataPath)
	}
	cfg.Symbol = backtestSymbol // 确保机器人逻辑也使用正确的 symbol

	backtestExchange := exchange.NewBacktestExchange(backtestSymbol, initialBalance, cfg.Leverage)
	gridBot := bot.NewGridTradingBot(cfg, backtestExchange, true)

	// 加载并处理历史数据
	file, err := os.Open(dataPath)
	if err != nil {
		log.Fatalf("无法打开历史数据文件: %v", err)
	}
	defer file.Close()

	// --- 重构数据读取以捕获时间 ---
	records, err := csv.NewReader(file).ReadAll()
	if err != nil {
		log.Fatalf("无法读取所有CSV记录: %v", err)
	}
	if len(records) <= 1 { // 至少需要表头和一行数据
		log.Fatal("历史数据文件为空或只有表头。")
	}

	// 移除表头
	records = records[1:]

	// 解析开始和结束时间
	startTimeMs, _ := strconv.ParseInt(records[0][0], 10, 64)
	endTimeMs, _ := strconv.ParseInt(records[len(records)-1][0], 10, 64)
	startTime := time.UnixMilli(startTimeMs)
	endTime := time.UnixMilli(endTimeMs)

	// --- 使用第一行数据进行初始化 ---
	initialRecord := records[0]
	initialTimeMs, _ := strconv.ParseInt(initialRecord[0], 10, 64)
	initialTime := time.UnixMilli(initialTimeMs)
	initialHigh, errH := strconv.ParseFloat(initialRecord[2], 64)
	initialLow, errL := strconv.ParseFloat(initialRecord[3], 64)
	initialClose, errC := strconv.ParseFloat(initialRecord[4], 64)
	if errH != nil || errL != nil || errC != nil {
		log.Fatalf("无法解析初始价格: high=%v, low=%v, close=%v", errH, errL, errC)
	}

	backtestExchange.SetPrice(initialHigh, initialLow, initialClose, initialTime)
	gridBot.SetCurrentPrice(initialClose)
	if err := gridBot.StartForBacktest(); err != nil {
		log.Fatalf("回测机器人初始化失败: %v", err)
	}
	log.Printf("使用初始价格 %.2f 完成机器人初始化。\n", initialClose)

	// --- 循环处理所有数据点 ---
	log.Println("开始回测...")
	for _, record := range records {
		// 在每次循环开始时检查是否已爆仓
		if backtestExchange.IsLiquidated() {
			log.Println("检测到爆仓，提前终止回测循环。")
			break
		}

		timestampMs, errT := strconv.ParseInt(record[0], 10, 64)
		high, errH := strconv.ParseFloat(record[2], 64)
		low, errL := strconv.ParseFloat(record[3], 64)
		closePrice, errC := strconv.ParseFloat(record[4], 64)
		if errT != nil || errH != nil || errL != nil || errC != nil {
			log.Printf("无法解析K线数据，跳过此条记录: %v", record)
			continue
		}
		timestamp := time.UnixMilli(timestampMs)
		backtestExchange.SetPrice(high, low, closePrice, timestamp)
		gridBot.ProcessBacktestTick()
	}

	log.Println("回测结束。")

	// --- 生成并打印回测报告 ---
	reporter.GenerateReport(backtestExchange, dataPath, startTime, endTime)
}
