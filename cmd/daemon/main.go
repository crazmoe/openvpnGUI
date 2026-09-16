package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"openvpnGUI/internal/ipc"
)

const (
	MgmtSocket    = "/run/vpn-openvpn-mgmt.sock"
	ConfigDir     = "/etc/vpn-manager/configs"
	SecureTempDir = "/run/vpn-manager-temp"
	InterfaceName = "tun0"
)

type VPNManager struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
}

func main() {
	cleanupSockets()

	// Verzeichnisse absichern
	for _, dir := range []string{ConfigDir, SecureTempDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			log.Fatalf("Verzeichnis konnte nicht erstellt werden (%s): %v", dir, err)
		}
	}

	listener, err := net.Listen("unix", ipc.SocketPath)
	if err != nil {
		log.Fatalf("Socket konnte nicht erstellt werden: %v", err)
	}
	defer listener.Close()

	setupSocketPermissions()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	manager := &VPNManager{}

	go func() {
		<-ctx.Done()
		log.Println("Daemon fährt herunter... Stoppe VPN.")
		_ = manager.stopVPN()
		cleanupSockets()
		_ = os.RemoveAll(SecureTempDir)
		os.Exit(0)
	}()

	log.Println("VPN-Daemon läuft. Warte auf Befehle...")

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}
		go manager.handleConnection(conn)
	}
}

// -----------------------------------------------------------------------------
// OPENVPN PROZESS & PUSH_REPLY STREAM PARSER
// -----------------------------------------------------------------------------

func (m *VPNManager) startVPN(req ipc.Request) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cmd != nil {
		return errors.New("VPN läuft bereits")
	}

	configPath, err := resolveConfigPath(req.Config)
	if err != nil {
		return err
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return fmt.Errorf("konfigurationsdatei existiert nicht: %s", filepath.Base(configPath))
	}

	args := []string{
		"--config", configPath,
		"--management", MgmtSocket, "unix",
		"--auth-retry", "nointeract",
		"--dev", InterfaceName,
		"--verb", "3", // Verbosity 3 liefert den PUSH_REPLY String in Stdout
	}

	var authFile string
	if req.Username != "" && req.Password != "" {
		tmpFile, err := os.CreateTemp(SecureTempDir, "auth-*.txt")
		if err != nil {
			return fmt.Errorf("konnte temporäre Auth-Datei nicht erstellen: %w", err)
		}
		authFile = tmpFile.Name()

		content := fmt.Sprintf("%s\n%s\n", req.Username, req.Password)
		if _, err := tmpFile.WriteString(content); err != nil {
			_ = tmpFile.Close()
			_ = os.Remove(authFile)
			return fmt.Errorf("konnte Anmeldedaten nicht schreiben: %w", err)
		}
		_ = tmpFile.Close()

		args = append(args, "--auth-user-pass", authFile)
	}

	cmd := exec.Command("/usr/sbin/openvpn", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		if authFile != "" {
			_ = os.Remove(authFile)
		}
		return fmt.Errorf("stdout pipe konnte nicht erstellt werden: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		if authFile != "" {
			_ = os.Remove(authFile)
		}
		return fmt.Errorf("openvpn konnte nicht gestartet werden: %w", err)
	}

	m.cmd = cmd
	m.done = make(chan struct{})

	// Überwachungs- & Log-Parsing Goroutine
	go func(c *exec.Cmd, aFile string, doneChan chan struct{}) {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()

			// Routine-Management-Meldungen (Statusabfragen) ignorieren
			if strings.Contains(line, "MANAGEMENT:") {
				continue
			}

			// Alle verbleibenden OpenVPN-Logzeilen weiterleiten
			log.Printf("[OpenVPN] %s", line)

			// Parse den vom OpenVPN-Server gelieferten PUSH_REPLY
			if strings.Contains(line, "PUSH_REPLY") {
				dnsServers, domains := parsePushReply(line)
				if len(dnsServers) > 0 {
					log.Printf("Empfangene DNS-Server: %v, Domains: %v", dnsServers, domains)
					applyDNS(InterfaceName, dnsServers, domains)
				}
			}
		}

		_ = c.Wait()

		// DNS-Einstellungen und Interface beim Beenden zurücksetzen
		revertDNS(InterfaceName)

		if aFile != "" {
			_ = os.Remove(aFile)
		}
		_ = os.Remove(MgmtSocket)

		m.mu.Lock()
		if m.cmd == c {
			m.cmd = nil
			close(doneChan)
		}
		m.mu.Unlock()
	}(cmd, authFile, m.done)

	return nil
}

