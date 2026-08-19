package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type componentDefinition struct {
	Packages []string
	Service  string
}

type serviceStatus struct {
	Name      string `json:"name"`
	Service   string `json:"service"`
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	Enabled   bool   `json:"enabled"`
	Version   string `json:"version"`
}

type resourceUsage struct {
	Total     uint64 `json:"total"`
	Used      uint64 `json:"used"`
	Available uint64 `json:"available"`
}

type cpuOverview struct {
	Cores        int       `json:"cores"`
	UsagePercent float64   `json:"usagePercent"`
	LoadAverage  []float64 `json:"loadAverage"`
}

type networkOverview struct {
	ReceivedBytes    uint64 `json:"receivedBytes"`
	TransmittedBytes uint64 `json:"transmittedBytes"`
}

type systemOverview struct {
	Hostname        string          `json:"hostname"`
	IPAddresses     []string        `json:"ipAddresses"`
	PublicIPAddress string          `json:"publicIpAddress"`
	Kernel          string          `json:"kernel"`
	UptimeSeconds   uint64          `json:"uptimeSeconds"`
	CPU             cpuOverview     `json:"cpu"`
	Memory          resourceUsage   `json:"memory"`
	Disk            resourceUsage   `json:"disk"`
	Network         networkOverview `json:"network"`
}

type cpuSnapshot struct {
	total uint64
	idle  uint64
}

func parseCPUStat(data string) (cpuSnapshot, error) {
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var snapshot cpuSnapshot
		for index := 1; index < len(fields) && index <= 8; index++ {
			value, err := strconv.ParseUint(fields[index], 10, 64)
			if err != nil {
				return cpuSnapshot{}, err
			}
			snapshot.total += value
			if index == 4 || index == 5 {
				snapshot.idle += value
			}
		}
		return snapshot, nil
	}
	return cpuSnapshot{}, fmt.Errorf("未找到 CPU 统计")
}

func cpuPercent(before, after cpuSnapshot) float64 {
	if after.total <= before.total || after.idle < before.idle {
		return 0
	}
	total := after.total - before.total
	idle := after.idle - before.idle
	if idle >= total {
		return 0
	}
	return float64(total-idle) * 100 / float64(total)
}

func sampleCPUPercent() float64 {
	firstData, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	first, err := parseCPUStat(string(firstData))
	if err != nil {
		return 0
	}
	time.Sleep(100 * time.Millisecond)
	secondData, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	second, err := parseCPUStat(string(secondData))
	if err != nil {
		return 0
	}
	return cpuPercent(first, second)
}

func parseLoadAverage(data string) []float64 {
	fields := strings.Fields(data)
	loads := make([]float64, 0, 3)
	for index := 0; index < len(fields) && index < 3; index++ {
		value, err := strconv.ParseFloat(fields[index], 64)
		if err != nil {
			return nil
		}
		loads = append(loads, value)
	}
	return loads
}

func parseMemoryInfo(data string) resourceUsage {
	values := make(map[string]uint64)
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err == nil {
			values[strings.TrimSuffix(fields[0], ":")] = value * 1024
		}
	}
	total := values["MemTotal"]
	available := values["MemAvailable"]
	if available == 0 {
		available = values["MemFree"] + values["Buffers"] + values["Cached"]
	}
	if available > total {
		available = total
	}
	return resourceUsage{Total: total, Used: total - available, Available: available}
}

func displayNetworkInterface(name string) bool {
	if name == "lo" {
		return false
	}
	for _, prefix := range []string{"docker", "veth", "br-", "virbr"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return true
}

func parseNetworkDev(data string, interfaces map[string]bool) networkOverview {
	var overview networkOverview
	for _, line := range strings.Split(data, "\n") {
		nameAndData := strings.SplitN(line, ":", 2)
		if len(nameAndData) != 2 {
			continue
		}
		name := strings.TrimSpace(nameAndData[0])
		if !displayNetworkInterface(name) || (len(interfaces) > 0 && !interfaces[name]) {
			continue
		}
		fields := strings.Fields(nameAndData[1])
		if len(fields) < 9 {
			continue
		}
		received, receivedErr := strconv.ParseUint(fields[0], 10, 64)
		transmitted, transmittedErr := strconv.ParseUint(fields[8], 10, 64)
		if receivedErr == nil && transmittedErr == nil {
			overview.ReceivedBytes += received
			overview.TransmittedBytes += transmitted
		}
	}
	return overview
}

func serverNetwork() ([]string, map[string]bool) {
	var addresses []string
	interfaceNames := make(map[string]bool)
	interfaces, err := net.Interfaces()
	if err != nil {
		return addresses, interfaceNames
	}
	for _, networkInterface := range interfaces {
		if networkInterface.Flags&net.FlagUp == 0 || networkInterface.Flags&net.FlagLoopback != 0 || !displayNetworkInterface(networkInterface.Name) {
			continue
		}
		interfaceAddresses, err := networkInterface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range interfaceAddresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err != nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
				continue
			}
			addresses = append(addresses, ip.String())
			interfaceNames[networkInterface.Name] = true
		}
	}
	sort.Strings(addresses)
	return addresses, interfaceNames
}

