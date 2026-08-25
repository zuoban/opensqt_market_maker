package position

import "testing"

func TestCompareAndSwapSlotOrderStatusRejectsStaleCleanerResult(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	const price = 100.0
	slot := spm.getOrCreateSlot(price)
	slot.OrderID = 41
	slot.ClientOID = "10000_B_1700000000001"
	slot.OrderStatus = OrderStatusConfirmed

	// 同价槽位已经被新订单接管时，旧撤单结果不能反写。
	slot.OrderID = 99
	slot.ClientOID = "10000_B_1700000000099"
	if spm.CompareAndSwapSlotOrderStatus(
		price, 41, "10000_B_1700000000001",
		OrderStatusConfirmed, OrderStatusCancelRequested,
	) {
		t.Fatal("stale cleaner CAS unexpectedly succeeded after replacement")
	}
	if slot.OrderStatus != OrderStatusConfirmed || slot.OrderID != 99 {
		t.Fatalf("replacement order was overwritten: id=%d status=%s", slot.OrderID, slot.OrderStatus)
	}

	// 身份未变化时才允许进入 CANCEL_REQUESTED。
	if !spm.CompareAndSwapSlotOrderStatus(
		price, 99, "10000_B_1700000000099",
		OrderStatusConfirmed, OrderStatusCancelRequested,
	) {
		t.Fatal("matching cleaner CAS failed")
	}
	if slot.OrderStatus != OrderStatusCancelRequested {
		t.Fatalf("matching cleaner status = %s, want CANCEL_REQUESTED", slot.OrderStatus)
	}
}
