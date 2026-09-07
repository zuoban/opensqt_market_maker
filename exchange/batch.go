package exchange

import "context"

const (
	// BinanceUSDMPPlaceOrderBatchSize 是 USD-M 永续 POST /fapi/v1/batchOrders 的官方上限。
	BinanceUSDMPPlaceOrderBatchSize = 5
)

// PlaceOrderBatchItem 是批量下单中与请求下标对齐的单笔结果。
type PlaceOrderBatchItem struct {
	Order *Order
	Err   error
}

// PlaceOrderBatcher 是可选的原生批量下单能力。未实现时执行器回退为逐笔 PlaceOrder。
type PlaceOrderBatcher interface {
	PlaceOrderBatchSize() int
	PlaceOrderBatch(ctx context.Context, orders []*OrderRequest) ([]PlaceOrderBatchItem, error)
}
