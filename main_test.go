package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func newAuthTestApp(t *testing.T, dataDir string) *app {
	t.Helper()
	db, err := openAuthDatabase(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &app{
		db: db, rootReal: "/test-root", sessions: make(map[string]*sessionInfo),
	}
}

func authRequest(t *testing.T, handler http.HandlerFunc, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "/", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler(response, request)
	return response
}

func authenticatedRequest(t *testing.T, handler http.HandlerFunc, method, body, csrf string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "/", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrf)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler(response, request)
	return response
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestFreshDatabaseRequiresSetupAndCreatesAdministrator(t *testing.T) {
	dataDir := t.TempDir()
	a := newAuthTestApp(t, dataDir)
	required, err := a.setupRequired()
	if err != nil || !required {
		t.Fatalf("fresh database must require setup: required=%v err=%v", required, err)
	}

	response := authRequest(t, a.handleSetup, http.MethodPost, `{"username":"owner","password":"strong-password-123"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("setup failed: status=%d body=%s", response.Code, response.Body.String())
	}
	var count int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM administrators").Scan(&count); err != nil || count != 1 {
		t.Fatalf("setup must create exactly one administrator: count=%d err=%v", count, err)
	}
	info, err := os.Stat(filepath.Join(dataDir, authDatabaseName))
	if err != nil {
		t.Fatalf("database file missing: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("database permissions must be 0600: mode=%v", info.Mode().Perm())
	}

	response = authRequest(t, a.handleSetup, http.MethodPost, `{"username":"other","password":"another-password-456"}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("second setup must be rejected: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestLegacyAdministratorMigrationAndLogin(t *testing.T) {
	dataDir := t.TempDir()
	salt := randomToken(16)
	hash, err := derivePassword("legacy-password-123", salt)
	if err != nil {
		t.Fatal(err)
	}
	legacy := authFile{Username: "legacy-admin", Salt: salt, Hash: hash}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	a := newAuthTestApp(t, dataDir)
	required, err := a.setupRequired()
	if err != nil || required {
		t.Fatalf("migrated database must not require setup: required=%v err=%v", required, err)
	}

	response := authRequest(t, a.handleLogin, http.MethodPost, `{"username":"legacy-admin","password":"legacy-password-123"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("migrated login failed: status=%d body=%s", response.Code, response.Body.String())
	}
	response = authRequest(t, a.handleLogin, http.MethodPost, `{"username":"legacy-admin","password":"wrong-password"}`)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password must fail: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestLoginProtectionLocksRepeatedFailures(t *testing.T) {
	dataDir := t.TempDir()
	a := newAuthTestApp(t, dataDir)
	response := authRequest(t, a.handleSetup, http.MethodPost, `{"username":"owner","password":"strong-password-123"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("setup failed: %s", response.Body.String())
	}
	for attempt := 0; attempt < 5; attempt++ {
		response = authRequest(t, a.handleLogin, http.MethodPost, `{"username":"owner","password":"wrong-password"}`)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d returned %d: %s", attempt+1, response.Code, response.Body.String())
		}
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	a = newAuthTestApp(t, dataDir)
	response = authRequest(t, a.handleLogin, http.MethodPost, `{"username":"owner","password":"strong-password-123"}`)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" {
		t.Fatalf("locked login returned %d without Retry-After: %s", response.Code, response.Body.String())
	}
}

func TestClientIPOnlyTrustsLoopbackProxy(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/login", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("X-Real-IP", "203.0.113.8")
	if ip := requestClientIP(request); ip != "203.0.113.8" {
		t.Fatalf("loopback proxy IP was not used: %s", ip)
	}
	request.RemoteAddr = "198.51.100.7:4321"
	request.Header.Set("X-Real-IP", "203.0.113.9")
	if ip := requestClientIP(request); ip != "198.51.100.7" {
		t.Fatalf("untrusted forwarded IP was used: %s", ip)
	}
}

func TestDecodeJSONRejectsTrailingData(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"owner"}{"password":"extra"}`))
	response := httptest.NewRecorder()
	var body struct {
		Username string `json:"username"`
	}
	err := decodeJSON(response, request, &body)
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("trailing JSON returned %v", err)
	}
}

func TestTerminalOriginRequiresExactHost(t *testing.T) {
	tests := []struct {
		name   string
		origin string
		want   bool
	}{
		{name: "exact HTTP origin", origin: "http://panel.example:8787", want: true},
		{name: "exact HTTPS origin", origin: "https://panel.example:8787", want: true},
		{name: "missing origin", origin: "", want: false},
		{name: "host prefix attack", origin: "https://panel.example:8787.evil.example", want: false},
		{name: "different host", origin: "https://evil.example", want: false},
		{name: "unsupported scheme", origin: "file://panel.example:8787", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://panel.example:8787/ws/terminal", nil)
			request.Host = "panel.example:8787"
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if got := terminalOriginAllowed(request); got != test.want {
				t.Fatalf("terminalOriginAllowed() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestSPAHandlerServesFrontendRoutesOnly(t *testing.T) {
	handler := spaHandler([]byte("<main>HostDesk</main>"), http.NotFoundHandler())
	for _, requestPath := range []string{"/", "/sites", "/certificates", "/server-settings", "/files?path=var/www"} {
		request := httptest.NewRequest(http.MethodGet, requestPath, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || response.Body.String() != "<main>HostDesk</main>" {
			t.Fatalf("frontend route %s was not served by the SPA: status=%d body=%q", requestPath, response.Code, response.Body.String())
		}
		if contentType := response.Header().Get("Content-Type"); contentType != "text/html; charset=utf-8" {
			t.Fatalf("frontend route %s returned content type %q", requestPath, contentType)
		}
	}
	for _, requestPath := range []string{"/api/missing", "/ws/missing", "/app/assets/missing.js", "/vendor/missing.js", "/missing.css"} {
		request := httptest.NewRequest(http.MethodGet, requestPath, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("non-SPA path %s returned status %d", requestPath, response.Code)
		}
	}
}

func TestSecurityHeadersProtectHTTPSAPIResponses(t *testing.T) {
	handler := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "http://panel.example/api/session", nil)
	request.Header.Set("X-Forwarded-Proto", "https")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if got := response.Header().Get("Strict-Transport-Security"); got != "max-age=31536000" {
		t.Fatalf("Strict-Transport-Security = %q", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestTerminalSize(t *testing.T) {
	for _, test := range []struct {
		rows, cols         int
		wantRows, wantCols int
	}{
		{24, 80, 24, 80},
		{0, 0, 30, 100},
		{1000, 1000, 30, 100},
		{40, 1, 40, 100},
	} {
		rows, cols := terminalSize(test.rows, test.cols)
		if rows != test.wantRows || cols != test.wantCols {
			t.Fatalf("terminalSize(%d, %d) = (%d, %d), want (%d, %d)", test.rows, test.cols, rows, cols, test.wantRows, test.wantCols)
		}
	}
}

func TestLocalTerminalWebSocket(t *testing.T) {
	a := &app{sessions: map[string]*sessionInfo{
		"terminal-test": {CSRF: "terminal-csrf", Expires: time.Now().Add(time.Minute), User: "admin"},
	}}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback sockets unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(a.handleTerminal))
	server.Listener = listener
	server.Start()
	defer server.Close()

	header := http.Header{}
	header.Set("Origin", server.URL)
	header.Set("Cookie", (&http.Cookie{Name: "hostdesk_session", Value: "terminal-test"}).String())
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), header)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.WriteJSON(terminalMessage{Type: "connect", CSRF: "terminal-csrf", Rows: 24, Cols: 80}); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	commandSent := false
	var output strings.Builder
	for !strings.Contains(output.String(), "HOSTDESK_PTY_OK") {
		var message terminalMessage
		if err := connection.ReadJSON(&message); err != nil {
			t.Fatalf("read terminal message: %v; output=%q", err, output.String())
		}
		switch message.Type {
		case "ready":
			if !commandSent {
				commandSent = true
				if err := connection.WriteJSON(terminalMessage{Type: "input", Data: "printf 'HOSTDESK_PTY_OK\\n'\r"}); err != nil {
					t.Fatal(err)
				}
			}
		case "data":
			output.WriteString(message.Data)
		case "error":
			t.Fatalf("terminal returned an error: %q", message.Data)
		}
	}
	if !commandSent {
		t.Fatal("terminal never became ready")
	}
}

func TestSSHWebSocketRequiresCSRF(t *testing.T) {
	a := &app{
		sessions:   map[string]*sessionInfo{"ssh-test": {CSRF: "expected-csrf", Expires: time.Now().Add(time.Minute), User: "admin"}},
		allowedSSH: map[string]bool{"127.0.0.1": true},
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback sockets unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(a.handleSSH))
	server.Listener = listener
	server.Start()
	defer server.Close()
	header := http.Header{}
	header.Set("Cookie", (&http.Cookie{Name: "hostdesk_session", Value: "ssh-test"}).String())
	header.Set("Origin", server.URL)
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), header)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.WriteJSON(sshMessage{Type: "connect", CSRF: "wrong-csrf", Host: "127.0.0.1", Port: 22, Username: "root"}); err != nil {
		t.Fatal(err)
	}
	var response map[string]string
	if err := connection.ReadJSON(&response); err != nil {
		t.Fatal(err)
	}
	if response["type"] != "error" {
		t.Fatalf("invalid CSRF response = %#v", response)
	}
}

func TestAdministratorCredentialsCanBeChanged(t *testing.T) {
	a := newAuthTestApp(t, t.TempDir())
	setup := authRequest(t, a.handleSetup, http.MethodPost, `{"username":"owner","password":"strong-password-123"}`)
	if setup.Code != http.StatusCreated {
		t.Fatalf("setup failed: %s", setup.Body.String())
	}
	var session struct {
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(setup.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	cookies := setup.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("setup returned %d cookies", len(cookies))
	}
	response := authenticatedRequest(t, a.handleAccountUpdate, http.MethodPut,
		`{"username":"new-owner","currentPassword":"wrong-password","newPassword":"new-strong-password-456"}`, session.CSRF, cookies[0])
	if response.Code != http.StatusForbidden {
		t.Fatalf("wrong current password returned %d: %s", response.Code, response.Body.String())
	}
	response = authenticatedRequest(t, a.handleAccountUpdate, http.MethodPut,
		`{"username":"new-owner","currentPassword":"strong-password-123","newPassword":"new-strong-password-456"}`, session.CSRF, cookies[0])
	if response.Code != http.StatusOK {
		t.Fatalf("credential update failed: %d %s", response.Code, response.Body.String())
	}
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(cookies[0])
	if oldSession, _ := a.session(request); oldSession != nil {
		t.Fatal("old session remained valid after credential update")
	}
	administrator, err := a.administrator()
	if err != nil || administrator.Username != "new-owner" {
		t.Fatalf("administrator was not updated: %+v err=%v", administrator, err)
	}
	candidate, err := derivePassword("new-strong-password-456", administrator.Salt)
	if err != nil || candidate != administrator.Hash {
		t.Fatal("new password hash does not match")
	}
}

func TestPHPPackagesByVersion(t *testing.T) {
	php85 := phpPackages("php85")
	if slices.Contains(php85, "php85-opcache") {
		t.Fatal("PHP 8.5 OPcache is built in and must not be installed as a package")
	}
	if !slices.Contains(php85, "php85-fpm") || !slices.Contains(php85, "php85-mysqli") || !slices.Contains(php85, "php85-ctype") {
		t.Fatal("PHP 8.5 base package list is incomplete")
	}
	if suffix, ok := phpExtensionPackages["ctype"]; !ok || suffix != "ctype" {
		t.Fatal("ctype must be available in PHP extension management")
	}
	if !slices.Contains(phpPackages("php84"), "php84-opcache") {
		t.Fatal("PHP 8.4 must retain its separate OPcache package")
	}
	if phpService("php85") != "php-fpm85" || phpService("php84") != "php-fpm84" {
		t.Fatal("PHP OpenRC service name is invalid")
	}
}

func TestSystemOverviewParsers(t *testing.T) {
	memory := parseMemoryInfo("MemTotal: 2048000 kB\nMemFree: 128000 kB\nMemAvailable: 1024000 kB\n")
	if memory.Total != 2048000*1024 || memory.Available != 1024000*1024 || memory.Used != 1024000*1024 {
		t.Fatalf("unexpected memory usage: %+v", memory)
	}

	network := parseNetworkDev("Inter-| Receive | Transmit\n  lo: 100 0 0 0 0 0 0 0 200 0\neth0: 1024 0 0 0 0 0 0 0 2048 0\ndocker0: 4096 0 0 0 0 0 0 0 8192 0\n", map[string]bool{"eth0": true})
	if network.ReceivedBytes != 1024 || network.TransmittedBytes != 2048 {
		t.Fatalf("unexpected network usage: %+v", network)
	}

	loads := parseLoadAverage("0.25 0.50 0.75 1/100 123\n")
	if len(loads) != 3 || loads[0] != 0.25 || loads[2] != 0.75 {
		t.Fatalf("unexpected load averages: %v", loads)
	}
}

func TestCPUUsageCalculation(t *testing.T) {
	before, err := parseCPUStat("cpu  100 0 50 850 0 0 0 0 0 0\ncpu0 50 0 25 425\n")
	if err != nil {
		t.Fatal(err)
	}
	after, err := parseCPUStat("cpu  140 0 70 890 0 0 0 0 0 0\n")
	if err != nil {
		t.Fatal(err)
	}
	if usage := cpuPercent(before, after); usage != 60 {
		t.Fatalf("unexpected CPU usage: %.2f", usage)
	}
}

func TestContainerListAndInspectParsing(t *testing.T) {
	list, err := parseContainerList(`{"ID":"abc123","Names":"web","Image":"nginx:alpine","State":"running","Status":"Up 2 hours","Ports":"0.0.0.0:8080->80/tcp","Networks":"frontend","CreatedAt":"2026-07-21 10:00:00 +0800 CST","Labels":"com.docker.compose.project=demo"}`)
	if err != nil || len(list) != 1 {
		t.Fatalf("container list parsing failed: %+v, %v", list, err)
	}
	if list[0].Name != "web" || list[0].ManagedBy != "Docker Compose" || list[0].State != "running" {
		t.Fatalf("unexpected container list item: %+v", list[0])
	}
	applyContainerStats(list, `{"Container":"abc123","Name":"web","CPUPerc":"1.25%","MemUsage":"32MiB / 256MiB","MemPerc":"12.5%","NetIO":"1MB / 2MB","PIDs":"8"}`)
	if list[0].CPUPercent != "1.25%" || list[0].MemoryUsage != "32MiB / 256MiB" || list[0].NetworkIO != "1MB / 2MB" {
		t.Fatalf("container stats were not applied: %+v", list[0])
	}

	detail, err := parseContainerInspect(`[{"Id":"abc123","Name":"/web","Created":"2026-07-21T02:00:00Z","RestartCount":2,"State":{"Status":"running","Running":true,"Paused":false,"ExitCode":0,"StartedAt":"2026-07-21T02:01:00Z"},"Config":{"Hostname":"abc123","Image":"nginx:alpine","Env":["MODE=prod"],"Cmd":["nginx","-g","daemon off;"],"Labels":{"com.docker.compose.project":"demo","com.docker.compose.service":"web"}},"HostConfig":{"RestartPolicy":{"Name":"unless-stopped","MaximumRetryCount":0},"Memory":268435456,"NanoCpus":1500000000,"NetworkMode":"frontend","Privileged":false,"ReadonlyRootfs":true},"Mounts":[{"Type":"bind","Source":"/srv/web","Destination":"/usr/share/nginx/html","Mode":"ro","RW":false}],"NetworkSettings":{"Ports":{"80/tcp":[{"HostIp":"0.0.0.0","HostPort":"8080"}]},"Networks":{"frontend":{}}}}]`)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Name != "web" || detail.CPUs != 1.5 || detail.MemoryBytes != 268435456 || detail.ComposeProject != "demo" || len(detail.Ports) != 1 || len(detail.Mounts) != 1 {
		t.Fatalf("unexpected container detail: %+v", detail)
	}
}

func TestContainerSettingsValidation(t *testing.T) {
	valid := containerSettings{Name: "web-1", RestartPolicy: "unless-stopped", CPUs: 1.5, MemoryMB: 256}
	if err := validateContainerSettings(&valid); err != nil {
		t.Fatalf("valid container settings rejected: %v", err)
	}
	for _, settings := range []containerSettings{
		{Name: "../bad", RestartPolicy: "always"},
		{Name: "web", RestartPolicy: "sometimes"},
		{Name: "web", RestartPolicy: "no", CPUs: -1},
		{Name: "web", RestartPolicy: "no", MemoryMB: 5},
	} {
		if err := validateContainerSettings(&settings); err == nil {
			t.Fatalf("invalid container settings accepted: %+v", settings)
		}
	}
	if err := validateContainerIdentifier("../../etc/passwd"); err == nil {
		t.Fatal("unsafe container identifier accepted")
	}
}

func TestServerSettingsValidation(t *testing.T) {
	hostname, err := validateHostname("Web-01.Example.COM")
	if err != nil || hostname != "web-01.example.com" {
		t.Fatalf("valid hostname rejected: %q, %v", hostname, err)
	}
	for _, value := range []string{"", "-server", "server-", "server_name", "server;reboot", strings.Repeat("a", 64) + ".com"} {
		if _, err := validateHostname(value); err == nil {
			t.Fatalf("invalid hostname accepted: %q", value)
		}
	}
	if timezone, err := validateTimezone("Asia/Shanghai"); err != nil || timezone != "Asia/Shanghai" {
		t.Fatalf("valid timezone rejected: %q, %v", timezone, err)
	}
	for _, value := range []string{"", "../etc/passwd", "/etc/localtime", "Missing/Timezone"} {
		if _, err := validateTimezone(value); err == nil {
			t.Fatalf("invalid timezone accepted: %q", value)
		}
	}
}

func TestSwapParsingAndFstabRendering(t *testing.T) {
	swaps := parseSwaps("Filename Type Size Used Priority\n/swapfile file 2097148 160000 -2\n" + hostDeskSwapPath + " file 1048572 0 -3\n")
	if len(swaps) != 2 || !swaps[0].Active || swaps[0].Managed || !swaps[1].Managed || swaps[1].SizeBytes != 1048572*1024 {
		t.Fatalf("unexpected swap parsing result: %+v", swaps)
	}
	original := []byte("UUID=root / ext4 defaults 0 1\n/swapfile none swap defaults 0 0\n")
	enabled := renderManagedSwapFstab(original, true)
	if strings.Count(string(enabled), hostDeskSwapMarker) != 1 || strings.Count(string(enabled), hostDeskSwapPath+" none swap defaults 0 0") != 1 {
		t.Fatalf("managed swap entry was not added once:\n%s", enabled)
	}
	disabled := renderManagedSwapFstab(enabled, false)
	if strings.Contains(string(disabled), hostDeskSwapPath) || !strings.Contains(string(disabled), "/swapfile none swap defaults 0 0") {
		t.Fatalf("managed swap removal changed unrelated entries:\n%s", disabled)
	}
}

func TestAdminInputValidation(t *testing.T) {
	validSite := siteDefinition{Domain: "example.com", Type: "proxy", Upstream: "http://127.0.0.1:8787"}
	if err := validateSite(&validSite); err != nil {
		t.Fatalf("valid proxy site rejected: %v", err)
	}
	for _, site := range []siteDefinition{
		{Domain: "invalid", Type: "static"},
		{Domain: "example.com", Type: "proxy", Upstream: "file:///etc/passwd"},
		{Domain: "example.com", Type: "static", Root: "/etc"},
	} {
		if err := validateSite(&site); err == nil {
			t.Fatalf("invalid site accepted: %+v", site)
		}
	}
	if err := validateNginxSettings(nginxSettings{ClientMaxBodySize: "64m", KeepaliveTimeout: 65}); err != nil {
		t.Fatalf("valid nginx settings rejected: %v", err)
	}
	if err := validateNginxSettings(nginxSettings{ClientMaxBodySize: "0m", KeepaliveTimeout: 65}); err == nil {
		t.Fatal("invalid nginx size accepted")
	}
	if err := validatePHPSettings(phpSettings{UploadMaxFilesize: "64m", PostMaxSize: "64m", MemoryLimit: "256m", MaxExecutionTime: 60}); err != nil {
		t.Fatalf("valid PHP settings rejected: %v", err)
	}
	if err := validatePHPSettings(phpSettings{UploadMaxFilesize: "64m", PostMaxSize: "64m", MemoryLimit: "256m", MaxExecutionTime: 3601}); err == nil {
		t.Fatal("invalid PHP execution time accepted")
	}
	customRewrite := siteDefinition{
		Domain: "example.com", Type: "php", Root: "/var/www/example.com", RunDirectory: "public",
		RewriteMode: "custom", RewriteRules: "rewrite ^/old$ /new permanent;",
	}
	if err := validateSite(&customRewrite); err != nil {
		t.Fatalf("valid custom rewrite rejected: %v", err)
	}
	if customRewrite.RunDirectory != "public" || siteDocumentRoot(customRewrite) != "/var/www/example.com/public" {
		t.Fatalf("PHP run directory was not normalized: %+v", customRewrite)
	}
	for _, runDirectory := range []string{"../secret", "/etc", "public/../../secret"} {
		invalid := customRewrite
		invalid.RunDirectory = runDirectory
		if err := validateSite(&invalid); err == nil {
			t.Fatalf("unsafe PHP run directory accepted: %q", runDirectory)
		}
	}
	for _, rules := range []string{"include /etc/nginx/nginx.conf;", "rewrite ^ /index.php last; include /etc/nginx/nginx.conf;", "rewrite ^ /index.php last;\n}"} {
		invalid := customRewrite
		invalid.RewriteRules = rules
		if err := validateSite(&invalid); err == nil {
			t.Fatalf("unsafe rewrite rules accepted: %q", rules)
		}
	}
}

func TestRenderPHPRewritePresets(t *testing.T) {
	base := siteDefinition{Domain: "example.com", Type: "php", Root: "/var/www/example.com", RunDirectory: "public"}
	tests := []struct {
		mode string
		want string
	}{
		{"", "try_files $uri $uri/ /index.php?$query_string;"},
		{"none", "try_files $uri $uri/ =404;"},
		{"wordpress", "try_files $uri $uri/ /index.php?$args;"},
		{"thinkphp", "try_files $uri $uri/ /index.php?s=$uri&$args;"},
	}
	for _, test := range tests {
		site := base
		site.RewriteMode = test.mode
		if config := renderSiteConfig(site, defaultNginxSettings()); !strings.Contains(config, test.want) {
			t.Fatalf("rewrite mode %q missing %q", test.mode, test.want)
		}
	}
	site := base
	site.RewriteMode = "custom"
	site.RewriteRules = "rewrite ^/old$ /new permanent;"
	if config := renderSiteConfig(site, defaultNginxSettings()); !strings.Contains(config, "        rewrite ^/old$ /new permanent;") {
		t.Fatal("custom rewrite was not rendered inside the location block")
	}
	if config := renderSiteConfig(base, defaultNginxSettings()); !strings.Contains(config, "error_page 404 /404.html;") || !strings.Contains(config, "location = /404.html { internal; }") {
		t.Fatal("PHP site config does not use the managed 404 page")
	}
	if config := renderSiteConfig(base, defaultNginxSettings()); !strings.Contains(config, "    root /var/www/example.com/public;") {
		t.Fatal("PHP site config does not use the selected run directory")
	}
}

func TestSiteRunDirectoriesListsNestedFoldersWithoutFollowingSymlinks(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"storage", "app/public", "app/cache"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "index.php"), []byte("<?php"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "app"), filepath.Join(root, "linked-app")); err != nil {
		t.Fatal(err)
	}
	directories, err := siteRunDirectories(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"", "app", "app/cache", "app/public", "storage"}
	if !slices.Equal(directories, want) {
		t.Fatalf("directories=%q, want %q", directories, want)
	}
}

func TestSiteRunDirectoriesAllowsMissingRoot(t *testing.T) {
	directories, err := siteRunDirectories(filepath.Join(t.TempDir(), "not-created"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(directories, []string{""}) {
		t.Fatalf("directories=%q, want root option", directories)
	}
}

func TestSiteRunDirectoriesRejectsSymlinkRoot(t *testing.T) {
	target := t.TempDir()
	root := filepath.Join(t.TempDir(), "site")
	if err := os.Symlink(target, root); err != nil {
		t.Fatal(err)
	}
	if _, err := siteRunDirectories(root); err == nil {
		t.Fatal("symlink website root was accepted")
	}
}

func TestRenderSiteConfigUsesCustomOverride(t *testing.T) {
	site := siteDefinition{
		ID:          "example-com",
		Domain:      "example.com",
		Type:        "static",
		Root:        "/var/www/example.com/public",
		NginxConfig: "# custom\nserver { listen 8080; }\n",
	}
	if config := renderSiteConfig(site, defaultNginxSettings()); config != site.NginxConfig {
		t.Fatalf("renderSiteConfig() = %q, want custom override %q", config, site.NginxConfig)
	}
}

func TestNormalizeSiteNginxConfig(t *testing.T) {
	config, err := normalizeSiteNginxConfig("server { listen 80; }")
	if err != nil {
		t.Fatalf("normalizeSiteNginxConfig() error = %v", err)
	}
	if config != "server { listen 80; }\n" {
		t.Fatalf("normalizeSiteNginxConfig() = %q", config)
	}
	for _, value := range []string{" \n\t", "server {\x00}"} {
		if _, err := normalizeSiteNginxConfig(value); err == nil {
			t.Fatalf("normalizeSiteNginxConfig(%q) unexpectedly succeeded", value)
		}
	}
	if _, err := normalizeSiteNginxConfig(strings.Repeat("a", maxSiteNginxConfigBytes+1)); err == nil {
		t.Fatal("oversized Nginx config unexpectedly succeeded")
	}
}

func TestDefaultSiteFiles(t *testing.T) {
	staticRoot := t.TempDir()
	staticSite := siteDefinition{Domain: "static.example.com", Type: "static", Root: staticRoot}
	if err := ensureDefaultSiteFiles(staticSite); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(staticRoot, "index.html")
	index, err := os.ReadFile(indexPath)
	if err != nil || !strings.Contains(string(index), "static.example.com") || !strings.Contains(string(index), "网站运行正常") {
		t.Fatalf("invalid static index: %v, %s", err, index)
	}
	notFound, err := os.ReadFile(filepath.Join(staticRoot, "404.html"))
	if err != nil || !strings.Contains(string(notFound), "404") || !strings.Contains(string(notFound), "static.example.com") {
		t.Fatalf("invalid 404 page: %v, %s", err, notFound)
	}
	if err := os.WriteFile(indexPath, []byte("custom index"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ensureDefaultSiteFiles(staticSite); err != nil {
		t.Fatal(err)
	}
	index, _ = os.ReadFile(indexPath)
	if string(index) != "custom index" {
		t.Fatal("existing site index was overwritten")
	}

	phpRoot := t.TempDir()
	phpSite := siteDefinition{Domain: "php.example.com", Type: "php", Root: phpRoot, RunDirectory: "public"}
	if err := os.MkdirAll(siteDocumentRoot(phpSite), 0755); err != nil {
		t.Fatal(err)
	}
	if err := ensureDefaultSiteFiles(phpSite); err != nil {
		t.Fatal(err)
	}
	probePath := filepath.Join(phpRoot, "public", "index.php")
	probe, err := os.ReadFile(probePath)
	if err != nil || !strings.Contains(string(probe), "PHP_VERSION") || !strings.Contains(string(probe), "upload_max_filesize") || strings.Contains(string(probe), "phpinfo(") {
		t.Fatalf("invalid PHP probe: %v", err)
	}
	if php, err := exec.LookPath("php"); err == nil {
		if output, err := exec.Command(php, "-l", probePath).CombinedOutput(); err != nil {
			t.Fatalf("PHP probe syntax error: %v\n%s", err, output)
		}
	}
}

func TestProtectedDatabaseUsers(t *testing.T) {
	for _, user := range []string{"", "root", "ROOT", "mariadb.sys", "mysql", "PUBLIC"} {
		if !protectedDatabaseUser(user) {
			t.Fatalf("database user %q must be protected", user)
		}
	}
	if protectedDatabaseUser("application_user") {
		t.Fatal("application database user must remain manageable")
	}
}

func TestRenderSiteConfigIncludesManagedSettings(t *testing.T) {
	config := renderSiteConfig(
		siteDefinition{ID: "example-com", Domain: "example.com", Type: "proxy", Upstream: "http://127.0.0.1:8787"},
		nginxSettings{ClientMaxBodySize: "64m", KeepaliveTimeout: 65, Gzip: true},
	)
	for _, directive := range []string{"client_max_body_size 64m;", "keepalive_timeout 65;", "gzip on;"} {
		if !strings.Contains(config, directive) {
			t.Fatalf("managed site config is missing %q", directive)
		}
	}
}

func TestRenderSSLConfigUsesPrivateSessionCache(t *testing.T) {
	config := renderSiteConfig(
		siteDefinition{ID: "example-com", Domain: "example.com", Type: "proxy", Upstream: "http://127.0.0.1:8787", SSL: true, Certificate: "/cert.pem", PrivateKey: "/key.pem"},
		defaultNginxSettings(),
	)
	if !strings.Contains(config, "shared:HostDeskSSL:10m") || strings.Contains(config, "shared:SSL:10m") {
		t.Fatal("HostDesk TLS cache must not conflict with Alpine's shared SSL cache")
	}
}

func TestRenderSiteConfigExposesACMEChallenge(t *testing.T) {
	config := renderSiteConfig(
		siteDefinition{ID: "example-com", Domain: "example.com", Type: "proxy", Upstream: "http://127.0.0.1:8787", SSL: true, Certificate: "/cert.pem", PrivateKey: "/key.pem"},
		defaultNginxSettings(),
	)
	if strings.Count(config, "location ^~ /.well-known/acme-challenge/") != 2 || !strings.Contains(config, acmeHTTPRoot) {
		t.Fatal("HTTP-01 challenge must be served by HTTP and HTTPS server blocks")
	}
}

func TestCertificateDomainValidation(t *testing.T) {
	domains, err := normalizeCertificateDomains([]string{"example.com", "www.example.com", "example.com"}, "http")
	if err != nil || len(domains) != 2 {
		t.Fatalf("valid certificate domains rejected: %v, %v", domains, err)
	}
	if _, err := normalizeCertificateDomains([]string{"*.example.com"}, "http"); err == nil {
		t.Fatal("HTTP-01 must reject wildcard domains")
	}
	if _, err := normalizeCertificateDomains([]string{"*.example.com"}, "dns-cloudflare"); err != nil {
		t.Fatalf("DNS-01 wildcard rejected: %v", err)
	}
}

func testCertificatePEM(t *testing.T, domains []string, notBefore, notAfter time.Time) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: domains[0]}, DNSNames: domains,
		NotBefore: notBefore, NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func TestCustomCertificateValidation(t *testing.T) {
	now := time.Now()
	certificatePEM, privateKeyPEM := testCertificatePEM(t, []string{"example.com", "www.example.com"}, now.Add(-time.Hour), now.Add(time.Hour))
	if err := validateCertificatePEM(certificatePEM, privateKeyPEM, []string{"example.com", "www.example.com"}); err != nil {
		t.Fatalf("valid certificate rejected: %v", err)
	}
	if err := validateCertificatePEM(certificatePEM, privateKeyPEM, []string{"api.example.com"}); err == nil {
		t.Fatal("certificate for another domain was accepted")
	}
	_, otherKey := testCertificatePEM(t, []string{"example.com"}, now.Add(-time.Hour), now.Add(time.Hour))
	if err := validateCertificatePEM(certificatePEM, otherKey, []string{"example.com"}); err == nil {
		t.Fatal("mismatched certificate and private key were accepted")
	}
	expiredCertificate, expiredKey := testCertificatePEM(t, []string{"example.com"}, now.Add(-2*time.Hour), now.Add(-time.Hour))
	if err := validateCertificatePEM(expiredCertificate, expiredKey, []string{"example.com"}); err == nil {
		t.Fatal("expired certificate was accepted")
	}
}

func TestSiteViewDoesNotExposeCertificatePaths(t *testing.T) {
	site := siteDefinition{
		ID: "example-com", Domain: "example.com", Aliases: []string{}, Type: "php", Root: "/var/www/example.com", RunDirectory: "public",
		SSL: true, Certificate: "/secret/fullchain.pem", PrivateKey: "/secret/privkey.pem", NginxConfig: "# secret custom config",
	}
	encoded, err := json.Marshal(siteToView(site, nil))
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if strings.Contains(text, "/secret/") || strings.Contains(text, "privateKey") || strings.Contains(text, `"certificate"`) || strings.Contains(text, "secret custom config") || strings.Contains(text, "nginxConfig") {
		t.Fatalf("site view exposed certificate paths: %s", text)
	}
	if !strings.Contains(text, `"certificateMode":"custom"`) || !strings.Contains(text, `"certificateConfigured":true`) {
		t.Fatalf("site view omitted certificate state: %s", text)
	}
	if !strings.Contains(text, `"runDirectory":"public"`) {
		t.Fatalf("site view omitted PHP run directory: %s", text)
	}
}

func TestCredentialEncryption(t *testing.T) {
	a := &app{dataDir: t.TempDir()}
	encrypted, err := a.encryptCredential("cloudflare-secret-token")
	if err != nil || strings.Contains(string(encrypted), "cloudflare-secret-token") {
		t.Fatalf("credential was not encrypted: %v", err)
	}
	plain, err := a.decryptCredential(encrypted)
	if err != nil || plain != "cloudflare-secret-token" {
		t.Fatalf("credential decrypt failed: %q, %v", plain, err)
	}
}

func TestDNSSettingsAreEncryptedInDatabase(t *testing.T) {
	dataDir := t.TempDir()
	a := newAuthTestApp(t, dataDir)
	a.dataDir = dataDir
	request := dnsSettingsRequest{
		DefaultEmail:    "admin@example.com",
		CloudflareToken: "cloudflare-token-value-123456",
	}
	if err := a.saveDNSSettings(request); err != nil {
		t.Fatal(err)
	}
	for key, plain := range map[string]string{settingCloudflareToken: request.CloudflareToken} {
		stored, err := a.appSetting(key)
		if err != nil || stored == "" || strings.Contains(stored, plain) {
			t.Fatalf("setting %s was not encrypted: value=%q err=%v", key, stored, err)
		}
	}
	view, err := a.dnsSettingsView()
	if err != nil || !view.CloudflareConfigured {
		t.Fatalf("unexpected DNS settings view: %+v err=%v", view, err)
	}
	credentials, err := a.dnsProviderCredentials("cloudflare", "")
	if err != nil || credentials.Token != request.CloudflareToken {
		t.Fatalf("Cloudflare credentials did not round trip: %+v err=%v", credentials, err)
	}
}

func TestDNSSettingsRejectUpdateAndClearTogether(t *testing.T) {
	a := newAuthTestApp(t, t.TempDir())
	for name, request := range map[string]dnsSettingsRequest{
		"cloudflare": {CloudflareToken: "cloudflare-token-value-123456", ClearCloudflare: true},
	} {
		t.Run(name, func(t *testing.T) {
			if err := a.saveDNSSettings(request); err == nil {
				t.Fatal("simultaneous credential update and clear was accepted")
			}
		})
	}
}

func TestSSHSettingsEncryptPassword(t *testing.T) {
	dataDir := t.TempDir()
	a := newAuthTestApp(t, dataDir)
	a.dataDir = dataDir
	a.allowedSSH = map[string]bool{"127.0.0.1": true}
	request := sshSettingsRequest{Host: "127.0.0.1", Port: 22, Username: "root", Password: "server-password"}
	view, err := a.saveSSHSettings(request)
	if err != nil || !view.PasswordConfigured || view.Username != "root" {
		t.Fatalf("SSH settings were not saved: %+v err=%v", view, err)
	}
	stored, err := a.appSetting(settingSSHPassword)
	if err != nil || stored == "" || strings.Contains(stored, request.Password) {
		t.Fatalf("SSH password was not encrypted: value=%q err=%v", stored, err)
	}
	credentials, err := a.savedSSHCredentials()
	if err != nil || credentials.Password != request.Password {
		t.Fatalf("SSH password did not round trip: %+v err=%v", credentials, err)
	}
	if _, err := a.saveSSHSettings(sshSettingsRequest{Host: "not-allowed", Port: 22, Username: "root"}); err == nil {
		t.Fatal("disallowed SSH host was saved")
	}
	if err := a.clearSSHSettings(); err != nil {
		t.Fatal(err)
	}
	view, err = a.sshSettings()
	if err != nil || view.PasswordConfigured || view.Host != "" {
		t.Fatalf("SSH settings were not cleared: %+v err=%v", view, err)
	}
}

func TestVersionComparison(t *testing.T) {
	for _, test := range []struct {
		current string
		latest  string
		want    bool
	}{
		{"v1.0.0", "v1.1.0", true},
		{"v1.2.0", "v1.1.9", false},
		{"v2.0.0", "v2.0.0", false},
		{"dev", "v9.0.0", false},
	} {
		if got := versionLess(test.current, test.latest); got != test.want {
			t.Fatalf("versionLess(%q, %q)=%v, want %v", test.current, test.latest, got, test.want)
		}
	}
}

func TestUpdateAssetVerification(t *testing.T) {
	for architecture, want := range map[string]string{
		"386": "hostdesk-linux-386", "amd64": "hostdesk-linux-amd64", "arm64": "hostdesk-linux-arm64", "arm": "hostdesk-linux-armv7",
	} {
		asset, err := updateAssetName(architecture)
		if err != nil || asset != want {
			t.Fatalf("unexpected asset for %s: %q, %v", architecture, asset, err)
		}
	}
	if _, err := updateAssetName("mips64"); err == nil {
		t.Fatal("unsupported architecture was accepted")
	}
	binary := []byte("verified release binary")
	digest := sha256.Sum256(binary)
	checksums := []byte(hex.EncodeToString(digest[:]) + "  hostdesk-linux-amd64\n")
	if err := verifyUpdateAsset(binary, checksums, "hostdesk-linux-amd64"); err != nil {
		t.Fatalf("valid release checksum rejected: %v", err)
	}
	if err := verifyUpdateAsset([]byte("tampered"), checksums, "hostdesk-linux-amd64"); err == nil {
		t.Fatal("tampered release binary was accepted")
	}
	if err := verifyUpdateAsset(binary, checksums, "hostdesk-linux-arm64"); err == nil {
		t.Fatal("missing architecture checksum was accepted")
	}
}

func TestFTPConfigurationAndValidation(t *testing.T) {
	config := renderVSFTPDConfig()
	for _, directive := range []string{"anonymous_enable=NO", "local_enable=YES", "write_enable=YES", "local_umask=002", "chroot_local_user=YES", "seccomp_sandbox=NO", "local_root=/srv/ftp/$USER", "user_config_dir=/etc/vsftpd/users", "pasv_min_port=40000", "pasv_max_port=40100"} {
		if !strings.Contains(config, directive) {
			t.Fatalf("vsftpd configuration missing %q", directive)
		}
	}
	pamConfig := renderVSFTPDPAMConfig()
	for _, directive := range []string{"auth requisite pam_succeed_if.so user ingroup hostdesk-ftp", "auth include base-auth", "account include base-account", "session include base-session-noninteractive"} {
		if !strings.Contains(pamConfig, directive) {
			t.Fatalf("vsftpd PAM configuration missing %q", directive)
		}
	}
	if !ftpUsernamePattern.MatchString("ftp_user-1") || ftpUsernamePattern.MatchString("Root") {
		t.Fatal("FTP username validation is incorrect")
	}
	if err := validateFTPPassword("strong-password-123"); err != nil {
		t.Fatalf("valid FTP password rejected: %v", err)
	}
	if err := validateFTPPassword("short"); err == nil {
		t.Fatal("short FTP password accepted")
	}
	definition, ok := components()["ftp"]
	if !ok || definition.Service != "vsftpd" || !slices.Contains(definition.Packages, "vsftpd") {
		t.Fatalf("FTP component definition invalid: %+v", definition)
	}
	sites := []siteDefinition{
		{ID: "static", Domain: "static.example.com", Type: "static", Root: "/var/www/static.example.com/public"},
		{ID: "php", Domain: "php.example.com", Type: "php", Root: "/var/www/php.example.com/public"},
		{ID: "proxy", Domain: "proxy.example.com", Type: "proxy", Upstream: "http://127.0.0.1:8080"},
		{ID: "outside", Domain: "outside.example.com", Type: "static", Root: "/srv/outside"},
	}
	options := availableFTPSites(sites)
	if len(options) != 2 || options[0].ID != "static" || options[1].ID != "php" {
		t.Fatalf("unexpected FTP site options: %+v", options)
	}
	if site, err := resolveFTPSite(sites, "php"); err != nil || site.Root != "/var/www/php.example.com/public" {
		t.Fatalf("valid FTP site was not resolved: %+v, %v", site, err)
	}
	if _, err := resolveFTPSite(sites, "proxy"); err == nil {
		t.Fatal("proxy site was accepted as an FTP root")
	}
}

func TestDockerComponentCanBeInstalledAndManaged(t *testing.T) {
	definition, ok := components()["docker"]
	if !ok || definition.Service != "docker" || !slices.Contains(definition.Packages, "docker") {
		t.Fatalf("Docker component definition invalid: %+v", definition)
	}
	if !allowedService("docker") {
		t.Fatal("Docker service is not available to the service controller")
	}
}

func TestCacheComponentsCanBeInstalledAndManaged(t *testing.T) {
	for name, want := range map[string]componentDefinition{
		"redis":     {Packages: []string{"redis"}, Service: "redis"},
		"memcached": {Packages: []string{"memcached"}, Service: "memcached"},
	} {
		definition, ok := components()[name]
		if !ok || definition.Service != want.Service || !slices.Equal(definition.Packages, want.Packages) {
			t.Fatalf("%s component definition invalid: %+v", name, definition)
		}
		if !allowedService(want.Service) {
			t.Fatalf("%s service is not available to the service controller", name)
		}
	}
	if pkg, err := extensionPackage("memcached"); err != nil || pkg != phpPrefix()+"-pecl-memcached" {
		t.Fatalf("PHP memcached extension package=%q, err=%v", pkg, err)
	}
}

func TestFTPUserSiteMigration(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), authDatabaseName)
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE ftp_users (
		username TEXT PRIMARY KEY,
		home TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`)
	if err == nil {
		_, err = db.Exec("INSERT INTO ftp_users (username, home, created_at, updated_at) VALUES ('legacy', '/srv/ftp/legacy', 'now', 'now')")
	}
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}

	db, err = openAuthDatabase(filepath.Dir(databasePath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var siteID string
	if err := db.QueryRow("SELECT site_id FROM ftp_users WHERE username = 'legacy'").Scan(&siteID); err != nil {
		t.Fatal(err)
	}
	if siteID != "" {
		t.Fatalf("legacy FTP user received unexpected site binding: %q", siteID)
	}
}

func TestFileSnapshotRestore(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "config.conf")
	if err := os.WriteFile(filename, []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := captureFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("new\n"), 0644); err != nil {
		t.Fatal(err)
	}
	restoreFiles(snapshot)
	data, err := os.ReadFile(filename)
	if err != nil || string(data) != "old\n" {
		t.Fatalf("snapshot was not restored: %q, %v", data, err)
	}
	info, err := os.Stat(filename)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("snapshot mode was not restored: %v, %v", info.Mode().Perm(), err)
	}
}

