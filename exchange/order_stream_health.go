package exchange

// OrderStreamHealthProvider 暴露交易所订单流的真实健康状态。
//
// 该能力没有并入 IExchange，以保持接口兼容；交易门禁会通过类型断言读取它，
// 未实现或无法观测订单流健康状态时一律按未就绪处理（fail-closed）。
type OrderStreamHealthProvider interface {
	IsOrderStreamReady() bool
	GetOrderStreamState() string
}
