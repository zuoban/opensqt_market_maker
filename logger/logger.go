package logger

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// LogLevel 日志级别
type LogLevel int

const (
	DEBUG LogLevel = iota // 调试信息（最详细）
	INFO                  // 一般信息（正常运行信息）
	WARN                  // 警告信息（需要注意但不影响运行）
	ERROR                 // 错误信息（需要关注的问题）
	FATAL                 // 致命错误（程序无法继续）
)

var (
	globalLevel atomic.Int32

	// 文件日志相关
	fileLogger     *log.Logger
	logFile        *os.File
	currentDate    string
	fileMu         sync.Mutex
	logDir         = "log" // 日志文件夹
	fileSink       atomic.Pointer[asyncLogSink]
	consoleSink    atomic.Pointer[asyncLogSink]
	lifecycleMu    sync.Mutex
	consoleDropped atomic.Uint64
	fileDropped    atomic.Uint64

	ringCap  = 200
	ringMu   sync.Mutex
	ringBuf  [200]LogEntry
	ringNext int
	ringSize int
)

func init() {
	globalLevel.Store(int32(INFO))
	startConsoleLogger()
}

type asyncLogItem struct {
	time      time.Time
	levelText string
	message   string
}

// LogEntry 环缓冲中的一条日志
type LogEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

// String 返回日志级别的字符串表示
func (l LogLevel) String() string {
	switch l {
	case DEBUG:
		return "DEBUG"
	case INFO:
		return "INFO"
	case WARN:
		return "WARN"
	case ERROR:
		return "ERROR"
	case FATAL:
		return "FATAL"
	default:
		return "UNKNOWN"
	}
}

// ParseLogLevel 解析日志级别字符串
func ParseLogLevel(level string) LogLevel {
	level = strings.ToUpper(strings.TrimSpace(level))
	switch level {
	case "DEBUG":
		return DEBUG
	case "INFO":
		return INFO
	case "WARN", "WARNING":
		return WARN
	case "ERROR":
		return ERROR
	case "FATAL":
		return FATAL
	default:
		return INFO // 默认INFO级别
	}
}

// SetLevel 设置全局日志级别
func SetLevel(level LogLevel) {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	startConsoleLogger()
	globalLevel.Store(int32(level))

	// 如果设置为DEBUG级别，启用文件日志
	if level == DEBUG {
		initFileLogger()
	} else {
		closeFileLogger()
	}
}

// initFileLogger 初始化文件日志（当日志级别为DEBUG时）
func initFileLogger() {
	fileMu.Lock()
	defer fileMu.Unlock()

	today := time.Now().Format("2006-01-02")
	if fileLogger == nil || currentDate != today {
		if logFile != nil {
			logFile.Close()
			logFile = nil
		}

		if err := os.MkdirAll(logDir, 0755); err != nil {
			log.Printf("[WARN] 创建日志文件夹失败: %v，将只输出到控制台", err)
			return
		}

		logFileName := filepath.Join(logDir, fmt.Sprintf("opensqt-%s.log", today))
		file, err := os.OpenFile(logFileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Printf("[WARN] 打开日志文件失败: %v，将只输出到控制台", err)
			return
		}

		logFile = file
		currentDate = today
		fileLogger = log.New(file, "", 0)
		log.Printf("[INFO] 文件日志已启用，日志文件: %s", logFileName)
	}

	if fileSink.Load() == nil {
		fileSink.Store(newAsyncLogSink(4096, &fileDropped, func(item asyncLogItem) {
			fileMu.Lock()
			defer fileMu.Unlock()
			checkAndRotateLog()
			if fileLogger != nil {
				fileLogger.Printf("%s [%s] %s", item.time.Format("2006/01/02 15:04:05"), item.levelText, item.message)
			}
		}))
	}
}

// closeFileLogger 由 lifecycleMu 串行化；先停止入队并排空，再关闭文件。
func closeFileLogger() {
	fileSink.Swap(nil).close()
	fileMu.Lock()
	defer fileMu.Unlock()
	if logFile != nil {
		logFile.Close()
		logFile = nil
		fileLogger = nil
		currentDate = ""
	}
}