func TestCleanRelativeRejectsTraversal(t *testing.T) {
	unsafe := []string{"../etc/passwd", "docs/../../secret", `..\\secret`}
	for _, value := range unsafe {
		if _, err := cleanRelative(value); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
	if value, err := cleanRelative("/docs/./readme.md"); err != nil || value != "docs/readme.md" {
		t.Fatalf("unexpected normalization: %q, %v", value, err)
	}
}

func TestSafeArchiveName(t *testing.T) {
	for _, value := range []string{"../etc/passwd", "/etc/passwd", `C:\\Windows\\file`} {
		if safeArchiveName(value) {
			t.Fatalf("expected %q to be unsafe", value)
		}
	}
	if !safeArchiveName("folder/file.txt") {
		t.Fatal("safe path rejected")
	}
}

func TestRemoteDownloadSavesFileAndUsesResponseName(t *testing.T) {
	root := t.TempDir()
	a := &app{
		root: root, rootReal: root, uploadMax: 1024,
		sessions: map[string]*sessionInfo{"session": {CSRF: "csrf", Expires: time.Now().Add(time.Hour), User: "admin"}},
		remoteClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.Header.Get("User-Agent") != "HostDesk/"+version {
				t.Fatalf("unexpected user agent: %q", request.Header.Get("User-Agent"))
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Disposition": []string{`attachment; filename="remote.txt"`}},
				Body:       io.NopCloser(strings.NewReader("remote content")),
				Request:    request,
			}, nil
		})},
	}
	response := authenticatedRequest(t, a.handleRemoteDownload, http.MethodPost,
		`{"url":"https://example.com/ignored.bin","destination":"","name":""}`,
		"csrf", &http.Cookie{Name: "hostdesk_session", Value: "session"})
	if response.Code != http.StatusCreated {
		t.Fatalf("remote download failed: status=%d body=%s", response.Code, response.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(root, "remote.txt"))
	if err != nil || string(data) != "remote content" {
		t.Fatalf("unexpected downloaded file: %q, %v", data, err)
	}
}

