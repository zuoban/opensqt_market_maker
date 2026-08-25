package exchangeerr

import (
	"errors"
	"fmt"
)

// ErrOrderPlacementUnknown 表示交易所可能已经接受订单，但客户端无法确认结果。
// 调用方必须停止盲目重试，并通过相同的 clientOrderID 对账。
var ErrOrderPlacementUnknown = errors.New("订单下单结果未知")

// WrapOrderPlacementUnknown 保留底层错误链，同时标记“交易所可能已经接受订单”。
// 具体适配器应在请求已进入远端边界、但响应无法可靠确认时使用它。
func WrapOrderPlacementUnknown(err error) error {
	if err == nil {
		return ErrOrderPlacementUnknown
	}
	if errors.Is(err, ErrOrderPlacementUnknown) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrOrderPlacementUnknown, err)
}
