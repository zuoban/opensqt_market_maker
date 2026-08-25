package binance

import (
	"fmt"
	"strings"
	"testing"
)

func validKlineStreamJSON(overrides string) []byte {
	body := `{"stream":"ethusdt@kline_1m","data":{"e":"kline","s":"ETHUSDT","k":{"t":1700000000000,"s":"ETHUSDT","i":"1m","o":"100","c":"101","h":"102","l":"99","v":"12.5","x":false}}}`
	if overrides == "" {
		return []byte(body)
	}
	parts := strings.SplitN(overrides, "=>", 2)
	return []byte(strings.Replace(body, parts[0], parts[1], 1))
}

func TestParseKlineStreamMessage(t *testing.T) {
	candle, err := parseKlineStreamMessage(validKlineStreamJSON(""), []string{"BTCUSDT", "ETHUSDT"}, "1m")
	if err != nil {
		t.Fatalf("parseKlineStreamMessage() error = %v", err)
	}
	if candle.Symbol != "ETHUSDT" || candle.Open != 100 || candle.High != 102 || candle.Low != 99 ||
		candle.Close != 101 || candle.Volume != 12.5 || candle.Timestamp != 1700000000000 || candle.IsClosed {
		t.Fatalf("candle = %+v", candle)
	}
}

func TestParseKlineStreamMessageRejectsUnsafePayloads(t *testing.T) {
	tests := []struct {
		name     string
		override string
		want     string
	}{
		{name: "wrong event", override: `"e":"kline"=>"e":"trade"`, want: "事件类型"},
		{name: "unsubscribed symbol", override: `"s":"ETHUSDT"=>"s":"BNBUSDT"`, want: "交易对字段无效"},
		{name: "wrong interval", override: `"i":"1m"=>"i":"5m"`, want: "周期不匹配"},
		{name: "malformed number", override: `"o":"100"=>"o":"broken"`, want: "有效数字"},
		{name: "non finite", override: `"o":"100"=>"o":"NaN"`, want: "有限数字"},
		{name: "negative volume", override: `"v":"12.5"=>"v":"-1"`, want: "数值范围"},
		{name: "invalid ohlc", override: `"h":"102"=>"h":"98"`, want: "OHLC"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseKlineStreamMessage(validKlineStreamJSON(tt.override), []string{"ETHUSDT"}, "1m")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %s", err, fmt.Sprintf("substring %q", tt.want))
			}
		})
	}
}
