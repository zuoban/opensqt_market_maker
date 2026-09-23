package position

// 执行器可选能力：一轮规划只生成一个真实批次，未轮到的槽位不创建
// reservation。生产适配器始终提供此能力；旧兼容执行器保留原接口行为。
type placementBatchSizer interface {
	PlacementBatchSize() int
}

type adjustmentBatch struct {
	limit, count int
	deferred     bool
}

func newAdjustmentBatch(executor OrderExecutorInterface) adjustmentBatch {
	if sizer, ok := executor.(placementBatchSizer); ok {
		return adjustmentBatch{limit: max(1, sizer.PlacementBatchSize())}
	}
	return adjustmentBatch{}
}

func (b *adjustmentBatch) accept() bool {
	if b.limit == 0 {
		return true
	}
	// 每轮只保留交易所原生批量上限；下一轮重新检查健康与库存。
	if b.count >= b.limit {
		b.deferred = true
		return false
	}
	b.count++
	return true
}
