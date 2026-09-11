package config

import (
	"strings"
	"testing"
)

func TestTelegramConfigValidation(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  TelegramConfig
		want string
	}{
		{name: "disabled by default"},
		{name: "missing token", cfg: TelegramConfig{Enabled: true, ChatID: "-100123"}, want: "telegram.bot_token"},
		{name: "missing chat", cfg: TelegramConfig{Enabled: true, BotToken: "test-token"}, want: "telegram.chat_id"},
		{name: "blank token", cfg: TelegramConfig{Enabled: true, BotToken: " \n", ChatID: "1"}, want: "telegram.bot_token"},
		{name: "configured", cfg: TelegramConfig{Enabled: true, BotToken: " test-token ", ChatID: " -100123 "}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validTradingConfig()
			cfg.Telegram = tt.cfg
			err := cfg.Validate()
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("Validate() = %v, want %s", err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Telegram.BotToken != strings.TrimSpace(tt.cfg.BotToken) || cfg.Telegram.ChatID != strings.TrimSpace(tt.cfg.ChatID) {
				t.Fatal("Telegram credentials were not trimmed")
			}
		})
	}
}

func TestTelegramYAMLAndEnvOverrides(t *testing.T) {
	path := writeTempYAML(t, envTestYAML+`
telegram:
  enabled: true
  bot_token: yaml-token
  chat_id: "-100123"
`)
	for _, key := range []string{"OPENSQT_TELEGRAM_ENABLED", "OPENSQT_TELEGRAM_BOT_TOKEN", "OPENSQT_TELEGRAM_CHAT_ID"} {
		t.Setenv(key, "")
	}
	cfg, err := LoadConfig(path)
	if err != nil || !cfg.Telegram.Enabled || cfg.Telegram.BotToken != "yaml-token" || cfg.Telegram.ChatID != "-100123" {
		t.Fatalf("Telegram YAML configuration failed: %v", err)
	}
	t.Setenv("OPENSQT_TELEGRAM_ENABLED", "true")
	t.Setenv("OPENSQT_TELEGRAM_BOT_TOKEN", " env-token ")
	t.Setenv("OPENSQT_TELEGRAM_CHAT_ID", "@test_channel")
	cfg, err = LoadConfig(writeTempYAML(t, envTestYAML))
	if err != nil || !cfg.Telegram.Enabled || cfg.Telegram.BotToken != "env-token" || cfg.Telegram.ChatID != "@test_channel" {
		t.Fatalf("Telegram environment configuration failed: %v", err)
	}
	t.Setenv("OPENSQT_TELEGRAM_ENABLED", "false")
	cfg, err = LoadConfig(path)
	if err != nil || cfg.Telegram.Enabled || cfg.Telegram.BotToken != "env-token" || cfg.Telegram.ChatID != "@test_channel" {
		t.Fatalf("Telegram environment override failed: %v", err)
	}
	t.Setenv("OPENSQT_TELEGRAM_ENABLED", "invalid")
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "OPENSQT_TELEGRAM_ENABLED") {
		t.Fatalf("invalid Telegram enabled flag error = %v", err)
	}
}