func parsePushReply(line string) (dnsServers []string, domains []string) {
	start := strings.Index(line, "PUSH_REPLY")
	if start == -1 {
		return
	}

	content := strings.TrimSuffix(line[start:], "'")
	tokens := strings.Split(content, ",")

	for _, token := range tokens {
		fields := strings.Fields(strings.TrimSpace(token))
		if len(fields) >= 3 && fields[0] == "dhcp-option" {
			optType := strings.ToUpper(fields[1])
			optVal := fields[2]
			switch optType {
			case "DNS", "DNS6":
				dnsServers = append(dnsServers, optVal)
			case "DOMAIN", "DOMAIN-SEARCH":
				domains = append(domains, optVal)
			}
		}
	}
	return
}

// -----------------------------------------------------------------------------
// DNS REGISTRIERUNG & RESET (REINES GO)
// -----------------------------------------------------------------------------

func applyDNS(iface string, dnsServers []string, domains []string) {
	// 1. systemd-resolved (resolvectl)
	if _, err := exec.LookPath("resolvectl"); err == nil {
		cmdArgs := append([]string{"dns", iface}, dnsServers...)
		if err := exec.Command("resolvectl", cmdArgs...).Run(); err != nil {
			log.Printf("Fehler bei resolvectl dns: %v", err)
		}

		if len(domains) > 0 {
			domArgs := append([]string{"domain", iface}, domains...)
			_ = exec.Command("resolvectl", domArgs...).Run()
		} else {
			_ = exec.Command("resolvectl", "domain", iface, "~.").Run()
		}
		log.Printf("DNS via resolvectl erfolgreich gesetzt für %s", iface)
		return
	}

	// 2. systemd-resolved (systemd-resolve)
	if _, err := exec.LookPath("systemd-resolve"); err == nil {
		args := []string{"--interface=" + iface}
		for _, d := range dnsServers {
			args = append(args, "--set-dns="+d)
		}
		if len(domains) > 0 {
			for _, dom := range domains {
				args = append(args, "--set-domain="+dom)
			}
		} else {
			args = append(args, "--set-domain=~.")
		}
		_ = exec.Command("systemd-resolve", args...).Run()
		log.Printf("DNS via systemd-resolve erfolgreich gesetzt für %s", iface)
		return
	}

	// 3. Fallback: resolvconf
	if _, err := exec.LookPath("resolvconf"); err == nil {
		var input strings.Builder
		for _, d := range dnsServers {
			input.WriteString(fmt.Sprintf("nameserver %s\n", d))
		}
		for _, dom := range domains {
			input.WriteString(fmt.Sprintf("search %s\n", dom))
		}
		cmd := exec.Command("resolvconf", "-a", iface)
		cmd.Stdin = strings.NewReader(input.String())
		_ = cmd.Run()
		log.Printf("DNS via resolvconf erfolgreich gesetzt für %s", iface)
		return
	}
}

func revertDNS(iface string) {
	if _, err := exec.LookPath("resolvectl"); err == nil {
		_ = exec.Command("resolvectl", "revert", iface).Run()
		log.Printf("DNS-Konfiguration für %s zurückgesetzt (resolvectl)", iface)
	} else if _, err := exec.LookPath("systemd-resolve"); err == nil {
		_ = exec.Command("systemd-resolve", "--interface="+iface, "--revert").Run()
		log.Printf("DNS-Konfiguration für %s zurückgesetzt (systemd-resolve)", iface)
	} else if _, err := exec.LookPath("resolvconf"); err == nil {
		_ = exec.Command("resolvconf", "-d", iface).Run()
		log.Printf("DNS-Konfiguration für %s zurückgesetzt (resolvconf)", iface)
	}

	// TUN-Schnittstelle explizit entfernen, um verbleibende Interfaces zu bereinigen
	if err := exec.Command("ip", "link", "delete", iface).Run(); err == nil {
		log.Printf("Schnittstelle %s wurde erfolgreich entfernt.", iface)
	} else {
		log.Printf("Hinweis: Schnittstelle %s konnte nicht gelöscht werden oder existierte nicht mehr.", iface)
	}
}

// -----------------------------------------------------------------------------
// VERWALTUNGS- & SOCKET-LOGIK
// -----------------------------------------------------------------------------

func (m *VPNManager) stopVPN() error {
	m.mu.Lock()
	if m.cmd == nil || m.cmd.Process == nil {
		m.mu.Unlock()
		return errors.New("VPN läuft nicht")
	}

	proc := m.cmd.Process
	done := m.done
	m.mu.Unlock()

	conn, err := net.DialTimeout("unix", MgmtSocket, 1*time.Second)
	if err == nil {
		_ = conn.SetDeadline(time.Now().Add(1 * time.Second))
		_, _ = fmt.Fprintf(conn, "signal SIGTERM\nquit\n")
		_ = conn.Close()
	} else {
		_ = proc.Signal(syscall.SIGTERM)
	}

	select {
	case <-done:
		return nil
	case <-time.After(3 * time.Second):
		m.mu.Lock()
		if m.cmd != nil && m.cmd.Process != nil {
			_ = m.cmd.Process.Kill()
		}
		m.mu.Unlock()
		return nil
	}
}

