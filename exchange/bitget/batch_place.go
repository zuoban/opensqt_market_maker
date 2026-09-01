package bitget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"opensqt/exchange/exchangeerr"
	"opensqt/logger"
)

const maxPlaceOrderBatchSize = 20

type PlaceOrderBatchItem struct {
	Order *Order
	Err   error
}

func (b *BitgetAdapter) PlaceOrderBatchSize() int {
	return maxPlaceOrderBatchSize
}

func (b *BitgetAdapter) PlaceOrderBatch(ctx context.Context, orders []*OrderRequest) ([]PlaceOrderBatchItem, error) {
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

func (b *BitgetAdapter) placeOrderBatchChunk(ctx context.Context, orders []*OrderRequest, results []PlaceOrderBatchItem) error {
	orderList := make([]map[string]interface{}, 0, len(orders))
	indexByClientOID := make(map[string]int, len(orders))
	for i, req := range orders {
		if req == nil {
			results[i].Err = exchangeerr.WrapOrderPlacementRejected(fmt.Errorf("订单请求不能为空"))
			continue
		}
		payload, clientOID, err := b.placeOrderEntry(req)
		if err != nil {
			results[i].Err = err
			continue
		}
		if clientOID == "" {
			results[i].Err = exchangeerr.WrapOrderPlacementRejected(fmt.Errorf("Bitget 批量下单缺少 clientOid"))
			continue
		}
		if _, exists := indexByClientOID[clientOID]; exists {
			results[i].Err = exchangeerr.WrapOrderPlacementRejected(fmt.Errorf("Bitget 批量下单 clientOid 重复: %s", clientOID))
			continue
		}
		indexByClientOID[clientOID] = i
		orderList = append(orderList, payload)
	}
	if len(orderList) == 0 {
		return nil
	}

	body := map[string]interface{}{
		"symbol":      b.symbol,
		"productType": b.productType,
		"marginMode":  "crossed",
		"marginCoin":  "USDT",
		"orderList":   orderList,
	}
	resp, err := b.client.DoRequest(ctx, "POST", "/api/v2/mix/order/batch-place-order", body)
	if err != nil {
		classified := b.wrapPlacementError(err)
		if errors.Is(classified, exchangeerr.ErrOrderPlacementUnknown) {
			for _, req := range orders {
				if req == nil || req.ClientOrderID == "" {
					continue
				}
				if idx, ok := indexByClientOID[req.ClientOrderID]; ok && results[idx].Err == nil && results[idx].Order == nil {
					results[idx].Err = classified
				}
			}
			return nil
		}
		for _, idx := range indexByClientOID {
			if results[idx].Err != nil || results[idx].Order != nil {
				continue
			}
			results[idx].Err = classified
		}
		return nil
	}

	var data struct {
		SuccessList []struct {
			OrderID       string `json:"orderId"`
			ClientOrderID string `json:"clientOid"`
		} `json:"successList"`
		FailureList []struct {
			OrderID       string `json:"orderId"`
			ClientOrderID string `json:"clientOid"`
			ErrorMsg      string `json:"errorMsg"`
			ErrorCode     string `json:"errorCode"`
		} `json:"failureList"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		unknown := exchangeerr.WrapOrderPlacementUnknown(fmt.Errorf("解析 Bitget 批量下单响应失败: %w", err))
		for _, idx := range indexByClientOID {
			if results[idx].Err == nil && results[idx].Order == nil {
				results[idx].Err = unknown
			}
		}
		return nil
	}

	seen := make(map[string]struct{}, len(orderList))
	for _, item := range data.SuccessList {
		clientOID := item.ClientOrderID
		idx, ok := indexByClientOID[clientOID]
		if !ok {
			continue
		}
		seen[clientOID] = struct{}{}
		orderID, _ := strconv.ParseInt(item.OrderID, 10, 64)
		if orderID == 0 {
			results[idx].Err = exchangeerr.WrapOrderPlacementUnknown(
				fmt.Errorf("Bitget 批量下单成功项 orderId 为空或无效"))
			continue
		}
		results[idx] = PlaceOrderBatchItem{Order: b.newAcceptedOrder(orders[idx], orderID, clientOID)}
	}
	for _, item := range data.FailureList {
		clientOID := item.ClientOrderID
		idx, ok := indexByClientOID[clientOID]
		if !ok {
			continue
		}
		seen[clientOID] = struct{}{}
		apiErr := &APIError{StatusCode: 200, Code: item.ErrorCode, Message: item.ErrorMsg}
		results[idx].Err = b.wrapPlacementError(apiErr)
	}
	for clientOID, idx := range indexByClientOID {
		if _, ok := seen[clientOID]; ok || results[idx].Err != nil || results[idx].Order != nil {
			continue
		}
		results[idx].Err = exchangeerr.WrapOrderPlacementUnknown(
			fmt.Errorf("Bitget 批量下单未返回 clientOid=%s 的结果", clientOID))
	}
	return nil
}

func (b *BitgetAdapter) BatchPlaceOrders(ctx context.Context, orders []*OrderRequest) ([]*Order, bool) {
	items, err := b.PlaceOrderBatch(ctx, orders)
	placedOrders := make([]*Order, 0, len(orders))
	hasMarginError := false
	if err != nil {
		logger.Warn("⚠️ [Bitget] 批量下单失败: %v", err)
		return placedOrders, strings.Contains(err.Error(), "保证金不足")
	}
	for i, item := range items {
		if item.Err != nil {
			price := 0.0
			side := ""
			if i < len(orders) && orders[i] != nil {
				price = orders[i].Price
				side = string(orders[i].Side)
			}
			logger.Warn("⚠️ [Bitget] 下单失败 %.2f %s: %v", price, side, item.Err)
			if strings.Contains(item.Err.Error(), "保证金不足") {
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

func rejectUnsentBatchResults(results []PlaceOrderBatchItem, from int, err error) {
	for i := from; i < len(results); i++ {
		if results[i].Order == nil && results[i].Err == nil {
			results[i].Err = err
		}
	}
}

func batchChunkHasUnknown(items []PlaceOrderBatchItem) bool {
	for _, item := range items {
		if errors.Is(item.Err, exchangeerr.ErrOrderPlacementUnknown) {
			return true
		}
	}
	return false
}
