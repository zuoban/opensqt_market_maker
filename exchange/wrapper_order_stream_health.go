package exchange

import "opensqt/exchange/streamhealth"

var (
	_ OrderStreamHealthProvider = (*bybitWrapper)(nil)
	_ OrderStreamHealthProvider = (*backpackWrapper)(nil)
	_ OrderStreamHealthProvider = (*bitgetWrapper)(nil)
	_ OrderStreamHealthProvider = (*gateWrapper)(nil)
)

func (w *bybitWrapper) IsOrderStreamReady() bool {
	return w != nil && w.adapter != nil && w.adapter.IsOrderStreamReady()
}

func (w *bybitWrapper) GetOrderStreamState() string {
	if w == nil || w.adapter == nil {
		return string(streamhealth.StateStopped)
	}
	return w.adapter.GetOrderStreamState()
}

func (w *backpackWrapper) IsOrderStreamReady() bool {
	return w != nil && w.adapter != nil && w.adapter.IsOrderStreamReady()
}

func (w *backpackWrapper) GetOrderStreamState() string {
	if w == nil || w.adapter == nil {
		return string(streamhealth.StateStopped)
	}
	return w.adapter.GetOrderStreamState()
}

func (w *bitgetWrapper) IsOrderStreamReady() bool {
	return w != nil && w.adapter != nil && w.adapter.IsOrderStreamReady()
}

func (w *bitgetWrapper) GetOrderStreamState() string {
	if w == nil || w.adapter == nil {
		return string(streamhealth.StateStopped)
	}
	return w.adapter.GetOrderStreamState()
}

func (w *gateWrapper) IsOrderStreamReady() bool {
	return w != nil && w.adapter != nil && w.adapter.IsOrderStreamReady()
}

func (w *gateWrapper) GetOrderStreamState() string {
	if w == nil || w.adapter == nil {
		return string(streamhealth.StateStopped)
	}
	return w.adapter.GetOrderStreamState()
}
