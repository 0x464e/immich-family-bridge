package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/0x464e/immich-family-bridge/internal/reconcile"
)

func TestPollingRunsImmediatelyAndContinues(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := make(chan struct{}, 3)
	done := make(chan struct{})
	go func() {
		runPolling(ctx, 10*time.Millisecond, func() { calls <- struct{}{} })
		close(done)
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-calls:
		case <-time.After(time.Second):
			t.Fatal("polling did not run immediately and again after the interval")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("polling did not stop after cancellation")
	}
}

func TestDryRunLogsProposedActionsAndSummary(t *testing.T) {
	var output bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&output, nil))
	logDryRunActions(log, []reconcile.Action{
		{Kind: "discover_origin", AlbumID: "album-1", AlbumName: "Together", MemberID: "alice", ImmichAssetID: "asset-1"},
		{Kind: "link_and_import", AlbumID: "album-1", AlbumName: "Together", MemberID: "bob", SourceMemberID: "alice", ImmichAssetID: "asset-1"},
	})
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d log lines, want 3: %s", len(lines), output.String())
	}
	var records []map[string]any
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if records[0]["msg"] != "dry-run: new source asset found in album" || records[0]["immich_asset_id"] != "asset-1" || records[0]["album_name"] != "Together" {
		t.Fatalf("source discovery not reported: %v", records[0])
	}
	if records[1]["msg"] != "dry-run: would share asset with member through hardlink and import" || records[1]["member_id"] != "bob" || records[1]["source_member_id"] != "alice" {
		t.Fatalf("recipient action not reported: %v", records[1])
	}
	if records[2]["msg"] != "dry-run cycle complete" || records[2]["action_count"] != float64(2) {
		t.Fatalf("cycle summary not reported: %v", records[2])
	}
}
