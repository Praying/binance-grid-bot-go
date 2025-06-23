package main

import (
	"binance-grid-bot-go/internal/bot"
	"binance-grid-bot-go/internal/config"
	"binance-grid-bot-go/internal/downloader"
	"binance-grid-bot-go/internal/exchange"
	"binance-grid-bot-go/internal/logger" // 新增 logger 包
	"binance-grid-bot-go/internal/models"
	"binance-grid-bot-go/internal/reporter"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"go.uber.org/zap"
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

	// --- 初始化日志 (提前) ---
	// 为了在加载.env或配置时就能记录日志，我们需要先于其他逻辑初始化一个临时的或默认的logger
	// 这里我们假设InitLogger可以被安全地提前调用
	logger.InitLogger(models.LogConfig{Level: "info", Output: "console"}) // 使用一个默认配置

	// --- 加载 .env 文件 ---
	err := godotenv.Load()
	if err != nil {
		logger.S().Info("未找到 .env 文件，将从系统环境变量中读取。")
	} else {
		logger.S().Info("成功从 .env 文件加载配置。")
	}

	// --- 加载 JSON 配置 ---
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		logger.S().Fatalf("无法加载配置文件: %v", err)
	}

	// --- 使用文件中的配置重新初始化日志 ---
	logger.InitLogger(cfg.LogConfig)
	defer logger.S().Sync() // 确保在main函数退出时刷新所有缓冲的日志

	// --- 根据模式执行 ---
	switch *mode {
	case "live":
		runLiveMode(cfg)
	case "backtest":
		finalDataPath, err := handleBacktestMode(*symbol, *startDate, *endDate, *dataPath)
		if err != nil {
			logger.S().Fatal(err)
		}
		runBacktestMode(cfg, finalDataPath)
	default:
		logger.S().Fatalf("未知的运行模式: %s。请选择 'live' 或 'backtest'。", *mode)
	}
}

// handleBacktestMode 处理回测模式的启动逻辑，包括数据下载。
// 成功后返回数据文件路径，失败则返回错误。
func handleBacktestMode(symbol, startDate, endDate, dataPath string) (string, error) {
	// 检查是否需要下载数据
	shouldDownload := symbol != "" && startDate != "" && endDate != ""

	if shouldDownload {
		startTime, err1 := time.Parse("2006-01-02", startDate)
		endTime, err2 := time.Parse("2006-01-02", endDate)
		if err1 != nil || err2 != nil {
			return "", fmt.Errorf("日期格式错误，请使用 YYYY-MM-DD 格式。start: %v, end: %v", err1, err2)
		}

		// 确保数据目录存在
		if _, err := os.Stat("data"); os.IsNotExist(err) {
			if err := os.Mkdir("data", 0755); err != nil {
				return "", fmt.Errorf("创建 data 目录失败: %v", err)
			}
		}

		downloader := downloader.NewKlineDownloader()
		fileName := fmt.Sprintf("data/%s-%s-%s.csv", symbol, startDate, endDate)
		logger.S().Infof("开始下载 %s 从 %s 到 %s 的K线数据...", symbol, startDate, endDate)

		if err := downloader.DownloadKlines(symbol, fileName, startTime, endTime); err != nil {
			return "", fmt.Errorf("下载数据失败: %v", err)
		}
		return fileName, nil // 返回下载好的文件路径
	}

	// 如果不下载，则必须提供数据路径
	if dataPath == "" {
		return "", fmt.Errorf("回测模式需要通过 --data 或 --symbol/start/end 参数指定数据源")
	}
	return dataPath, nil
}

// runLiveMode 运行实时交易机器人
func runLiveMode(cfg *models.Config) {
	logger.S().Info("--- 启动实时交易模式 ---")
	stateFilePath := "grid_state.json"

	// 从环境变量加载API密钥
	apiKey := os.Getenv("BINANCE_API_KEY")
	secretKey := os.Getenv("BINANCE_SECRET_KEY")
	if apiKey == "" || secretKey == "" {
		logger.S().Fatal("错误：BINANCE_API_KEY 和 BINANCE_SECRET_KEY 环境变量必须被设置。")
	}

	// 根据配置设置API URL
	var baseURL, wsBaseURL string
	if cfg.IsTestnet {
		baseURL = cfg.TestnetAPIURL
		wsBaseURL = cfg.TestnetWSURL
		logger.S().Info("正在使用币安测试网...")
	} else {
		baseURL = cfg.LiveAPIURL
		wsBaseURL = cfg.LiveWSURL
		logger.S().Info("正在使用币安生产网...")
	}
	cfg.BaseURL = baseURL
	cfg.WSBaseURL = wsBaseURL

	// 初始化交易所
	liveExchange, err := exchange.NewLiveExchange(apiKey, secretKey, cfg.BaseURL)
	if err != nil {
		logger.S().Fatalf("初始化交易所失败: %v", err)
	}
	defer liveExchange.Close() // 确保在函数退出时关闭后台任务

	// 初始化机器人
	gridBot := bot.NewGridTradingBot(cfg, liveExchange, false)

	// 加载状态
	if err := gridBot.LoadState(stateFilePath); err != nil {
		logger.S().Warnf("无法加载状态: %v，将以全新状态启动。", err)
	}

	// 启动机器人
	if err := gridBot.Start(); err != nil {
		logger.S().Fatalf("机器人启动失败: %v", err)
	}

	// 等待中断信号以实现优雅退出
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	// 停止机器人并保存状态
	gridBot.Stop()
	if err := gridBot.SaveState(stateFilePath); err != nil {
		logger.S().Fatalf("保存状态失败: %v", err)
	}
	logger.S().Info("机器人已成功停止，状态已保存。")
}