func checkAndRotateLog() {
	today := time.Now().Format("2006-01-02")
	if currentDate != today {
		if logFile != nil {
			logFile.Close()
			logFile = nil
		}

		if err := os.MkdirAll(logDir, 0755); err != nil {
			return
		}

		logFileName := filepath.Join(logDir, fmt.Sprintf("opensqt-%s.log", today))
		file, err := os.OpenFile(logFileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return
		}

		logFile = file
		currentDate = today
		fileLogger = log.New(file, "", 0)
	}
}

// Close 停止接收输出日志并等待控制台、文件队列排空（程序退出时调用）
func Close() {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	closeFileLogger()
	consoleSink.Swap(nil).close()
}

func recordLog(level LogLevel, msg string) {
	ringMu.Lock()
	ringBuf[ringNext] = LogEntry{
		Time:    time.Now(),
		Level:   level.String(),
		Message: msg,
	}
	ringNext = (ringNext + 1) % ringCap
	if ringSize < ringCap {
		ringSize++
	}
	ringMu.Unlock()
}

// RecentLogs 返回最近 n 条日志（旧→新）。n<=0 时返回全部。
func RecentLogs(n int) []LogEntry {
	ringMu.Lock()
	defer ringMu.Unlock()
	if ringSize == 0 {
		return nil
	}
	if n <= 0 || n > ringSize {
		n = ringSize
	}
	out := make([]LogEntry, n)
	start := ringNext - n
	if start < 0 {
		start += ringCap
	}
	for i := 0; i < n; i++ {
		out[i] = ringBuf[(start+i)%ringCap]
	}
	return out
}

// GetLevel 获取全局日志级别
func GetLevel() LogLevel {
	return LogLevel(globalLevel.Load())
}

// shouldLog 判断是否应该输出日志
func shouldLog(level LogLevel) bool {
	return int32(level) >= globalLevel.Load()
}

func enqueueFileLog(item asyncLogItem) {
	fileSink.Load().enqueue(item)
}

// DroppedLogs 返回输出队列饱和后丢弃的累计条数。面板环缓冲仍记录日志。
func DroppedLogs() (console, file uint64) {
	return consoleDropped.Load(), fileDropped.Load()
}

func emitLog(level LogLevel, message string) {
	recordLog(level, message)
	item := asyncLogItem{time: time.Now(), levelText: level.String(), message: message}
	if level == FATAL {
		// 致命错误不能因队列饱和丢失；退出路径允许等待控制台。
		consoleSink.Swap(nil).close()
		log.Printf("[%s] %s", item.levelText, message)
	} else {
		consoleSink.Load().enqueue(item)
	}
	if LogLevel(globalLevel.Load()) == DEBUG {
		enqueueFileLog(item)
	}
}

func logf(level LogLevel, format string, args ...interface{}) {
	if shouldLog(level) {
		emitLog(level, fmt.Sprintf(format, args...))
	}
}

func logln(level LogLevel, args ...interface{}) {
	if shouldLog(level) {
		emitLog(level, strings.TrimSpace(fmt.Sprintln(args...)))
	}
}

// Debug 输出调试日志
func Debug(format string, args ...interface{}) {
	logf(DEBUG, format, args...)
}

// Debugln 输出调试日志（无格式）
func Debugln(args ...interface{}) {
	logln(DEBUG, args...)
}

// Info 输出一般信息日志
func Info(format string, args ...interface{}) {
	logf(INFO, format, args...)
}

// Infoln 输出一般信息日志（无格式）
func Infoln(args ...interface{}) {
	logln(INFO, args...)
}

// Warn 输出警告日志
func Warn(format string, args ...interface{}) {
	logf(WARN, format, args...)
}

// Warnln 输出警告日志（无格式）
func Warnln(args ...interface{}) {
	logln(WARN, args...)
}

// Error 输出错误日志
func Error(format string, args ...interface{}) {
	logf(ERROR, format, args...)
}

// Errorln 输出错误日志（无格式）
func Errorln(args ...interface{}) {
	logln(ERROR, args...)
}

// Fatal 输出致命错误日志并退出程序
func Fatal(format string, args ...interface{}) {
	logf(FATAL, format, args...)
	Close()
	os.Exit(1)
}

// Fatalln 输出致命错误日志并退出程序（无格式）
func Fatalln(args ...interface{}) {
	logln(FATAL, args...)
	Close()
	os.Exit(1)
}

// Fatalf 输出致命错误日志并退出程序（兼容标准库）
func Fatalf(format string, args ...interface{}) {
	Fatal(format, args...)
}
