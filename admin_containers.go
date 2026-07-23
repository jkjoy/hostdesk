package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxContainerLogBytes = 256 << 10

var containerNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

type containerListItem struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Image         string `json:"image"`
	State         string `json:"state"`
	Status        string `json:"status"`
	Ports         string `json:"ports"`
	Networks      string `json:"networks"`
	CreatedAt     string `json:"createdAt"`
	ManagedBy     string `json:"managedBy,omitempty"`
	CPUPercent    string `json:"cpuPercent,omitempty"`
	MemoryUsage   string `json:"memoryUsage,omitempty"`
	MemoryPercent string `json:"memoryPercent,omitempty"`
	NetworkIO     string `json:"networkIO,omitempty"`
	ProcessCount  string `json:"processCount,omitempty"`
}

type containerMount struct {
	Type        string `json:"type"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Mode        string `json:"mode"`
	ReadWrite   bool   `json:"readWrite"`
}

type containerPortBinding struct {
	ContainerPort string `json:"containerPort"`
	HostIP        string `json:"hostIp"`
	HostPort      string `json:"hostPort"`
}

type containerDetail struct {
	ID                string                 `json:"id"`
	Name              string                 `json:"name"`
	Image             string                 `json:"image"`
	Hostname          string                 `json:"hostname"`
	State             string                 `json:"state"`
	Running           bool                   `json:"running"`
	Paused            bool                   `json:"paused"`
	ExitCode          int                    `json:"exitCode"`
	Created           string                 `json:"created"`
	StartedAt         string                 `json:"startedAt"`
	RestartCount      int                    `json:"restartCount"`
	RestartPolicy     string                 `json:"restartPolicy"`
	MaximumRetryCount int                    `json:"maximumRetryCount"`
	CPUs              float64                `json:"cpus"`
	MemoryBytes       int64                  `json:"memoryBytes"`
	NetworkMode       string                 `json:"networkMode"`
	Privileged        bool                   `json:"privileged"`
	ReadOnlyRootFS    bool                   `json:"readOnlyRootFs"`
	Environment       []string               `json:"environment"`
	Command           []string               `json:"command"`
	Mounts            []containerMount       `json:"mounts"`
	Ports             []containerPortBinding `json:"ports"`
	Networks          []string               `json:"networks"`
	ManagedBy         string                 `json:"managedBy,omitempty"`
	ComposeProject    string                 `json:"composeProject,omitempty"`
	ComposeService    string                 `json:"composeService,omitempty"`
}

type containerSettings struct {
	Name              string  `json:"name"`
	RestartPolicy     string  `json:"restartPolicy"`
	MaximumRetryCount int     `json:"maximumRetryCount"`
	CPUs              float64 `json:"cpus"`
	MemoryMB          int64   `json:"memoryMb"`
}

type dockerInspect struct {
	ID           string `json:"Id"`
	Name         string `json:"Name"`
	Created      string `json:"Created"`
	RestartCount int    `json:"RestartCount"`
	State        struct {
		Status    string `json:"Status"`
		Running   bool   `json:"Running"`
		Paused    bool   `json:"Paused"`
		ExitCode  int    `json:"ExitCode"`
		StartedAt string `json:"StartedAt"`
	} `json:"State"`
	Config struct {
		Hostname string            `json:"Hostname"`
		Image    string            `json:"Image"`
		Env      []string          `json:"Env"`
		Cmd      []string          `json:"Cmd"`
		Labels   map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		RestartPolicy struct {
			Name              string `json:"Name"`
			MaximumRetryCount int    `json:"MaximumRetryCount"`
		} `json:"RestartPolicy"`
		Memory         int64  `json:"Memory"`
		NanoCPUs       int64  `json:"NanoCpus"`
		CPUQuota       int64  `json:"CpuQuota"`
		CPUPeriod      int64  `json:"CpuPeriod"`
		NetworkMode    string `json:"NetworkMode"`
		Privileged     bool   `json:"Privileged"`
		ReadonlyRootfs bool   `json:"ReadonlyRootfs"`
	} `json:"HostConfig"`
	Mounts []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		Mode        string `json:"Mode"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
	NetworkSettings struct {
		Ports map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
		Networks map[string]json.RawMessage `json:"Networks"`
	} `json:"NetworkSettings"`
}

func dockerAvailable() bool {
	_, err := exec.LookPath("docker")
	return err == nil
}

func validateContainerIdentifier(value string) error {
	if !containerNamePattern.MatchString(value) {
		return &apiError{http.StatusBadRequest, "容器标识无效"}
	}
	return nil
}

