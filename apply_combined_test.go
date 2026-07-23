package partitionresizer

import (
	"strings"
	"testing"

	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

// TestApplyShrinkGrowCreate exercises Apply doing, in one call, a shrink, three
// grows, and a create -- an empty FAT32 at a specific reserved partition number.
// It asserts the relocated grows skip the number reserved for the create, the
// created partition is published (only) by the final write at that number, grown
// and shrunk content is preserved, an untouched partition is left alone, and a
// rerun is idempotent. The sample layout's labels are just example data; the
// resizer is label- and role-agnostic.
func TestApplyShrinkGrowCreate(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end EVE conversion")
	}
	fx := buildSampleLayout(t)
	before := gptByName(t, fx.path)
	espType := string(gpt.EFISystemPartition)
	linType := string(gpt.LinuxFilesystem)

	desired := []PartitionSpec{
		{Label: "EFI System", TypeGUID: espType, GUID: before["EFI System"].GUID, Index: espIndex, MinSize: 96 * MB},
		{Label: "IMGA", TypeGUID: linType, GUID: before["IMGA"].GUID, Index: imgaIndex, MinSize: 200 * MB},
		{Label: "IMGB", TypeGUID: linType, GUID: before["IMGB"].GUID, Index: imgbIndex, MinSize: 200 * MB},
		{Label: "EFI System", TypeGUID: espType, GUID: espBGUID, Index: 7, MinSize: 20 * MB, FS: FSFAT32},
	}
	shrink := &ShrinkSpec{ID: NewPartitionIdentifier(IdentifierByUUID, before["P3"].GUID), Size: (defaultP3MB - 600) * MB}

	if err := Apply(fx.path, desired, shrink, false, false); err != nil {
		t.Fatalf("Apply EVE conversion: %v", err)
	}

	// ESP-B: created at #7, empty FAT32, fresh-install GUID, EFI System label/type.
	espb := readPartition(t, fx.path, 7)
	if !strings.EqualFold(espb.GUID, espBGUID) {
		t.Errorf("ESP-B GUID = %s, want %s", espb.GUID, espBGUID)
	}
	if espb.Name != "EFI System" {
		t.Errorf("ESP-B label = %q, want \"EFI System\"", espb.Name)
	}
	if !strings.EqualFold(string(espb.Type), espType) {
		t.Errorf("ESP-B type = %s, want EFI System type", espb.Type)
	}
	if espb.GetSize() < 20*MB {
		t.Errorf("ESP-B size = %d, want >= %d", espb.GetSize(), 20*MB)
	}

	// ESP-A: grown in place-preserving fashion, number+GUID+content kept.
	espa := readPartition(t, fx.path, espIndex)
	if !strings.EqualFold(espa.GUID, before["EFI System"].GUID) {
		t.Errorf("ESP-A GUID changed: %s -> %s", before["EFI System"].GUID, espa.GUID)
	}
	if espa.GetSize() < 96*MB {
		t.Errorf("ESP-A size = %d, want >= %d", espa.GetSize(), 96*MB)
	}
	if got := readPartitionFile(t, fx.path, espIndex, "/esp-marker.txt"); got != "esp content" {
		t.Errorf("ESP-A content lost: marker = %q", got)
	}

	// IMGA/IMGB: grown, numbers preserved, raw squashfs content intact.
	imga := readPartition(t, fx.path, imgaIndex)
	imgb := readPartition(t, fx.path, imgbIndex)
	if imga.GetSize() < 200*MB || imgb.GetSize() < 200*MB {
		t.Errorf("IMG sizes: IMGA=%d IMGB=%d, want >= %d each", imga.GetSize(), imgb.GetSize(), 200*MB)
	}
	if partitionRegionSum(t, fx.path, imga.Start, 1*MB) != fx.imgaSum {
		t.Error("IMGA content not preserved through grow")
	}
	if partitionRegionSum(t, fx.path, imgb.Start, 1*MB) != fx.imgbSum {
		t.Error("IMGB content not preserved through grow")
	}

	// P3: shrunk to the requested size, number preserved, data intact.
	p3 := readPartition(t, fx.path, p3Index)
	if got := int64(p3.GetSize()); got != (defaultP3MB-600)*MB {
		t.Errorf("P3 size = %d, want %d", got, (defaultP3MB-600)*MB)
	}
	if got := readPartitionFile(t, fx.path, p3Index, "/p3-marker.txt"); got != "persist content" {
		t.Errorf("P3 data lost: marker = %q", got)
	}

	// The created ESP-B carries an empty FAT32.
	fs := getFilesystem(t, fx.path, 7)
	if fs.Type() != filesystem.TypeFat32 {
		t.Errorf("ESP-B filesystem = %v, want FAT32", fs.Type())
	}

	// Idempotent: rerunning the same conversion is a no-op.
	if err := Apply(fx.path, desired, shrink, false, false); err != nil {
		t.Fatalf("Apply rerun: %v", err)
	}
	if espb2 := readPartition(t, fx.path, 7); !strings.EqualFold(espb2.GUID, espBGUID) {
		t.Errorf("after rerun ESP-B GUID = %s, want %s", espb2.GUID, espBGUID)
	}
}
