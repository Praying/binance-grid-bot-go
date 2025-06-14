package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// 配置结构体
type Config struct {
	APIKey      string  `json:"api_key"`
	SecretKey   string  `json:"secret_key"`
	Symbol      string  `json:"symbol"`       // 交易对，如 "BTCUSDT"
	GridSpacing float64 `json:"grid_spacing"` // 网格间距比例，0.005 表示 0.5%
	GridValue   float64 `json:"grid_value"`   // 网格交易价值，初始设为7
	Leverage    int     `json:"leverage"`     // 杠杆倍数
	ReturnRate  float64 `json:"return_rate"`  // 预期回归价格比例，0.2 表示 20%
	BaseURL     string  `json:"base_url"`
	WSBaseURL   string  `json:"ws_base_url"`
}

// 账户信息结构体
type AccountInfo struct {
	TotalWalletBalance string `json:"totalWalletBalance"`
	AvailableBalance   string `json:"availableBalance"`
	Assets             []struct {
		Asset                  string `json:"asset"`
		WalletBalance          string `json:"walletBalance"`
		UnrealizedProfit       string `json:"unrealizedProfit"`
		MarginBalance          string `json:"marginBalance"`
		MaintMargin            string `json:"maintMargin"`
		InitialMargin          string `json:"initialMargin"`
		PositionInitialMargin  string `json:"positionInitialMargin"`
		OpenOrderInitialMargin string `json:"openOrderInitialMargin"`
		MaxWithdrawAmount      string `json:"maxWithdrawAmount"`
	} `json:"assets"`
}

// 持仓信息结构体
type Position struct {
	Symbol           string `json:"symbol"`
	PositionAmt      string `json:"positionAmt"`
	EntryPrice       string `json:"entryPrice"`
	MarkPrice        string `json:"markPrice"`
	UnRealizedProfit string `json:"unRealizedProfit"`
	LiquidationPrice string `json:"liquidationPrice"`
	Leverage         string `json:"leverage"`
	MaxNotionalValue string `json:"maxNotionalValue"`
	MarginType       string `json:"marginType"`
	IsolatedMargin   string `json:"isolatedMargin"`
	IsAutoAddMargin  string `json:"isAutoAddMargin"`
	PositionSide     string `json:"positionSide"`
	Notional         string `json:"notional"`
	IsolatedWallet   string `json:"isolatedWallet"`
	UpdateTime       int64  `json:"updateTime"`
}

// 订单信息结构体
type Order struct {
	Symbol        string `json:"symbol"`
	OrderId       int64  `json:"orderId"`
	ClientOrderId string `json:"clientOrderId"`
	Price         string `json:"price"`
	OrigQty       string `json:"origQty"`
	ExecutedQty   string `json:"executedQty"`
	CumQuote      string `json:"cumQuote"`
	Status        string `json:"status"`
	TimeInForce   string `json:"timeInForce"`
	Type          string `json:"type"`
	Side          string `json:"side"`
	StopPrice     string `json:"stopPrice"`
	IcebergQty    string `json:"icebergQty"`
	Time          int64  `json:"time"`
	UpdateTime    int64  `json:"updateTime"`
	IsWorking     bool   `json:"isWorking"`
	WorkingType   string `json:"workingType"`
	OrigType      string `json:"origType"`
	PositionSide  string `json:"positionSide"`
	ActivatePrice string `json:"activatePrice"`
	PriceRate     string `json:"priceRate"`
	ReduceOnly    bool   `json:"reduceOnly"`
	ClosePosition bool   `json:"closePosition"`
	PriceProtect  bool   `json:"priceProtect"`
}

// 网格信息结构体
type GridLevel struct {
	Price    float64 `json:"price"`
	Quantity float64 `json:"quantity"`
	OrderID  int64   `json:"order_id"`
	IsActive bool    `json:"is_active"`
	Side     string  `json:"side"` // "BUY" 或 "SELL"
}

// 网格交易机器人
type GridTradingBot struct {
	config       *Config
	client       *BinanceClient
	wsConn       *websocket.Conn
	gridLevels   []GridLevel
	currentPrice float64
	returnPrice  float64
	isRunning    bool
	mutex        sync.RWMutex
	stopChannel  chan bool
	logger       *log.Logger
}

