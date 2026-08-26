package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"os/user"
	"strconv"
)

const (
	SocketPath    = "/run/vpn-manager.sock"
	MgmtSocket    = "/run/vpn-openvpn-mgmt.sock"
	ConfigDir     = "/etc/vpn-manager/configs"
	SecureTempDir = "/run/vpn-manager-temp"
)

type VPNManager struct {
	mu  sync.Mutex
	cmd *exec.Cmd
}

type Request struct {
	Action   string `json:"action"`
	Config   string `json:"config"`
	Username string `json:"username"`
	Password string `json:"password"`
	Content  string `json:"content"`
}

type DaemonResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type VPNStatus struct {
	State   string `json:"state"`
	LocalIP string `json:"local_ip"`
}

func main() {
	_ = os.Remove(SocketPath)
	_ = os.Remove(MgmtSocket)

	// Verzeichnisse absichern (0700 = nur Root darf lesen/schreiben)
	if err := os.MkdirAll(ConfigDir, 0700); err != nil {
		log.Fatalf("Konfigurationsverzeichnis konnte nicht erstellt werden: %v", err)
	}
	if err := os.MkdirAll(SecureTempDir, 0700); err != nil {
		log.Fatalf("Temp-Verzeichnis konnte nicht erstellt werden: %v", err)
	}

	manager := &VPNManager{}

	listener, err := net.Listen("unix", SocketPath)
	if err != nil {
		log.Fatal("Socket konnte nicht erstellt werden: ", err)
	}
	defer listener.Close()

	// WICHTIG: 0660 nutzen! Du musst die Gruppe der Socket-Datei (z.B. per chown) 
	// auf eine Gruppe setzen, in der dein GUI-User ist (z.B. "vpnusers").
	_ = os.Chmod(SocketPath, 0660)

	if g, err := user.LookupGroup("vpnusers"); err == nil {
    if gid, err := strconv.Atoi(g.Gid); err == nil {
        _ = os.Chown(SocketPath, -1, gid) // -1 = User (root) beibehalten, Gruppe auf vpnusers ändern
    }
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("Daemon fährt herunter... Stoppe VPN.")
		_ = manager.stopVPN()
		_ = os.Remove(SocketPath)
		_ = os.Remove(MgmtSocket)
		_ = os.RemoveAll(SecureTempDir)
		os.Exit(0)
	}()

	log.Println("VPN-Daemon läuft. Warte auf Befehle...")

	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go manager.handleConnection(conn)
	}
}

func (m *VPNManager) handleConnection(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	var req Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}

	switch req.Action {
	case "start":
		err := m.startVPN(req)
		sendResponse(conn, err, "VPN gestartet", nil)

	case "stop":
		err := m.stopVPN()
		sendResponse(conn, err, "VPN gestoppt", nil)

	case "import":
		err := handleImport(req)
		sendResponse(conn, err, "Konfiguration importiert", nil)

	case "list":
		configs, err := handleList()
		sendResponse(conn, err, "Konfigurationsliste geladen", configs)

	case "delete":
		err := handleDelete(req)
		sendResponse(conn, err, "Konfiguration gelöscht", nil)

	case "status":
		status, err := m.getVPNStatus()
		sendResponse(conn, err, "Status ermittelt", status)

	default:
		sendResponse(conn, fmt.Errorf("unbekannte Aktion: %s", req.Action), "", nil)
	}
}