func (m *VPNManager) handleConnection(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	var req ipc.Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		sendResponse(conn, fmt.Errorf("ungültiger Request: %w", err), "", nil)
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

func (m *VPNManager) getVPNStatus() (ipc.VPNStatus, error) {
	conn, err := net.DialTimeout("unix", MgmtSocket, 1*time.Second)
	if err != nil {
		return ipc.VPNStatus{State: "DISCONNECTED"}, nil
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	scanner := bufio.NewScanner(conn)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, ">INFO:") {
			break
		}
	}

	if _, err := fmt.Fprintf(conn, "state\n"); err != nil {
		return ipc.VPNStatus{State: "UNKNOWN"}, err
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if strings.HasPrefix(line, ">") || line == "" || line == "END" || strings.HasPrefix(line, "SUCCESS:") {
			continue
		}

		parts := strings.Split(line, ",")
		if len(parts) >= 2 {
			status := ipc.VPNStatus{
				State: strings.TrimSpace(parts[1]),
			}

			if len(parts) >= 4 {
				status.LocalIP = strings.TrimSpace(parts[3])
			}

			return status, nil
		}
	}

	return ipc.VPNStatus{State: "WAITING"}, nil
}

func handleImport(req ipc.Request) error {
	configPath, err := resolveConfigPath(req.Config)
	if err != nil {
		return err
	}

	content, err := base64.StdEncoding.DecodeString(req.Content)
	if err != nil {
		return fmt.Errorf("base64 Dekodierung fehlgeschlagen: %w", err)
	}

	if err := os.WriteFile(configPath, content, 0600); err != nil {
		return fmt.Errorf("datei konnte nicht geschrieben werden: %w", err)
	}

	log.Printf("Erfolgreich importiert: %s", configPath)
	return nil
}

func handleList() ([]string, error) {
	files, err := os.ReadDir(ConfigDir)
	if err != nil {
		return nil, fmt.Errorf("konnte Verzeichnis nicht lesen: %w", err)
	}

	var configFiles []string
	for _, file := range files {
		if !file.IsDir() && strings.HasSuffix(file.Name(), ".ovpn") {
			configFiles = append(configFiles, file.Name())
		}
	}
	return configFiles, nil
}

func handleDelete(req ipc.Request) error {
	configPath, err := resolveConfigPath(req.Config)
	if err != nil {
		return err
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return fmt.Errorf("datei existiert nicht: %s", filepath.Base(configPath))
	}

	if err := os.Remove(configPath); err != nil {
		return fmt.Errorf("konnte Datei nicht löschen: %w", err)
	}

	log.Printf("Erfolgreich gelöscht: %s", configPath)
	return nil
}

func sendResponse(conn net.Conn, err error, successMsg string, data any) {
	resp := ipc.Response{Status: "success", Message: successMsg}

	if err != nil {
		resp.Status = "error"
		resp.Message = err.Error()
	} else if data != nil {
		raw, marshalErr := json.Marshal(data)
		if marshalErr != nil {
			resp.Status = "error"
			resp.Message = marshalErr.Error()
		} else {
			resp.Data = raw
		}
	}
	_ = json.NewEncoder(conn).Encode(resp)
}

func resolveConfigPath(rawFilename string) (string, error) {
	cleanName := sanitizeFileName(filepath.Base(rawFilename))
	if !strings.HasSuffix(cleanName, ".ovpn") {
		return "", errors.New("ungültiges Dateiformat (muss auf .ovpn enden)")
	}
	return filepath.Join(ConfigDir, cleanName), nil
}

var multiUnderscoreRegex = regexp.MustCompile(`_+`)
var invalidCharsRegex = regexp.MustCompile(`[^\w\.\-]`)

func sanitizeFileName(filename string) string {
	clean := strings.ReplaceAll(filename, " ", "_")
	clean = invalidCharsRegex.ReplaceAllString(clean, "")
	clean = multiUnderscoreRegex.ReplaceAllString(clean, "_")
	return clean
}

func cleanupSockets() {
	_ = os.Remove(ipc.SocketPath)
	_ = os.Remove(MgmtSocket)
}

func setupSocketPermissions() {
	if err := os.Chmod(ipc.SocketPath, 0660); err != nil {
		log.Printf("Warnung: Socket Chmod fehlgeschlagen: %v", err)
	}

	if g, err := user.LookupGroup("vpnusers"); err == nil {
		if gid, err := strconv.Atoi(g.Gid); err == nil {
			if err := os.Chown(ipc.SocketPath, -1, gid); err != nil {
				log.Printf("Warnung: Socket Chown auf vpnusers fehlgeschlagen: %v", err)
			}
		}
	}
}
