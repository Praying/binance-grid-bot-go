package exchange

import (
	"binance-grid-bot-go/internal/logger"
	"binance-grid-bot-go/internal/models"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	logMutex = &sync.Mutex{}
)

// LiveExchange 实现了 Exchange 接口，用于与币安的真实API进行交互。
type LiveExchange struct {
	apiKey          string
	secretKey       string
	baseURL         string
	client          *http.Client
	timeOffset      int64 // 服务器时间与本地时间的毫秒级差值
	timeOffsetMutex sync.RWMutex
	stopChan        chan struct{}
}

// NewLiveExchange 创建一个新的 LiveExchange 实例。
func NewLiveExchange(apiKey, secretKey, baseURL string) (*LiveExchange, error) {
	// 强制使用 HTTP/1.1，禁用 HTTP/2
	transport := &http.Transport{
		ForceAttemptHTTP2: false,
		TLSNextProto:      make(map[string]func(string, *tls.Conn) http.RoundTripper),
	}

	e := &LiveExchange{
		apiKey:    apiKey,
		secretKey: secretKey,
		baseURL:   baseURL,
		client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		},
		stopChan: make(chan struct{}),
	}

	// 首次同步时间，如果失败则无法启动
	if err := e.syncTime(); err != nil {
		return nil, fmt.Errorf("首次时间同步失败: %v", err)
	}

	// 启动后台 goroutine 定期同步时间
	go e.runTimeSyncer()

	return e, nil
}

// Close 优雅地关闭 LiveExchange，停止后台任务。
func (e *LiveExchange) Close() {
	close(e.stopChan)
}

// runTimeSyncer 启动一个 Ticker 来定期调用 syncTime。
func (e *LiveExchange) runTimeSyncer() {
	// 每 1 分钟同步一次时间，以更频繁地应对时钟漂移
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := e.syncTime(); err != nil {
				// 在后台只记录错误，不中断程序
				logger.S().Warnf("[TimeSync] 后台时间同步失败: %v", err)
			}
		case <-e.stopChan:
			return
		}
	}
}

// syncTime 与服务器同步时间并更新时间偏移量。
func (e *LiveExchange) syncTime() error {
	serverTime, err := e.GetServerTime()
	if err != nil {
		return err
	}
	localTime := time.Now().UnixMilli()
	offset := serverTime - localTime

	e.timeOffsetMutex.Lock()
	e.timeOffset = offset
	e.timeOffsetMutex.Unlock()

	logger.S().Debugf("[TimeSync] 时间同步完成。本地与服务器时间差: %d ms", offset)
	return nil
}

// getSynchronizedTimestamp 获取经过校准的时间戳。
func (e *LiveExchange) getSynchronizedTimestamp() int64 {
	e.timeOffsetMutex.RLock()
	offset := e.timeOffset
	e.timeOffsetMutex.RUnlock()
	// 只返回校准后的时间，不再添加持续增长的nonce
	return time.Now().UnixMilli() + offset
}

// sign 签名函数
func (e *LiveExchange) sign(query string) string {
	h := hmac.New(sha256.New, []byte(e.secretKey))
	h.Write([]byte(query))
	return hex.EncodeToString(h.Sum(nil))
}

// doRequest 发送请求
func (e *LiveExchange) doRequest(method, endpoint string, params map[string]string, needSign bool) ([]byte, error) {
	u, _ := url.Parse(e.baseURL + endpoint)

	if params == nil {
		params = make(map[string]string)
	}

	// 将签名和非签名参数分开处理
	queryParams := url.Values{}
	bodyParams := url.Values{}

	if needSign {
		// 通过原子增加一个nonce，确保即使在同一毫秒内发起多个请求，时间戳也是唯一的
		// 这是解决-1007超时的关键，因为币安服务器可能会拒绝时间戳完全相同的多个请求
		params["timestamp"] = strconv.FormatInt(e.getSynchronizedTimestamp(), 10)
		params["recvWindow"] = "5000"
	}

	// 填充参数
	for k, v := range params {
		if method == "POST" || method == "PUT" {
			bodyParams.Set(k, v)
		} else { // GET, DELETE
			queryParams.Set(k, v)
		}
	}

	// --- 核心逻辑修正: 签名应该只针对将要发送的数据 ---
	var dataToSign string
	if method == "POST" || method == "PUT" {
		dataToSign = bodyParams.Encode()
	} else {
		dataToSign = queryParams.Encode()
	}

	var finalQueryString string
	var finalBody *strings.Reader

	if needSign {
		signature := e.sign(dataToSign)
		if method == "POST" || method == "PUT" {
			// 对于POST/PUT，签名加在body里
			bodyParams.Set("signature", signature)
			finalBody = strings.NewReader(bodyParams.Encode())
			finalQueryString = "" // POST/PUT 的 query string 为空
		} else {
			// 对于GET/DELETE，签名加在query string里
			queryParams.Set("signature", signature)
			finalBody = nil
			finalQueryString = queryParams.Encode()
		}
	} else {
		// 非签名请求
		finalQueryString = queryParams.Encode()
		finalBody = strings.NewReader(bodyParams.Encode())
	}

	if finalQueryString != "" {
		u.RawQuery = finalQueryString
	}

	var bodyForRequest io.Reader
	if finalBody != nil {
		bodyForRequest = finalBody
	}

	req, err := http.NewRequest(method, u.String(), bodyForRequest)
	if err != nil {
		return nil, err
	}

	req.Header.Set("X-MBX-APIKEY", e.apiKey)
	if method == "POST" || method == "PUT" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	// --- 同步调试日志 ---
	// 使用 Debug 级别的日志记录详细的请求信息，避免在生产环境中刷屏
	logger.S().Debugw("Sending API Request",
		"method", method,
		"url", u.String(),
		"needs_sign", needSign,
		"data_to_sign", dataToSign,
	)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API error: %s", string(body))
	}

	return body, nil
}