func (m *VPNManager) startVPN(req Request) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cmd != nil {
		return errors.New("VPN läuft bereits")
	}

	safeFileName := filepath.Base(req.Config)
	configPath := filepath.Join(ConfigDir, safeFileName)

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return fmt.Errorf("konfigurationsdatei %s existiert nicht", safeFileName)
	}

	args := []string{
		"--config", configPath,
		"--management", MgmtSocket, "unix", // Sicherer Unix-Socket für Management
		"--auth-retry", "nointeract",
		"--dev", "tun0",
	}

	var authFile string
	if req.Username != "" && req.Password != "" {
		// Sicherer File-Erstellung statt hardcoded /tmp
		tmpFile, err := os.CreateTemp(SecureTempDir, "auth-*.txt")
		if err != nil {
			return fmt.Errorf("konnte temporäre auth-Datei nicht erstellen: %w", err)
		}
		authFile = tmpFile.Name()

		content := fmt.Sprintf("%s\n%s", req.Username, req.Password)
		if _, err := tmpFile.WriteString(content); err != nil {
			_ = tmpFile.Close()
			_ = os.Remove(authFile)
			return err
		}
		_ = tmpFile.Close()
		args = append(args, "--auth-user-pass", authFile)
	}

	cmd := exec.Command("/usr/sbin/openvpn", args...)
	
	cmd.Stdout = os.Stdout 
  cmd.Stderr = os.Stderr
	
	if err := cmd.Start(); err != nil {
		if authFile != "" {
			_ = os.Remove(authFile)
		}
		return err
	}

	m.cmd = cmd

	go func(c *exec.Cmd, aFile string) {
		_ = c.Wait()
		if aFile != "" {
			_ = os.Remove(aFile)
		}
		_ = os.Remove(MgmtSocket)

		m.mu.Lock()
		if m.cmd == c {
			m.cmd = nil
		}
		m.mu.Unlock()
	}(cmd, authFile)

	return nil
}

func (m *VPNManager) stopVPN() error {
  m.mu.Lock()
  
  if m.cmd == nil || m.cmd.Process == nil {
    m.mu.Unlock()
    return errors.New("VPN läuft nicht")
  }
  
  proc := m.cmd.Process
  m.mu.Unlock() 

  stopped := false

  // Versuch: Sauberes Beenden via Management-Socket
  conn, err := net.DialTimeout("unix", MgmtSocket, 1*time.Second)
  if err == nil {
    _ = conn.SetDeadline(time.Now().Add(1 * time.Second))
    if _, err := fmt.Fprintf(conn, "signal SIGTERM\nquit\n"); err == nil {
      stopped = true
    }
    _ = conn.Close()
  }

  // Fallback: Wenn Socket nicht erreichbar war oder fehlschlug -> direktes Linux-Signal
  if !stopped {
    _ = proc.Signal(syscall.SIGTERM)
  }

  // WICHTIG: Gib OpenVPN kurz Zeit (bis zu 3 Sekunden), um tun0 sauber abzubauen!
  // Wir prüfen im 200ms-Takt, ob die startVPN-Goroutine m.cmd auf nil gesetzt hat.
  for i := 0; i < 15; i++ {
    time.Sleep(200 * time.Millisecond)
    m.mu.Lock()
    running := (m.cmd != nil)
    m.mu.Unlock()
    
    if !running {
      // Perfekt, OpenVPN hat sich beendet und die Goroutine hat aufgeräumt!
      return nil
    }
  }

  // Wenn nach 3 Sekunden immer noch nicht beendet -> Harter Kill (SIGKILL)
  m.mu.Lock()
  if m.cmd != nil && m.cmd.Process != nil {
    _ = m.cmd.Process.Kill()
    m.cmd = nil
  }
  m.mu.Unlock()

  return nil
}

