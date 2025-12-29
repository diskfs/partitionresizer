package partitionresizer

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	sysDefaultPath = "/sys"
)

// findDisks find all disks and their partitions, including reference name and parition position.
// If a specific disk is given, only that disk is returned.
func findDisks(disk, syspath string) (map[string][]partitionData, error) {
	var (
		candidates []os.DirEntry
		err        error
	)
	if syspath == "" {
		syspath = sysDefaultPath
	}
	sysClassBlockPath := filepath.Join(syspath, "class", "block")
	// which candidates to check, depends if we were given a specific disk or not
	if disk != "" {
		// only check the given disk
		base := filepath.Base(disk)
		info, err := os.Stat(filepath.Join(sysClassBlockPath, base))
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, os.ErrNotExist
		}
		de, err := os.ReadDir(sysClassBlockPath)
		if err != nil {
			return nil, err
		}
		found := false
		for _, d := range de {
			if d.Name() == base {
				candidates = append(candidates, d)
				found = true
				break
			}
		}
		if !found {
			return nil, os.ErrNotExist
		}
	} else {
		// check all findDisks
		candidates, err = os.ReadDir(sysClassBlockPath)
		if err != nil {
			return nil, err
		}
	}
	var allDisks = make(map[string][]partitionData)
	for _, candidate := range candidates {
		if !candidate.IsDir() {
			continue
		}
		// we only care about disk types
		// - has partition child, is a partition
		// - has a loop child, is a loop
		// - has a dm child, is a device-mapper
		// - starts with "ram", is a ramdisk
		// - has a comp_algorithm child, is a zramdisk
		// - else is just a disk
		children, err := os.ReadDir(filepath.Join(sysClassBlockPath, candidate.Name()))
		if err != nil {
			return nil, err
		}
		isDisk := true
		for _, child := range children {
			name := child.Name()
			switch {
			case name == "partition":
				isDisk = false
			case name == "loop":
				isDisk = false
			case name == "dm":
				isDisk = false
			case len(name) >= 3 && name[0:3] == "ram":
				isDisk = false
			case name == "comp_algorithm":
				isDisk = false
			default:
				continue
			}
			if !isDisk {
				break
			}
		}
		// if we got this far, nothing caused it to break, so it's a disk
		if !isDisk {
			continue
		}
		// get the logical block size
		blockSize, err := readSysIntValue(filepath.Join(sysClassBlockPath, candidate.Name(), "queue", "logical_block_size"))
		if err != nil {
			return nil, err
		}

		// find all of the child partitions, and store them in the right order
		for _, child := range children {
			if !child.IsDir() {
				continue
			}
			name := child.Name()
			// find partition children
			partitionInfoFile := filepath.Join(sysClassBlockPath, candidate.Name(), name, "partition")
			if _, err := os.Stat(partitionInfoFile); err != nil {
				// not a partition
				continue
			}
			// read partition info: number, size, start
			id, err := readSysIntValue(partitionInfoFile)
			if err != nil {
				return nil, err
			}
			size, err := readSysIntValue(filepath.Join(sysClassBlockPath, candidate.Name(), name, "size"))
			if err != nil {
				return nil, err
			}
			start, err := readSysIntValue(filepath.Join(sysClassBlockPath, candidate.Name(), name, "start"))
			if err != nil {
				return nil, err
			}
			end := size - start + 1
			// read from uevent to get name
			ueventPath := filepath.Join(sysClassBlockPath, candidate.Name(), name, "uevent")
			ueventData, err := os.ReadFile(ueventPath)
			if err != nil {
				return nil, err
			}
			ue := parseKeyValueLines(ueventData)
			label := ue["PARTNAME"]
			pd := partitionData{
				name:   name,
				label:  label,
				size:   size * blockSize,
				start:  start * blockSize,
				end:    end * blockSize,
				number: int(id),
			}
			allDisks[candidate.Name()] = append(allDisks[candidate.Name()], pd)
		}
	}
	return allDisks, nil
}

// filterDisksByPartitions returns all of the disks that have all of the given partition identifiers
func filterDisksByPartitions(disks map[string][]partitionData, partIdentifiers []PartitionIdentifier) ([]string, error) {
	var found []string
	for disk, parts := range disks {
		matchedAll := true
		for _, pi := range partIdentifiers {
			matched := false
			for _, p := range parts {
				switch pi.By() {
				case IdentifierByName:
					if p.name == pi.Value() {
						matched = true
					}
				case IdentifierByLabel:
					if p.label == pi.Value() {
						matched = true
					}
				case IdentifierByUUID:
					if p.uuid == pi.Value() {
						matched = true
					}
				}
				if matched {
					break
				}
			}
			if !matched {
				matchedAll = false
				break
			}
		}
		if matchedAll {
			found = append(found, disk)
		}
	}
	return found, nil
}

func readSysIntValue(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	// trim newline or carriage return
	s := string(data)
	if len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return strconv.ParseInt(s, 10, 64)
}

// parseKeyValueLines parses the contents of key=value lines
// (KEY=VALUE\n...) into a map.
// Lines without '=' are ignored.
func parseKeyValueLines(data []byte) map[string]string {
	m := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		m[key] = val
	}
	return m
}
