package symphony

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommitCapabilityReceiptPersistsAndRejectsKeyReuse(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "pika.db")
	engine, err := Open(ctx, databasePath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, Init{Meta: CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, Status{})
	receipt := CommitCapabilityReceipt{WorkID: view.Works[0].ID, CommitSHA: strings.Repeat("a", 40), CommitKeySHA256: strings.Repeat("b", 64), RequestSHA256: strings.Repeat("c", 64)}
	if err := engine.RecordCommitCapability(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	engine, err = Open(ctx, databasePath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if stored, err := engine.CommitCapability(ctx, receipt.WorkID, receipt.CommitSHA); err != nil || stored != receipt {
		t.Fatalf("stored receipt = %+v, %v", stored, err)
	}
	conflict := receipt
	conflict.CommitSHA = strings.Repeat("d", 40)
	var domainErr *DomainError
	if err := engine.RecordCommitCapability(ctx, conflict); !errors.As(err, &domainErr) || domainErr.Code != CodeIdempotencyConflict {
		t.Fatalf("commit capability key reuse error = %v", err)
	}
}
