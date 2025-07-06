package exchange

import (
	"binance-grid-bot-go/internal/models"
	"binance-grid-bot-go/internal/storage"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// ExchangeStatus 定义了交易所的状态
type ExchangeStatus int32

const (
	// StatusInitializing 初始状态
	StatusInitializing ExchangeStatus = iota
	// StatusRecovering 正在从重启中恢复订单
	StatusRecovering
	// StatusRunning 正常运行中
	StatusRunning
	// StatusStopped 已停止
	StatusStopped
)

// LiveExchange 实现了 Exchange 接口，用于与真实的币安交易所进行交互。
type LiveExchange struct {
	apiKey     string
	secretKey  string
	baseURL    string
	wsBaseURL  string
	httpClient *http.Client
	logger     *zap.Logger
	mu         sync.Mutex
	wsConn     *websocket.Conn
	listenKey  string
	timeOffset int64
	status     atomic.Value // 使用 atomic.Value 来保证并发安全
	db         *sql.DB      // 数据库连接
}

// NewLiveExchange 创建一个新的 LiveExchange 实例，并与服务器同步时间。
func NewLiveExchange(apiKey, secretKey, baseURL, wsBaseURL string, logger *zap.Logger, db *sql.DB) (*LiveExchange, error) {
	e := &LiveExchange{
		apiKey:     apiKey,
		secretKey:  secretKey,
		baseURL:    baseURL,
		wsBaseURL:  wsBaseURL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		logger:     logger,
		db:         db,
	}
	e.status.Store(StatusInitializing)

	if err := e.syncTime(); err != nil {
		return nil, fmt.Errorf("与币安服务器同步时间失败: %v", err)
	}

	return e, nil
}

// syncTime 与币安服务器同步时间，计算时间偏移。
func (e *LiveExchange) syncTime() error {
	serverTime, err := e.GetServerTime()
	if err != nil {
		return err
	}
	localTime := time.Now().UnixMilli()
	e.timeOffset = serverTime - localTime
	e.logger.Info("与币安服务器时间同步完成", zap.Int64("timeOffset (ms)", e.timeOffset))
	return nil
}

// doRequest 是一个通用的请求处理函数，用于向币安API发送请求。
func (e *LiveExchange) doRequest(method, endpoint string, params url.Values, signed bool) ([]byte, error) {
	fullURL := fmt.Sprintf("%s%s", e.baseURL, endpoint)
	queryParams := url.Values{}
	if params != nil {
		for k, v := range params {
			queryParams[k] = v
		}
	}

	var encodedParams string
	if signed {
		timestamp := time.Now().UnixMilli() + e.timeOffset
		queryParams.Set("timestamp", fmt.Sprintf("%d", timestamp))

		payloadToSign := queryParams.Encode()
		signature := e.sign(payloadToSign)
		encodedParams = fmt.Sprintf("%s&signature=%s", payloadToSign, signature)
	} else {
		encodedParams = queryParams.Encode()
	}

	var req *http.Request
	var err error

	if method == "GET" {
		finalURL := fullURL
		if encodedParams != "" {
			finalURL = fmt.Sprintf("%s?%s", fullURL, encodedParams)
		}
		req, err = http.NewRequest(method, finalURL, nil)
	} else { // POST, PUT, DELETE
		req, err = http.NewRequest(method, fullURL, strings.NewReader(encodedParams))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %v", err)
	}

	req.Header.Set("X-MBX-APIKEY", e.apiKey)
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("执行请求失败: %v", err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应体失败: %v", err)
	}

	var binanceError models.Error
	if json.Unmarshal(body, &binanceError) == nil && binanceError.Code != 0 {
		if binanceError.Code == 200 {
			// This is a success response, continue as if no error
		} else {
			return body, &binanceError
		}
	}

	if resp.StatusCode != http.StatusOK {
		return body, fmt.Errorf("API请求失败, 状态码: %d, 响应: %s", resp.StatusCode, string(body))
	}

	return body, nil
}

// sign 对请求参数进行签名。
func (e *LiveExchange) sign(data string) string {
	h := hmac.New(sha256.New, []byte(e.secretKey))
	h.Write([]byte(data))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// --- Exchange 接口实现 ---

// GetPrice 获取指定交易对的当前价格。
func (e *LiveExchange) GetPrice(symbol string) (float64, error) {
	params := url.Values{}
	params.Set("symbol", symbol)
	data, err := e.doRequest("GET", "/fapi/v1/ticker/price", params, false)
	if err != nil {
		return 0, err
	}

	var ticker struct {
		Price string `json:"price"`
	}
	if err := json.Unmarshal(data, &ticker); err != nil {
		return 0, err
	}

	return strconv.ParseFloat(ticker.Price, 64)
}

// GetPositions 获取指定交易对的持仓信息。
func (e *LiveExchange) GetPositions(symbol string) ([]models.Position, error) {
	params := url.Values{}
	params.Set("symbol", symbol)
	data, err := e.doRequest("GET", "/fapi/v2/positionRisk", params, true)
	if err != nil {
		return nil, err
	}

	var positions []models.Position
	if err := json.Unmarshal(data, &positions); err != nil {
		return nil, err
	}

	var activePositions []models.Position
	for _, p := range positions {
		posAmt, _ := strconv.ParseFloat(p.PositionAmt, 64)
		if posAmt != 0 {
			activePositions = append(activePositions, p)
		}
	}

	return activePositions, nil
}

// PlaceOrder 下单。
func (e *LiveExchange) PlaceOrder(symbol, side, orderType string, quantity, price float64, clientOrderID string) (*models.Order, error) {
	if e.status.Load() != StatusRunning {
		return nil, fmt.Errorf("交易所未处于运行状态，当前状态: %v", e.status.Load())
	}
	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("side", side)
	params.Set("type", orderType)
	params.Set("quantity", fmt.Sprintf("%f", quantity))

	if orderType == "LIMIT" {
		params.Set("timeInForce", "GTC")
		params.Set("price", fmt.Sprintf("%f", price))
	}
	if clientOrderID != "" {
		params.Set("newClientOrderId", clientOrderID)
	}

	data, err := e.doRequest("POST", "/fapi/v1/order", params, true)
	if err != nil {
		e.logger.Error("下单请求失败，交易所返回错误", zap.Error(err), zap.String("raw_response", string(data)))
		return nil, err
	}

	var order models.Order
	if err := json.Unmarshal(data, &order); err != nil {
		return nil, err
	}

	return &order, nil
}

// CancelOrder 取消订单。
func (e *LiveExchange) CancelOrder(symbol string, orderID int64, clientOrderID string) (*models.Order, error) {
	currentStatus := e.status.Load().(ExchangeStatus)
	if currentStatus != StatusRunning && currentStatus != StatusRecovering {
		return nil, fmt.Errorf("交易所未处于可执行取消操作的状态，当前状态: %v", currentStatus)
	}
	params := url.Values{}
	params.Set("symbol", symbol)
	if orderID > 0 {
		params.Set("orderId", strconv.FormatInt(orderID, 10))
	} else if clientOrderID != "" {
		params.Set("origClientOrderId", clientOrderID)
	} else {
		return nil, fmt.Errorf("取消订单需要 orderID 或 clientOrderID")
	}
	data, err := e.doRequest("DELETE", "/fapi/v1/order", params, true)
	if err != nil {
		return nil, err
	}
	var order models.Order
	if err := json.Unmarshal(data, &order); err != nil {
		return nil, err
	}
	return &order, nil
}

// SetLeverage 设置杠杆。
func (e *LiveExchange) SetLeverage(symbol string, leverage int) error {
	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("leverage", strconv.Itoa(leverage))
	_, err := e.doRequest("POST", "/fapi/v1/leverage", params, true)
	return err
}

// SetPositionMode 设置持仓模式。
func (e *LiveExchange) SetPositionMode(isHedgeMode bool) error {
	params := url.Values{}
	params.Set("dualSidePosition", fmt.Sprintf("%v", isHedgeMode))
	_, err := e.doRequest("POST", "/fapi/v1/positionSide/dual", params, true)

	if err != nil {
		if binanceErr, ok := err.(*models.Error); ok && binanceErr.Code == -4059 {
			e.logger.Info("持仓模式无需更改，已是目标模式。")
			return nil
		}
		return err
	}
	return nil
}

// GetPositionMode 获取当前持仓模式。
func (e *LiveExchange) GetPositionMode() (bool, error) {
	data, err := e.doRequest("GET", "/fapi/v1/positionSide/dual", nil, true)
	if err != nil {
		return false, fmt.Errorf("获取持仓模式失败: %v", err)
	}

	var result struct {
		DualSidePosition bool `json:"dualSidePosition"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return false, fmt.Errorf("解析持仓模式响应失败: %v", err)
	}

	return result.DualSidePosition, nil
}

// SetMarginType 设置保证金模式。
func (e *LiveExchange) SetMarginType(symbol string, marginType string) error {
	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("marginType", marginType)
	_, err := e.doRequest("POST", "/fapi/v1/marginType", params, true)

	if err != nil {
		if binanceErr, ok := err.(*models.Error); ok && binanceErr.Code == -4046 {
			e.logger.Info("保证金模式无需更改，已是目标模式。")
			return nil
		}
		return err
	}

	return nil
}

// GetMarginType 获取指定交易对的保证金模式。
func (e *LiveExchange) GetMarginType(symbol string) (string, error) {
	params := url.Values{}
	params.Set("symbol", symbol)
	data, err := e.doRequest("GET", "/fapi/v2/positionRisk", params, true)
	if err != nil {
		return "", fmt.Errorf("获取持仓风险信息以确定保证金模式失败: %v", err)
	}

	var positions []models.Position
	if err := json.Unmarshal(data, &positions); err != nil {
		return "", fmt.Errorf("解析持仓风险响应失败: %v", err)
	}

	if len(positions) == 0 {
		return "", fmt.Errorf("API未返回交易对 %s 的持仓风险信息", symbol)
	}

	return strings.ToUpper(positions[0].MarginType), nil
}

// GetAccountInfo 获取账户信息。
func (e *LiveExchange) GetAccountInfo() (*models.AccountInfo, error) {
	data, err := e.doRequest("GET", "/fapi/v2/account", nil, true)
	if err != nil {
		return nil, fmt.Errorf("获取账户信息失败: %v", err)
	}

	var accInfo models.AccountInfo
	if err := json.Unmarshal(data, &accInfo); err != nil {
		return nil, fmt.Errorf("解析账户信息失败: %v", err)
	}
	return &accInfo, nil
}

// CancelAllOpenOrders 取消所有挂单。
func (e *LiveExchange) CancelAllOpenOrders(symbol string) error {
	params := url.Values{}
	params.Set("symbol", symbol)
	body, err := e.doRequest("DELETE", "/fapi/v1/allOpenOrders", params, true)

	if err != nil {
		e.logger.Error("取消所有挂单失败", zap.Error(err), zap.String("response", string(body)))
		return err
	}

	e.logger.Info("成功取消所有挂单（或无挂单需要取消）。", zap.String("symbol", symbol))
	return nil
}

// GetOrderStatus 获取订单状态。
func (e *LiveExchange) GetOrderStatus(symbol string, orderID int64, clientOrderID string) (*models.Order, error) {
	params := url.Values{}
	params.Set("symbol", symbol)
	if orderID > 0 {
		params.Set("orderId", strconv.FormatInt(orderID, 10))
	} else if clientOrderID != "" {
		params.Set("origClientOrderId", clientOrderID)
	} else {
		return nil, fmt.Errorf("查询订单状态需要 orderID 或 clientOrderID")
	}
	data, err := e.doRequest("GET", "/fapi/v1/order", params, true)
	if err != nil {
		return nil, err
	}

	var order models.Order
	if err := json.Unmarshal(data, &order); err != nil {
		return nil, err
	}
	return &order, nil
}

// GetCurrentTime 返回当前时间。
func (e *LiveExchange) GetCurrentTime() time.Time {
	return time.Now()
}

// GetAccountState 获取账户状态。
func (e *LiveExchange) GetAccountState(symbol string) (positionValue float64, accountEquity float64, err error) {
	accInfo, err := e.GetAccountInfo()
	if err != nil {
		return 0, 0, fmt.Errorf("获取账户状态失败: %v", err)
	}

	equity, err := strconv.ParseFloat(accInfo.TotalWalletBalance, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("解析账户总权益失败: %v", err)
	}

	positions, err := e.GetPositions(symbol)
	if err != nil {
		return 0, 0, fmt.Errorf("获取持仓信息失败: %v", err)
	}

	var totalPositionValue float64
	for _, pos := range positions {
		notional, _ := strconv.ParseFloat(pos.Notional, 64)
		totalPositionValue += notional
	}

	return totalPositionValue, equity, nil
}

// GetSymbolInfo 获取交易对的交易规则。
func (e *LiveExchange) GetSymbolInfo(symbol string) (*models.SymbolInfo, error) {
	data, err := e.doRequest("GET", "/fapi/v1/exchangeInfo", nil, false)
	if err != nil {
		return nil, err
	}

	var exchangeInfo models.ExchangeInfo
	if err := json.Unmarshal(data, &exchangeInfo); err != nil {
		return nil, err
	}

	for _, s := range exchangeInfo.Symbols {
		if s.Symbol == symbol {
			return &s, nil
		}
	}

	return nil, fmt.Errorf("未找到交易对 %s 的信息", symbol)
}

// GetOpenOrders 获取所有挂单。
func (e *LiveExchange) GetOpenOrders(symbol string) ([]models.Order, error) {
	params := url.Values{}
	params.Set("symbol", symbol)
	data, err := e.doRequest("GET", "/fapi/v1/openOrders", params, true)
	if err != nil {
		return nil, err
	}

	var openOrders []models.Order
	if err := json.Unmarshal(data, &openOrders); err != nil {
		return nil, err
	}
	return openOrders, nil
}

// GetServerTime 获取服务器时间。
func (e *LiveExchange) GetServerTime() (int64, error) {
	data, err := e.doRequest("GET", "/fapi/v1/time", nil, false)
	if err != nil {
		return 0, err
	}
	var serverTime struct {
		ServerTime int64 `json:"serverTime"`
	}
	if err := json.Unmarshal(data, &serverTime); err != nil {
		return 0, err
	}
	return serverTime.ServerTime, nil
}

// GetLastTrade 获取最新成交。
func (e *LiveExchange) GetLastTrade(symbol string, orderID int64) (*models.Trade, error) {
	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("limit", "1")
	data, err := e.doRequest("GET", "/fapi/v1/userTrades", params, true)
	if err != nil {
		return nil, err
	}

	var trades []models.Trade
	if err := json.Unmarshal(data, &trades); err != nil {
		return nil, err
	}

	if len(trades) > 0 {
		return &trades[0], nil
	}

	return nil, fmt.Errorf("未找到订单 %d 的成交记录", orderID)
}

// GetMaxWalletExposure 在真实交易中不适用。
func (e *LiveExchange) GetMaxWalletExposure() float64 {
	return 0
}

// CreateListenKey 创建一个新的 listenKey 用于 WebSocket 连接。
func (e *LiveExchange) CreateListenKey() (string, error) {
	data, err := e.doRequest("POST", "/fapi/v1/listenKey", nil, true)
	if err != nil {
		return "", fmt.Errorf("创建 listenKey 失败: %v", err)
	}

	var response struct {
		ListenKey string `json:"listenKey"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return "", fmt.Errorf("解析 listenKey 响应失败: %v", err)
	}
	e.listenKey = response.ListenKey
	return e.listenKey, nil
}

// KeepAliveListenKey 延长 listenKey 的有效期。
func (e *LiveExchange) KeepAliveListenKey(listenKey string) error {
	_, err := e.doRequest("PUT", "/fapi/v1/listenKey", nil, true)
	return err
}

// GetBalance 获取账户余额。
func (e *LiveExchange) GetBalance() (float64, error) {
	info, err := e.GetAccountInfo()
	if err != nil {
		return 0, err
	}
	balance, err := strconv.ParseFloat(info.TotalWalletBalance, 64)
	if err != nil {
		return 0, err
	}
	return balance, nil
}

// Recover 实现了订单恢复逻辑。
func (e *LiveExchange) Recover(symbol string) error {
	e.status.Store(StatusRecovering)
	e.logger.Info("开始执行订单恢复流程...")

	// Step 1: 从数据库加载本地活跃订单
	localOrders, err := storage.GetActiveOrders(e.db, symbol)
	if err != nil {
		e.status.Store(StatusStopped)
		return fmt.Errorf("恢复流程失败：无法从数据库加载本地订单: %w", err)
	}
	localOrderMap := make(map[string]*models.Order)
	for i := range localOrders {
		localOrderMap[localOrders[i].ClientOrderId] = &localOrders[i]
	}

	// Step 2: 从交易所获取所有开放订单
	exchangeOrders, err := e.GetOpenOrders(symbol)
	if err != nil {
		e.status.Store(StatusStopped)
		return fmt.Errorf("恢复流程失败：无法从交易所获取开放订单: %w", err)
	}

	// Step 3: 订单双向核对与同步
	for _, exchangeOrder := range exchangeOrders {
		if _, exists := localOrderMap[exchangeOrder.ClientOrderId]; exists {
			// 情况A: 订单双边匹配
			e.logger.Info("恢复流程：匹配到订单", zap.String("clientOrderId", exchangeOrder.ClientOrderId))
			delete(localOrderMap, exchangeOrder.ClientOrderId)
		} else {
			// 情况C: 交易所存在，本地不存在 (孤儿订单)
			e.logger.Warn("恢复流程：发现孤儿订单，将立即取消", zap.String("clientOrderId", exchangeOrder.ClientOrderId), zap.Int64("exchangeOrderId", exchangeOrder.OrderId))
			_, cancelErr := e.CancelOrder(symbol, 0, exchangeOrder.ClientOrderId)
			if cancelErr != nil {
				e.logger.Error("恢复流程：取消孤儿订单失败", zap.Error(cancelErr))
			}
		}
	}

	// 情况B: 本地存在，交易所不存在
	for clientOrderID, localOrder := range localOrderMap {
		e.logger.Info("恢复流程：本地订单在交易所不存在，查询最终状态...", zap.String("clientOrderId", clientOrderID))
		finalOrder, queryErr := e.GetOrderStatus(symbol, localOrder.OrderId, clientOrderID)
		if queryErr != nil {
			e.logger.Error("恢复流程：查询订单最终状态失败", zap.Error(queryErr), zap.String("clientOrderId", clientOrderID))
			continue
		}
		if err := storage.UpdateOrder(e.db, finalOrder); err != nil {
			e.logger.Error("恢复流程：更新订单到数据库失败", zap.Error(err), zap.String("clientOrderId", clientOrderID))
		}
	}

	e.logger.Info("订单恢复流程完成。")
	e.status.Store(StatusRunning)
	return nil
}

// ConnectWebSocket 建立与币安 WebSocket API 的连接。
func (e *LiveExchange) ConnectWebSocket(listenKey string) (*websocket.Conn, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.wsConn != nil {
		e.logger.Info("WebSocket connection already established.")
		return e.wsConn, nil
	}

	wsURL := fmt.Sprintf("%s/ws/%s", e.wsBaseURL, listenKey)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		e.logger.Error("Failed to connect to WebSocket", zap.Error(err), zap.String("url", wsURL))
		return nil, fmt.Errorf("failed to dial websocket: %w", err)
	}

	e.wsConn = conn
	e.listenKey = listenKey
	e.logger.Info("WebSocket connection established successfully.")

	// Start a goroutine to handle ping/pong and keep the connection alive
	go e.handleWebSocketConnection()

	return e.wsConn, nil
}

// handleWebSocketConnection 负责处理 WebSocket 连接的生命周期，包括 ping/pong 和自动重连。
func (e *LiveExchange) handleWebSocketConnection() {
	defer func() {
		e.mu.Lock()
		if e.wsConn != nil {
			e.wsConn.Close()
			e.wsConn = nil
		}
		e.mu.Unlock()
		e.logger.Info("WebSocket connection closed.")
	}()

	// Set a read deadline to detect dead connections
	e.wsConn.SetReadDeadline(time.Now().Add(60 * time.Second))
	e.wsConn.SetPongHandler(func(string) error {
		e.wsConn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	// Periodically send pings to the server
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			e.mu.Lock()
			if e.wsConn == nil {
				e.mu.Unlock()
				return
			}
			// Send ping message
			if err := e.wsConn.WriteMessage(websocket.PingMessage, nil); err != nil {
				e.logger.Error("Error sending ping message", zap.Error(err))
				e.mu.Unlock()
				return
			}
			e.mu.Unlock()
		}
	}
}

// --- State Persistence Methods ---

// SaveState persists the bot's state into the database.
func (e *LiveExchange) SaveState(state *models.BotState) error {
	return storage.SaveBotState(e.db, state)
}

// LoadState retrieves the bot's state from the database.
func (e *LiveExchange) LoadState() (*models.BotState, error) {
	return storage.LoadBotState(e.db)
}

// ClearState removes the bot's state from the database.
func (e *LiveExchange) ClearState() error {
	return storage.ClearBotState(e.db)
}

// GetNextCycleID retrieves and increments the persistent cycle counter.
func (e *LiveExchange) GetNextCycleID() (int64, error) {
	return storage.GetNextCycleID(e.db)
}
