package docparser

import (
	"context"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestMarkerDoclingEnginesRegistered(t *testing.T) {
	names := make(map[string]bool)
	for _, e := range localEngines {
		names[e.Name()] = true
	}
	for _, want := range []string{"builtin", "simple", "weknoracloud", "mineru", "mineru_cloud", "paddleocr_vl", "paddleocr_vl_cloud", "marker", "docling"} {
		if !names[want] {
			t.Errorf("engine %q not registered; got %v", want, names)
		}
	}
}

func TestMarkerEngineAvailability(t *testing.T) {
	e := &markerEngine{}

	// No endpoint configured → unavailable with a helpful reason
	avail, reason := e.CheckAvailable(false, map[string]string{})
	if avail {
		t.Fatal("marker should be unavailable without endpoint")
	}
	if !strings.Contains(reason, "not configured") {
		t.Fatalf("unexpected reason: %q", reason)
	}

	// Unreachable endpoint → unavailable with a network reason
	avail, reason = e.CheckAvailable(false, map[string]string{"marker_endpoint": "http://127.0.0.1:1"})
	if avail {
		t.Fatal("marker should be unavailable for unreachable endpoint")
	}
	if reason == "" {
		t.Fatal("expected non-empty reason for unreachable endpoint")
	}
}

func TestDoclingEngineAvailability(t *testing.T) {
	e := &doclingEngine{}

	avail, reason := e.CheckAvailable(false, map[string]string{})
	if avail {
		t.Fatal("docling should be unavailable without endpoint")
	}
	if !strings.Contains(reason, "not configured") {
		t.Fatalf("unexpected reason: %q", reason)
	}

	avail, reason = e.CheckAvailable(false, map[string]string{"docling_endpoint": "http://127.0.0.1:1"})
	if avail {
		t.Fatal("docling should be unavailable for unreachable endpoint")
	}
	if reason == "" {
		t.Fatal("expected non-empty reason for unreachable endpoint")
	}
}

func TestMarkerReaderMissingConfig(t *testing.T) {
	r := NewMarkerReader(map[string]string{})
	result, err := r.Read(context.Background(), &types.ReadRequest{FileName: "test.pdf"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Error == "" {
		t.Fatal("expected error result for unconfigured marker endpoint")
	}
}

func TestDoclingReaderMissingConfig(t *testing.T) {
	r := NewDoclingReader(map[string]string{})
	result, err := r.Read(context.Background(), &types.ReadRequest{FileName: "test.pdf"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Error == "" {
		t.Fatal("expected error result for unconfigured docling endpoint")
	}
}

func TestListAllEnginesIncludesMarkerDocling(t *testing.T) {
	engines := ListAllEngines(true, map[string]string{}, nil)
	var marker, docling *bool
	for i := range engines {
		switch engines[i].Name {
		case "marker":
			v := engines[i].Available
			marker = &v
		case "docling":
			v := engines[i].Available
			docling = &v
		}
	}
	if marker == nil || docling == nil {
		t.Fatalf("marker/docling missing from ListAllEngines: %+v", engines)
	}
	// Without endpoints they must be reported unavailable but listed.
	if *marker || *docling {
		t.Fatal("marker/docling must be unavailable without endpoint overrides")
	}
}
