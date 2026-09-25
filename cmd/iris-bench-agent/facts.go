package main

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// collectFacts gathers non-secret host identity and control facts using only
// standard-library reads of normally world-readable files. Missing files are
// omitted rather than treated as fatal so the agent works on varied hosts.
func collectFacts() map[string]string {
	facts := map[string]string{
		"goos":      runtime.GOOS,
		"goarch":    runtime.GOARCH,
		"numCPU":    strconv.Itoa(runtime.NumCPU()),
		"golangVer": runtime.Version(),
	}
	if h, err := os.Hostname(); err == nil {
		facts["hostname"] = h
	}
	readFirstLine(facts, "kernel", "/proc/sys/kernel/osrelease")
	readFirstLine(facts, "bootID", "/proc/sys/kernel/random/boot_id")
	readFirstLine(facts, "load1", "/proc/loadavg")
	readFirstLine(facts, "uptimeSeconds", "/proc/uptime")
	readFirstLine(facts, "distro", "/etc/os-release", "PRETTY_NAME")
	readFirstLine(facts, "cpuGovernor", "/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor")
	readFirstLine(facts, "numaNodes", "/sys/devices/system/node/online")
	facts["cgroupVersion"] = "unknown"
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		facts["cgroupVersion"] = "v2"
		readFirstLine(facts, "cgroupControllers", "/sys/fs/cgroup/cgroup.controllers")
	}
	if mem := memTotalMiB(); mem > 0 {
		facts["memoryTotalMiB"] = strconv.FormatInt(mem, 10)
	}
	if cores := physicalCores(); cores > 0 {
		facts["physicalCores"] = strconv.Itoa(cores)
	}
	facts["smt"] = smtState()
	return facts
}

func readFirstLine(dst map[string]string, key, path string, prefixes ...string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if len(prefixes) > 0 {
			match := false
			for _, p := range prefixes {
				if strings.HasPrefix(line, p) {
					match = true
					break
				}
			}
			if !match {
				continue
			}
			if i := strings.IndexByte(line, '='); i >= 0 {
				dst[key] = strings.Trim(strings.TrimSpace(line[i+1:]), `"`)
				return
			}
		}
		dst[key] = line
		return
	}
}

func memTotalMiB() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kib, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kib / 1024
	}
	return 0
}

// physicalCores counts unique (physical id, core id) pairs in /proc/cpuinfo.
func physicalCores() int {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	seen := map[string]struct{}{}
	var physID, coreID string
	flush := func() {
		if physID != "" || coreID != "" {
			seen[physID+":"+coreID] = struct{}{}
		}
		physID, coreID = "", ""
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "physical id":
			physID = strings.TrimSpace(val)
		case "core id":
			coreID = strings.TrimSpace(val)
		}
	}
	flush()
	return len(seen)
}

// smtState reports "on" when threads exceed unique physical cores.
func smtState() string {
	cores := physicalCores()
	if cores == 0 {
		return "unknown"
	}
	if runtime.NumCPU() > cores {
		return "on"
	}
	return "off"
}