// runBacktestMode 运行回测模式
func runBacktestMode(cfg *models.Config, dataPath string) {
	logger.S().Info("--- 启动回测模式 ---")
	cfg.WSBaseURL = "ws://localhost" // 在回测中，我们不需要真实的ws连接

	// 从数据路径中提取 symbol，并用它来覆盖 config 中的值
	backtestSymbol := extractSymbolFromPath(dataPath)
	if backtestSymbol == "" {
		logger.S().Fatalf("无法从数据文件路径 %s 中提取交易对", dataPath)
	}
	cfg.Symbol = backtestSymbol // 确保机器人逻辑也使用正确的 symbol

	// 使用新的构造函数，并传入完整的 config
	backtestExchange := exchange.NewBacktestExchange(cfg)
	gridBot := bot.NewGridTradingBot(cfg, backtestExchange, true)

	// 加载并处理历史数据
	file, err := os.Open(dataPath)
	if err != nil {
		logger.S().Fatalf("无法打开历史数据文件: %v", err)
	}
	defer file.Close()

	// --- 重构数据读取以捕获时间 ---
	records, err := csv.NewReader(file).ReadAll()
	if err != nil {
		logger.S().Fatalf("无法读取所有CSV记录: %v", err)
	}
	if len(records) <= 1 { // 至少需要表头和一行数据
		logger.S().Fatal("历史数据文件为空或只有表头。")
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
	initialOpen, errO := strconv.ParseFloat(initialRecord[1], 64)
	initialHigh, errH := strconv.ParseFloat(initialRecord[2], 64)
	initialLow, errL := strconv.ParseFloat(initialRecord[3], 64)
	initialClose, errC := strconv.ParseFloat(initialRecord[4], 64)
	if errO != nil || errH != nil || errL != nil || errC != nil {
		logger.S().With(
			zap.Error(errO),
			zap.Error(errH),
			zap.Error(errL),
			zap.Error(errC),
		).Fatal("无法解析初始价格")
	}

	backtestExchange.SetPrice(initialOpen, initialHigh, initialLow, initialClose, initialTime)
	gridBot.SetCurrentPrice(initialClose)
	if err := gridBot.StartForBacktest(); err != nil {
		logger.S().Fatalf("回测机器人初始化失败: %v", err)
	}
	logger.S().Infof("使用初始价格 %.2f 完成机器人初始化。\n", initialClose)

	// --- 循环处理所有数据点 ---
	logger.S().Info("开始回测...")
	for _, record := range records {
		// 检查是否爆仓或进入暂停状态
		if backtestExchange.IsLiquidated() {
			logger.S().Warn("检测到爆仓，提前终止回测循环。")
			break
		}
		if gridBot.IsHalted() {
			logger.S().Info("机器人已暂停，提前终止回测循环。")
			break
		}

		timestampMs, errT := strconv.ParseInt(record[0], 10, 64)
		openPrice, errO := strconv.ParseFloat(record[1], 64)
		high, errH := strconv.ParseFloat(record[2], 64)
		low, errL := strconv.ParseFloat(record[3], 64)
		closePrice, errC := strconv.ParseFloat(record[4], 64)
		if errT != nil || errO != nil || errH != nil || errL != nil || errC != nil {
			logger.S().Warnf("无法解析K线数据，跳过此条记录: %v", record)
			continue
		}
		timestamp := time.UnixMilli(timestampMs)
		backtestExchange.SetPrice(openPrice, high, low, closePrice, timestamp)
		gridBot.ProcessBacktestTick()
	}

	logger.S().Info("回测结束。")

	// --- 生成并打印回测报告 ---
	reporter.GenerateReport(backtestExchange, dataPath, startTime, endTime)
}