// 币安API客户端
type BinanceClient struct {
	apiKey    string
	secretKey string
	baseURL   string
	client    *http.Client
}

// 创建新的币安客户端
func NewBinanceClient(apiKey, secretKey, baseURL string) *BinanceClient {
	return &BinanceClient{
		apiKey:    apiKey,
		secretKey: secretKey,
		baseURL:   baseURL,
		client:    &http.Client{Timeout: 30 * time.Second},
	}
}

// 签名函数
func (bc *BinanceClient) sign(query string) string {
	h := hmac.New(sha256.New, []byte(bc.secretKey))
	h.Write([]byte(query))
	return hex.EncodeToString(h.Sum(nil))
}

// 发送请求
func (bc *BinanceClient) doRequest(method, endpoint string, params map[string]string, needSign bool) ([]byte, error) {
	u, _ := url.Parse(bc.baseURL + endpoint)

	if params == nil {
		params = make(map[string]string)
	}

	if needSign {
		params["timestamp"] = strconv.FormatInt(time.Now().UnixMilli(), 10)
		params["recvWindow"] = "5000"
	}

	// 构建查询字符串
	query := url.Values{}
	for k, v := range params {
		query.Set(k, v)
	}
	queryString := query.Encode()

	if needSign {
		signature := bc.sign(queryString)
		queryString += "&signature=" + signature
	}

	if method == "GET" || method == "DELETE" {
		u.RawQuery = queryString
	}

	req, err := http.NewRequest(method, u.String(), strings.NewReader(queryString))
	if err != nil {
		return nil, err
	}

	req.Header.Set("X-MBX-APIKEY", bc.apiKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := bc.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body := make([]byte, 0)
	buf := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			body = append(body, buf[:n]...)
		}
		if err != nil {
			break
		}
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API error: %s", string(body))
	}

	return body, nil
}

// 获取账户信息
func (bc *BinanceClient) GetAccountInfo() (*AccountInfo, error) {
	data, err := bc.doRequest("GET", "/fapi/v2/account", nil, true)
	if err != nil {
		return nil, err
	}

	var account AccountInfo
	err = json.Unmarshal(data, &account)
	return &account, err
}

// 获取持仓信息
func (bc *BinanceClient) GetPositions() ([]Position, error) {
	data, err := bc.doRequest("GET", "/fapi/v2/positionRisk", nil, true)
	if err != nil {
		return nil, err
	}

	var positions []Position
	err = json.Unmarshal(data, &positions)
	return positions, err
}

// 获取当前价格
func (bc *BinanceClient) GetPrice(symbol string) (float64, error) {
	params := map[string]string{"symbol": symbol}
	data, err := bc.doRequest("GET", "/fapi/v1/ticker/price", params, false)
	if err != nil {
		return 0, err
	}

	var priceInfo struct {
		Symbol string `json:"symbol"`
		Price  string `json:"price"`
	}

	err = json.Unmarshal(data, &priceInfo)
	if err != nil {
		return 0, err
	}

	return strconv.ParseFloat(priceInfo.Price, 64)
}

// 下单
func (bc *BinanceClient) PlaceOrder(symbol, side, orderType string, quantity, price float64, positionSide string) (*Order, error) {
	params := map[string]string{
		"symbol":       symbol,
		"side":         side,
		"type":         orderType,
		"quantity":     fmt.Sprintf("%.8f", quantity),
		"positionSide": positionSide,
	}

	if orderType == "LIMIT" {
		params["price"] = fmt.Sprintf("%.8f", price)
		params["timeInForce"] = "GTC"
	}

	data, err := bc.doRequest("POST", "/fapi/v1/order", params, true)
	if err != nil {
		return nil, err
	}

	var order Order
	err = json.Unmarshal(data, &order)
	return &order, err
}

// 取消订单
func (bc *BinanceClient) CancelOrder(symbol string, orderID int64) error {
	params := map[string]string{
		"symbol":  symbol,
		"orderId": strconv.FormatInt(orderID, 10),
	}

	_, err := bc.doRequest("DELETE", "/fapi/v1/order", params, true)
	return err
}