func collectSystemOverview() systemOverview {
	overview := systemOverview{}
	overview.Hostname, _ = os.Hostname()
	addresses, interfaceNames := serverNetwork()
	overview.IPAddresses = addresses
	overview.CPU.Cores = runtime.NumCPU()
	overview.CPU.UsagePercent = sampleCPUPercent()

	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		overview.CPU.LoadAverage = parseLoadAverage(string(data))
	}
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		overview.Memory = parseMemoryInfo(string(data))
	}
	if data, err := os.ReadFile("/proc/uptime"); err == nil {
		if fields := strings.Fields(string(data)); len(fields) > 0 {
			seconds, _ := strconv.ParseFloat(fields[0], 64)
			overview.UptimeSeconds = uint64(seconds)
		}
	}
	if data, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		overview.Kernel = strings.TrimSpace(string(data))
	}
	if data, err := os.ReadFile("/proc/net/dev"); err == nil {
		overview.Network = parseNetworkDev(string(data), interfaceNames)
	}
	var filesystem syscall.Statfs_t
	if err := syscall.Statfs("/", &filesystem); err == nil {
		blockSize := uint64(filesystem.Bsize)
		total := filesystem.Blocks * blockSize
		available := filesystem.Bavail * blockSize
		free := filesystem.Bfree * blockSize
		overview.Disk = resourceUsage{Total: total, Used: total - free, Available: available}
	}
	return overview
}

func phpPrefix() string {
	for _, prefix := range []string{"php85", "php84"} {
		if packageInstalled(prefix) {
			return prefix
		}
	}
	return "php85"
}

func phpPackages(prefix string) []string {
	packages := []string{prefix, prefix + "-fpm"}
	if prefix == "php84" {
		packages = append(packages, prefix+"-opcache")
	}
	for _, suffix := range []string{"ctype", "curl", "gd", "intl", "mbstring", "mysqlnd", "mysqli", "pdo", "pdo_mysql", "xml", "zip"} {
		packages = append(packages, prefix+"-"+suffix)
	}
	return packages
}

func phpService(prefix string) string {
	return "php-fpm" + strings.TrimPrefix(prefix, "php")
}

func components() map[string]componentDefinition {
	php := phpPrefix()
	return map[string]componentDefinition{
		"nginx": {Packages: []string{"nginx"}, Service: "nginx"},
		"php": {
			Packages: phpPackages(php),
			Service:  phpService(php),
		},
		"mysql": {Packages: []string{"mariadb", "mariadb-client"}, Service: "mariadb"},
		"ftp":   {Packages: []string{"vsftpd"}, Service: "vsftpd"},
		"redis": {Packages: []string{"redis"}, Service: "redis"},
		"memcached": {
			Packages: []string{"memcached"},
			Service:  "memcached",
		},
		"docker": {Packages: []string{"docker"}, Service: "docker"},
	}
}

func runAdmin(timeout time.Duration, command string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, command, args...).CombinedOutput()
	text := strings.TrimSpace(string(output))
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return text, fmt.Errorf("操作超时")
	}
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return text, fmt.Errorf("%s", text)
	}
	return text, nil
}

func runAdminInput(timeout time.Duration, input, command string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	process := exec.CommandContext(ctx, command, args...)
	process.Stdin = strings.NewReader(input)
	output, err := process.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return text, fmt.Errorf("操作超时")
	}
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return text, fmt.Errorf("%s", text)
	}
	return text, nil
}

func packageInstalled(name string) bool {
	command := exec.Command("apk", "info", "-e", name)
	return command.Run() == nil
}

func packagesInstalled(names []string) bool {
	if len(names) == 0 {
		return false
	}
	for _, name := range names {
		if !packageInstalled(name) {
			return false
		}
	}
	return true
}

func commandVersion(command string, args ...string) string {
	output, err := exec.Command(command, args...).CombinedOutput()
	if err != nil {
		return ""
	}
	line := strings.SplitN(strings.TrimSpace(string(output)), "\n", 2)[0]
	return strings.TrimSpace(line)
}

func openRCEnabled(service string) bool {
	entries, err := filepath.Glob("/etc/runlevels/default/*")
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if filepath.Base(entry) == service {
			return true
		}
	}
	return false
}

