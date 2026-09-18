package position

// 执行器可选能力：一轮规划只生成一个真实批次，未轮到的槽位不创建
// reservation。生产适配器始终提供此能力；旧兼容执行器保留原接口行为。
type placementBatchSizer interface {
	PlacementBatchSize() int
}

type adjustmentBatch struct {
	limit, count     int
	closed, deferred bool
}

func newAdjustmentBatch(executor OrderExecutorInterface) adjustmentBatch {
	if sizer, ok := executor.(placementBatchSizer); ok {
		return adjustmentBatch{limit: max(1, sizer.PlacementBatchSize())}
	}
	return adjustmentBatch{}
}

func (b *adjustmentBatch) accept(req *OrderRequest) bool {
	if b.limit == 0 {
		return true
	}
	// 与执行器原生分批规则相同：近盘口请求独占一批，深度批次遇到它
	// 时先结束；下一轮会按最新盘口重新规划。
	if b.closed || b.count >= b.limit || b.count > 0 && req.NearTouch {
		b.closed, b.deferred = true, true
		return false
	}
	b.count++
	b.closed = req.NearTouch
	return true
}
