package binance

import (
	"context"
	"sync"
)

// 每次 POST 各自持有 lease。只读确认在 release 之后进行，同 ID 重试
// 会再次进入执行器的限流、槽位身份和健康/盘口复检。
func beginOrderSubmission(ctx context.Context, req *OrderRequest) (func(), error) {
	if req.BeginSubmission == nil {
		return func() {}, nil
	}
	release, err := req.BeginSubmission(ctx)
	if err != nil {
		return nil, err
	}
	if release == nil {
		return func() {}, nil
	}
	return sync.OnceFunc(release), nil
}
