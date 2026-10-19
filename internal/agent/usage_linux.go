//go:build linux

package agent

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func readHost(dataDir string) (hostReading, error) {
	var reading hostReading
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return reading, err
	}
	if reading.cpu, err = parseProcStat(stat); err != nil {
		return reading, err
	}
	meminfo, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return reading, err
	}
	if reading.memoryTotal, reading.memoryFree, err = parseMeminfo(meminfo); err != nil {
		return reading, err
	}
	var filesystem unix.Statfs_t
	if err := unix.Statfs(dataDir, &filesystem); err != nil {
		return reading, err
	}
	blockSize := uint64(filesystem.Bsize)
	reading.diskTotal = filesystem.Blocks * blockSize
	reading.diskAvailable = filesystem.Bavail * blockSize
	return reading, nil
}

// parseProcStat reads the aggregate cpu line; idle includes iowait.
func parseProcStat(contents []byte) (cpuTimes, error) {
	line, _, _ := bytes.Cut(contents, []byte{'\n'})
	fields := strings.Fields(string(line))
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuTimes{}, errors.New("unexpected /proc/stat format")
	}
	var times cpuTimes
	// guest and guest_nice are already counted in user and nice.
	for index, field := range fields[1:min(len(fields), 9)] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return cpuTimes{}, errors.New("unexpected /proc/stat format")
		}
		times.total += value
		if index == 3 || index == 4 {
			times.idle += value
		}
	}
	return times, nil
}

func parseMeminfo(contents []byte) (total, available uint64, err error) {
	var haveTotal, haveAvailable bool
	scanner := bufio.NewScanner(bytes.NewReader(contents))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		value, parseErr := strconv.ParseUint(fields[1], 10, 64)
		if parseErr != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total, haveTotal = value<<10, true
		case "MemAvailable:":
			available, haveAvailable = value<<10, true
		}
	}
	if !haveTotal || !haveAvailable {
		return 0, 0, errors.New("unexpected /proc/meminfo format")
	}
	return total, available, nil
}