func validateContainerSettings(settings *containerSettings) error {
	settings.Name = strings.TrimSpace(settings.Name)
	if !containerNamePattern.MatchString(settings.Name) {
		return &apiError{http.StatusBadRequest, "容器名称只能包含字母、数字、点、下划线和连字符"}
	}
	validPolicies := map[string]bool{"no": true, "always": true, "unless-stopped": true, "on-failure": true}
	if !validPolicies[settings.RestartPolicy] {
		return &apiError{http.StatusBadRequest, "重启策略无效"}
	}
	if settings.MaximumRetryCount < 0 || settings.MaximumRetryCount > 1000 {
		return &apiError{http.StatusBadRequest, "失败重试次数必须在 0 到 1000 之间"}
	}
	if settings.CPUs < 0 || settings.CPUs > 1024 {
		return &apiError{http.StatusBadRequest, "CPU 限制无效"}
	}
	if settings.MemoryMB < 0 || (settings.MemoryMB > 0 && settings.MemoryMB < 6) || settings.MemoryMB > 1<<30 {
		return &apiError{http.StatusBadRequest, "内存限制必须为 0 或至少 6 MB"}
	}
	return nil
}

func parseContainerList(data string) ([]containerListItem, error) {
	var containers []containerListItem
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row struct {
			ID        string `json:"ID"`
			Names     string `json:"Names"`
			Image     string `json:"Image"`
			State     string `json:"State"`
			Status    string `json:"Status"`
			Ports     string `json:"Ports"`
			Networks  string `json:"Networks"`
			CreatedAt string `json:"CreatedAt"`
			Labels    string `json:"Labels"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("Docker 容器列表格式无效: %w", err)
		}
		managedBy := ""
		if strings.Contains(row.Labels, "com.docker.compose.project=") {
			managedBy = "Docker Compose"
		}
		containers = append(containers, containerListItem{
			ID: row.ID, Name: row.Names, Image: row.Image, State: row.State, Status: row.Status,
			Ports: row.Ports, Networks: row.Networks, CreatedAt: row.CreatedAt, ManagedBy: managedBy,
		})
	}
	return containers, nil
}

func applyContainerStats(containers []containerListItem, data string) {
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row struct {
			Container string `json:"Container"`
			Name      string `json:"Name"`
			CPUPerc   string `json:"CPUPerc"`
			MemUsage  string `json:"MemUsage"`
			MemPerc   string `json:"MemPerc"`
			NetIO     string `json:"NetIO"`
			PIDs      string `json:"PIDs"`
		}
		if json.Unmarshal([]byte(line), &row) != nil {
			continue
		}
		for index := range containers {
			if containers[index].ID == row.Container || strings.HasPrefix(containers[index].ID, row.Container) || containers[index].Name == row.Name {
				containers[index].CPUPercent = row.CPUPerc
				containers[index].MemoryUsage = row.MemUsage
				containers[index].MemoryPercent = row.MemPerc
				containers[index].NetworkIO = row.NetIO
				containers[index].ProcessCount = row.PIDs
				break
			}
		}
	}
}

func parseContainerInspect(data string) (containerDetail, error) {
	var records []dockerInspect
	if err := json.Unmarshal([]byte(data), &records); err != nil || len(records) != 1 {
		if err == nil {
			err = errors.New("未返回容器详情")
		}
		return containerDetail{}, err
	}
	record := records[0]
	detail := containerDetail{
		ID: record.ID, Name: strings.TrimPrefix(record.Name, "/"), Image: record.Config.Image,
		Hostname: record.Config.Hostname, State: record.State.Status, Running: record.State.Running,
		Paused: record.State.Paused, ExitCode: record.State.ExitCode, Created: record.Created,
		StartedAt: record.State.StartedAt, RestartCount: record.RestartCount,
		RestartPolicy: record.HostConfig.RestartPolicy.Name, MaximumRetryCount: record.HostConfig.RestartPolicy.MaximumRetryCount,
		MemoryBytes: record.HostConfig.Memory, NetworkMode: record.HostConfig.NetworkMode,
		Privileged: record.HostConfig.Privileged, ReadOnlyRootFS: record.HostConfig.ReadonlyRootfs,
		Environment: record.Config.Env, Command: record.Config.Cmd,
	}
	if record.HostConfig.NanoCPUs > 0 {
		detail.CPUs = float64(record.HostConfig.NanoCPUs) / 1e9
	} else if record.HostConfig.CPUQuota > 0 && record.HostConfig.CPUPeriod > 0 {
		detail.CPUs = float64(record.HostConfig.CPUQuota) / float64(record.HostConfig.CPUPeriod)
	}
	for _, mount := range record.Mounts {
		detail.Mounts = append(detail.Mounts, containerMount{Type: mount.Type, Source: mount.Source, Destination: mount.Destination, Mode: mount.Mode, ReadWrite: mount.RW})
	}
	for containerPort, bindings := range record.NetworkSettings.Ports {
		if len(bindings) == 0 {
			detail.Ports = append(detail.Ports, containerPortBinding{ContainerPort: containerPort})
			continue
		}
		for _, binding := range bindings {
			detail.Ports = append(detail.Ports, containerPortBinding{ContainerPort: containerPort, HostIP: binding.HostIP, HostPort: binding.HostPort})
		}
	}
	for network := range record.NetworkSettings.Networks {
		detail.Networks = append(detail.Networks, network)
	}
	sortStrings(detail.Networks)
	if labels := record.Config.Labels; labels["com.docker.compose.project"] != "" {
		detail.ManagedBy = "Docker Compose"
		detail.ComposeProject = labels["com.docker.compose.project"]
		detail.ComposeService = labels["com.docker.compose.service"]
	}
	return detail, nil
}

func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for current := index; current > 0 && values[current] < values[current-1]; current-- {
			values[current], values[current-1] = values[current-1], values[current]
		}
	}
}

func inspectContainer(id string) (containerDetail, error) {
	output, err := runAdmin(20*time.Second, "docker", "inspect", id)
	if err != nil {
		return containerDetail{}, err
	}
	return parseContainerInspect(output)
}

func (a *app) handleContainersList(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, false) == nil {
		return
	}
	if !dockerAvailable() {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "containers": []containerListItem{}})
		return
	}
	output, err := runAdmin(20*time.Second, "docker", "ps", "-a", "--no-trunc", "--format", "{{json .}}")
	if err != nil {
		writeError(w, fmt.Errorf("无法读取 Docker 容器: %w", err))
		return
	}
	containers, err := parseContainerList(output)
	if err != nil {
		writeError(w, err)
		return
	}
	if stats, statsErr := runAdmin(20*time.Second, "docker", "stats", "--no-stream", "--no-trunc", "--format", "{{json .}}"); statsErr == nil {
		applyContainerStats(containers, stats)
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "containers": containers})
}

func (a *app) handleContainerGet(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, false) == nil {
		return
	}
	id := r.PathValue("id")
	if err := validateContainerIdentifier(id); err != nil {
		writeError(w, err)
		return
	}
	detail, err := inspectContainer(id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (a *app) handleContainerUpdate(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	id := r.PathValue("id")
	if err := validateContainerIdentifier(id); err != nil {
		writeError(w, err)
		return
	}
	var settings containerSettings
	if err := decodeJSON(w, r, &settings); err != nil {
		writeError(w, err)
		return
	}
	if err := validateContainerSettings(&settings); err != nil {
		writeError(w, err)
		return
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	current, err := inspectContainer(id)
	if err != nil {
		writeError(w, err)
		return
	}
	restart := settings.RestartPolicy
	if restart == "on-failure" && settings.MaximumRetryCount > 0 {
		restart += ":" + strconv.Itoa(settings.MaximumRetryCount)
	}
	args := []string{"update", "--restart", restart, "--cpus", strconv.FormatFloat(settings.CPUs, 'f', -1, 64), "--memory", strconv.FormatInt(settings.MemoryMB, 10) + "m", id}
	if _, err := runAdmin(time.Minute, "docker", args...); err != nil {
		writeError(w, err)
		return
	}
	if settings.Name != current.Name {
		if _, err := runAdmin(30*time.Second, "docker", "rename", id, settings.Name); err != nil {
			writeError(w, err)
			return
		}
	}
	updated, err := inspectContainer(id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (a *app) handleContainerAction(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	id := r.PathValue("id")
	if err := validateContainerIdentifier(id); err != nil {
		writeError(w, err)
		return
	}
	action := r.PathValue("action")
	if action != "start" && action != "stop" && action != "restart" && action != "pause" && action != "unpause" {
		writeError(w, &apiError{http.StatusBadRequest, "不支持该容器操作"})
		return
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	args := []string{action}
	if action == "stop" || action == "restart" {
		args = append(args, "--time", "20")
	}
	args = append(args, id)
	if _, err := runAdmin(time.Minute, "docker", args...); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleContainerDelete(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	id := r.PathValue("id")
	if err := validateContainerIdentifier(id); err != nil {
		writeError(w, err)
		return
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	if _, err := runAdmin(time.Minute, "docker", "rm", "-f", id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleContainerLogs(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, false) == nil {
		return
	}
	id := r.PathValue("id")
	if err := validateContainerIdentifier(id); err != nil {
		writeError(w, err)
		return
	}
	output, err := runAdmin(30*time.Second, "docker", "logs", "--tail", "300", "--timestamps", id)
	if err != nil {
		writeError(w, err)
		return
	}
	if len(output) > maxContainerLogBytes {
		output = "[日志内容已截断]\n" + output[len(output)-maxContainerLogBytes:]
	}
	writeJSON(w, http.StatusOK, map[string]string{"logs": output})
}
