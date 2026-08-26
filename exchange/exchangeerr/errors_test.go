package exchangeerr

import (
	"errors"
	"fmt"
	"testing"
)

func TestOrderPlacementRejectedContract(t *testing.T) {
	cause := errors.New("request rejected")
	err := WrapOrderPlacementRejected(cause)

	if !IsOrderPlacementRejected(err) {
		t.Fatalf("IsOrderPlacementRejected(%v) = false, want true", err)
	}
	if !errors.Is(err, ErrOrderPlacementRejected) || !errors.Is(err, cause) {
		t.Fatalf("rejected error does not preserve sentinels: %v", err)
	}
	if errors.Is(err, ErrOrderPlacementUnknown) {
		t.Fatalf("rejected error unexpectedly contains UNKNOWN: %v", err)
	}
}

func TestOrderPlacementUnknownTakesPriorityOverRejected(t *testing.T) {
	rejected := WrapOrderPlacementRejected(errors.New("business rejection"))
	unknown := WrapOrderPlacementUnknown(rejected)

	if !errors.Is(unknown, ErrOrderPlacementUnknown) {
		t.Fatalf("error = %v, want UNKNOWN", unknown)
	}
	if IsOrderPlacementRejected(unknown) {
		t.Fatalf("IsOrderPlacementRejected(%v) = true, UNKNOWN must win", unknown)
	}
	if got := WrapOrderPlacementRejected(unknown); got != unknown {
		t.Fatalf("WrapOrderPlacementRejected(UNKNOWN) changed the error: %v", got)
	}

	wrapper := fmt.Errorf("placement: %w", unknown)
	if IsOrderPlacementRejected(wrapper) {
		t.Fatalf("wrapped UNKNOWN was misclassified as rejected: %v", wrapper)
	}
}

func TestLooksLikeAmbiguousOrderPlacementFailure(t *testing.T) {
	for _, message := range []string{
		"Server Timeout", "SERVER_ERROR", "backend error", "service temporarily unavailable", "unknown error",
	} {
		if !LooksLikeAmbiguousOrderPlacementFailure(message) {
			t.Fatalf("LooksLikeAmbiguousOrderPlacementFailure(%q) = false", message)
		}
	}
	for _, message := range []string{"invalid quantity", "insufficient margin", "post only rejected"} {
		if LooksLikeAmbiguousOrderPlacementFailure(message) {
			t.Fatalf("LooksLikeAmbiguousOrderPlacementFailure(%q) = true", message)
		}
	}
}
