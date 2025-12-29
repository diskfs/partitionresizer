package partitionresizer

import (
	"sort"

	"github.com/diskfs/go-diskfs/partition/gpt"
)

type usableBlock struct {
	start int64
	end   int64
	size  int64
}

// calculateResizes determines the necessary resize operations to perform
// based on the current partitions, the partition to shrink (if any), and
// the partitions to grow. Assume we will not be growing the partitions,
// but creating new ones in the free space, copying over and deleting the old ones.
func calculateResizes(size int64, parts []*gpt.Partition, partitionResizes []partitionResizeTarget) (resizes []partitionResizeTarget, err error) {
	// next find the free space on the disk
	var used, unused []usableBlock
	// get a list of all of the used space
	for _, p := range parts {
		used = append(used, usableBlock{start: p.GetStart(), end: p.GetSize() + p.GetStart() - 1, size: p.GetSize()})
	}
	sort.Slice(used, func(i, j int) bool {
		return used[i].start < used[j].start
	})
	unused = computeUnused(size, used)

	// now go through each of the grow partitions and find space for them
	for i, gp := range partitionResizes {
		found := false
		for j := 0; j < len(unused); j++ {
			u := &unused[j]
			available := u.end - u.start + 1
			if available >= gp.target.size {
				// allocate at the start of this gap
				gp.target.start = u.start
				gp.target.end = u.start + gp.target.size - 1
				u.start += gp.target.size
				if u.start > u.end {
					unused = append(unused[:j], unused[j+1:]...)
				}
				found = true
				break
			}
		}
		if !found {
			return nil, NewInsufficientSpaceError(partitionResizes[i].original.label, partitionResizes[i].target.size)
		}
		resizes = append(resizes, gp)
	}

	return resizes, nil
}

func computeUnused(size int64, used []usableBlock) []usableBlock {
	var unused []usableBlock

	var prevEnd int64 = 0

	for _, u := range used {
		// gap before this used block
		if u.start > prevEnd {
			unused = append(unused, usableBlock{
				start: prevEnd + 1,
				end:   u.start - 1,
			})
		}
		prevEnd = u.end
	}

	// gap after last used block
	if prevEnd < size {
		unused = append(unused, usableBlock{
			start: prevEnd + 1,
			end:   size - 1,
		})
	}

	return unused
}