// 设置杠杆
func (bc *BinanceClient) SetLeverage(symbol string, leverage int) error {
	params := map[string]string{
		"symbol":   symbol,
		"leverage": strconv.Itoa(leverage),
	}

	_, err := bc.doRequest("POST", "/fapi/v1/leverage", params, true)
	return err
}

// 创建网格交易机器人
func NewGridTradingBot(config *Config) *GridTradingBot {
	return &GridTradingBot{
		config:      config,
		client:      NewBinanceClient(config.APIKey, config.SecretKey, config.BaseURL),
		gridLevels:  make([]GridLevel, 0),
		isRunning:   false,
		stopChannel: make(chan bool),
		logger:      log.New(log.Writer(), "[GridBot] ", log.LstdFlags),
	}
}

// 初始化网格
func (bot *GridTradingBot) initializeGrid() error {
	bot.mutex.Lock()
	defer bot.mutex.Unlock()

	// 获取当前价格
	currentPrice, err := bot.client.GetPrice(bot.config.Symbol)
	if err != nil {
		return fmt.Errorf("获取当前价格失败: %v", err)
	}

	bot.currentPrice = currentPrice
	bot.returnPrice = currentPrice * (1 + bot.config.ReturnRate)

	bot.logger.Printf("当前价格: %.8f, 回归价格: %.8f", bot.currentPrice, bot.returnPrice)

	// 设置杠杆
	err = bot.client.SetLeverage(bot.config.Symbol, bot.config.Leverage)
	if err != nil {
		bot.logger.Printf("设置杠杆失败: %v", err)
	}

	// 计算网格价位
	bot.calculateGridLevels()

	return nil
}

// 计算网格价位
func (bot *GridTradingBot) calculateGridLevels() {
	bot.gridLevels = make([]GridLevel, 0)

	basePrice := bot.currentPrice
	gridSpacing := bot.config.GridSpacing

	// 创建多个网格档位（买入档位在当前价格下方，卖出档位在当前价格上方）
	for i := 1; i <= 10; i++ {
		// 买入档位（价格下跌时触发）
		buyPrice := basePrice * (1 - float64(i)*gridSpacing)
		buyQuantity := bot.calculateQuantity(buyPrice)

		bot.gridLevels = append(bot.gridLevels, GridLevel{
			Price:    buyPrice,
			Quantity: buyQuantity,
			Side:     "BUY",
			IsActive: false,
		})

		// 卖出档位（价格上涨时触发，仅在有持仓时）
		sellPrice := basePrice * (1 + float64(i)*gridSpacing)
		sellQuantity := bot.calculateQuantity(sellPrice)

		bot.gridLevels = append(bot.gridLevels, GridLevel{
			Price:    sellPrice,
			Quantity: sellQuantity,
			Side:     "SELL",
			IsActive: false,
		})
	}

	// 按价格排序
	sort.Slice(bot.gridLevels, func(i, j int) bool {
		return bot.gridLevels[i].Price < bot.gridLevels[j].Price
	})

	bot.logger.Printf("创建了 %d 个网格档位", len(bot.gridLevels))
}

// 计算订单数量
func (bot *GridTradingBot) calculateQuantity(price float64) float64 {
	// 根据网格交易价值和价格计算数量
	// 这里使用固定的USDT价值，你可以根据需要调整
	usdtValue := bot.config.GridValue
	quantity := usdtValue / price

	// 确保数量满足交易所的最小数量要求
	minQuantity := 0.001 // 例如，BTC的最小数量
	if quantity < minQuantity {
		quantity = minQuantity
	}

	return math.Floor(quantity*100000) / 100000 // 保留5位小数
}