// GetAccountInfo 获取账户信息
func (e *LiveExchange) GetAccountInfo() (*models.AccountInfo, error) {
	data, err := e.doRequest("GET", "/fapi/v2/account", nil, true)
	if err != nil {
		return nil, err
	}

	var account models.AccountInfo
	err = json.Unmarshal(data, &account)
	return &account, err
}

// GetPositions 获取指定交易对的持仓信息
func (e *LiveExchange) GetPositions(symbol string) ([]models.Position, error) {
	params := map[string]string{"symbol": symbol}
	data, err := e.doRequest("GET", "/fapi/v2/positionRisk", params, true)
	if err != nil {
		return nil, err
	}

	var positions []models.Position
	err = json.Unmarshal(data, &positions)
	return positions, err
}

// GetPrice 获取当前价格
func (e *LiveExchange) GetPrice(symbol string) (float64, error) {
	params := map[string]string{"symbol": symbol}
	data, err := e.doRequest("GET", "/fapi/v1/ticker/price", params, false)
	if err != nil {
		return 0, err
	}

	var priceInfo struct {
		Price string `json:"price"`
	}
	err = json.Unmarshal(data, &priceInfo)
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(priceInfo.Price, 64)
}

// PlaceOrder 下单
func (e *LiveExchange) PlaceOrder(symbol, side, orderType string, quantity, price float64, clientOrderID string) (*models.Order, error) {
	params := map[string]string{
		"symbol":           symbol,
		"side":             side,
		"type":             orderType,
		"quantity":         strconv.FormatFloat(quantity, 'f', -1, 64),
		"newClientOrderId": clientOrderID,
	}

	if orderType == "LIMIT" {
		params["price"] = strconv.FormatFloat(price, 'f', -1, 64)
		params["timeInForce"] = "GTC"
	}

	data, err := e.doRequest("POST", "/fapi/v1/order", params, true)
	if err != nil {
		return nil, err
	}

	var order models.Order
	err = json.Unmarshal(data, &order)
	return &order, err
}

// CancelOrder 取消订单
func (e *LiveExchange) CancelOrder(symbol string, orderID int64) error {
	params := map[string]string{
		"symbol":  symbol,
		"orderId": strconv.FormatInt(orderID, 10),
	}
	_, err := e.doRequest("DELETE", "/fapi/v1/order", params, true)
	return err
}

// SetLeverage 设置杠杆
func (e *LiveExchange) SetLeverage(symbol string, leverage int) error {
	params := map[string]string{
		"symbol":   symbol,
		"leverage": strconv.Itoa(leverage),
	}
	_, err := e.doRequest("POST", "/fapi/v1/leverage", params, true)
	return err
}

// SetMarginType 设置指定交易对的保证金模式
func (e *LiveExchange) SetMarginType(symbol string, marginType string) error {
	params := map[string]string{
		"symbol":     symbol,
		"marginType": marginType,
	}
	_, err := e.doRequest("POST", "/fapi/v1/marginType", params, true)
	// 忽略 "No need to change margin type" 错误
	if err != nil && strings.Contains(err.Error(), "-4046") {
		logger.S().Infof("保证金模式已经是 %s，无需更改。", marginType)
		return nil
	}
	return err
}

// SetPositionMode 设置持仓模式（单向/双向）
func (e *LiveExchange) SetPositionMode(isHedgeMode bool) error {
	params := map[string]string{
		"dualSidePosition": strconv.FormatBool(isHedgeMode),
	}
	_, err := e.doRequest("POST", "/fapi/v1/positionSide/dual", params, true)
	// 忽略 "No need to change position side" 错误
	if err != nil && strings.Contains(err.Error(), "-4059") {
		logger.S().Infof("持仓模式无需更改。")
		return nil
	}
	return err
}

// CancelAllOpenOrders 取消指定交易对的所有挂单
func (e *LiveExchange) CancelAllOpenOrders(symbol string) error {
	params := map[string]string{
		"symbol": symbol,
	}
	_, err := e.doRequest("DELETE", "/fapi/v1/allOpenOrders", params, true)
	return err
}

