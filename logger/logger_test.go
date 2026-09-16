package logger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogLevelParsingAndSetting(t *testing.T) {
	defer SetLevel(INFO)

	SetLevel(WARN)
	if GetLevel() != WARN {
		t.Fatalf("GetLevel() = %v, want %v", GetLevel(), WARN)
	}

	if shouldLog(DEBUG) {
		t.Fatal("shouldLog(DEBUG) = true when level is WARN")
	}
	if !shouldLog(WARN) {
		t.Fatal("shouldLog(WARN) = false when level is WARN")
	}
	if !shouldLog(ERROR) {
		t.Fatal("shouldLog(ERROR) = false when level is WARN")
	}
}

func TestRecentLogsRingBuffer(t *testing.T) {
	defer SetLevel(INFO)
	SetLevel(INFO)

	msg := "test_ring_buffer_unique_message"
	Info("%s", msg)

	logs := RecentLogs(10)
	found := false
	for _, l := range logs {
		if strings.Contains(l.Message, msg) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("RecentLogs did not contain logged message %q", msg)
	}
}

func TestAsyncFileLogging(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "logger_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	fileMu.Lock()
	oldLogDir := logDir
	logDir = tempDir
	fileMu.Unlock()
	defer func() {
		fileMu.Lock()
		logDir = oldLogDir
		fileMu.Unlock()
		SetLevel(INFO)
	}()

	SetLevel(DEBUG)

	testMsg := "async_file_log_verification_line"
	Debug("%s", testMsg)

	// Close waits for worker to flush and closes file
	Close()

	today := time.Now().Format("2006-01-02")
	logFilePath := filepath.Join(tempDir, "opensqt-"+today+".log")
	content, err := os.ReadFile(logFilePath)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	if !strings.Contains(string(content), testMsg) {
		t.Fatalf("log file did not contain %q, content:\n%s", testMsg, string(content))
	}
}
