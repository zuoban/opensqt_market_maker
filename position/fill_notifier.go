package position

// SetFilledOrderNotifier 注册已接受、已去重的全成交通知。
// 回调在槽位解锁并通知补单后执行，必须只做非阻塞入队，不能发送网络请求。
// 与成交历史一致：只通知本次运行新确认的 FILLED，不通知部分成交或终态修正。
func (spm *SuperPositionManager) SetFilledOrderNotifier(notifier func(FilledOrderRecord)) {
	spm.filledOrderNotifierMu.Lock()
	spm.filledOrderNotifier = notifier
	spm.filledOrderNotifierMu.Unlock()
}

func (spm *SuperPositionManager) notifyFilledOrder(record FilledOrderRecord) {
	spm.filledOrderNotifierMu.RLock()
	notifier := spm.filledOrderNotifier
	spm.filledOrderNotifierMu.RUnlock()
	if notifier != nil {
		notifier(record)
	}
}
