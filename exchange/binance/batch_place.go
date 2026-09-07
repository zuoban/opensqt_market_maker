package binance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"opensqt/exchange/exchangeerr"
	"opensqt/logger"
	"opensqt/utils"

	"github.com/adshao/go-binance/v2/common"
	"github.com/adshao/go-binance/v2/futures"
)

const maxPlaceOrderBatchSize = 5

type PlaceOrderBatchItem struct {
	Order *Order
	Err   error
}

type preparedOrder struct {
	index      int
	req        *OrderRequest
	normalized normalizedOrder
	clientOID  string
	createSvc  *futures.CreateOrderService
}

type unknownBatchResolution struct {
	item  preparedOrder
	cause error
}

func (b *BinanceAdapter) PlaceOrderBatchSize() int {
	return maxPlaceOrderBatchSize
}

func (b *BinanceAdapter) PlaceOrderBatch(ctx context.Context, orders []*OrderRequest) ([]PlaceOrderBatchItem, error) {
	results := make([]PlaceOrderBatchItem, len(orders))
	if len(orders) == 0 {
		return results, nil
	}
	for start := 0; start < len(orders); start += maxPlaceOrderBatchSize {
		if err := ctx.Err(); err != nil {
			rejectUnsentBatchResults(results, start, exchangeerr.WrapOrderPlacementRejected(err))
			return results, nil
		}
		end := start + maxPlaceOrderBatchSize
		if end > len(orders) {
			end = len(orders)
		}
		if err := b.placeOrderBatchChunk(ctx, orders[start:end], results[start:end]); err != nil {
			rejectUnsentBatchResults(results, start, exchangeerr.WrapOrderPlacementRejected(err))
			return results, nil
		}
		if batchChunkHasUnknown(results[start:end]) {
			rejectUnsentBatchResults(results, end, exchangeerr.WrapOrderPlacementRejected(
				fmt.Errorf("前一批下单结果未知，停止后续批量提交")))
			return results, nil
		}
	}
	return results, nil
}

func (b *BinanceAdapter) placeOrderBatchChunk(ctx context.Context, orders []*OrderRequest, results []PlaceOrderBatchItem) error {
	if b.contractSpec == nil {
		err := exchangeerr.WrapOrderPlacementRejected(fmt.Errorf("Binance 合约规格未初始化"))
		for i := range results {
			results[i].Err = err
		}
		return nil
	}

	prepared := make([]preparedOrder, 0, len(orders))
	for i, req := range orders {
		if req == nil {
			results[i].Err = exchangeerr.WrapOrderPlacementRejected(fmt.Errorf("订单请求不能为空"))
			continue
		}
		normalized, err := b.contractSpec.normalizeOrder(req)
		if err != nil {
			results[i].Err = exchangeerr.WrapOrderPlacementRejected(fmt.Errorf("Binance 订单参数无效: %w", err))
			continue
		}
		timeInForce := futures.TimeInForceTypeGTC
		if req.PostOnly {
			timeInForce = futures.TimeInForceTypeGTX
		}
		if req.ClientOrderID == "" {
			results[i].Err = exchangeerr.WrapOrderPlacementRejected(fmt.Errorf("Binance 批量下单缺少 clientOrderID"))
			continue
		}
		clientOID := utils.AddBinanceBrokerPrefix(req.ClientOrderID)
		svc := b.client.NewCreateOrderService().
			Symbol(b.contractSpec.Symbol).
			Side(futures.SideType(req.Side)).
			Type(futures.OrderTypeLimit).
			TimeInForce(timeInForce).
			Quantity(normalized.QuantityText).
			Price(normalized.PriceText)
		if clientOID != "" {
			svc = svc.NewClientOrderID(clientOID)
		}
		if req.ReduceOnly {
			svc = svc.ReduceOnly(true)
		}
		prepared = append(prepared, preparedOrder{
			index:      i,
			req:        req,
			normalized: normalized,
			clientOID:  clientOID,
			createSvc:  svc,
		})
	}
	if len(prepared) == 0 {
		return nil
	}

	b.ensureStatusAwareTransport()
	services := make([]*futures.CreateOrderService, len(prepared))
	for i, item := range prepared {
		services[i] = item.createSvc
	}
	resp, createErr := b.client.NewCreateBatchOrdersService().OrderList(services).Do(ctx)
	if createErr != nil {
		b.resolveUnknownBatchChunk(ctx, prepared, results, createErr)
		return nil
	}
	if resp == nil || resp.N != len(prepared) {
		b.resolveUnknownBatchChunk(ctx, prepared, results, fmt.Errorf("Binance 批量下单响应条数=%d，期望 %d",
			batchResponseCount(resp), len(prepared)))
		return nil
	}

	successIdx := 0
	unknownItems := make([]unknownBatchResolution, 0, len(prepared))
	for i, item := range prepared {
		itemErr := resp.Errors[i]
		if itemErr != nil {
			results[item.index].Err = b.mapBatchItemError(itemErr, item.clientOID)
			if errors.Is(results[item.index].Err, exchangeerr.ErrOrderPlacementUnknown) && item.clientOID != "" {
				unknownItems = append(unknownItems, unknownBatchResolution{
					item:  item,
					cause: results[item.index].Err,
				})
			}
			continue
		}
		if successIdx >= len(resp.Orders) || resp.Orders[successIdx] == nil {
			unknownItems = append(unknownItems, unknownBatchResolution{
				item:  item,
				cause: fmt.Errorf("Binance 批量下单缺少第 %d 笔成功订单", i+1),
			})
			continue
		}
		sdkOrder := resp.Orders[successIdx]
		successIdx++
		if err := validateBatchAcceptedOrder(sdkOrder, b.contractSpec.Symbol, item.clientOID); err != nil {
			unknownItems = append(unknownItems, unknownBatchResolution{item: item, cause: err})
			continue
		}
		createdAt := time.Now()
		if sdkOrder.Time > 0 {
			createdAt = time.UnixMilli(sdkOrder.Time)
		}
		results[item.index] = PlaceOrderBatchItem{
			Order: overlayNormalizedOrder(&Order{
				OrderID:       sdkOrder.OrderID,
				ClientOrderID: sdkOrder.ClientOrderID,
				Symbol:        b.contractSpec.Symbol,
				Side:          item.req.Side,
				Type:          item.req.Type,
				Status:        OrderStatus(sdkOrder.Status),
				CreatedAt:     createdAt,
				UpdateTime:    sdkOrder.UpdateTime,
			}, item.req, item.normalized),
		}
	}
	b.resolveUnknownBatchItems(ctx, unknownItems, results)
	return nil
}

