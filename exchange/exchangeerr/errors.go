package exchangeerr

import (
	"errors"
	"fmt"
	"strings"
)

// ErrOrderPlacementUnknown 表示交易所可能已经接受订单，但客户端无法确认结果。
// 调用方必须停止盲目重试，并通过相同的 clientOrderID 对账。
var ErrOrderPlacementUnknown = errors.New("订单下单结果未知")

// ErrOrderPlacementRejected 表示本次下单请求明确未被交易所受理。
// 只有请求尚未发送，或交易所返回了可证明未创建订单的明确拒绝时才能使用。
var ErrOrderPlacementRejected = errors.New("订单明确未受理")

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

// WrapOrderPlacementRejected 保留底层错误链，同时标记“订单明确未受理”。
// UNKNOWN 的安全优先级高于 REJECTED；已含 UNKNOWN 的错误不得降级成明确拒绝。
func WrapOrderPlacementRejected(err error) error {
	if err == nil {
		return ErrOrderPlacementRejected
	}
	if errors.Is(err, ErrOrderPlacementUnknown) || errors.Is(err, ErrOrderPlacementRejected) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrOrderPlacementRejected, err)
}

// IsOrderPlacementRejected 仅在整个结果可以安全视为明确未受理时返回 true。
// 若错误链同时含有 UNKNOWN，则必须按 UNKNOWN 处理。
func IsOrderPlacementRejected(err error) bool {
	return err != nil &&
		!errors.Is(err, ErrOrderPlacementUnknown) &&
		errors.Is(err, ErrOrderPlacementRejected)
}

// LooksLikeAmbiguousOrderPlacementFailure 保守识别交易所业务响应中的
// 超时或服务端故障。即使 HTTP 是 2xx/4xx，这类响应也不能证明订单未落库。
func LooksLikeAmbiguousOrderPlacementFailure(parts ...string) bool {
	message := strings.ToLower(strings.Join(parts, " "))
	message = strings.NewReplacer("_", " ", "-", " ").Replace(message)
	markers := []string{
		"timeout", "timed out", "server error", "internal error", "backend error",
		"service unavailable", "temporarily unavailable", "try again later", "unknown error",
	}
	for _, marker := range markers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
