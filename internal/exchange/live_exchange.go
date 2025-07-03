package exchange

import (
	"binance-grid-bot-go/internal/models"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
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
}

// NewLiveExchange 创建一个新的 LiveExchange 实例，并与服务器同步时间。
func NewLiveExchange(apiKey, secretKey, baseURL, wsBaseURL string, logger *zap.Logger) (*LiveExchange, error) {
	e := &LiveExchange{
		apiKey:     apiKey,
		secretKey:  secretKey,
		baseURL:    baseURL,
		wsBaseURL:  wsBaseURL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		logger:     logger,
	}

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
	// 1. 准备基础 URL 和参数
	fullURL := fmt.Sprintf("%s%s", e.baseURL, endpoint)
	queryParams := url.Values{}
	if params != nil {
		for k, v := range params {
			queryParams[k] = v
		}
	}

	var encodedParams string
	if signed {
		// 2. 对于签名请求，添加时间戳并生成签名
		timestamp := time.Now().UnixMilli() + e.timeOffset
		queryParams.Set("timestamp", fmt.Sprintf("%d", timestamp))

		payloadToSign := queryParams.Encode()
		e.logger.Info("生成签名的Payload", zap.String("payload", payloadToSign))

		signature := e.sign(payloadToSign)
		// 将签名附加到已编码的参数字符串中
		encodedParams = fmt.Sprintf("%s&signature=%s", payloadToSign, signature)
	} else {
		// 对于非签名请求，直接编码
		encodedParams = queryParams.Encode()
	}

	// 3. 根据请求方法创建请求
	var req *http.Request
	var err error

	if method == "GET" {
		finalURL := fullURL
		if encodedParams != "" {
			finalURL = fmt.Sprintf("%s?%s", fullURL, encodedParams)
		}
		e.logger.Info("发送GET请求", zap.String("url", finalURL))
		req, err = http.NewRequest(method, finalURL, nil)
	} else { // POST, PUT, DELETE
		e.logger.Info("发送POST/PUT/DELETE请求", zap.String("body", encodedParams))
		req, err = http.NewRequest(method, fullURL, strings.NewReader(encodedParams))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %v", err)
	}

	// 4. 添加API Key并执行请求
	req.Header.Set("X-MBX-APIKEY", e.apiKey)
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("执行请求失败: %v", err)
	}
	defer resp.Body.Close()

	// 5. 读取和处理响应
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应体失败: %v", err)
	}

	var binanceError models.Error
	if json.Unmarshal(body, &binanceError) == nil && binanceError.Code != 0 {
		return body, &binanceError
	}

	if resp.StatusCode != http.StatusOK {
		// 当API返回非200状态码时，我们将响应体和错误一起返回
		// 以便上层调用者可以记录详细的错误信息。
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

	// 过滤掉没有持仓的条目
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
	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("side", side)
	params.Set("type", orderType)
	params.Set("quantity", fmt.Sprintf("%f", quantity))

	if orderType == "LIMIT" {
		params.Set("timeInForce", "GTC") // Good Till Cancel
		params.Set("price", fmt.Sprintf("%f", price))
	}
	if clientOrderID != "" {
		params.Set("newClientOrderId", clientOrderID)
	}

	data, err := e.doRequest("POST", "/fapi/v1/order", params, true)
	if err != nil {
		// 当 doRequest 返回错误时，第一个返回值是响应体 body，第二个是 error
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
func (e *LiveExchange) CancelOrder(symbol string, orderID int64) error {
	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("orderId", strconv.FormatInt(orderID, 10))
	_, err := e.doRequest("DELETE", "/fapi/v1/order", params, true)
	return err
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

	// 如果错误是币安的特定错误，并且错误码是 -4059 (无需更改), 则忽略该错误
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
	params.Set("marginType", marginType) // "ISOLATED" or "CROSSED"
	_, err := e.doRequest("POST", "/fapi/v1/marginType", params, true)

	// 如果错误是币安的特定错误，并且错误码是 -4046 (No need to change margin type), 则忽略该错误
	if err != nil {
		if binanceErr, ok := err.(*models.Error); ok && binanceErr.Code == -4046 {
			e.logger.Info("保证金模式无需更改，已是目标模式。")
			return nil // 忽略此错误，因为已经是目标状态
		}
		return err // 返回其他所有未处理的错误
	}

	return nil // 没有错误，成功
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

	// 保证金模式是针对交易对的，所以取第一个结果即可。
	// API返回的是小写 (e.g., "cross", "isolated")，配置中是大写，因此需要转换。
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
	_, err := e.doRequest("DELETE", "/fapi/v1/allOpenOrders", params, true)
	return err
}

// GetOrderStatus 获取订单状态。
func (e *LiveExchange) GetOrderStatus(symbol string, orderID int64) (*models.Order, error) {
	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("orderId", strconv.FormatInt(orderID, 10))
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

// GetCurrentTime 返回当前时间。在真实交易中，我们直接返回系统时间。
func (e *LiveExchange) GetCurrentTime() time.Time {
	return time.Now()
}

// GetAccountState 获取账户状态，包括总持仓价值和账户总权益
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

// GetSymbolInfo 获取交易对的交易规则
func (e *LiveExchange) GetSymbolInfo(symbol string) (*models.SymbolInfo, error) {
	params := url.Values{}
	params.Set("symbol", symbol)
	data, err := e.doRequest("GET", "/fapi/v1/exchangeInfo", params, false)
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

// GetOpenOrders 获取所有挂单
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

// GetServerTime 获取服务器时间
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

// GetLastTrade 获取最新成交
func (e *LiveExchange) GetLastTrade(symbol string, orderID int64) (*models.Trade, error) {
	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("limit", "1") // 我们只需要最新的那笔成交
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

// GetMaxWalletExposure 在真实交易中不适用，返回0
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
	params := url.Values{}
	params.Set("listenKey", listenKey)
	_, err := e.doRequest("PUT", "/fapi/v1/listenKey", params, true)
	if err != nil {
		return fmt.Errorf("保持 listenKey 存活失败: %v", err)
	}
	return nil
}

// GetBalance 获取账户中特定资产的余额
func (e *LiveExchange) GetBalance() (float64, error) {
	data, err := e.doRequest("GET", "/fapi/v2/balance", nil, true)
	if err != nil {
		return 0, fmt.Errorf("获取账户余额失败: %v", err)
	}

	var balances []models.Balance
	if err := json.Unmarshal(data, &balances); err != nil {
		return 0, fmt.Errorf("解析余额数据失败: %v", err)
	}

	// 通常我们关心 USDT 的余额作为保证金和计价货币
	for _, b := range balances {
		if b.Asset == "USDT" {
			return strconv.ParseFloat(b.AvailableBalance, 64)
		}
	}

	return 0, fmt.Errorf("未找到 USDT 余额")
}

// ConnectWebSocket 建立到币安用户数据流的 WebSocket 连接
func (e *LiveExchange) ConnectWebSocket(listenKey string) (*websocket.Conn, error) {
	// 正确的 WebSocket URL 格式是 wss://<wsBaseURL>/ws/<listenKey>
	wsURL := fmt.Sprintf("%s/ws/%s", e.wsBaseURL, listenKey)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("无法连接到 WebSocket: %v", err)
	}
	e.wsConn = conn
	return conn, nil
}
