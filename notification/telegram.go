// Package notification 发送不参与交易决策的后台通知。
package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"opensqt/config"
	"opensqt/logger"
	"opensqt/position"
)

const (
	telegramQueueSize     = 128
	telegramTimeout       = 10 * time.Second
	telegramResponseLimit = 64 << 10
	telegramMaxAttempts   = 3
)

// Telegram 使用一个固定容量队列和单个发送协程，避免网络故障阻塞订单流。
// 队列不落盘；满队列和关闭时允许丢弃通知，成交账目不受影响。
type Telegram struct {
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	queue      chan position.FilledOrderRecord
	dropped    atomic.Uint64
	client     *http.Client
	endpoint   string
	chatID     string
	quoteAsset string
}

// NewTelegram 使用已校验的配置启动通知；关闭配置不会创建协程或请求。
func NewTelegram(ctx context.Context, cfg config.TelegramConfig, quoteAsset string) *Telegram {
	return newTelegram(ctx, cfg, quoteAsset, "https://api.telegram.org", &http.Client{
		Timeout: telegramTimeout,
		// Bot Token 位于请求路径中，不允许随重定向转发。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	})
}

func newTelegram(ctx context.Context, cfg config.TelegramConfig, quoteAsset, baseURL string, client *http.Client) *Telegram {
	if !cfg.Enabled {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	n := &Telegram{
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
		queue:  make(chan position.FilledOrderRecord, telegramQueueSize),
		client: client, endpoint: baseURL + "/bot" + url.PathEscape(cfg.BotToken) + "/sendMessage",
		chatID: cfg.ChatID, quoteAsset: quoteAsset,
	}
	go n.run()
	return n
}

// NotifyFill 仅非阻塞入队；日志、限流、重试和网络请求都留在后台。
func (n *Telegram) NotifyFill(record position.FilledOrderRecord) {
	if n == nil || n.ctx.Err() != nil {
		return
	}
	select {
	case n.queue <- record:
	default:
		n.dropped.Add(1)
	}
}

// Close 取消在途请求和等待，不等待积压通知发送完毕。可重复、并发调用。
func (n *Telegram) Close() {
	if n == nil {
		return
	}
	n.cancel()
	<-n.done
}

func (n *Telegram) run() {
	defer close(n.done)
	defer n.logDropped()
	// 同一个聊天至少间隔一秒；群组等更严格的限制由 retry_after 控制。
	limiter := rate.NewLimiter(rate.Every(time.Second), 1)
	for {
		select {
		case <-n.ctx.Done():
			return
		case record := <-n.queue:
			n.logDropped()
			for attempt := 0; attempt < telegramMaxAttempts; attempt++ {
				if err := limiter.Wait(n.ctx); err != nil {
					return
				}
				retryAfter, err := n.send(record)
				if n.ctx.Err() != nil {
					return
				}
				if err == nil {
					break
				}
				// 只重试 Telegram 明确拒收的 429。超时等结果不确定的请求
				// 不重试，避免同一成交实际已送达却重复通知。
				if retryAfter > 0 && attempt+1 < telegramMaxAttempts {
					timer := time.NewTimer(retryAfter)
					select {
					case <-n.ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
						continue
					}
				}
				logger.Warn("⚠️ [Telegram] 成交通知发送失败，订单ID=%d: %v（交易继续运行）", record.OrderID, err)
				break
			}
		}
	}
}

func (n *Telegram) logDropped() {
	if count := n.dropped.Swap(0); count > 0 {
		logger.Warn("⚠️ [Telegram] 通知队列已满，丢弃 %d 条成交通知（交易继续运行）", count)
	}
}

func (n *Telegram) send(record position.FilledOrderRecord) (time.Duration, error) {
	payload, err := json.Marshal(struct {
		ChatID string `json:"chat_id"`
		Text   string `json:"text"`
	}{ChatID: n.chatID, Text: formatFill(record, n.quoteAsset)})
	if err != nil {
		return 0, errors.New("通知编码失败")
	}
	ctx, cancel := context.WithTimeout(n.ctx, telegramTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.endpoint, bytes.NewReader(payload))
	if err != nil {
		return 0, errors.New("无法创建 Telegram 请求")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		// net/http 的错误含完整 URL（含 Token），禁止原样返回或记录。
		return 0, errors.New("网络请求失败或超时")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, telegramResponseLimit+1))
	if err != nil || len(body) > telegramResponseLimit {
		return 0, errors.New("Telegram 响应读取失败或超过大小限制")
	}
	var result struct {
		OK         bool `json:"ok"`
		ErrorCode  int  `json:"error_code"`
		Parameters struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	decodeErr := json.Unmarshal(body, &result)
	if resp.StatusCode == http.StatusTooManyRequests || result.ErrorCode == http.StatusTooManyRequests {
		seconds := result.Parameters.RetryAfter
		if seconds <= 0 {
			seconds = 1
		}
		if seconds > 60 {
			return 0, errors.New("Telegram 限流等待超过 60 秒，跳过本条通知")
		}
		return time.Duration(seconds) * time.Second, errors.New("Telegram 限流")
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("Telegram HTTP 状态 %d", resp.StatusCode)
	}
	if decodeErr != nil {
		return 0, errors.New("Telegram 返回无效响应")
	}
	if !result.OK {
		// description 可能回显敏感内容，只记录数值错误码。
		return 0, fmt.Errorf("Telegram 拒绝发送（错误码 %d）", result.ErrorCode)
	}
	return 0, nil
}

func formatFill(record position.FilledOrderRecord, quoteAsset string) string {
	direction := "买入"
	if record.Side == "SELL" {
		direction = "卖出"
	}
	number := func(value float64) string { return strconv.FormatFloat(value, 'f', -1, 64) }
	text := fmt.Sprintf("OpenSQT 订单全部成交\n交易所：Binance\n交易对：%s\n方向：%s（%s）\n成交均价：%s %s\n成交数量：%s\n成交金额：%.8f %s\n订单 ID：%d\n成交时间：%s",
		record.Symbol, direction, record.Side, number(record.Price), quoteAsset,
		number(record.Quantity), record.Price*record.Quantity, quoteAsset, record.OrderID,
		record.FilledAt.In(time.Local).Format("2006-01-02 15:04:05 MST (Z07:00)"))
	if record.Side == "SELL" {
		text += fmt.Sprintf("\n已实现盈亏：%+.8f %s（未扣手续费）", record.RealizedPNL, quoteAsset)
	}
	return text
}
