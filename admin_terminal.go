package main

import (
	"crypto/subtle"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

type terminalMessage struct {
	Type string `json:"type"`
	CSRF string `json:"csrf"`
	Data string `json:"data"`
	Rows int    `json:"rows"`
	Cols int    `json:"cols"`
}

func terminalOriginAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	parsed, err := url.Parse(origin)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && strings.EqualFold(parsed.Host, r.Host)
}

func localShell() (string, error) {
	candidates := []string{strings.TrimSpace(os.Getenv("SHELL")), "/bin/ash", "/bin/bash", "/bin/sh"}
	for _, candidate := range candidates {
		if candidate == "" || !filepath.IsAbs(candidate) {
			continue
		}
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode().Perm()&0111 != 0 {
			return candidate, nil
		}
	}
	return "", errors.New("找不到可用的系统 Shell")
}

func (a *app) handleTerminal(w http.ResponseWriter, r *http.Request) {
	session := a.authorize(w, r, false)
	if session == nil {
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
	conn.SetReadLimit(1 << 20)
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	writer := &wsWriter{conn: conn}

	var connect terminalMessage
	if err := conn.ReadJSON(&connect); err != nil || connect.Type != "connect" || subtle.ConstantTimeCompare([]byte(connect.CSRF), []byte(session.CSRF)) != 1 {
		_ = writer.send(map[string]string{"type": "error", "message": "终端连接校验失败"})
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	shell, err := localShell()
	if err != nil {
		_ = writer.send(map[string]string{"type": "error", "message": err.Error()})
		return
	}
	rows, cols := terminalSize(connect.Rows, connect.Cols)
	command := exec.Command(shell, "-l")
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		command.Dir = home
	}
	command.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		log.Printf("启动本机终端失败: %v", err)
		_ = writer.send(map[string]string{"type": "error", "message": "无法启动本机终端"})
		return
	}
	defer terminal.Close()
	defer func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	}()
	if err := writer.send(map[string]string{"type": "ready"}); err != nil {
		return
	}

	done := make(chan struct{})
	var doneOnce sync.Once
	closeDone := func() { doneOnce.Do(func() { close(done) }) }
	go func() {
		buffer := make([]byte, 32*1024)
		for {
			count, readErr := terminal.Read(buffer)
			if count > 0 {
				if writer.send(map[string]string{"type": "data", "data": string(buffer[:count])}) != nil {
					closeDone()
					return
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, os.ErrClosed) {
					log.Printf("读取本机终端失败: %v", readErr)
				}
				closeDone()
				return
			}
		}
	}()
	go func() {
		_ = command.Wait()
		_ = writer.send(map[string]string{"type": "close"})
		closeDone()
	}()
	go func() {
		for {
			var message terminalMessage
			if err := conn.ReadJSON(&message); err != nil {
				closeDone()
				return
			}
			switch message.Type {
			case "input":
				if len(message.Data) <= 64<<10 {
					_, _ = io.WriteString(terminal, message.Data)
				}
			case "resize":
				resizeRows, resizeCols := terminalSize(message.Rows, message.Cols)
				_ = pty.Setsize(terminal, &pty.Winsize{Rows: uint16(resizeRows), Cols: uint16(resizeCols)})
			}
		}
	}()
	<-done
}