func (m *VPNManager) getVPNStatus() (VPNStatus, error) {
  conn, err := net.DialTimeout("unix", MgmtSocket, 1*time.Second)
  if err != nil {
    return VPNStatus{State: "DISCONNECTED"}, nil
  }
  defer conn.Close()

  _ = conn.SetDeadline(time.Now().Add(2 * time.Second))

  if _, err := fmt.Fprintf(conn, "state\n"); err != nil {
    return VPNStatus{State: "UNKNOWN"}, err
  }

  scanner := bufio.NewScanner(conn)
  for scanner.Scan() {
    line := strings.TrimSpace(scanner.Text())
    
    // DEBUG: Das zeigt uns im Terminal EXAKT, was OpenVPN antwortet!
    //fmt.Printf("DEBUG OpenVPN-State: '%s'\n", line)

    // Unnötige Zeilen (Banner, END, etc.) ignorieren
    if strings.HasPrefix(line, ">") || line == "" || line == "END" || strings.HasPrefix(line, "SUCCESS:") {
      continue
    }

    parts := strings.Split(line, ",")
    
    // Wir akzeptieren schon ab 2 Feldern (z.B. für CONNECTING, AUTH, etc.)
    if len(parts) >= 2 {
      status := VPNStatus{
        State: strings.TrimSpace(parts[1]),
      }
      
      // Die IP-Adresse vergeben wir nur, wenn sie auch wirklich mitgeliefert wurde
      if len(parts) >= 4 {
        status.LocalIP = strings.TrimSpace(parts[3])
      }
      
      return status, nil
    }
  }

  // Falls der Scanner durch einen Fehler abbrach
  if err := scanner.Err(); err != nil {
    fmt.Printf("DEBUG Scanner-Error: %v\n", err)
  }

  return VPNStatus{State: "WAITING"}, nil
}

func sendResponse(conn net.Conn, err error, successMsg string, data any) {
	resp := DaemonResponse{
		Status:  "success",
		Message: successMsg,
		Data:    data,
	}
	if err != nil {
		resp.Status = "error"
		resp.Message = err.Error()
		resp.Data = nil
	}
	_ = json.NewEncoder(conn).Encode(resp)
}

func handleImport(req Request) error {
	safeFileName := filepath.Base(req.Config)

	safeFileName = sanitizeFileName(safeFileName)

	if !strings.HasSuffix(safeFileName, ".ovpn") {
		return errors.New("ungültiges Dateiformat (muss .ovpn sein)")
	}

	targetPath := filepath.Join(ConfigDir, safeFileName)
	content, err := base64.StdEncoding.DecodeString(req.Content)
	if err != nil {
		return fmt.Errorf("base64 Dekodierung fehlgeschlagen: %w", err)
	}

	if err := os.WriteFile(targetPath, content, 0600); err != nil {
		return fmt.Errorf("Datei konnte nicht geschrieben werden: %w", err)
	}

	log.Printf("Erfolgreich importiert: %s", targetPath)
	return nil
}

func handleList() ([]string, error) {
	files, err := os.ReadDir(ConfigDir)
	if err != nil {
		return nil, err
	}

	var configFiles []string
	for _, file := range files {
		if !file.IsDir() && strings.HasSuffix(file.Name(), ".ovpn") {
			configFiles = append(configFiles, file.Name())
		}
	}
	return configFiles, nil
}

func handleDelete(req Request) error {
	safeFileName := filepath.Base(req.Config)
	if !strings.HasSuffix(safeFileName, ".ovpn") {
		return errors.New("ungültiges Dateiformat")
	}

	targetPath := filepath.Join(ConfigDir, safeFileName)
	if _, err := os.Stat(targetPath); os.IsNotExist(err) {
		return fmt.Errorf("datei %s existiert nicht", safeFileName)
	}

	if err := os.Remove(targetPath); err != nil {
		return fmt.Errorf("konnte datei nicht löschen: %w", err)
	}

	log.Printf("Erfolgreich gelöscht: %s", targetPath)
	return nil
}

// Diese Funktion bereinigt Dateinamen für Linux
func sanitizeFileName(filename string) string {
    // Ersetze Leerzeichen durch Unterstriche
    clean := strings.ReplaceAll(filename, " ", "_")
    
    // Entferne problematische Sonderzeichen wie Klammern
    clean = strings.ReplaceAll(clean, "(", "")
    clean = strings.ReplaceAll(clean, ")", "")
    clean = strings.ReplaceAll(clean, "'", "")
    clean = strings.ReplaceAll(clean, "\"", "")
    
    // Verhindere doppelte Unterstriche (z.B. aus " (_" -> "__" -> "_")
    for strings.Contains(clean, "__") {
        clean = strings.ReplaceAll(clean, "__", "_")
    }
    
    return clean
}