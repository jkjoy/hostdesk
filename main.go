package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	osuser "os/user"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/scrypt"
	"golang.org/x/crypto/ssh"
)

//go:embed public
var embeddedFiles embed.FS

const (
	sessionTTL         = 12 * time.Hour
	authRequestTimeout = 15 * time.Second
	maxEditorBytes     = 4 << 20
	maxJSONBytes       = 6 << 20
	defaultUploadMax   = 256 << 20
	defaultExtractMax  = int64(2 << 30)
)

type sessionInfo struct {
	CSRF    string
	Expires time.Time
	User    string
}

type app struct {
	root            string
	rootReal        string
	dataDir         string
	db              *sql.DB
	uploadMax       int64
	extractMax      int64
	secureCookie    bool
	allowedSSH      map[string]bool
	hostFingerprint string
	remoteClient    *http.Client

	mu              sync.Mutex
	adminMu         sync.Mutex
	publicIPMu      sync.Mutex
	updateMu        sync.Mutex
	updateInstallMu sync.Mutex
	sessions        map[string]*sessionInfo
	publicIP        string
	publicIPExpires time.Time
	updateCache     updateStatus
	updateExpires   time.Time
}

type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return e.Message }

type fileEntry struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Type     string    `json:"type"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	Mode     string    `json:"mode"`
	Owner    string    `json:"owner"`
	Group    string    `json:"group"`
}

type fileOwnership struct {
	UID int
	GID int
}

type resolvedPath struct {
	Absolute string
	Relative string
	Real     string
}

func main() {
	a, err := newApp()
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/setup", a.handleSetupState)
	mux.HandleFunc("POST /api/setup", a.handleSetup)
	mux.HandleFunc("POST /api/login", a.handleLogin)
	mux.HandleFunc("GET /api/session", a.handleSession)
	mux.HandleFunc("POST /api/logout", a.handleLogout)
	mux.HandleFunc("GET /api/admin/account", a.handleAccountGet)
	mux.HandleFunc("PUT /api/admin/account", a.handleAccountUpdate)
	mux.HandleFunc("GET /api/files", a.handleFiles)
	mux.HandleFunc("GET /api/file", a.handleReadFile)
	mux.HandleFunc("PUT /api/file", a.handleSaveFile)
	mux.HandleFunc("POST /api/create", a.handleCreate)
	mux.HandleFunc("POST /api/delete", a.handleDelete)
	mux.HandleFunc("POST /api/move", a.handleMove)
	mux.HandleFunc("POST /api/copy", a.handleCopy)
	mux.HandleFunc("POST /api/upload", a.handleUpload)
	mux.HandleFunc("POST /api/remote-download", a.handleRemoteDownload)
	mux.HandleFunc("GET /api/download", a.handleDownload)
	mux.HandleFunc("GET /api/file-identities", a.handleFileIdentities)
	mux.HandleFunc("POST /api/file-permissions", a.handleFilePermissions)
	mux.HandleFunc("POST /api/archive", a.handleArchive)
	mux.HandleFunc("POST /api/extract", a.handleExtract)
	mux.HandleFunc("GET /api/admin/overview", a.handleAdminOverview)
	mux.HandleFunc("GET /api/admin/update", a.handleUpdateCheck)
	mux.HandleFunc("POST /api/admin/update", a.handleUpdateInstall)
	mux.HandleFunc("GET /api/admin/server-settings", a.handleServerSettingsGet)
	mux.HandleFunc("PUT /api/admin/server-settings", a.handleServerSettingsPut)
	mux.HandleFunc("POST /api/admin/server-settings/swap", a.handleSwapCreate)
	mux.HandleFunc("DELETE /api/admin/server-settings/swap", a.handleSwapDelete)
	mux.HandleFunc("POST /api/admin/components/{component}/install", a.handleComponentInstall)
	mux.HandleFunc("POST /api/admin/components/{component}/remove", a.handleComponentRemove)
	mux.HandleFunc("POST /api/admin/services/{service}/{action}", a.handleServiceAction)
	mux.HandleFunc("GET /api/admin/nginx/settings", a.handleNginxSettingsGet)
	mux.HandleFunc("PUT /api/admin/nginx/settings", a.handleNginxSettingsPut)
	mux.HandleFunc("GET /api/admin/sites", a.handleSitesList)
	mux.HandleFunc("GET /api/admin/site-directories", a.handleSiteDirectoriesList)
	mux.HandleFunc("POST /api/admin/sites", a.handleSiteCreate)
	mux.HandleFunc("PUT /api/admin/sites/{id}", a.handleSiteUpdate)
	mux.HandleFunc("DELETE /api/admin/sites/{id}", a.handleSiteDelete)
	mux.HandleFunc("GET /api/admin/sites/{id}/nginx", a.handleSiteNginxGet)
	mux.HandleFunc("PUT /api/admin/sites/{id}/nginx", a.handleSiteNginxPut)
	mux.HandleFunc("DELETE /api/admin/sites/{id}/nginx", a.handleSiteNginxDelete)
	mux.HandleFunc("POST /api/admin/sites/{id}/{action}", a.handleSiteAction)
	mux.HandleFunc("GET /api/admin/php", a.handlePHPGet)
	mux.HandleFunc("PUT /api/admin/php/settings", a.handlePHPSettingsPut)
	mux.HandleFunc("POST /api/admin/php/extensions/{extension}/install", a.handlePHPExtensionInstall)
	mux.HandleFunc("POST /api/admin/php/extensions/{extension}/remove", a.handlePHPExtensionRemove)
	mux.HandleFunc("GET /api/admin/databases", a.handleDatabasesList)
	mux.HandleFunc("POST /api/admin/databases", a.handleDatabaseCreate)
	mux.HandleFunc("DELETE /api/admin/databases/{database}", a.handleDatabaseDelete)
	mux.HandleFunc("GET /api/admin/database-users", a.handleDatabaseUsersList)
	mux.HandleFunc("POST /api/admin/database-users", a.handleDatabaseUserCreate)
	mux.HandleFunc("PUT /api/admin/database-users/{user}", a.handleDatabaseUserUpdate)
	mux.HandleFunc("DELETE /api/admin/database-users/{user}", a.handleDatabaseUserDelete)
	mux.HandleFunc("GET /api/admin/certificates", a.handleCertificatesList)
	mux.HandleFunc("POST /api/admin/certificates", a.handleCertificateCreate)
	mux.HandleFunc("POST /api/admin/certificates/{id}/renew", a.handleCertificateRenew)
	mux.HandleFunc("GET /api/admin/dns-settings", a.handleDNSSettingsGet)
	mux.HandleFunc("PUT /api/admin/dns-settings", a.handleDNSSettingsPut)
	mux.HandleFunc("GET /api/admin/ftp", a.handleFTPGet)
	mux.HandleFunc("POST /api/admin/ftp/users", a.handleFTPUserCreate)
	mux.HandleFunc("PUT /api/admin/ftp/users/{username}", a.handleFTPUserUpdate)
	mux.HandleFunc("DELETE /api/admin/ftp/users/{username}", a.handleFTPUserDelete)
	mux.HandleFunc("GET /api/admin/containers", a.handleContainersList)
	mux.HandleFunc("GET /api/admin/containers/{id}", a.handleContainerGet)
	mux.HandleFunc("PUT /api/admin/containers/{id}", a.handleContainerUpdate)
	mux.HandleFunc("DELETE /api/admin/containers/{id}", a.handleContainerDelete)
	mux.HandleFunc("GET /api/admin/containers/{id}/logs", a.handleContainerLogs)
	mux.HandleFunc("POST /api/admin/containers/{id}/{action}", a.handleContainerAction)
	mux.HandleFunc("GET /api/admin/ssh-settings", a.handleSSHSettingsGet)
	mux.HandleFunc("PUT /api/admin/ssh-settings", a.handleSSHSettingsPut)
	mux.HandleFunc("DELETE /api/admin/ssh-settings", a.handleSSHSettingsDelete)
	mux.HandleFunc("GET /ws/ssh", a.handleSSH)
	mux.HandleFunc("GET /ws/terminal", a.handleTerminal)
	appIndex, err := embeddedFiles.ReadFile("public/app/index.html")
	if err != nil {
		log.Fatal(err)
	}
	static, err := fs.Sub(embeddedFiles, "public")
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/", spaHandler(appIndex, http.FileServer(http.FS(static))))

	host := envString("HOST", "127.0.0.1")
	port := envInt("PORT", 8787)
	server := &http.Server{
		Addr:              net.JoinHostPort(host, strconv.Itoa(port)),
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	log.Printf("HostDesk 已启动：http://%s", server.Addr)
	log.Printf("文件管理根目录：%s", a.rootReal)
	go a.certificateRenewalLoop()
	log.Fatal(server.ListenAndServe())
}

func spaHandler(appIndex []byte, static http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
		staticPath := requestPath == "/app" || requestPath == "/vendor" || strings.HasPrefix(requestPath, "/app/") || strings.HasPrefix(requestPath, "/vendor/")
		reservedPath := requestPath == "/api" || requestPath == "/ws" || strings.HasPrefix(requestPath, "/api/") || strings.HasPrefix(requestPath, "/ws/")
		if r.Method == http.MethodGet && !staticPath && !reservedPath && path.Ext(requestPath) == "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(appIndex)
			return
		}
		static.ServeHTTP(w, r)
	})
}

func newApp() (*app, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	root, err := filepath.Abs(envString("FILE_ROOT", home))
	if err != nil {
		return nil, err
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("FILE_ROOT 无效: %w", err)
	}
	configBase, err := os.UserConfigDir()
	if err != nil {
		configBase = home
	}
	dataDir := envString("DATA_DIR", filepath.Join(configBase, "hostdesk"))
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	db, err := openAuthDatabase(dataDir)
	if err != nil {
		return nil, err
	}

	allowed := make(map[string]bool)
	for _, host := range strings.Split(envString("SSH_HOSTS", "127.0.0.1,localhost,::1"), ",") {
		allowed[strings.TrimSpace(host)] = true
	}
	a := &app{
		root:            root,
		rootReal:        rootReal,
		dataDir:         dataDir,
		db:              db,
		uploadMax:       int64(envInt("MAX_UPLOAD_MB", 256)) << 20,
		extractMax:      int64(envInt("MAX_EXTRACT_MB", 2048)) << 20,
		secureCookie:    envBool("COOKIE_SECURE", false),
		allowedSSH:      allowed,
		hostFingerprint: strings.TrimSpace(os.Getenv("SSH_HOST_KEY_SHA256")),
		remoteClient:    remoteDownloadClient(),
		sessions:        make(map[string]*sessionInfo),
	}
	if err := a.migrateLegacyFTPBindings(); err != nil {
		log.Printf("迁移旧 FTP 网站绑定失败：%v", err)
	}
	return a, nil
}

func derivePassword(password, encodedSalt string) (string, error) {
	salt, err := base64.RawURLEncoding.DecodeString(encodedSalt)
	if err != nil {
		return "", err
	}
	key, err := scrypt.Key([]byte(password), salt, 32768, 8, 1, 32)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(key), nil
}

func randomToken(size int) string {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

func envString(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(key))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func envBool(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	return parsed && err == nil
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self' ws: wss:; img-src 'self' data:")
		if r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "服务器错误"
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		status, message = apiErr.Status, apiErr.Message
	} else if errors.Is(err, os.ErrNotExist) {
		status, message = http.StatusNotFound, "文件或目录不存在"
	} else if errors.Is(err, os.ErrExist) {
		status, message = http.StatusConflict, "目标已存在"
	} else if errors.Is(err, os.ErrPermission) {
		status, message = http.StatusForbidden, "没有操作权限"
	} else {
		log.Printf("请求失败: %v", err)
	}
	writeJSON(w, status, map[string]string{"error": message})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return &apiError{http.StatusRequestTimeout, "请求读取超时"}
		}
		return &apiError{http.StatusBadRequest, "请求格式错误"}
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return &apiError{http.StatusBadRequest, "请求格式错误"}
	}
	return nil
}

func limitRequestReadTime(w http.ResponseWriter, duration time.Duration) func() {
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(time.Now().Add(duration)); err != nil {
		return func() {}
	}
	return func() { _ = controller.SetReadDeadline(time.Time{}) }
}

func (a *app) session(r *http.Request) (*sessionInfo, string) {
	cookie, err := r.Cookie("hostdesk_session")
	if err != nil {
		return nil, ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	session := a.sessions[cookie.Value]
	if session == nil || time.Now().After(session.Expires) {
		delete(a.sessions, cookie.Value)
		return nil, ""
	}
	session.Expires = time.Now().Add(sessionTTL)
	return session, cookie.Value
}

func (a *app) authorize(w http.ResponseWriter, r *http.Request, mutation bool) *sessionInfo {
	session, _ := a.session(r)
	if session == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "登录已过期"})
		return nil
	}
	if mutation && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(session.CSRF)) != 1 {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "请求校验失败"})
		return nil
	}
	return session
}

func (a *app) handleLogin(w http.ResponseWriter, r *http.Request) {
	clearDeadline := limitRequestReadTime(w, authRequestTimeout)
	defer clearDeadline()
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	now := time.Now()
	administrator, err := a.administrator()
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "请先初始化管理员账号", "setupRequired": true})
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	clientIP := requestClientIP(r)
	validUser := subtle.ConstantTimeCompare([]byte(body.Username), []byte(administrator.Username)) == 1
	identities := loginProtectionIdentities(clientIP, administrator.Username, validUser)
	remaining, err := a.loginLockRemaining(identities, now)
	if err != nil {
		writeError(w, err)
		return
	}
	if remaining > 0 {
		log.Printf("security login_rate_limited ip=%q", clientIP)
		w.Header().Set("Retry-After", strconv.Itoa(int((remaining+time.Second-1)/time.Second)))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "登录尝试过多，请稍后再试"})
		return
	}

	candidate := ""
	if len(body.Password) <= 1024 {
		candidate, err = derivePassword(body.Password, administrator.Salt)
	}
	validPassword := err == nil && subtle.ConstantTimeCompare([]byte(candidate), []byte(administrator.Hash)) == 1
	if !validPassword || !validUser {
		if err := a.recordLoginFailure(identities, now); err != nil {
			writeError(w, err)
			return
		}
		log.Printf("security login_failed ip=%q username_valid=%t", clientIP, validUser)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "账号或密码错误"})
		return
	}
	if err := a.clearLoginFailures(identities); err != nil {
		writeError(w, err)
		return
	}

	id, session := a.newSession(administrator.Username, now)
	a.mu.Lock()
	a.sessions[id] = session
	a.mu.Unlock()
	a.setSessionCookie(w, id)
	log.Printf("security login_success ip=%q", clientIP)
	writeJSON(w, http.StatusOK, map[string]string{"csrf": session.CSRF, "user": session.User, "fileRoot": a.rootReal})
}

func (a *app) handleSession(w http.ResponseWriter, r *http.Request) {
	session := a.authorize(w, r, false)
	if session == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"csrf": session.CSRF, "user": session.User, "fileRoot": a.rootReal})
}

func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	_, id := a.session(r)
	a.mu.Lock()
	delete(a.sessions, id)
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "hostdesk_session", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.secureCookie, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func cleanRelative(input string) (string, error) {
	if strings.ContainsRune(input, 0) {
		return "", &apiError{http.StatusBadRequest, "无效路径"}
	}
	value := strings.TrimLeft(strings.ReplaceAll(input, "\\", "/"), "/")
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return "", &apiError{http.StatusBadRequest, "路径不能包含 .."}
		}
	}
	value = path.Clean(value)
	if value == "." {
		return "", nil
	}
	return value, nil
}

func (a *app) resolve(input string) (resolvedPath, error) {
	relative, err := cleanRelative(input)
	if err != nil {
		return resolvedPath{}, err
	}
	absolute := filepath.Join(a.root, filepath.FromSlash(relative))
	relCheck, err := filepath.Rel(a.root, absolute)
	if err != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
		return resolvedPath{}, &apiError{http.StatusForbidden, "路径超出管理范围"}
	}
	return resolvedPath{Absolute: absolute, Relative: relative}, nil
}

func inside(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (a *app) existing(input string) (resolvedPath, error) {
	target, err := a.resolve(input)
	if err != nil {
		return resolvedPath{}, err
	}
	real, err := filepath.EvalSymlinks(target.Absolute)
	if err != nil {
		return resolvedPath{}, err
	}
	if !inside(a.rootReal, real) {
		return resolvedPath{}, &apiError{http.StatusForbidden, "符号链接指向管理范围以外"}
	}
	target.Real = real
	return target, nil
}

func (a *app) writable(input string) (resolvedPath, error) {
	target, err := a.resolve(input)
	if err != nil {
		return resolvedPath{}, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(target.Absolute))
	if err != nil {
		return resolvedPath{}, err
	}
	if !inside(a.rootReal, parent) {
		return resolvedPath{}, &apiError{http.StatusForbidden, "目标目录超出管理范围"}
	}
	return target, nil
}

func fileType(info fs.FileInfo) string {
	if info.IsDir() {
		return "directory"
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "link"
	}
	return "file"
}

func identityFile(filename string) ([]string, map[uint32]string) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return []string{}, map[uint32]string{}
	}
	names := make([]string, 0)
	byID := make(map[uint32]string)
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Split(line, ":")
		if len(parts) < 3 || parts[0] == "" {
			continue
		}
		identifier, err := strconv.ParseUint(parts[2], 10, 32)
		if err != nil {
			continue
		}
		byID[uint32(identifier)] = parts[0]
		if !seen[parts[0]] {
			seen[parts[0]] = true
			names = append(names, parts[0])
		}
	}
	sort.Strings(names)
	return names, byID
}

func ownershipFromInfo(info fs.FileInfo) fileOwnership {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileOwnership{UID: -1, GID: -1}
	}
	return fileOwnership{UID: int(stat.Uid), GID: int(stat.Gid)}
}

func identityLabel(identifier int, names map[uint32]string) string {
	if name := names[uint32(identifier)]; name != "" {
		return name
	}
	return strconv.Itoa(identifier)
}

func (a *app) handleFiles(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, false) == nil {
		return
	}
	target, err := a.existing(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, err)
		return
	}
	entries, err := os.ReadDir(target.Real)
	if err != nil {
		writeError(w, err)
		return
	}
	result := make([]fileEntry, 0, len(entries))
	_, usersByID := identityFile("/etc/passwd")
	_, groupsByID := identityFile("/etc/group")
	for _, entry := range entries {
		info, err := os.Lstat(filepath.Join(target.Real, entry.Name()))
		if err != nil {
			continue
		}
		ownership := ownershipFromInfo(info)
		result = append(result, fileEntry{Name: entry.Name(), Path: path.Join(target.Relative, entry.Name()), Type: fileType(info), Size: info.Size(), Modified: info.ModTime(), Mode: fmt.Sprintf("%03o", info.Mode().Perm()), Owner: identityLabel(ownership.UID, usersByID), Group: identityLabel(ownership.GID, groupsByID)})
	}
	sort.Slice(result, func(i, j int) bool {
		if (result[i].Type == "directory") != (result[j].Type == "directory") {
			return result[i].Type == "directory"
		}
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})
	writeJSON(w, http.StatusOK, map[string]any{"path": target.Relative, "entries": result})
}

func (a *app) handleFileIdentities(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, false) == nil {
		return
	}
	users, _ := identityFile("/etc/passwd")
	groups, _ := identityFile("/etc/group")
	writeJSON(w, http.StatusOK, map[string]any{"users": users, "groups": groups})
}

func identityID(value string, group bool) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return -1, nil
	}
	if identifier, err := strconv.Atoi(value); err == nil && identifier >= 0 {
		return identifier, nil
	}
	var identifier string
	if group {
		entry, err := osuser.LookupGroup(value)
		if err != nil {
			return -1, &apiError{http.StatusBadRequest, "用户组不存在"}
		}
		identifier = entry.Gid
	} else {
		entry, err := osuser.Lookup(value)
		if err != nil {
			return -1, &apiError{http.StatusBadRequest, "用户不存在"}
		}
		identifier = entry.Uid
	}
	result, err := strconv.Atoi(identifier)
	if err != nil || result < 0 {
		return -1, &apiError{http.StatusBadRequest, "系统账号信息无效"}
	}
	return result, nil
}

func applyFilePermissions(filename string, info fs.FileInfo, ownership fileOwnership, mode fs.FileMode) error {
	if info.Mode()&os.ModeSymlink != 0 {
		if ownership.UID >= 0 || ownership.GID >= 0 {
			return os.Lchown(filename, ownership.UID, ownership.GID)
		}
		return nil
	}
	if ownership.UID >= 0 || ownership.GID >= 0 {
		if err := os.Chown(filename, ownership.UID, ownership.GID); err != nil {
			return err
		}
	}
	return os.Chmod(filename, mode)
}

func applyPathMetadata(filename string, ownership fileOwnership, mode fs.FileMode) error {
	info, err := os.Lstat(filename)
	if err != nil {
		return err
	}
	current := ownershipFromInfo(info)
	if current != ownership {
		if info.Mode()&os.ModeSymlink != 0 {
			if err := os.Lchown(filename, ownership.UID, ownership.GID); err != nil {
				return err
			}
		} else if err := os.Chown(filename, ownership.UID, ownership.GID); err != nil {
			return err
		}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	return os.Chmod(filename, mode)
}

func inheritPathMetadata(filename, parent string, mode fs.FileMode) error {
	info, err := os.Stat(parent)
	if err != nil {
		return err
	}
	return applyPathMetadata(filename, ownershipFromInfo(info), mode)
}

func (a *app) handleFilePermissions(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	var body struct {
		Path      string `json:"path"`
		Owner     string `json:"owner"`
		Group     string `json:"group"`
		Mode      string `json:"mode"`
		Recursive bool   `json:"recursive"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	target, err := a.existing(body.Path)
	if err != nil {
		writeError(w, err)
		return
	}
	modeText := strings.TrimSpace(body.Mode)
	if len(modeText) == 4 && modeText[0] == '0' {
		modeText = modeText[1:]
	}
	if len(modeText) != 3 || strings.Trim(modeText, "01234567") != "" {
		writeError(w, &apiError{http.StatusBadRequest, "权限模式必须是 000 到 777"})
		return
	}
	modeValue, _ := strconv.ParseUint(modeText, 8, 32)
	uid, err := identityID(body.Owner, false)
	if err != nil {
		writeError(w, err)
		return
	}
	gid, err := identityID(body.Group, true)
	if err != nil {
		writeError(w, err)
		return
	}
	apply := func(filename string, info fs.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		return applyFilePermissions(filename, info, fileOwnership{UID: uid, GID: gid}, fs.FileMode(modeValue))
	}
	if body.Recursive {
		err = filepath.Walk(target.Real, apply)
	} else {
		var info fs.FileInfo
		info, err = os.Lstat(target.Real)
		if err == nil {
			err = apply(target.Real, info, nil)
		}
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleReadFile(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, false) == nil {
		return
	}
	target, err := a.existing(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, err)
		return
	}
	info, err := os.Stat(target.Real)
	if err != nil {
		writeError(w, err)
		return
	}
	if !info.Mode().IsRegular() {
		writeError(w, &apiError{http.StatusBadRequest, "不是普通文件"})
		return
	}
	if info.Size() > maxEditorBytes {
		writeError(w, &apiError{http.StatusRequestEntityTooLarge, "在线编辑仅支持 4 MB 以内的文件"})
		return
	}
	data, err := os.ReadFile(target.Real)
	if err != nil {
		writeError(w, err)
		return
	}
	for _, value := range data[:min(len(data), 8192)] {
		if value == 0 {
			writeError(w, &apiError{http.StatusUnsupportedMediaType, "二进制文件不能在线编辑"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": target.Relative, "content": string(data), "modified": info.ModTime()})
}

func (a *app) handleSaveFile(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	target, err := a.existing(body.Path)
	if err != nil {
		writeError(w, err)
		return
	}
	info, err := os.Stat(target.Real)
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, &apiError{http.StatusBadRequest, "不是普通文件"})
		return
	}
	temp, err := os.CreateTemp(filepath.Dir(target.Real), ".hostdesk-*.tmp")
	if err == nil {
		err = temp.Chmod(info.Mode().Perm())
	}
	if err == nil {
		_, err = io.WriteString(temp, body.Content)
	}
	if temp != nil {
		if closeErr := temp.Close(); err == nil {
			err = closeErr
		}
	}
	if err == nil {
		err = applyPathMetadata(temp.Name(), ownershipFromInfo(info), info.Mode().Perm())
	}
	if err == nil {
		err = os.Rename(temp.Name(), target.Real)
	}
	if err != nil {
		if temp != nil {
			_ = os.Remove(temp.Name())
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleCreate(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	var body struct {
		Path string `json:"path"`
		Type string `json:"type"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	target, err := a.writable(body.Path)
	created := false
	if err == nil && target.Relative == "" {
		err = &apiError{http.StatusBadRequest, "不能创建管理根目录"}
	}
	if err == nil {
		if body.Type == "directory" {
			err = os.Mkdir(target.Absolute, 0755)
			created = err == nil
		} else {
			var file *os.File
			file, err = os.OpenFile(target.Absolute, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
			if err == nil {
				created = true
				err = file.Close()
			}
		}
	}
	if err == nil {
		mode := fs.FileMode(0644)
		if body.Type == "directory" {
			mode = 0755
		}
		err = inheritPathMetadata(target.Absolute, filepath.Dir(target.Absolute), mode)
	}
	if err != nil {
		if created {
			_ = os.RemoveAll(target.Absolute)
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

func (a *app) handleDelete(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	target, err := a.resolve(body.Path)
	if err == nil && target.Relative == "" {
		err = &apiError{http.StatusBadRequest, "不能删除管理根目录"}
	}
	if err == nil {
		_, err = os.Lstat(target.Absolute)
	}
	if err == nil {
		err = os.RemoveAll(target.Absolute)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleMove(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	source, err := a.resolve(body.From)
	if err != nil || source.Relative == "" {
		writeError(w, &apiError{http.StatusBadRequest, "无效源路径"})
		return
	}
	target, err := a.writable(body.To)
	if err == nil {
		if _, statErr := os.Lstat(target.Absolute); statErr == nil {
			err = os.ErrExist
		} else if !errors.Is(statErr, os.ErrNotExist) {
			err = statErr
		}
	}
	if err == nil {
		err = os.Rename(source.Absolute, target.Absolute)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func copyPath(source, target string, ownership fileOwnership) (err error) {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return &apiError{http.StatusBadRequest, "为安全起见，不复制符号链接"}
	}
	if info.IsDir() {
		if err := os.Mkdir(target, 0700); err != nil {
			return err
		}
		defer func() {
			if err != nil {
				_ = os.RemoveAll(target)
			}
		}()
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyPath(filepath.Join(source, entry.Name()), filepath.Join(target, entry.Name()), ownership); err != nil {
				return err
			}
		}
		return applyPathMetadata(target, ownership, info.Mode().Perm())
	}
	if !info.Mode().IsRegular() {
		return &apiError{http.StatusBadRequest, "仅支持复制普通文件和目录"}
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(target)
		}
	}()
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	return applyPathMetadata(target, ownership, info.Mode().Perm())
}

func (a *app) handleCopy(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	source, err := a.existing(body.From)
	if err != nil {
		writeError(w, err)
		return
	}
	target, err := a.writable(body.To)
	if err == nil {
		var parentInfo fs.FileInfo
		parentInfo, err = os.Stat(filepath.Dir(target.Absolute))
		if err == nil {
			err = copyPath(source.Real, target.Absolute, ownershipFromInfo(parentInfo))
		}
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleUpload(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	directory, err := a.existing(r.URL.Query().Get("dir"))
	if err != nil {
		writeError(w, err)
		return
	}
	info, err := os.Stat(directory.Real)
	if err != nil || !info.IsDir() {
		writeError(w, &apiError{http.StatusBadRequest, "上传目标不是目录"})
		return
	}
	name := filepath.Base(r.URL.Query().Get("name"))
	if name == "" || name == "." || name == ".." {
		writeError(w, &apiError{http.StatusBadRequest, "文件名无效"})
		return
	}
	target := filepath.Join(directory.Real, name)
	if _, err := os.Lstat(target); err == nil {
		writeError(w, os.ErrExist)
		return
	}
	temp, err := os.CreateTemp(directory.Real, ".hostdesk-upload-*.tmp")
	if err != nil {
		writeError(w, err)
		return
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	r.Body = http.MaxBytesReader(w, r.Body, a.uploadMax)
	_, err = io.Copy(temp, r.Body)
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = inheritPathMetadata(tempName, directory.Real, 0644)
	}
	if err == nil {
		err = os.Rename(tempName, target)
	}
	if err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			err = &apiError{http.StatusRequestEntityTooLarge, "上传文件过大"}
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

func parseRemoteDownloadURL(value string) (*url.URL, error) {
	if len(value) > 8192 {
		return nil, &apiError{http.StatusBadRequest, "下载地址过长"}
	}
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, &apiError{http.StatusBadRequest, "仅支持有效的 HTTP 或 HTTPS 地址"}
	}
	if parsed.User != nil {
		return nil, &apiError{http.StatusBadRequest, "下载地址不能包含账号信息"}
	}
	return parsed, nil
}

func validDownloadName(value string) bool {
	return value != "" && value != "." && value != ".." && len([]byte(value)) <= 255 &&
		!strings.ContainsAny(value, "/\\\x00") && filepath.Base(value) == value
}

func remoteDownloadName(requested string, response *http.Response) (string, error) {
	if requested = strings.TrimSpace(requested); requested != "" {
		if !validDownloadName(requested) {
			return "", &apiError{http.StatusBadRequest, "保存文件名无效"}
		}
		return requested, nil
	}
	candidates := make([]string, 0, 2)
	if _, params, err := mime.ParseMediaType(response.Header.Get("Content-Disposition")); err == nil {
		candidates = append(candidates, params["filename"])
	}
	if decoded, err := url.PathUnescape(path.Base(response.Request.URL.Path)); err == nil {
		candidates = append(candidates, decoded)
	}
	for _, candidate := range candidates {
		candidate = path.Base(strings.ReplaceAll(strings.TrimSpace(candidate), "\\", "/"))
		if validDownloadName(candidate) {
			return candidate, nil
		}
	}
	return "download", nil
}

func remoteDownloadClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return &apiError{http.StatusBadGateway, "远程下载重定向次数过多"}
			}
			_, err := parseRemoteDownloadURL(request.URL.String())
			return err
		},
	}
}

func (a *app) handleRemoteDownload(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	var body struct {
		URL         string `json:"url"`
		Destination string `json:"destination"`
		Name        string `json:"name"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	remoteURL, err := parseRemoteDownloadURL(body.URL)
	if err != nil {
		writeError(w, err)
		return
	}
	destination, err := a.existing(body.Destination)
	if err != nil {
		writeError(w, err)
		return
	}
	info, err := os.Stat(destination.Real)
	if err != nil || !info.IsDir() {
		writeError(w, &apiError{http.StatusBadRequest, "下载目标不是目录"})
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, remoteURL.String(), nil)
	if err != nil {
		writeError(w, &apiError{http.StatusBadRequest, "下载地址无效"})
		return
	}
	request.Header.Set("User-Agent", "HostDesk/"+version)
	client := a.remoteClient
	if client == nil {
		client = remoteDownloadClient()
	}
	response, err := client.Do(request)
	if err != nil {
		writeError(w, &apiError{http.StatusBadGateway, "无法连接远程下载地址"})
		return
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		writeError(w, &apiError{http.StatusBadGateway, fmt.Sprintf("远程服务器返回 HTTP %d", response.StatusCode)})
		return
	}
	name, err := remoteDownloadName(body.Name, response)
	if err != nil {
		writeError(w, err)
		return
	}
	target := filepath.Join(destination.Real, name)
	if _, err := os.Lstat(target); err == nil {
		writeError(w, os.ErrExist)
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		writeError(w, err)
		return
	}
	temp, err := os.CreateTemp(destination.Real, ".hostdesk-download-*.tmp")
	if err != nil {
		writeError(w, err)
		return
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	written, copyErr := io.Copy(temp, response.Body)
	closeErr := temp.Close()
	if copyErr != nil {
		writeError(w, &apiError{http.StatusBadGateway, "远程下载中断"})
		return
	}
	if closeErr != nil {
		writeError(w, closeErr)
		return
	}
	if err := inheritPathMetadata(tempName, destination.Real, 0644); err != nil {
		writeError(w, err)
		return
	}
	if err := os.Link(tempName, target); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "name": name, "path": path.Join(destination.Relative, name), "size": written})
}

func (a *app) handleDownload(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, false) == nil {
		return
	}
	target, err := a.existing(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, err)
		return
	}
	file, err := os.Open(target.Real)
	if err != nil {
		writeError(w, err)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, &apiError{http.StatusBadRequest, "目录请先打包后下载"})
		return
	}
	name := filepath.Base(target.Relative)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", strings.ReplaceAll(url.QueryEscape(name), "+", "%20")))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, name, info.ModTime(), file)
}

func addToArchive(writer *tar.Writer, source string) error {
	root := filepath.Dir(source)
	return filepath.Walk(source, func(current string, info fs.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return &apiError{http.StatusBadRequest, "为安全起见，不打包含符号链接的内容"}
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return &apiError{http.StatusBadRequest, "仅支持打包普通文件和目录"}
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relative)
		header.Uid, header.Gid = 0, 0
		header.Uname, header.Gname = "", ""
		if info.IsDir() && !strings.HasSuffix(header.Name, "/") {
			header.Name += "/"
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			file, err := os.Open(current)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(writer, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			return closeErr
		}
		return nil
	})
}

func (a *app) handleArchive(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	var body struct {
		Paths       []string `json:"paths"`
		Name        string   `json:"name"`
		Destination string   `json:"destination"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	if len(body.Paths) == 0 {
		writeError(w, &apiError{http.StatusBadRequest, "请选择需要打包的内容"})
		return
	}
	destination, err := a.existing(body.Destination)
	if err != nil {
		writeError(w, err)
		return
	}
	name := filepath.Base(body.Name)
	if !strings.HasSuffix(strings.ToLower(name), ".tar.gz") {
		name += ".tar.gz"
	}
	output := filepath.Join(destination.Real, name)
	if _, err := os.Lstat(output); err == nil {
		writeError(w, os.ErrExist)
		return
	}
	temp, err := os.CreateTemp(a.dataDir, "archive-*.tar.gz")
	if err != nil {
		writeError(w, err)
		return
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	gzipWriter := gzip.NewWriter(temp)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, selected := range body.Paths {
		target, resolveErr := a.existing(selected)
		if resolveErr != nil {
			err = resolveErr
			break
		}
		if resolveErr = addToArchive(tarWriter, target.Real); resolveErr != nil {
			err = resolveErr
			break
		}
	}
	if closeErr := tarWriter.Close(); err == nil {
		err = closeErr
	}
	if closeErr := gzipWriter.Close(); err == nil {
		err = closeErr
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		var destinationInfo fs.FileInfo
		destinationInfo, err = os.Stat(destination.Real)
		if err == nil {
			err = copyPath(tempName, output, ownershipFromInfo(destinationInfo))
		}
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "path": path.Join(destination.Relative, name)})
}

func safeArchiveName(name string) bool {
	if name == "" || strings.ContainsRune(name, 0) || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "\\") {
		return false
	}
	if len(name) >= 2 && name[1] == ':' {
		return false
	}
	for _, part := range strings.Split(strings.ReplaceAll(name, "\\", "/"), "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

func extractTarget(destination, name string) (string, error) {
	if !safeArchiveName(name) {
		return "", &apiError{http.StatusBadRequest, "压缩包包含不安全路径"}
	}
	target := filepath.Join(destination, filepath.FromSlash(strings.ReplaceAll(name, "\\", "/")))
	if !inside(destination, target) {
		return "", &apiError{http.StatusBadRequest, "压缩包包含不安全路径"}
	}
	return target, nil
}

func prepareExtractPath(destination, target string, directory bool, ownership fileOwnership) error {
	relative, err := filepath.Rel(destination, target)
	if err != nil {
		return err
	}
	current := destination
	parts := strings.Split(relative, string(filepath.Separator))
	limit := len(parts)
	if !directory {
		limit--
	}
	for index := 0; index < limit; index++ {
		current = filepath.Join(current, parts[index])
		info, statErr := os.Lstat(current)
		if statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return &apiError{http.StatusBadRequest, "解压路径包含链接或非目录项"}
			}
			continue
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		if err := os.Mkdir(current, 0755); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if err := os.Chown(current, ownership.UID, ownership.GID); err != nil {
			return err
		}
	}
	if !directory {
		if info, statErr := os.Lstat(target); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return &apiError{http.StatusBadRequest, "解压目标是符号链接"}
		} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
	}
	return nil
}

func extractOwnership(destination string) (fileOwnership, error) {
	info, err := os.Stat(destination)
	if err != nil {
		return fileOwnership{}, err
	}
	return ownershipFromInfo(info), nil
}

func applyExtractMetadata(target string, mode fs.FileMode, ownership fileOwnership) error {
	if err := os.Chown(target, ownership.UID, ownership.GID); err != nil {
		return err
	}
	return os.Chmod(target, mode)
}

func safeFileMode(mode fs.FileMode, directory bool) fs.FileMode {
	if directory {
		mode &= 0755
		if mode == 0 {
			return 0755
		}
		return mode
	}
	mode &= 0666
	if mode == 0 {
		return 0644
	}
	return mode
}

func (a *app) extractZip(archive, destination string) error {
	ownership, err := extractOwnership(destination)
	if err != nil {
		return err
	}
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return &apiError{http.StatusBadRequest, "ZIP 文件损坏"}
	}
	defer reader.Close()
	if len(reader.File) > 100000 {
		return &apiError{http.StatusRequestEntityTooLarge, "压缩包文件数量过多"}
	}
	var total int64
	for _, item := range reader.File {
		if item.Mode()&os.ModeSymlink != 0 || (!item.FileInfo().IsDir() && !item.Mode().IsRegular()) {
			return &apiError{http.StatusBadRequest, "压缩包包含不支持的链接或特殊文件"}
		}
		total += int64(item.UncompressedSize64)
		if total > a.extractMax {
			return &apiError{http.StatusRequestEntityTooLarge, "解压后内容超过限制"}
		}
		if _, err := extractTarget(destination, item.Name); err != nil {
			return err
		}
	}
	for _, item := range reader.File {
		target, _ := extractTarget(destination, item.Name)
		if item.FileInfo().IsDir() {
			if err := prepareExtractPath(destination, target, true, ownership); err != nil {
				return err
			}
			if err := applyExtractMetadata(target, safeFileMode(item.Mode().Perm(), true), ownership); err != nil {
				return err
			}
			continue
		}
		if err := prepareExtractPath(destination, target, false, ownership); err != nil {
			return err
		}
		input, err := item.Open()
		if err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, safeFileMode(item.Mode().Perm(), false))
		if err == nil {
			_, err = io.Copy(output, input)
		}
		input.Close()
		if output != nil {
			if closeErr := output.Close(); err == nil {
				err = closeErr
			}
		}
		if err != nil {
			return err
		}
		if err := applyExtractMetadata(target, safeFileMode(item.Mode().Perm(), false), ownership); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) extractTar(archive, destination string, compressed bool) error {
	ownership, err := extractOwnership(destination)
	if err != nil {
		return err
	}
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	var reader io.Reader = file
	if compressed {
		gzipReader, gzipErr := gzip.NewReader(file)
		if gzipErr != nil {
			return &apiError{http.StatusBadRequest, "GZIP 文件损坏"}
		}
		defer gzipReader.Close()
		reader = gzipReader
	}
	tarReader := tar.NewReader(reader)
	var total int64
	count := 0
	for {
		header, readErr := tarReader.Next()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return &apiError{http.StatusBadRequest, "TAR 文件损坏"}
		}
		count++
		if count > 100000 {
			return &apiError{http.StatusRequestEntityTooLarge, "压缩包文件数量过多"}
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA && header.Typeflag != tar.TypeDir {
			return &apiError{http.StatusBadRequest, "压缩包包含不支持的链接或特殊文件"}
		}
		total += header.Size
		if header.Size < 0 || total > a.extractMax {
			return &apiError{http.StatusRequestEntityTooLarge, "解压后内容超过限制"}
		}
		target, err := extractTarget(destination, header.Name)
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeDir {
			if err := prepareExtractPath(destination, target, true, ownership); err != nil {
				return err
			}
			if err := applyExtractMetadata(target, safeFileMode(fs.FileMode(header.Mode), true), ownership); err != nil {
				return err
			}
			continue
		}
		if err := prepareExtractPath(destination, target, false, ownership); err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, safeFileMode(fs.FileMode(header.Mode), false))
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(output, tarReader, header.Size)
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if err := applyExtractMetadata(target, safeFileMode(fs.FileMode(header.Mode), false), ownership); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) handleExtract(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	var body struct {
		Path        string `json:"path"`
		Destination string `json:"destination"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	archive, err := a.existing(body.Path)
	if err != nil {
		writeError(w, err)
		return
	}
	destination, err := a.existing(body.Destination)
	if err != nil {
		writeError(w, err)
		return
	}
	info, err := os.Stat(destination.Real)
	if err != nil || !info.IsDir() {
		writeError(w, &apiError{http.StatusBadRequest, "解压目标不是目录"})
		return
	}
	lower := strings.ToLower(archive.Relative)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		err = a.extractZip(archive.Real, destination.Real)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		err = a.extractTar(archive.Real, destination.Real, true)
	case strings.HasSuffix(lower, ".tar"):
		err = a.extractTar(archive.Real, destination.Real, false)
	default:
		err = &apiError{http.StatusUnsupportedMediaType, "支持 .zip、.tar、.tar.gz 和 .tgz"}
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type sshMessage struct {
	Type       string `json:"type"`
	CSRF       string `json:"csrf"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	PrivateKey string `json:"privateKey"`
	Data       string `json:"data"`
	Rows       int    `json:"rows"`
	Cols       int    `json:"cols"`
	UseSaved   bool   `json:"useSavedCredential"`
}

type wsWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *wsWriter) send(value any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.WriteJSON(value)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (a *app) handleSSH(w http.ResponseWriter, r *http.Request) {
	authSession := a.authorize(w, r, false)
	if authSession == nil {
		return
	}
	upgrader := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     terminalOriginAllowed,
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(2 << 20)
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	writer := &wsWriter{conn: conn}

	var connect sshMessage
	if err := conn.ReadJSON(&connect); err != nil || connect.Type != "connect" || subtle.ConstantTimeCompare([]byte(connect.CSRF), []byte(authSession.CSRF)) != 1 {
		_ = writer.send(map[string]string{"type": "error", "message": "无效连接请求"})
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	connect.Host = strings.TrimSpace(connect.Host)
	connect.Username = strings.TrimSpace(connect.Username)
	if connect.Port == 0 {
		connect.Port = 22
	}
	if err := a.validateSSHIdentity(connect.Host, connect.Port, connect.Username); err != nil {
		_ = writer.send(map[string]string{"type": "error", "message": err.Error()})
		return
	}
	if connect.UseSaved && connect.Password == "" && connect.PrivateKey == "" {
		saved, savedErr := a.savedSSHCredentials()
		if savedErr != nil || !saved.PasswordConfigured || saved.Host != connect.Host || saved.Port != connect.Port || saved.Username != connect.Username {
			_ = writer.send(map[string]string{"type": "error", "message": "已保存的 SSH 凭据不可用"})
			return
		}
		connect.Password = saved.Password
	}

	authMethods := make([]ssh.AuthMethod, 0, 2)
	if connect.Password != "" {
		authMethods = append(authMethods, ssh.Password(connect.Password))
	}
	if connect.PrivateKey != "" {
		signer, parseErr := ssh.ParsePrivateKey([]byte(connect.PrivateKey))
		if parseErr != nil {
			_ = writer.send(map[string]string{"type": "error", "message": "私钥格式错误"})
			return
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}
	if len(authMethods) == 0 {
		authMethods = append(authMethods, ssh.Password(""))
	}
	hostKeyCallback := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		fingerprint := ssh.FingerprintSHA256(key)
		if a.hostFingerprint != "" {
			if subtle.ConstantTimeCompare([]byte(fingerprint), []byte(a.hostFingerprint)) != 1 {
				return fmt.Errorf("SSH 主机指纹不匹配，收到 %s", fingerprint)
			}
			return nil
		}
		if isLoopbackHost(connect.Host) {
			return nil
		}
		return fmt.Errorf("远程 SSH 必须配置 SSH_HOST_KEY_SHA256，收到 %s", fingerprint)
	}
	client, err := ssh.Dial("tcp", net.JoinHostPort(connect.Host, strconv.Itoa(connect.Port)), &ssh.ClientConfig{User: connect.Username, Auth: authMethods, HostKeyCallback: hostKeyCallback, Timeout: 15 * time.Second})
	if err != nil {
		_ = writer.send(map[string]string{"type": "error", "message": err.Error()})
		return
	}
	defer client.Close()
	sshSession, err := client.NewSession()
	if err != nil {
		_ = writer.send(map[string]string{"type": "error", "message": err.Error()})
		return
	}
	defer sshSession.Close()
	stdin, _ := sshSession.StdinPipe()
	stdout, _ := sshSession.StdoutPipe()
	stderr, _ := sshSession.StderrPipe()
	rows, cols := terminalSize(connect.Rows, connect.Cols)
	if err := sshSession.RequestPty("xterm-256color", rows, cols, ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}); err != nil {
		_ = writer.send(map[string]string{"type": "error", "message": err.Error()})
		return
	}
	if err := sshSession.Shell(); err != nil {
		_ = writer.send(map[string]string{"type": "error", "message": err.Error()})
		return
	}
	_ = writer.send(map[string]string{"type": "ready"})

	done := make(chan struct{})
	var doneOnce sync.Once
	closeDone := func() { doneOnce.Do(func() { close(done) }) }
	stream := func(reader io.Reader) {
		buffer := make([]byte, 32*1024)
		for {
			count, readErr := reader.Read(buffer)
			if count > 0 {
				if writer.send(map[string]string{"type": "data", "data": string(buffer[:count])}) != nil {
					closeDone()
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}
	go stream(stdout)
	go stream(stderr)
	go func() {
		_ = sshSession.Wait()
		_ = writer.send(map[string]string{"type": "close"})
		closeDone()
	}()
	go func() {
		for {
			var message sshMessage
			if err := conn.ReadJSON(&message); err != nil {
				closeDone()
				return
			}
			switch message.Type {
			case "input":
				_, _ = io.WriteString(stdin, message.Data)
			case "resize":
				if message.Rows > 0 && message.Cols > 0 && message.Rows < 1000 && message.Cols < 1000 {
					_ = sshSession.WindowChange(message.Rows, message.Cols)
				}
			}
		}
	}()
	<-done
}

func terminalSize(rows, cols int) (int, int) {
	if rows < 1 || rows > 999 {
		rows = 30
	}
	if cols < 2 || cols > 999 {
		cols = 100
	}
	return rows, cols
}

func init() {
	_ = mime.AddExtensionType(".js", "text/javascript; charset=utf-8")
	_ = mime.AddExtensionType(".css", "text/css; charset=utf-8")
}
