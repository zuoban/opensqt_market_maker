package web

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"opensqt/config"
	"opensqt/exchange"
	"opensqt/logger"
	"opensqt/monitor"
	"opensqt/position"
	"opensqt/safety"
)

// Options 监控面板依赖（全部只读）
type Options struct {
	Cfg      *config.Config
	Version  string
	Price    *monitor.PriceMonitor
	Position *position.SuperPositionManager
	Risk     *safety.RiskMonitor
	Exchange exchange.IExchange
}

// Server 本地只读监控 HTTP/WS 服务
type Server struct {
	cfg        *config.Config
	version    string
	listen     string
	assembler  *assembler
	account    *AccountCache
	hub        *hub
	httpServer *http.Server
	cancel     context.CancelFunc
	addr       string
	started    time.Time
}

type wsEnvelope struct {
	Type string    `json:"type"`
	Data *Snapshot `json:"data"`
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// New 创建面板服务。调用方在独立 goroutine 里 Start，失败不得退出做市。
func New(opt Options) *Server {
	started := time.Now()
	interval := 10 * time.Second
	listen := "127.0.0.1:8787"
	if opt.Cfg != nil {
		if opt.Cfg.Dashboard.AccountRefreshSec > 0 {
			interval = time.Duration(opt.Cfg.Dashboard.AccountRefreshSec) * time.Second
		}
		if opt.Cfg.Dashboard.Listen != "" {
			listen = opt.Cfg.Dashboard.Listen
		}
	}
	var src AccountSource
	if opt.Exchange != nil {
		src = opt.Exchange
	}
	symbol := ""
	if opt.Cfg != nil {
		symbol = opt.Cfg.Trading.Symbol
	}
	cache := newAccountCache(src, symbol, interval)
	return &Server{
		cfg:     opt.Cfg,
		version: opt.Version,
		listen:  listen,
		account: cache,
		hub:     newHub(),
		started: started,
		assembler: &assembler{
			cfg:     opt.Cfg,
			version: opt.Version,
			started: started,
			price:   opt.Price,
			pos:     opt.Position,
			risk:    opt.Risk,
			account: cache,
		},
	}
}

// Addr 实际监听地址（Start 之后才有值）
func (s *Server) Addr() string {
	if s == nil {
		return ""
	}
	return s.addr
}

// Start 阻塞监听。Listen 失败时返回错误，不 panic。
func (s *Server) Start() error {
	if s == nil || s.cfg != nil && !s.cfg.DashboardEnabled() {
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	go s.hub.run()
	go s.account.Run(ctx)
	go s.pushLoop(ctx)

	mux := http.NewServeMux()
	staticRoot, err := fs.Sub(staticFS, "static")
	if err != nil {
		return err
	}
	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(staticRoot))))
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/snapshot", s.requireToken(s.handleSnapshot))
	mux.HandleFunc("/ws", s.requireToken(s.handleWS))

	ln, err := net.Listen("tcp", s.listen)
	if err != nil {
		cancel()
		return err
	}
	s.addr = ln.Addr().String()
	if isNonLocalListen(s.listen) {
		logger.Warn("⚠️ 监控面板监听 %s，账户与仓位可被局域网访问。建议改回 127.0.0.1 或设置 dashboard.token", s.listen)
	}
	logger.Info("🖥️ 监控面板: http://%s", displayURL(s.addr))

	s.httpServer = &http.Server{Handler: mux}
	err = s.httpServer.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown 在 timeout 内关闭监听
func (s *Server) Shutdown(timeout time.Duration) {
	if s == nil {
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.httpServer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = s.httpServer.Shutdown(ctx)
}

func (s *Server) pushLoop(ctx context.Context) {
	interval := 400 * time.Millisecond
	if s.cfg != nil && s.cfg.Dashboard.PushIntervalMS > 0 {
		interval = time.Duration(s.cfg.Dashboard.PushIntervalMS) * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.hub.clientCount() == 0 {
				continue
			}
			s.hub.broadcast <- wsEnvelope{Type: "snapshot", Data: s.assembler.Build()}
		}
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "index not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":        true,
		"version":   s.version,
		"uptimeSec": time.Since(s.started).Seconds(),
	})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.assembler.Build())
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Debug("监控面板 WS upgrade 失败: %v", err)
		return
	}
	client := &wsClient{conn: conn}
	s.hub.register <- client
	_ = client.writeJSON(wsEnvelope{Type: "snapshot", Data: s.assembler.Build()})
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			s.hub.unregister <- client
			return
		}
	}
}

func (s *Server) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	if s == nil || s.cfg == nil || s.cfg.Dashboard.Token == "" {
		return true
	}
	want := s.cfg.Dashboard.Token
	token := r.URL.Query().Get("token")
	if token == "" {
		token = r.Header.Get("X-Dashboard-Token")
	}
	if token == "" {
		if ah := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(ah), "bearer ") {
			token = strings.TrimSpace(ah[7:])
		}
	}
	return token == want
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func isNonLocalListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

func displayURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
