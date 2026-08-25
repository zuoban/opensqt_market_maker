package binance

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type boundedTransportFunc func(*http.Request) (*http.Response, error)

func (f boundedTransportFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func responseWithBody(status int, size int64) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(make([]byte, int(size)))),
	}
}

func TestStatusAwareTransportRejectsOversizedDefaultResponse(t *testing.T) {
	transport := &statusAwareTransport{base: boundedTransportFunc(func(*http.Request) (*http.Response, error) {
		return responseWithBody(http.StatusOK, defaultSDKResponseBodyLimit+1), nil
	})}
	req, err := http.NewRequest(http.MethodGet, "https://fapi.binance.com/fapi/v2/account", nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := transport.RoundTrip(req)
	if resp != nil {
		t.Fatalf("RoundTrip() response = %+v, want nil", resp)
	}
	var limitErr *responseBodyTooLargeError
	if !errors.As(err, &limitErr) {
		t.Fatalf("RoundTrip() error = %v, want responseBodyTooLargeError", err)
	}
	if limitErr.Method != http.MethodGet || limitErr.Path != "/fapi/v2/account" ||
		limitErr.StatusCode != http.StatusOK || limitErr.Limit != defaultSDKResponseBodyLimit {
		t.Fatalf("responseBodyTooLargeError = %+v", limitErr)
	}
	if !strings.Contains(err.Error(), "响应体超过限制") {
		t.Fatalf("error = %v, want explicit limit message", err)
	}
}

func TestStatusAwareTransportUsesLargerExchangeInfoBudget(t *testing.T) {
	size := defaultSDKResponseBodyLimit + 1
	if size >= exchangeInfoResponseBodyLimit {
		t.Fatalf("test requires exchangeInfo limit larger than default: default=%d exchangeInfo=%d",
			defaultSDKResponseBodyLimit, exchangeInfoResponseBodyLimit)
	}
	transport := &statusAwareTransport{base: boundedTransportFunc(func(*http.Request) (*http.Response, error) {
		return responseWithBody(http.StatusOK, size), nil
	})}
	req, err := http.NewRequest(http.MethodGet, "https://fapi.binance.com"+exchangeInfoEndpoint, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(body)) != size {
		t.Fatalf("buffered response length = %d, want %d", len(body), size)
	}
	if resp.ContentLength != size {
		t.Fatalf("ContentLength = %d, want %d", resp.ContentLength, size)
	}
}

func TestStatusAwareTransportRejectsOversizedExchangeInfoResponse(t *testing.T) {
	transport := &statusAwareTransport{base: boundedTransportFunc(func(*http.Request) (*http.Response, error) {
		return responseWithBody(http.StatusOK, exchangeInfoResponseBodyLimit+1), nil
	})}
	req, err := http.NewRequest(http.MethodGet, "https://fapi.binance.com"+exchangeInfoEndpoint, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = transport.RoundTrip(req)
	var limitErr *responseBodyTooLargeError
	if !errors.As(err, &limitErr) || limitErr.Limit != exchangeInfoResponseBodyLimit {
		t.Fatalf("RoundTrip() error = %v, want exchangeInfo response limit", err)
	}
}

func TestPlaceOrderOversizedResponseConfirmsByClientOrderID(t *testing.T) {
	const brokerID = "x-zdfVM8vYgrid-order-1"
	var postCalls, queryCalls atomic.Int32
	oversizedBody := strings.Repeat("x", int(defaultSDKResponseBodyLimit+1))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != createOrderEndpoint {
			http.NotFound(w, req)
			return
		}
		if req.Method == http.MethodPost {
			postCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, oversizedBody)
			return
		}
		queryCalls.Add(1)
		if got := req.URL.Query().Get("origClientOrderId"); got != brokerID {
			t.Errorf("origClientOrderId = %q, want %q", got, brokerID)
		}
		writeJSON(t, w, http.StatusOK, orderFixture(91, brokerID))
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	order, err := adapter.PlaceOrder(context.Background(), placementRequest())
	if err != nil {
		t.Fatalf("PlaceOrder() error = %v", err)
	}
	if order.OrderID != 91 || order.ClientOrderID != brokerID {
		t.Fatalf("PlaceOrder() order = %+v", order)
	}
	if postCalls.Load() != 1 || queryCalls.Load() != 1 {
		t.Fatalf("calls = post:%d query:%d, want 1/1", postCalls.Load(), queryCalls.Load())
	}
}