// GetOrderStatus 获取特定订单的状态
func (e *LiveExchange) GetOrderStatus(symbol string, orderID int64) (*models.Order, error) {
	params := map[string]string{
		"symbol":  symbol,
		"orderId": strconv.FormatInt(orderID, 10),
	}
	data, err := e.doRequest("GET", "/fapi/v1/order", params, true)
	if err != nil {
		return nil, err
	}

	var order models.Order
	err = json.Unmarshal(data, &order)
	return &order, err
}

func (e *LiveExchange) GetCurrentTime() time.Time {
	return time.Now()
}

// GetAccountState 获取实盘账户的状态，包括总持仓价值和账户总权益
func (e *LiveExchange) GetAccountState(symbol string) (positionValue float64, accountEquity float64, err error) {
	// 1. 获取持仓信息以得到持仓名义价值
	positions, err := e.GetPositions(symbol)
	if err != nil {
		return 0, 0, fmt.Errorf("获取持仓信息失败: %v", err)
	}

	// 在我们的单交易对场景下，我们只关心一个持仓
	if len(positions) > 0 {
		// 注意：'notional' 通常是带符号的，正数代表多头，负数代表空头。我们取绝对值。
		notionalValue, err := strconv.ParseFloat(positions[0].Notional, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("解析持仓名义价值失败: %v", err)
		}
		if notionalValue < 0 {
			notionalValue = -notionalValue
		}
		positionValue = notionalValue
	}

	// 2. 获取账户信息以得到总钱包余额
	accountInfo, err := e.GetAccountInfo()
	if err != nil {
		return 0, 0, fmt.Errorf("获取账户信息失败: %v", err)
	}

	totalBalance, err := strconv.ParseFloat(accountInfo.TotalWalletBalance, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("解析总钱包余额失败: %v", err)
	}
	accountEquity = totalBalance

	return positionValue, accountEquity, nil
}

// GetSymbolInfo 获取指定交易对的交易规则
func (e *LiveExchange) GetSymbolInfo(symbol string) (*models.SymbolInfo, error) {
	params := map[string]string{}
	// 如果提供了交易对，就只获取该交易对的信息以提高效率
	if symbol != "" {
		params["symbol"] = symbol
	}
	data, err := e.doRequest("GET", "/fapi/v1/exchangeInfo", params, false)
	if err != nil {
		return nil, err
	}

	var exchangeInfo models.ExchangeInfo
	if err := json.Unmarshal(data, &exchangeInfo); err != nil {
		return nil, fmt.Errorf("无法解析 aexchange info: %v", err)
	}

	for _, s := range exchangeInfo.Symbols {
		if s.Symbol == symbol {
			return &s, nil
		}
	}

	return nil, fmt.Errorf("未找到交易对 %s 的信息", symbol)
}

// GetOpenOrders 获取指定交易对的所有当前挂单
func (e *LiveExchange) GetOpenOrders(symbol string) ([]models.Order, error) {
	params := map[string]string{
		"symbol": symbol,
	}
	data, err := e.doRequest("GET", "/fapi/v1/openOrders", params, true)
	if err != nil {
		return nil, err
	}

	var orders []models.Order
	err = json.Unmarshal(data, &orders)
	return orders, err
}

// GetServerTime 获取币安服务器时间
func (e *LiveExchange) GetServerTime() (int64, error) {
	data, err := e.doRequest("GET", "/fapi/v1/time", nil, false)
	if err != nil {
		return 0, err
	}

	var serverTime struct {
		ServerTime int64 `json:"serverTime"`
	}
	err = json.Unmarshal(data, &serverTime)
	if err != nil {
		return 0, err
	}
	return serverTime.ServerTime, nil
}

// GetMaxWalletExposure 在实盘模式下不适用，返回 0.0 以满足接口要求。
func (e *LiveExchange) GetMaxWalletExposure() float64 {
	return 0.0
}

// GetLastTrade 获取指定订单的最新成交详情
func (e *LiveExchange) GetLastTrade(symbol string, orderID int64) (*models.Trade, error) {
	params := map[string]string{
		"symbol":  symbol,
		"orderId": strconv.FormatInt(orderID, 10),
		"limit":   "1", // 我们只需要最新的成交记录
	}

	data, err := e.doRequest("GET", "/fapi/v1/userTrades", params, true)
	if err != nil {
		return nil, fmt.Errorf("获取成交记录失败: %v", err)
	}

	var trades []models.Trade
	if err := json.Unmarshal(data, &trades); err != nil {
		return nil, fmt.Errorf("解析成交记录失败: %v", err)
	}

	if len(trades) == 0 {
		return nil, fmt.Errorf("未找到订单 %d 的成交记录", orderID)
	}

	// 币安有时会返回与订单无关的成交，我们需要验证一下
	// 但在这个场景下，既然是按orderId查询，可以信赖返回结果
	return &trades[0], nil
}