func serviceRunning(service string) bool {
	return exec.Command("rc-service", service, "status").Run() == nil
}

func componentStatus(name string, definition componentDefinition) serviceStatus {
	installed := packagesInstalled(definition.Packages)
	status := serviceStatus{Name: name, Service: definition.Service, Installed: installed, Running: installed && serviceRunning(definition.Service), Enabled: installed && openRCEnabled(definition.Service)}
	switch name {
	case "nginx":
		status.Version = commandVersion("nginx", "-v")
	case "php":
		status.Version = commandVersion("php", "-v")
	case "mysql":
		status.Version = commandVersion("mariadb", "--version")
	case "ftp":
		status.Version = commandVersion("vsftpd", "-v")
	case "redis":
		status.Version = commandVersion("redis-server", "--version")
	case "memcached":
		status.Version = commandVersion("memcached", "-h")
	case "docker":
		status.Version = commandVersion("docker", "--version")
	}
	return status
}

func (a *app) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, false) == nil {
		return
	}
	definitions := components()
	statuses := make([]serviceStatus, 0, len(definitions))
	for _, name := range []string{"nginx", "php", "mysql", "redis", "memcached", "ftp"} {
		statuses = append(statuses, componentStatus(name, definitions[name]))
	}
	system := collectSystemOverview()
	system.PublicIPAddress = a.publicIPAddress()
	writeJSON(w, http.StatusOK, map[string]any{
		"services": statuses,
		"platform": "Alpine Linux / OpenRC",
		"system":   system,
	})
}

func (a *app) handleComponentInstall(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	name := r.PathValue("component")
	definition, ok := components()[name]
	if !ok {
		writeError(w, &apiError{http.StatusBadRequest, "不支持该组件"})
		return
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	args := append([]string{"add", "--no-cache"}, definition.Packages...)
	output, err := runAdmin(15*time.Minute, "apk", args...)
	if err == nil {
		_, _ = runAdmin(30*time.Second, "rc-update", "add", definition.Service, "default")
		switch name {
		case "nginx":
			err = ensureNginxLayout()
		case "php":
			_, err = runAdmin(time.Minute, "rc-service", definition.Service, "start")
		case "mysql":
			if _, statErr := os.Stat("/var/lib/mysql/mysql"); errors.Is(statErr, os.ErrNotExist) {
				_, _ = runAdmin(2*time.Minute, "rc-service", "mariadb", "setup")
			}
			_, err = runAdmin(time.Minute, "rc-service", "mariadb", "start")
		case "ftp":
			err = ensureVSFTPDConfig()
			if err == nil {
				_, err = runAdmin(time.Minute, "rc-service", definition.Service, "start")
			}
		case "redis", "memcached", "docker":
			_, err = runAdmin(2*time.Minute, "rc-service", definition.Service, "start")
		}
	}
	if err != nil {
		writeError(w, &apiError{http.StatusInternalServerError, err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "output": output, "status": componentStatus(name, definition)})
}

func (a *app) handleComponentRemove(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	name := r.PathValue("component")
	definition, ok := components()[name]
	if !ok {
		writeError(w, &apiError{http.StatusBadRequest, "不支持该组件"})
		return
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	_, _ = runAdmin(time.Minute, "rc-service", definition.Service, "stop")
	_, _ = runAdmin(30*time.Second, "rc-update", "del", definition.Service, "default")
	args := append([]string{"del"}, definition.Packages...)
	output, err := runAdmin(10*time.Minute, "apk", args...)
	if err != nil {
		writeError(w, &apiError{http.StatusInternalServerError, err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "output": output})
}

func allowedService(name string) bool {
	for _, definition := range components() {
		if name == definition.Service {
			return true
		}
	}
	return false
}

func (a *app) handleServiceAction(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	service := r.PathValue("service")
	action := r.PathValue("action")
	if !allowedService(service) {
		writeError(w, &apiError{http.StatusBadRequest, "不支持该服务"})
		return
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	var err error
	switch action {
	case "start", "stop", "restart":
		_, err = runAdmin(time.Minute, "rc-service", service, action)
	case "enable":
		_, err = runAdmin(30*time.Second, "rc-update", "add", service, "default")
	case "disable":
		_, err = runAdmin(30*time.Second, "rc-update", "del", service, "default")
	default:
		err = &apiError{http.StatusBadRequest, "不支持该操作"}
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "running": serviceRunning(service), "enabled": openRCEnabled(service)})
}

func installedPackages(prefix string) []string {
	output, err := exec.Command("apk", "info").Output()
	if err != nil {
		return nil
	}
	var result []string
	for _, name := range strings.Fields(string(output)) {
		if strings.HasPrefix(name, prefix) {
			result = append(result, name)
		}
	}
	sort.Strings(result)
	return result
}