func (b *BinanceAdapter) resolveUnknownBatchChunk(ctx context.Context, prepared []preparedOrder, results []PlaceOrderBatchItem, createErr error) {
	unknownItems := make([]unknownBatchResolution, 0, len(prepared))
	for _, item := range prepared {
		unknownItems = append(unknownItems, unknownBatchResolution{item: item, cause: createErr})
	}
	b.resolveUnknownBatchItems(ctx, unknownItems, results)
}

func (b *BinanceAdapter) resolveUnknownBatchItems(ctx context.Context, unknownItems []unknownBatchResolution, results []PlaceOrderBatchItem) {
	if len(unknownItems) == 0 {
		return
	}
	timeout := b.orderConfirmationTimeout
	if timeout <= 0 {
		timeout = defaultOrderConfirmationTimeout
	}
	// 原批量下单上下文可能已经超时；整批确认共享一个独立的有界窗口，
	// 避免每笔订单依次消耗完整超时时间。
	confirmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	type confirmation struct {
		order *Order
		err   error
	}
	confirmations := make([]confirmation, len(unknownItems))
	var wg sync.WaitGroup
	for i, unknown := range unknownItems {
		if unknown.item.clientOID == "" {
			continue
		}
		wg.Add(1)
		go func(index int, preparedItem preparedOrder) {
			defer wg.Done()
			confirmations[index].order, confirmations[index].err = b.confirmOrderByClientIDWithContext(
				confirmCtx, b.contractSpec.Symbol, preparedItem.clientOID)
		}(i, unknown.item)
	}
	wg.Wait()

	for i, unknown := range unknownItems {
		b.assignUnknownBatchItemFromConfirmation(
			ctx, unknown.item, results, unknown.cause, confirmations[i].order, confirmations[i].err)
	}
}

func (b *BinanceAdapter) assignUnknownBatchItemFromConfirmation(
	ctx context.Context,
	item preparedOrder,
	results []PlaceOrderBatchItem,
	createErr error,
	confirmed *Order,
	confirmErr error,
) {
	if item.clientOID == "" {
		results[item.index].Err = fmt.Errorf("%w: Binance 批量下单失败且未提供 clientOrderID: %v",
			exchangeerr.ErrOrderPlacementUnknown, createErr)
		return
	}
	if confirmErr == nil {
		results[item.index] = PlaceOrderBatchItem{Order: overlayNormalizedOrder(confirmed, item.req, item.normalized)}
		return
	}
	if isBinanceAPIErrorCode(confirmErr, -2013) && !isCreateResponseDecodeError(createErr) &&
		!isDuplicateClientOrderIDError(createErr) {
		// 已证明远端没有该 ID。同一 ID 走单笔路径，允许在明确 -2013 后安全重试。
		if ctxErr := ctx.Err(); ctxErr != nil {
			// -2013 只足以允许沿用同一 clientOrderID 重试；它不能排除原批量
			// 请求稍后落库。原上下文已结束时不再重提，也不能释放 reservation。
			results[item.index].Err = fmt.Errorf(
				"%w: Binance 批量下单结果不确定，clientOrderID=%s 暂未查到且原下单上下文已结束: %v",
				exchangeerr.ErrOrderPlacementUnknown, item.clientOID, ctxErr)
			return
		}
		order, err := b.PlaceOrder(ctx, item.req)
		if err != nil {
			results[item.index].Err = err
			return
		}
		results[item.index] = PlaceOrderBatchItem{Order: overlayNormalizedOrder(order, item.req, item.normalized)}
		return
	}
	results[item.index].Err = fmt.Errorf("%w: Binance 批量下单返回不确定结果 (%v)，按 clientOrderID=%s 确认失败: %v",
		exchangeerr.ErrOrderPlacementUnknown, createErr, item.clientOID, confirmErr)
}