func TestRemoteDownloadRejectsInvalidURLAndIgnoresUploadLimit(t *testing.T) {
	root := t.TempDir()
	a := &app{
		root: root, rootReal: root, uploadMax: 4,
		sessions: map[string]*sessionInfo{"session": {CSRF: "csrf", Expires: time.Now().Add(time.Hour), User: "admin"}},
		remoteClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        make(http.Header),
				Body:          io.NopCloser(strings.NewReader("larger than upload limit")),
				ContentLength: int64(len("larger than upload limit")),
				Request:       request,
			}, nil
		})},
	}
	cookie := &http.Cookie{Name: "hostdesk_session", Value: "session"}
	response := authenticatedRequest(t, a.handleRemoteDownload, http.MethodPost,
		`{"url":"file:///etc/passwd","destination":"","name":"passwd"}`, "csrf", cookie)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid remote URL returned %d: %s", response.Code, response.Body.String())
	}
	response = authenticatedRequest(t, a.handleRemoteDownload, http.MethodPost,
		`{"url":"https://example.com/large.bin","destination":"","name":"large.bin"}`, "csrf", cookie)
	if response.Code != http.StatusCreated {
		t.Fatalf("remote file above upload limit returned %d: %s", response.Code, response.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(root, "large.bin"))
	if err != nil || string(data) != "larger than upload limit" {
		t.Fatalf("unexpected unlimited remote download: %q, %v", data, err)
	}
}

