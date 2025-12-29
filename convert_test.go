package partitionresizer

import (
	"strings"
	"testing"

	"github.com/diskfs/go-diskfs/backend"
	"github.com/diskfs/go-diskfs/partition/gpt"
	"github.com/diskfs/go-diskfs/partition/part"
)

// fakeTable implements partition.Table for testing.
// fakeTable implements partition.Table for testing.
// fakeTable implements partition.Table for testing.
type fakeTable struct {
	parts []part.Partition
}

// Type satisfies partition.Table interface.
func (f *fakeTable) Type() string { return "" }

// Write satisfies partition.Table interface.
func (f *fakeTable) Write(_ backend.WritableFile, _ int64) error { return nil }

// Repair satisfies partition.Table interface (no-op).
func (f *fakeTable) Repair(_ uint64) error { return nil }

// Verify satisfies partition.Table interface (no-op).
func (f *fakeTable) Verify(_ backend.File, _ uint64) error { return nil }

// UUID satisfies partition.Table interface (no-op).
func (f *fakeTable) UUID() string { return "" }

func (f *fakeTable) GetPartitions() []part.Partition { return f.parts }

// TestPartitionIdentifiersToData_ByName verifies matching by partition name.
func TestPartitionIdentifiersToData_ByName(t *testing.T) {
	// create one GPT partition
	gp := &gpt.Partition{Start: 100, Size: 50 * 512, Name: "p1", GUID: "uuid1"}
	tbl := &fakeTable{parts: []part.Partition{gp}}
	// simulate sysfs data with same name
	diskData := []partitionData{{name: "p1", label: "L", uuid: "uuid1", start: 100, size: 50 * 512, end: 149, number: 0}}
	pi := NewPartitionIdentifier(IdentifierByName, "p1")
	got, err := partitionIdentifiersToData(tbl, diskData, []PartitionIdentifier{pi})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got[0].start != 100*512 || got[0].size != 50*512 {
		t.Errorf("unexpected partitionData %+v", got[0])
	}
}

// TestPartitionIdentifiersToData_NotFound triggers an error for missing identifier.
func TestPartitionIdentifiersToData_NotFound(t *testing.T) {
	tbl := &fakeTable{parts: []part.Partition{}}
	diskData := []partitionData{}
	pi := NewPartitionIdentifier(IdentifierByLabel, "nope")
	_, err := partitionIdentifiersToData(tbl, diskData, []PartitionIdentifier{pi})
	if err == nil {
		t.Fatal("expected not-found error, got nil")
	}
}

// TestPartitionChangesToResizeTarget_Mismatch verifies length-mismatch error.
func TestPartitionChangesToResizeTarget_Mismatch(t *testing.T) {
	// no diskData => mismatch
	tbl := &fakeTable{parts: []part.Partition{}}
	diskData := []partitionData{}
	pc := NewPartitionChange(IdentifierByName, "p", 123)
	_, err := partitionChangesToResizeTarget(tbl, diskData, []PartitionChange{pc})
	if err == nil || !strings.HasPrefix(err.Error(), "could not find partition for identifier:") {
		t.Fatalf("unexpected error: %v", err)
	}
}
