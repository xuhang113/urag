package main

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// secondModelBytes 是再放一个 Qwen3 4B 权重大约需要的空闲内存。
// float16 权重约 7.5GB，这里按 8GiB 判断。不够就不要和嵌入模型一起加载重排模型。
const secondModelBytes = 8 << 30

func secondModelFits() bool {
	free, ok := freeBytes()
	if !ok {
		return false
	}
	return free >= secondModelBytes
}

func freeBytes() (uint64, bool) {
	switch runtime.GOOS {
	case "darwin":
		return darwinFreeBytes()
	case "linux":
		return linuxAvailableBytes()
	default:
		return 0, false
	}
}

func darwinFreeBytes() (uint64, bool) {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0, false
	}
	pages, ok := parseVMStatFreePages(string(out))
	if !ok {
		return 0, false
	}
	size := os.Getpagesize()
	if size <= 0 {
		return 0, false
	}
	return pages * uint64(size), true
}

func parseVMStatFreePages(text string) (uint64, bool) {
	sc := bufio.NewScanner(bytes.NewReader([]byte(text)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "Pages free:") {
			continue
		}
		return parseTrailingUint(line)
	}
	return 0, false
}

func parseTrailingUint(line string) (uint64, bool) {
	line = strings.TrimSuffix(strings.TrimSpace(line), ".")
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return 0, false
	}
	n, err := strconv.ParseUint(fields[len(fields)-1], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func linuxAvailableBytes() (uint64, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}
