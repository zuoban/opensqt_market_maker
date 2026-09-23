package order

import (
	"context"
	"fmt"
	"sync/atomic"
)

// 首次 lease 已由执行器获取，原生批次也已通过交易健康复检。适配器
// 接管它的释放时机；后续同 ID POST 必须独立限流并重新获取/复检。
// 只读确认的独立 context 不得用于提交，原新单代际始终参与门禁检查。
func (oe *ExchangeOrderExecutor) submissionBoundary(req *OrderRequest, initialRelease func(), generationCtx context.Context) func(context.Context) (func(), error) {
	var entered atomic.Bool
	return func(ctx context.Context) (func(), error) {
		if !entered.Swap(true) {
			return initialRelease, nil
		}
		fail := func(err error) (func(), error) {
			// 前一次 POST 已发出；重试被本地拦截并不证明旧请求未落库。
			// 立即停止原代际，避免同一原生批次的其它成员继续回退提交。
			oe.newOrdersMu.Lock()
			if oe.newOrdersCtx == generationCtx {
				oe.newOrdersEnabled.Store(false)
				oe.newOrdersCancel()
			}
			oe.newOrdersMu.Unlock()
			return nil, unknownSubmissionError(req, "同 ID 重试复检失败", err)
		}
		if err := oe.submissionContextError(generationCtx); err != nil {
			return fail(err)
		}
		if err := oe.waitForRateLimit(ctx); err != nil {
			return fail(oe.placementContextError("同 ID 重试限流失败", err))
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		release, err := oe.acquireGuardedSubmission(generationCtx, req)
		if err != nil {
			return fail(err)
		}
		if err := ctx.Err(); err != nil {
			release()
			return fail(err)
		}
		if req.OnSubmissionStarted != nil {
			req.OnSubmissionStarted()
		}
		return release, nil
	}
}

func (oe *ExchangeOrderExecutor) submissionContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return oe.placementContextError("下单前交易门禁已关闭", err)
	}
	if !oe.newOrdersEnabled.Load() {
		return ErrNewOrdersStopped
	}
	return nil
}

// 调用方已完成限流；任何失败都释放 lease，健康失败先释放再关闭新单。
func (oe *ExchangeOrderExecutor) acquireGuardedSubmission(ctx context.Context, req *OrderRequest) (func(), error) {
	release, ok := acquireSubmissionLease(req)
	if !ok {
		return nil, ErrOrderSubmissionStale
	}
	if err := oe.submissionContextError(ctx); err != nil {
		release()
		return nil, err
	}
	if guardErr := oe.checkSubmissionHealth(); guardErr != nil {
		release()
		oe.StopNewOrders()
		return nil, fmt.Errorf("%w: %v", ErrTradingHealthGuardRejected, guardErr)
	}
	if err := oe.submissionContextError(ctx); err != nil {
		release()
		return nil, err
	}
	return release, nil
}
