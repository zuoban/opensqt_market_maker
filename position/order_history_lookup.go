package position

import "strconv"

// lookupOrderHistory preserves filledOrderKey's exact identity and precedence.
// The caller holds the history's lock. The temporary byte-to-string conversion
// is used only for map lookup: it never publishes a key backed by this buffer.
// 64 bytes cover Binance's 36-character ClientOrderID plus our namespace; longer
// compatibility identities still use the original allocating representation.
func lookupOrderHistory[T any](history map[string]T, update OrderUpdate) (T, bool) {
	var buffer [64]byte
	var key []byte
	if update.ClientOrderID != "" {
		const prefix = "client:"
		if len(update.ClientOrderID) > len(buffer)-len(prefix) {
			value, found := history[filledOrderKey(update)]
			return value, found
		}
		n := copy(buffer[:], prefix)
		n += copy(buffer[n:], update.ClientOrderID)
		key = buffer[:n]
	} else if update.OrderID != 0 {
		key = append(buffer[:0], "order:"...)
		key = strconv.AppendInt(key, update.OrderID, 10)
	} else {
		var zero T
		return zero, false
	}
	value, found := history[string(key)]
	return value, found
}
