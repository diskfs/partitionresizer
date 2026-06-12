package partitionresizer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"strings"
)

// The helpers in this file parse GPT structures by reading raw disk sectors
// rather than going through go-diskfs. That is deliberate: the chaos tests
// inspect the disk immediately after a SIGKILL, when the GPT may be torn -- a
// half-written entry array whose CRC fails, or a backup that is newer than the
// primary. go-diskfs's gpt.Read validates the header and entry-array CRCs and
// silently falls back to the backup GPT when the primary fails
// (Table.RecoveredFromBackup), so it would heal away exactly the torn and
// primary/backup-divergent states these tests exist to observe. Reading the
// sectors ourselves is the only way to see them. Tests that operate on a
// known-good disk (gptByName, gptDump) use go-diskfs as normal.

// readAt reads n bytes at off, returning nil on a short read or non-EOF error,
// so callers can treat a torn or truncated table as simply absent.
func readAt(f *os.File, off, n int64) []byte {
	if off < 0 || n <= 0 {
		return nil
	}
	b := make([]byte, n)
	if _, e := f.ReadAt(b, off); e != nil && e != io.EOF {
		return nil
	}
	return b
}

// gptHeaderInfo parses a 512-byte GPT header sector and reports whether its
// signature and self-CRC are valid, plus the fields needed to locate and check
// its partition-entry array. A GPT header stores a CRC32 of itself (with the
// CRC field zeroed) and a separate CRC32 of the entry array.
func gptHeaderInfo(hdr []byte) (sigOK, crcOK bool, entriesCRC uint32, entriesLBA int64, num, esize uint32) {
	if len(hdr) < 92 || string(hdr[0:8]) != "EFI PART" {
		return false, false, 0, 0, 0, 0
	}
	sigOK = true
	hsize := binary.LittleEndian.Uint32(hdr[12:16])
	if hsize < 92 || int(hsize) > len(hdr) {
		hsize = 92
	}
	stored := binary.LittleEndian.Uint32(hdr[16:20])
	tmp := make([]byte, hsize)
	copy(tmp, hdr[:hsize])
	binary.LittleEndian.PutUint32(tmp[16:20], 0) // header CRC is computed with this field zeroed
	crcOK = crc32.ChecksumIEEE(tmp) == stored
	entriesCRC = binary.LittleEndian.Uint32(hdr[88:92])
	entriesLBA = int64(binary.LittleEndian.Uint64(hdr[72:80]))
	num = binary.LittleEndian.Uint32(hdr[80:84])
	esize = binary.LittleEndian.Uint32(hdr[84:88])
	return
}

// gptEntryName decodes a GPT entry's 72-byte UTF-16LE name (ASCII labels only).
func gptEntryName(b []byte) string {
	var sb strings.Builder
	for i := 0; i+1 < len(b); i += 2 {
		c := uint16(b[i]) | uint16(b[i+1])<<8
		if c == 0 {
			break
		}
		sb.WriteRune(rune(c))
	}
	return sb.String()
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// gptPartitions parses the primary GPT entry array (LBA 1 header) and returns a
// map of partition label -> [firstLBA, lastLBA]. Used to locate a grow target
// and its source on disk without depending on the in-memory fixture layout.
func gptPartitions(f *os.File) map[string][2]int64 {
	sig, _, _, eLBA, num, esize := gptHeaderInfo(readAt(f, 512, 512))
	if !sig || num == 0 || esize < 128 {
		return nil
	}
	ent := readAt(f, eLBA*512, int64(num)*int64(esize))
	if ent == nil {
		return nil
	}
	out := map[string][2]int64{}
	for i := 0; i < int(num); i++ {
		e := ent[i*int(esize) : i*int(esize)+128]
		if allZero(e[0:16]) { // empty type GUID => unused slot
			continue
		}
		first := int64(binary.LittleEndian.Uint64(e[32:40]))
		last := int64(binary.LittleEndian.Uint64(e[40:48]))
		out[gptEntryName(e[56:128])] = [2]int64{first, last}
	}
	return out
}

// gptIntegrity inspects the on-disk GPT of a 512-byte-sector image and returns a
// one-line summary: whether the primary (LBA 1) and backup (last LBA) header
// CRCs are valid, whether each header's entry-array CRC matches, and whether the
// primary and backup entry arrays are byte-identical. After a mid-updatePartitions
// kill this shows whether the kill left a CRC error or a primary/backup mismatch
// (each table spans multiple sectors: 1 header + 32 entry sectors).
func gptIntegrity(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "gpt:open-error:" + err.Error()
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return "gpt:stat-error"
	}
	nSec := fi.Size() / 512
	pSig, pCRC, pECRC, pELBA, pN, pE := gptHeaderInfo(readAt(f, 1*512, 512))        // primary header at LBA 1
	bSig, bCRC, bECRC, bELBA, bN, bE := gptHeaderInfo(readAt(f, (nSec-1)*512, 512)) // backup header at last LBA
	var pent, bent []byte
	if pSig {
		pent = readAt(f, pELBA*512, int64(pN)*int64(pE))
	}
	if bSig {
		bent = readAt(f, bELBA*512, int64(bN)*int64(bE))
	}
	pEntCRCok := pent != nil && crc32.ChecksumIEEE(pent) == pECRC
	bEntCRCok := bent != nil && crc32.ChecksumIEEE(bent) == bECRC
	entriesMatch := pent != nil && bent != nil && bytes.Equal(pent, bent)
	return fmt.Sprintf("gpt: primHdrCRC=%v backupHdrCRC=%v primEntCRC=%v backupEntCRC=%v entriesMatch=%v",
		pCRC, bCRC, pEntCRCok, bEntCRCok, entriesMatch)
}
