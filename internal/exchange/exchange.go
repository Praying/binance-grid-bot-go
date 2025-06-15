package exchange

import "binance-grid-bot-go/internal/models"

// Exchange 定义了所有交易所实现必须提供的通用方法。
// 这使得交易机器人可以在真实交易和回测之间轻松切换。
type Exchange interface {
	GetPrice(symbol string) (float64, error)
	GetPositions(symbol string) ([]models.Position, error)
	PlaceOrder(symbol, side, orderType string, quantity, price float64, positionSide string) (*models.Order, error)
	CancelOrder(symbol string, orderID int64) error
	SetLeverage(symbol string, leverage int) error
	GetAccountInfo() (*models.AccountInfo, error)
	CancelAllOpenOrders(symbol string) error
	GetOrderStatus(symbol string, orderID int64) (*models.Order, error)
}
