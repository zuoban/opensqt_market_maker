package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestRemovedExecutionSettingsHaveNoEffect(t *testing.T) {
	baseline, err := LoadConfig(writeTempYAML(t, envTestYAML))
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{
		"maker_guard_ticks", "quote_stale_ms", "post_only_retry_min_ms",
		"post_only_retry_max_ms", "post_only_retry_burst", "catch_up_mode",
		"max_active_catch_up_slots", "max_catch_up_slots_per_adjust",
		"max_catch_up_distance_ratio", "near_touch_single_order_ratio", "max_gap_stack_slots",
	}
	body := envTestYAML + "execution:\n"
	for _, key := range keys {
		// Even stale/malformed values must not restore defaults or execution features.
		t.Setenv("OPENSQT_EXECUTION_"+strings.ToUpper(key), "removed")
		body += "  " + key + ": removed\n"
	}
	got, err := LoadConfig(writeTempYAML(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, baseline) {
		t.Fatal("removed execution settings changed the loaded configuration")
	}
}
