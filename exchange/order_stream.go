package exchange

// NewOrderUpdate 从各所适配器的同形字段构造统一订单更新。
func NewOrderUpdate(
	orderID int64,
	clientOrderID, symbol, side, typ, status string,
	price, quantity, executedQty, avgPrice float64,
	updateTime int64,
	realizedPNL float64,
	realizedPNLIncremental bool,
) OrderUpdate {
	return OrderUpdate{
		OrderID:                orderID,
		ClientOrderID:          clientOrderID,
		Symbol:                 symbol,
		Side:                   Side(side),
		Type:                   OrderType(typ),
		Status:                 OrderStatus(status),
		Price:                  price,
		Quantity:               quantity,
		ExecutedQty:            executedQty,
		AvgPrice:               avgPrice,
		UpdateTime:             updateTime,
		RealizedPNL:            realizedPNL,
		RealizedPNLIncremental: realizedPNLIncremental,
	}
}
