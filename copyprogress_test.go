package partitionresizer

import (
	"bytes"
	"os"
	"sort"
	"strings"
)

// copyDestState classifies each "<label>_resized2" grow target's data region to
// distinguish "nothing written" from a partial or complete copy -- the certain
// empty-vs-something signal the KILL_STEP marker alone cannot give. For a
// raw-copied source (squashfs/unknown) it compares the target's head and tail to
// the source: a head mismatch means nothing was copied, head match + tail
// mismatch means partial, both match means complete. For a filesystem copy
// (FAT32/ext4) a byte compare is meaningless (fresh metadata differs), so it
// looks for that filesystem's on-disk signature, which CreateFilesystem writes
// before any file data -- present means something was written.
//
// It locates partitions with gptPartitions (a raw entry-array read) for the same
// reason as the other helpers in gpt_raw_test.go: it runs on a post-kill disk
// whose table may be torn.
func copyDestState(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "copydest:open-error"
	}
	defer func() { _ = f.Close() }()
	parts := gptPartitions(f)
	if parts == nil {
		return "copydest:no-gpt"
	}
	const chunk = 4096
	var states []string
	for label, tgt := range parts {
		if !strings.HasSuffix(label, alternateLabelSuffix) {
			continue
		}
		src, ok := parts[strings.TrimSuffix(label, alternateLabelSuffix)]
		if !ok {
			states = append(states, label+"=no-source")
			continue
		}
		srcStart := src[0] * 512
		srcLen := (src[1] - src[0] + 1) * 512
		tgtStart := tgt[0] * 512
		srcHead, tgtHead := readAt(f, srcStart, chunk), readAt(f, tgtStart, chunk)
		var state string
		switch {
		case srcHead == nil || tgtHead == nil:
			state = "read-error"
		case len(srcHead) >= 4 && string(srcHead[0:4]) == "hsqs": // squashfs => raw copy
			switch {
			case !bytes.Equal(srcHead, tgtHead):
				state = "empty"
			case srcLen <= chunk:
				state = "complete"
			default:
				to := srcLen - chunk
				if bytes.Equal(readAt(f, srcStart+to, chunk), readAt(f, tgtStart+to, chunk)) {
					state = "complete"
				} else {
					state = "partial"
				}
			}
		default: // FAT32/ext4 => filesystem copy; detect the target's fs signature
			fat := len(tgtHead) >= 512 && tgtHead[510] == 0x55 && tgtHead[511] == 0xAA
			ext4sb := readAt(f, tgtStart+1024, 512)
			ext4 := len(ext4sb) >= 58 && ext4sb[56] == 0x53 && ext4sb[57] == 0xEF
			if fat || ext4 {
				state = "written"
			} else {
				state = "empty"
			}
		}
		states = append(states, label+"="+state)
	}
	sort.Strings(states)
	return "copydest: " + strings.Join(states, " ")
}
