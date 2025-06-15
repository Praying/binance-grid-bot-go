package exchange

import (
	"binance-grid-bot-go/internal/models"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// LiveExchange 实现了 Exchange 接口，用于与币安的真实API进行交互。
type LiveExchange struct {
	apiKey    string
	secretKey string
	baseURL   string
	client    *http.Client
}

// NewLiveExchange 创建一个新的 LiveExchange 实例。
func NewLiveExchange(apiKey, secretKey, baseURL string) *LiveExchange {
	return &LiveExchange{
		apiKey:    apiKey,
		secretKey: secretKey,
		baseURL:   baseURL,
		client:    &http.Client{Timeout: 30 * time.Second},
	}
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

	if needSign {
		params["timestamp"] = strconv.FormatInt(time.Now().UnixMilli(), 10)
		params["recvWindow"] = "5000"
	}

	query := url.Values{}
	for k, v := range params {
		query.Set(k, v)
	}
	queryString := query.Encode()

	if needSign {
		signature := e.sign(queryString)
		queryString += "&signature=" + signature
	}

	var req *http.Request
	var err error

	if method == "GET" || method == "DELETE" {
		u.RawQuery = queryString
		req, err = http.NewRequest(method, u.String(), nil)
	} else {
		req, err = http.NewRequest(method, u.String(), strings.NewReader(queryString))
	}

	if err != nil {
		return nil, err
	}

	req.Header.Set("X-MBX-APIKEY", e.apiKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

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
func (e *LiveExchange) PlaceOrder(symbol, side, orderType string, quantity, price float64, positionSide string) (*models.Order, error) {
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