func (b *BinanceAdapter) mapBatchItemError(err error, _ string) error {
	if err == nil {
		return nil
	}
	if isDuplicateClientOrderIDError(err) || isUnknownPlacementResult(err) ||
		exchangeerr.LooksLikeAmbiguousOrderPlacementFailure(err.Error()) {
		return exchangeerr.WrapOrderPlacementUnknown(err)
	}
	var apiErr *common.APIError
	if errors.As(err, &apiErr) && apiErr != nil {
		return exchangeerr.WrapOrderPlacementRejected(err)
	}
	return exchangeerr.WrapOrderPlacementUnknown(err)
}

func overlayNormalizedOrder(order *Order, req *OrderRequest, normalized normalizedOrder) *Order {
	if order == nil {
		return nil
	}
	order.Price = normalized.Price
	order.Quantity = normalized.Quantity
	if req != nil {
		if order.Side == "" {
			order.Side = req.Side
		}
		if order.Type == "" {
			order.Type = req.Type
		}
		if order.Symbol == "" && req.Symbol != "" {
			order.Symbol = req.Symbol
		}
	}
	return order
}

func validateBatchAcceptedOrder(order *futures.Order, symbol, clientOrderID string) error {
	if order == nil {
		return fmt.Errorf("Binance 批量下单返回空订单")
	}
	if order.OrderID <= 0 {
		return fmt.Errorf("Binance 批量下单返回无效 orderId=%d", order.OrderID)
	}
	if order.Symbol != symbol {
		return fmt.Errorf("Binance 批量下单返回交易对不匹配: got %q, want %q", order.Symbol, symbol)
	}
	if strings.TrimSpace(order.ClientOrderID) == "" {
		return fmt.Errorf("Binance 批量下单响应缺少 clientOrderId")
	}
	if clientOrderID != "" && order.ClientOrderID != clientOrderID {
		return fmt.Errorf("Binance 批量下单返回 clientOrderId 不匹配: got %q, want %q", order.ClientOrderID, clientOrderID)
	}
	if strings.TrimSpace(string(order.Status)) == "" {
		return fmt.Errorf("Binance 批量下单响应缺少 status")
	}
	return nil
}

func batchChunkHasUnknown(items []PlaceOrderBatchItem) bool {
	for _, item := range items {
		if errors.Is(item.Err, exchangeerr.ErrOrderPlacementUnknown) {
			return true
		}
	}
	return false
}

func rejectUnsentBatchResults(results []PlaceOrderBatchItem, from int, err error) {
	for i := from; i < len(results); i++ {
		if results[i].Order == nil && results[i].Err == nil {
			results[i].Err = err
		}
	}
}

func batchResponseCount(resp *futures.CreateBatchOrdersResponse) int {
	if resp == nil {
		return 0
	}
	return resp.N
}

func (b *BinanceAdapter) BatchPlaceOrders(ctx context.Context, orders []*OrderRequest) ([]*Order, bool) {
	items, err := b.PlaceOrderBatch(ctx, orders)
	placedOrders := make([]*Order, 0, len(orders))
	hasMarginError := false
	if err != nil {
		logger.Warn("⚠️ [Binance] 批量下单失败: %v", err)
		return placedOrders, strings.Contains(err.Error(), "-2019") || strings.Contains(strings.ToLower(err.Error()), "insufficient")
	}
	for i, item := range items {
		if item.Err != nil {
			price := 0.0
			side := ""
			if i < len(orders) && orders[i] != nil {
				price = orders[i].Price
				side = string(orders[i].Side)
			}
			logger.Warn("⚠️ [Binance] 下单失败 %.2f %s: %v", price, side, item.Err)
			if strings.Contains(item.Err.Error(), "-2019") || strings.Contains(strings.ToLower(item.Err.Error()), "insufficient") {
				hasMarginError = true
			}
			continue
		}
		if item.Order != nil {
			placedOrders = append(placedOrders, item.Order)
		}
	}
	return placedOrders, hasMarginError
}
