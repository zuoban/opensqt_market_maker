package binance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const (
	positionModeEndpoint                = "/fapi/v1/positionSide/dual"
	exchangeInfoEndpoint                = "/fapi/v1/exchangeInfo"
	createOrderEndpoint                 = "/fapi/v1/order"
	maxCapturedResponseBody             = 64 << 10
	defaultSDKResponseBodyLimit   int64 = 1 << 20
	exchangeInfoResponseBodyLimit int64 = 8 << 20
)

// responseBodyTooLargeError 表示 Binance REST 响应超过端点预算。
// Method/Path 用于让创建订单调用将该错误归类为“执行结果未知”。
type responseBodyTooLargeError struct {
	Method     string
	Path       string
	StatusCode int
	Limit      int64
}

func (e *responseBodyTooLargeError) Error() string {
	return fmt.Sprintf("Binance HTTP 响应体超过限制: %s %s, status=%d, limit=%d bytes",
		e.Method, e.Path, e.StatusCode, e.Limit)
}

// httpStatusError 保留 go-binance SDK 默认会丢弃的 HTTP 状态码。
// Binance 对下单接口返回 503 时，执行结果可能是 UNKNOWN，不能直接重下。
type httpStatusError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *httpStatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("Binance HTTP %d (%s)", e.StatusCode, e.Status)
	}
	return fmt.Sprintf("Binance HTTP %d (%s): %s", e.StatusCode, e.Status, e.Body)
}

type statusAwareTransport struct {
	base http.RoundTripper
}

func (t *statusAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if err := bufferBoundedSDKResponse(req, resp); err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices && req.URL.Path == positionModeEndpoint {
		if err := validatePositionModeHTTPResponse(resp); err != nil {
			return nil, err
		}
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		return resp, nil
	}

	body, truncated, readErr := readBoundedResponseBody(resp.Body)
	bodyText := string(body)
	if truncated {
		bodyText += " (响应体已截断)"
	}
	if readErr != nil {
		bodyText = fmt.Sprintf("%s (读取响应失败: %v)", bodyText, readErr)
	}

	return nil, &httpStatusError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Body:       bodyText,
	}
}

func sdkResponseBodyLimit(path string) int64 {
	if path == exchangeInfoEndpoint {
		return exchangeInfoResponseBodyLimit
	}
	return defaultSDKResponseBodyLimit
}

// bufferBoundedSDKResponse 在 go-binance SDK 的无界 io.ReadAll 之前完成限长读取，
// 并恢复响应体供 SDK 正常解码。
func bufferBoundedSDKResponse(req *http.Request, resp *http.Response) error {
	limit := sdkResponseBodyLimit(req.URL.Path)
	body, truncated, err := readResponseBodyWithLimit(resp.Body, limit)
	if err != nil {
		return fmt.Errorf("读取 Binance HTTP 响应失败 (%s %s): %w", req.Method, req.URL.Path, err)
	}
	if truncated {
		return &responseBodyTooLargeError{
			Method:     req.Method,
			Path:       req.URL.Path,
			StatusCode: resp.StatusCode,
			Limit:      limit,
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	return nil
}

func validatePositionModeHTTPResponse(resp *http.Response) error {
	body, truncated, err := readBoundedResponseBody(resp.Body)
	// SDK 仍需读取同一响应，因此校验后恢复响应体。
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	if err != nil {
		return fmt.Errorf("读取 Binance 持仓模式响应失败: %w", err)
	}
	if truncated {
		return fmt.Errorf("Binance 持仓模式响应超过 %d 字节限制", maxCapturedResponseBody)
	}

	var payload struct {
		DualSidePosition *bool `json:"dualSidePosition"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("Binance 持仓模式响应不是有效 JSON: %w", err)
	}
	if payload.DualSidePosition == nil {
		return fmt.Errorf("Binance 持仓模式响应缺少布尔字段 dualSidePosition")
	}
	return nil
}

func readBoundedResponseBody(body io.ReadCloser) ([]byte, bool, error) {
	return readResponseBodyWithLimit(body, maxCapturedResponseBody)
}

func readResponseBodyWithLimit(body io.ReadCloser, limit int64) ([]byte, bool, error) {
	if body == nil {
		return nil, false, nil
	}
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	_ = body.Close()
	if int64(len(data)) > limit {
		return data[:limit], true, err
	}
	return data, false, err
}
