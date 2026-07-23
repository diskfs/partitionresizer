package partitionresizer

import (
	"strings"
	"testing"

	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

const (
	MiB = int64(1) << 20
	GiB = int64(1) << 30

	espAGUID = "AD6871EE-31F9-4CF3-9E09-6F7A25C30051"
	imgaGUID = "AD6871EE-31F9-4CF3-9E09-6F7A25C30052"
	imgbGUID = "AD6871EE-31F9-4CF3-9E09-6F7A25C30053"
	confGUID = "AD6871EE-31F9-4CF3-9E09-6F7A25C30054"
	espBGUID = "AD6871EE-31F9-4CF3-9E09-6F7A25C30056"
	p3GUID   = "AD6871EE-31F9-4CF3-9E09-6F7A25C30059"
)

// preEspBTable builds a pre-ESP-B small-geometry GPT: ESP-A(1), IMGA(2),
// IMGB(3), CONFIG(4), P3(9). Sizes are small so the EVE-k specs require grows.
func preEspBTable() *gpt.Table {
	return &gpt.Table{
		LogicalSectorSize: 512,
		Partitions: []*gpt.Partition{
			{Index: 1, Name: "EFI System", Type: gpt.EFISystemPartition, GUID: espAGUID, Start: 2048, Size: uint64(1 * MiB)},
			{Index: 2, Name: "IMGA", Type: gpt.LinuxFilesystem, GUID: imgaGUID, Start: 100000, Size: uint64(300 * MiB)},
			{Index: 3, Name: "IMGB", Type: gpt.LinuxFilesystem, GUID: imgbGUID, Start: 800000, Size: uint64(300 * MiB)},
			{Index: 4, Name: "CONFIG", Type: gpt.LinuxFilesystem, GUID: confGUID, Start: 1500000, Size: uint64(100 * MiB)},
			{Index: 9, Name: "P3", Type: gpt.LinuxFilesystem, GUID: p3GUID, Start: 2000000, Size: uint64(50 * GiB)},
		},
	}
}

// eveKDesired is the target EVE-k geometry: grow ESP-A/IMGA/IMGB, create ESP-B.
func eveKDesired() []PartitionSpec {
	espType := string(gpt.EFISystemPartition)
	return []PartitionSpec{
		{Label: "EFI System", TypeGUID: espType, GUID: espAGUID, Index: 1, MinSize: 2 * GiB},
		{Label: "IMGA", TypeGUID: string(gpt.LinuxFilesystem), GUID: imgaGUID, Index: 2, MinSize: 10 * GiB},
		{Label: "IMGB", TypeGUID: string(gpt.LinuxFilesystem), GUID: imgbGUID, Index: 3, MinSize: 10 * GiB},
		{Label: "EFI System", TypeGUID: espType, GUID: espBGUID, Index: 7, MinSize: 2 * GiB, FS: FSFAT32},
	}
}

// byOrigNumber / byCreateGUID locate a target in the plan result.
func byOrigNumber(ts []partitionResizeTarget, n int) (partitionResizeTarget, bool) {
	for _, t := range ts {
		if !t.create && t.original.number == n {
			return t, true
		}
	}
	return partitionResizeTarget{}, false
}

func byCreateGUID(ts []partitionResizeTarget, guid string) (partitionResizeTarget, bool) {
	for _, t := range ts {
		if t.create && strings.EqualFold(t.target.uuid, guid) {
			return t, true
		}
	}
	return partitionResizeTarget{}, false
}

// TestPlanPartitionSpecs_GrowCreate checks the diff stage: the EVE-k specs yield
// three grows (numbers and target sizes preserved from the source) and one
// create for ESP-B at #7.
func TestPlanPartitionSpecs_GrowCreate(t *testing.T) {
	grows, creates, err := planPartitionSpecs(preEspBTable(), eveKDesired())
	if err != nil {
		t.Fatalf("planPartitionSpecs: %v", err)
	}
	if len(grows) != 3 || len(creates) != 1 {
		t.Fatalf("got %d grows + %d creates, want 3 + 1", len(grows), len(creates))
	}

	for _, want := range []struct {
		num  int
		size int64
	}{{1, 2 * GiB}, {2, 10 * GiB}, {3, 10 * GiB}} {
		g, ok := byOrigNumber(grows, want.num)
		if !ok {
			t.Fatalf("no grow target for partition %d", want.num)
		}
		if g.target.size != want.size {
			t.Errorf("partition %d grow target size = %d, want %d", want.num, g.target.size, want.size)
		}
		if g.target.number != want.num {
			t.Errorf("partition %d grow target number = %d, want preserved %d", want.num, g.target.number, want.num)
		}
	}

	c, ok := byCreateGUID(creates, espBGUID)
	if !ok {
		t.Fatalf("no create target for ESP-B (GUID %s)", espBGUID)
	}
	if c.target.number != 7 {
		t.Errorf("ESP-B create number = %d, want 7", c.target.number)
	}
	if c.target.size != 2*GiB {
		t.Errorf("ESP-B create size = %d, want %d", c.target.size, 2*GiB)
	}
	if c.fsType != FSFAT32 {
		t.Errorf("ESP-B create fsType = %v, want FSFAT32", c.fsType)
	}
	if c.target.label != "EFI System" {
		t.Errorf("ESP-B create label = %q, want \"EFI System\"", c.target.label)
	}
}

// TestPlanPartitionSpecs_MatchByLabel checks the folded-in by-label grow: a spec
// with Match locates the partition by label and grows it, no GUID needed.
func TestPlanPartitionSpecs_MatchByLabel(t *testing.T) {
	desired := []PartitionSpec{{Match: NewPartitionIdentifier(IdentifierByLabel, "IMGA"), MinSize: 10 * GiB}}
	grows, creates, err := planPartitionSpecs(preEspBTable(), desired)
	if err != nil {
		t.Fatalf("planPartitionSpecs: %v", err)
	}
	if len(grows) != 1 || len(creates) != 0 {
		t.Fatalf("got %d grows + %d creates, want 1 + 0", len(grows), len(creates))
	}
	if grows[0].original.number != 2 || grows[0].target.size != 10*GiB {
		t.Errorf("unexpected grow %+v", grows[0])
	}
}

// TestPlanPartitionSpecs_MatchAbsentErrors verifies a Match target that does not
// exist is an error, never a create.
func TestPlanPartitionSpecs_MatchAbsentErrors(t *testing.T) {
	desired := []PartitionSpec{{Match: NewPartitionIdentifier(IdentifierByLabel, "NOPE"), MinSize: 1 * GiB}}
	if _, _, err := planPartitionSpecs(preEspBTable(), desired); err == nil {
		t.Fatal("expected error for absent Match target, got nil")
	}
}

// TestPlanApply_ExplicitShrink checks the full plan with an explicit-size shrink:
// P3 is reduced to 20 GiB in place while the grows/creates are allocated.
func TestPlanApply_ExplicitShrink(t *testing.T) {
	table := preEspBTable()
	d := &disk.Disk{Size: 60 * GiB}
	shrink := &ShrinkSpec{ID: NewPartitionIdentifier(IdentifierByUUID, p3GUID), Size: 20 * GiB}
	resizes, err := planApply(d, table, eveKDesired(), shrink)
	if err != nil {
		t.Fatalf("planApply: %v", err)
	}
	s, ok := byOrigNumber(resizes, 9)
	if !ok {
		t.Fatalf("no shrink target for P3 (partition 9)")
	}
	if s.target.size != 20*GiB {
		t.Errorf("P3 shrink target size = %d, want %d", s.target.size, 20*GiB)
	}
	if s.target.size >= s.original.size {
		t.Errorf("P3 target %d not smaller than original %d", s.target.size, s.original.size)
	}
	if s.target.start != s.original.start {
		t.Errorf("P3 shrink not in place: start %d != %d", s.target.start, s.original.start)
	}
}

// TestPlanApply_ShrinkToFit checks shrink-to-fit: on a disk with no room for the
// grows/creates, P3 (Size 0 shrink) is reduced by exactly the total requested
// size, rounded up to a whole GB.
func TestPlanApply_ShrinkToFit(t *testing.T) {
	table := preEspBTable()
	// Disk barely larger than P3's end, so the grows/creates cannot fit without
	// shrinking: they need 2+10+10 GiB (grows) + 2 GiB (create) = 24 GiB.
	d := &disk.Disk{Size: 55 * GiB}
	shrink := &ShrinkSpec{ID: NewPartitionIdentifier(IdentifierByUUID, p3GUID)} // Size 0 => to-fit
	resizes, err := planApply(d, table, eveKDesired(), shrink)
	if err != nil {
		t.Fatalf("planApply: %v", err)
	}
	s, ok := byOrigNumber(resizes, 9)
	if !ok {
		t.Fatalf("no shrink target for P3 (partition 9)")
	}
	if want := 50*GiB - 24*GiB; s.target.size != want {
		t.Errorf("P3 shrink-to-fit target size = %d, want %d", s.target.size, want)
	}
}

func TestPlanPartitionSpecs_IdempotentNoOp(t *testing.T) {
	// A disk already at the EVE-k geometry (ESP-A 2G, IMGA/IMGB 10G, ESP-B present)
	// -> nothing to do.
	table := &gpt.Table{
		LogicalSectorSize: 512,
		Partitions: []*gpt.Partition{
			{Index: 1, Name: "EFI System", Type: gpt.EFISystemPartition, GUID: espAGUID, Start: 2048, Size: uint64(2 * GiB)},
			{Index: 2, Name: "IMGA", Type: gpt.LinuxFilesystem, GUID: imgaGUID, Start: 5000000, Size: uint64(10 * GiB)},
			{Index: 3, Name: "IMGB", Type: gpt.LinuxFilesystem, GUID: imgbGUID, Start: 30000000, Size: uint64(10 * GiB)},
			{Index: 7, Name: "EFI System", Type: gpt.EFISystemPartition, GUID: espBGUID, Start: 60000000, Size: uint64(2 * GiB)},
		},
	}
	grows, creates, err := planPartitionSpecs(table, eveKDesired())
	if err != nil {
		t.Fatalf("planPartitionSpecs: %v", err)
	}
	if len(grows)+len(creates) != 0 {
		t.Fatalf("expected no targets on an already-satisfied disk, got %d grows + %d creates", len(grows), len(creates))
	}
}

func TestPlanPartitionSpecs_NeverShrinkViaDesired(t *testing.T) {
	// IMGA is already LARGER than MinSize; the desired spec must not shrink it.
	table := preEspBTable()
	table.Partitions[1].Size = uint64(12 * GiB) // IMGA already 12 GiB > 10 GiB target
	desired := []PartitionSpec{
		{Label: "IMGA", TypeGUID: string(gpt.LinuxFilesystem), GUID: imgaGUID, Index: 2, MinSize: 10 * GiB},
	}
	grows, creates, err := planPartitionSpecs(table, desired)
	if err != nil {
		t.Fatalf("planPartitionSpecs: %v", err)
	}
	if len(grows)+len(creates) != 0 {
		t.Fatalf("desired spec shrank a larger partition; got %d grows + %d creates", len(grows), len(creates))
	}
}

func TestPlanPartitionSpecs_CreateSlotOccupied(t *testing.T) {
	// Requesting create at #4, which is occupied (CONFIG) by a different GUID.
	desired := []PartitionSpec{
		{Label: "EFI System", TypeGUID: string(gpt.EFISystemPartition), GUID: espBGUID, Index: 4, MinSize: 2 * GiB, FS: FSFAT32},
	}
	if _, _, err := planPartitionSpecs(preEspBTable(), desired); err == nil {
		t.Fatal("expected error creating at an occupied slot, got nil")
	}
}

func TestPlanPartitionSpecs_TypeMismatchAborts(t *testing.T) {
	// GUID matches ESP-A but the spec claims the wrong type -> abort.
	desired := []PartitionSpec{
		{Label: "EFI System", TypeGUID: string(gpt.LinuxFilesystem), GUID: espAGUID, Index: 1, MinSize: 2 * GiB},
	}
	if _, _, err := planPartitionSpecs(preEspBTable(), desired); err == nil {
		t.Fatal("expected type-mismatch error, got nil")
	}
}