func TestFilePermissionsUpdatesOwnershipModeAndChildren(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "folder")
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(directory, "child.txt")
	if err := os.WriteFile(child, []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	a := &app{
		root: root, rootReal: root,
		sessions: map[string]*sessionInfo{"session": {CSRF: "csrf", Expires: time.Now().Add(time.Hour), User: "admin"}},
	}
	body := fmt.Sprintf(`{"path":"folder","owner":%q,"group":%q,"mode":"0750","recursive":true}`, strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getgid()))
	response := authenticatedRequest(t, a.handleFilePermissions, http.MethodPost, body, "csrf", &http.Cookie{Name: "hostdesk_session", Value: "session"})
	if response.Code != http.StatusOK {
		t.Fatalf("permission update failed: status=%d body=%s", response.Code, response.Body.String())
	}
	for _, filename := range []string{directory, child} {
		info, err := os.Stat(filename)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0750 {
			t.Fatalf("unexpected mode for %s: %v", filename, info.Mode().Perm())
		}
	}
}

func TestCreatedFileMetadataInheritsParentOwnership(t *testing.T) {
	parent := t.TempDir()
	if os.Geteuid() == 0 {
		if err := os.Chown(parent, 65534, 65534); err != nil {
			t.Logf("cannot assign alternate test ownership: %v", err)
		}
	}
	child := filepath.Join(parent, "upload.txt")
	if err := os.WriteFile(child, []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	parentInfo, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := inheritPathMetadata(child, parent, 0644); err != nil {
		t.Fatal(err)
	}
	childInfo, err := os.Stat(child)
	if err != nil {
		t.Fatal(err)
	}
	if ownershipFromInfo(childInfo) != ownershipFromInfo(parentInfo) {
		t.Fatalf("child ownership=%+v, want parent ownership=%+v", ownershipFromInfo(childInfo), ownershipFromInfo(parentInfo))
	}
	if childInfo.Mode().Perm() != 0644 {
		t.Fatalf("child mode=%v, want 0644", childInfo.Mode().Perm())
	}
}

func TestUploadUsesDestinationOwnershipAndReadableMode(t *testing.T) {
	root := t.TempDir()
	a := &app{
		root: root, rootReal: root, uploadMax: 1024,
		sessions: map[string]*sessionInfo{"session": {CSRF: "csrf", Expires: time.Now().Add(time.Hour), User: "admin"}},
	}
	request := httptest.NewRequest(http.MethodPost, "/?dir=&name=upload.txt", strings.NewReader("content"))
	request.Header.Set("X-CSRF-Token", "csrf")
	request.AddCookie(&http.Cookie{Name: "hostdesk_session", Value: "session"})
	response := httptest.NewRecorder()
	a.handleUpload(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("upload failed: status=%d body=%s", response.Code, response.Body.String())
	}
	parentInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Stat(filepath.Join(root, "upload.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if ownershipFromInfo(fileInfo) != ownershipFromInfo(parentInfo) {
		t.Fatalf("upload ownership=%+v, want destination ownership=%+v", ownershipFromInfo(fileInfo), ownershipFromInfo(parentInfo))
	}
	if fileInfo.Mode().Perm() != 0644 {
		t.Fatalf("upload mode=%v, want 0644", fileInfo.Mode().Perm())
	}
}

func TestCreateDoesNotRemoveExistingTarget(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(existing, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	a := &app{
		root: root, rootReal: root,
		sessions: map[string]*sessionInfo{"session": {CSRF: "csrf", Expires: time.Now().Add(time.Hour), User: "admin"}},
	}
	response := authenticatedRequest(t, a.handleCreate, http.MethodPost,
		`{"path":"existing.txt","type":"file"}`, "csrf", &http.Cookie{Name: "hostdesk_session", Value: "session"})
	if response.Code == http.StatusCreated {
		t.Fatalf("existing target was unexpectedly created: %s", response.Body.String())
	}
	content, err := os.ReadFile(existing)
	if err != nil || string(content) != "keep" {
		t.Fatalf("existing target was changed or removed: %q, %v", content, err)
	}
}

func TestCopyPathUsesDestinationOwnership(t *testing.T) {
	sourceRoot := t.TempDir()
	source := filepath.Join(sourceRoot, "source")
	if err := os.Mkdir(source, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "file.txt"), []byte("content"), 0640); err != nil {
		t.Fatal(err)
	}
	destinationRoot := t.TempDir()
	destinationInfo, err := os.Stat(destinationRoot)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(destinationRoot, "copy")
	ownership := ownershipFromInfo(destinationInfo)
	if err := copyPath(source, destination, ownership); err != nil {
		t.Fatal(err)
	}
	for _, filename := range []string{destination, filepath.Join(destination, "file.txt")} {
		info, err := os.Stat(filename)
		if err != nil {
			t.Fatal(err)
		}
		if ownershipFromInfo(info) != ownership {
			t.Fatalf("%s ownership=%+v, want %+v", filename, ownershipFromInfo(info), ownership)
		}
	}
}

func TestCopyPathDoesNotRemoveExistingTarget(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.txt")
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(source, []byte("source"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := copyPath(source, target, ownershipFromInfo(rootInfo)); !errors.Is(err, os.ErrExist) {
		t.Fatalf("copy error=%v, want os.ErrExist", err)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "keep" {
		t.Fatalf("existing target was changed or removed: %q, %v", content, err)
	}
}

func TestAddToArchiveUsesSelectedNameAsRoot(t *testing.T) {
	root := t.TempDir()
	selected := filepath.Join(root, "site", "public")
	if err := os.MkdirAll(selected, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(selected, "index.php"), []byte("<?php phpinfo();"), 0644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	if err := addToArchive(writer, selected); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(&output)
	var names []string
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
	if !slices.Equal(names, []string{"public/", "public/index.php"}) {
		t.Fatalf("unexpected archive paths: %v", names)
	}
}

func TestExtractZipRejectsTraversalAndLinks(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "bad.zip")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	item, _ := writer.Create("../escape.txt")
	_, _ = item.Write([]byte("bad"))
	_ = writer.Close()
	_ = file.Close()
	a := &app{extractMax: defaultExtractMax}
	if err := a.extractZip(archive, root); err == nil {
		t.Fatal("expected traversal archive to be rejected")
	}
}

func TestExtractTarRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "bad.tar.gz")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	writer := tar.NewWriter(gzipWriter)
	_ = writer.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc", Mode: 0777})
	_ = writer.Close()
	_ = gzipWriter.Close()
	_ = file.Close()
	a := &app{extractMax: defaultExtractMax}
	if err := a.extractTar(archive, root, true); err == nil {
		t.Fatal("expected symlink archive to be rejected")
	}
}

func TestExtractTarInheritsDestinationGroup(t *testing.T) {
	destination := t.TempDir()
	desiredGroup := -1
	if os.Geteuid() == 0 {
		desiredGroup = 65534
	} else if groups, err := os.Getgroups(); err == nil {
		for _, group := range groups {
			if group != os.Getgid() {
				desiredGroup = group
				break
			}
		}
	}
	if desiredGroup < 0 {
		t.Skip("no alternate group available for inheritance test")
	}
	if err := os.Chown(destination, -1, desiredGroup); err != nil {
		t.Skipf("cannot set destination group: %v", err)
	}
	archive := filepath.Join(t.TempDir(), "content.tar")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	if err := writer.WriteHeader(&tar.Header{Name: "nested/file.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: 4}); err == nil {
		_, err = writer.Write([]byte("test"))
	}
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	a := &app{extractMax: defaultExtractMax}
	if err := a.extractTar(archive, destination, false); err != nil {
		t.Fatal(err)
	}
	for _, filename := range []string{filepath.Join(destination, "nested"), filepath.Join(destination, "nested", "file.txt")} {
		info, err := os.Stat(filename)
		if err != nil {
			t.Fatal(err)
		}
		if ownership := ownershipFromInfo(info); ownership.GID != desiredGroup {
			t.Fatalf("%s group=%d, want %d", filename, ownership.GID, desiredGroup)
		}
	}
}

func TestPrepareExtractPathRejectsExistingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareExtractPath(root, filepath.Join(root, "linked", "file.txt"), false, ownershipFromInfo(info)); err == nil {
		t.Fatal("expected existing symlink to be rejected")
	}
}