// 执行网格策略
func (bot *GridTradingBot) executeGridStrategy() {
	bot.mutex.Lock()
	defer bot.mutex.Unlock()

	// 检查是否需要调整回归价格
	if bot.currentPrice >= bot.returnPrice {
		// 检查是否有持仓
		positions, err := bot.client.GetPositions()
		if err != nil {
			bot.logger.Printf("获取持仓信息失败: %v", err)
			return
		}

		hasPosition := false
		for _, pos := range positions {
			if pos.Symbol == bot.config.Symbol {
				posAmt, _ := strconv.ParseFloat(pos.PositionAmt, 64)
				if math.Abs(posAmt) > 0.001 {
					hasPosition = true
					break
				}
			}
		}

		if !hasPosition {
			// 重新设置回归价格
			bot.returnPrice = bot.currentPrice * (1 + bot.config.ReturnRate)
			bot.logger.Printf("调整回归价格到: %.8f", bot.returnPrice)

			// 重新计算网格
			bot.calculateGridLevels()
		}
	}

	// 检查是否有网格档位需要触发
	for i := range bot.gridLevels {
		grid := &bot.gridLevels[i]

		if grid.IsActive {
			continue
		}

		shouldTrigger := false
		if grid.Side == "BUY" && bot.currentPrice <= grid.Price {
			shouldTrigger = true
		} else if grid.Side == "SELL" && bot.currentPrice >= grid.Price {
			// 只有在有多头持仓时才执行卖出
			shouldTrigger = bot.hasLongPosition()
		}

		if shouldTrigger {
			bot.placeGridOrder(grid)
		}
	}
}

// 检查是否有多头持仓
func (bot *GridTradingBot) hasLongPosition() bool {
	positions, err := bot.client.GetPositions()
	if err != nil {
		return false
	}

	for _, pos := range positions {
		if pos.Symbol == bot.config.Symbol {
			posAmt, _ := strconv.ParseFloat(pos.PositionAmt, 64)
			if posAmt > 0.001 { // 有多头持仓
				return true
			}
		}
	}
	return false
}

// 下网格订单
func (bot *GridTradingBot) placeGridOrder(grid *GridLevel) {
	order, err := bot.client.PlaceOrder(
		bot.config.Symbol,
		grid.Side,
		"LIMIT",
		grid.Quantity,
		grid.Price,
		"LONG", // 只做多策略
	)

	if err != nil {
		bot.logger.Printf("下单失败 - 价格: %.8f, 方向: %s, 错误: %v",
			grid.Price, grid.Side, err)
		return
	}

	grid.OrderID = order.OrderId
	grid.IsActive = true

	bot.logger.Printf("下单成功 - 订单ID: %d, 价格: %.8f, 数量: %.8f, 方向: %s",
		order.OrderId, grid.Price, grid.Quantity, grid.Side)
}

// 连接WebSocket获取实时价格
func (bot *GridTradingBot) connectWebSocket() error {
	wsURL := fmt.Sprintf("%s/ws/%s@ticker", bot.config.WSBaseURL, strings.ToLower(bot.config.Symbol))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("WebSocket连接失败: %v", err)
	}

	bot.wsConn = conn
	bot.logger.Printf("WebSocket连接成功: %s", wsURL)

	return nil
}

// 处理WebSocket消息
func (bot *GridTradingBot) handleWebSocketMessages() {
	defer bot.wsConn.Close()

	for bot.isRunning {
		_, message, err := bot.wsConn.ReadMessage()
		if err != nil {
			bot.logger.Printf("WebSocket读取消息失败: %v", err)
			break
		}

		var ticker struct {
			Symbol string `json:"s"`
			Price  string `json:"c"`
		}

		if err := json.Unmarshal(message, &ticker); err != nil {
			continue
		}

		price, err := strconv.ParseFloat(ticker.Price, 64)
		if err != nil {
			continue
		}

		bot.mutex.Lock()
		bot.currentPrice = price
		bot.mutex.Unlock()

		// 执行网格策略
		bot.executeGridStrategy()
	}
}

// 启动机器人
func (bot *GridTradingBot) Start() error {
	bot.logger.Println("正在启动网格交易机器人...")

	// 初始化网格
	if err := bot.initializeGrid(); err != nil {
		return fmt.Errorf("初始化网格失败: %v", err)
	}

	// 连接WebSocket
	if err := bot.connectWebSocket(); err != nil {
		return fmt.Errorf("连接WebSocket失败: %v", err)
	}

	bot.isRunning = true

	// 启动WebSocket消息处理
	go bot.handleWebSocketMessages()

	// 启动状态监控
	go bot.monitorStatus()

	bot.logger.Println("网格交易机器人启动成功")

	return nil
}

// 停止机器人
func (bot *GridTradingBot) Stop() {
	bot.logger.Println("正在停止网格交易机器人...")

	bot.isRunning = false

	if bot.wsConn != nil {
		bot.wsConn.Close()
	}

	// 取消所有活跃订单
	bot.cancelAllActiveOrders()

	bot.logger.Println("网格交易机器人已停止")
}

// 取消所有活跃订单
func (bot *GridTradingBot) cancelAllActiveOrders() {
	bot.mutex.Lock()
	defer bot.mutex.Unlock()

	for i := range bot.gridLevels {
		grid := &bot.gridLevels[i]
		if grid.IsActive && grid.OrderID > 0 {
			err := bot.client.CancelOrder(bot.config.Symbol, grid.OrderID)
			if err != nil {
				bot.logger.Printf("取消订单失败 - 订单ID: %d, 错误: %v", grid.OrderID, err)
			} else {
				bot.logger.Printf("取消订单成功 - 订单ID: %d", grid.OrderID)
				grid.IsActive = false
				grid.OrderID = 0
			}
		}
	}
}

// 监控状态
func (bot *GridTradingBot) monitorStatus() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for bot.isRunning {
		select {
		case <-ticker.C:
			bot.printStatus()
		case <-bot.stopChannel:
			return
		}
	}
}

// 打印状态
func (bot *GridTradingBot) printStatus() {
	bot.mutex.RLock()
	defer bot.mutex.RUnlock()

	// 获取账户信息
	account, err := bot.client.GetAccountInfo()
	if err != nil {
		bot.logger.Printf("获取账户信息失败: %v", err)
		return
	}

	// 获取持仓信息
	positions, err := bot.client.GetPositions()
	if err != nil {
		bot.logger.Printf("获取持仓信息失败: %v", err)
		return
	}

	bot.logger.Println("=== 网格交易状态 ===")
	bot.logger.Printf("当前价格: %.8f", bot.currentPrice)
	bot.logger.Printf("回归价格: %.8f", bot.returnPrice)
	bot.logger.Printf("总余额: %s USDT", account.TotalWalletBalance)

	// 打印持仓信息
	for _, pos := range positions {
		if pos.Symbol == bot.config.Symbol {
			posAmt, _ := strconv.ParseFloat(pos.PositionAmt, 64)
			if math.Abs(posAmt) > 0.001 {
				unrealizedPnl, _ := strconv.ParseFloat(pos.UnRealizedProfit, 64)
				liquidationPrice, _ := strconv.ParseFloat(pos.LiquidationPrice, 64)

				bot.logger.Printf("持仓: %.8f, 未实现盈亏: %.4f, 强平价格: %.8f",
					posAmt, unrealizedPnl, liquidationPrice)

				// 计算爆仓距离
				if liquidationPrice > 0 {
					distance := math.Abs(bot.currentPrice-liquidationPrice) / bot.currentPrice * 100
					bot.logger.Printf("爆仓距离: %.2f%%", distance)
				}
			}
		}
	}

	// 统计活跃订单
	activeOrders := 0
	for _, grid := range bot.gridLevels {
		if grid.IsActive {
			activeOrders++
		}
	}
	bot.logger.Printf("活跃网格订单: %d/%d", activeOrders, len(bot.gridLevels))
	bot.logger.Println("==================")
}

// 主函数
func main() {
	// 配置参数
	config := &Config{
		APIKey:      "your_api_key_here",         // 请替换为你的API Key
		SecretKey:   "your_secret_key_here",      // 请替换为你的Secret Key
		Symbol:      "BTCUSDT",                   // 交易对
		GridSpacing: 0.005,                       // 网格间距 0.5%
		GridValue:   7.0,                         // 网格交易价值
		Leverage:    10,                          // 杠杆倍数
		ReturnRate:  0.2,                         // 回归价格比例 20%
		BaseURL:     "https://fapi.binance.com",  // 币安期货API地址
		WSBaseURL:   "wss://fstream.binance.com", // WebSocket地址
	}

	// 创建网格交易机器人
	bot := NewGridTradingBot(config)

	// 启动机器人
	if err := bot.Start(); err != nil {
		log.Fatalf("启动机器人失败: %v", err)
	}

	// 等待中断信号
	select {}
}
