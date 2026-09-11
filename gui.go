package main

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/data/binding"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

const SocketPath = "/run/vpn-manager.sock"

type DaemonResponse struct {
	Status  string          `json:"status"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type Request struct {
	Action   string `json:"action"`
	Config   string `json:"config"`
	Username string `json:"username"`
	Password string `json:"password"`
	Content  string `json:"content"`
}

type VPNStatus struct {
	State   string `json:"state"`
	LocalIP string `json:"local_ip"`
}

//go:embed assets/*.png
var assets embed.FS

type AppUI struct {
	app              fyne.App
	window           fyne.Window
	trayApp          desktop.App
	trayMenu         *fyne.Menu
	disconnectItem   *fyne.MenuItem
	startBtn         *widget.Button
	stopBtn          *widget.Button
	userEntry        *widget.Entry
	passEntry        *widget.Entry
	configSelect     *widget.Select
	statusData       binding.String
	ipData           binding.String
	connectedBinding binding.Bool
	selectedConfig   string

	iconRed    fyne.Resource
	iconYellow fyne.Resource
	iconGreen  fyne.Resource
}

func main() {
	startMinimized := flag.Bool("minimized", false, "Startet die App direkt minimiert im System-Tray")
	flag.Parse()

	ui := newAppUI()
	ui.buildUI()
	ui.setupTray()

	go ui.refreshConfigList()
	go ui.startStatusLoop()

	if *startMinimized {
		ui.app.Run()
	} else {
		ui.window.ShowAndRun()
	}
}

func newAppUI() *AppUI {
	myApp := app.NewWithID("com.vpnmanager.client")
	win := myApp.NewWindow("VPN Manager")
	win.Resize(fyne.NewSize(380, 280))

	ui := &AppUI{
		app:              myApp,
		window:           win,
		statusData:       binding.NewString(),
		ipData:           binding.NewString(),
		connectedBinding: binding.NewBool(),
	}

	ui.iconRed = ui.loadIcon("red")
	ui.iconYellow = ui.loadIcon("yellow")
	ui.iconGreen = ui.loadIcon("green")

	return ui
}

func (ui *AppUI) loadIcon(name string) fyne.Resource {
	path := "assets/" + name + ".png"
	data, err := assets.ReadFile(path)
	if err != nil {
		log.Printf("FEHLER: Konnte Icon %s nicht laden: %v", path, err)
		return theme.WarningIcon()
	}
	return fyne.NewStaticResource(name+".png", data)
}

func (ui *AppUI) buildUI() {
	_ = ui.statusData.Set("Status: Bereit")

	statusLabel := widget.NewLabelWithData(ui.statusData)
	ipLabel := widget.NewLabelWithData(ui.ipData)

	ui.configSelect = widget.NewSelect([]string{}, func(selected string) {
		ui.selectedConfig = selected
	})

	ui.userEntry = widget.NewEntry()
	ui.userEntry.SetPlaceHolder("Benutzername")
	// Enter-Taste im Benutzer-Feld wechselt ins Passwort-Feld
	ui.userEntry.OnSubmitted = func(_ string) {
		ui.window.Canvas().Focus(ui.passEntry)
	}

	ui.passEntry = widget.NewPasswordEntry()
	ui.passEntry.SetPlaceHolder("Passwort")
	// Enter-Taste im Passwort-Feld startet die Verbindung
	ui.passEntry.OnSubmitted = func(_ string) {
		ui.handleStart()
	}

	importBtn := widget.NewButton("Config Importieren", ui.handleImport)
	deleteBtn := widget.NewButton("Config löschen", ui.handleDelete)

	ui.startBtn = widget.NewButton("  Verbinden  ", ui.handleStart)
	ui.stopBtn = widget.NewButton("  Trennen  ", ui.handleStop)

	ui.connectedBinding.AddListener(binding.NewDataListener(func() {
		if val, _ := ui.connectedBinding.Get(); val {
			ui.window.Hide()
		}
	}))

	ui.setUIState(false)

	spacer := layout.NewSpacer()
	importBtns := container.NewHBox(importBtn, spacer, deleteBtn)
	startBtns := container.NewHBox(ui.startBtn, spacer, ui.stopBtn)

	// Anordnung optimiert für die TAB-Fokus-Reihenfolge (Top-Down)
	content := container.NewVBox(
		ui.configSelect,
		ui.userEntry,
		ui.passEntry,
		startBtns,
		statusLabel,
		ipLabel,
		importBtns,
	)

	ui.window.SetContent(content)
	ui.window.SetCloseIntercept(func() { ui.window.Hide() })
}

// -----------------------------------------------------------------------------
// BUTTON HANDLER & ACTIONS
// -----------------------------------------------------------------------------

func (ui *AppUI) handleImport() {
	fileDialog := dialog.NewFileOpen(func(reader fyne.URIReadCloser, err error) {
		if err != nil {
			ui.showError(err)
			return
		}
		if reader == nil {
			return
		}
		defer reader.Close()

		content, err := io.ReadAll(reader)
		if err != nil {
			ui.showError(fmt.Errorf("konnte Datei nicht lesen: %w", err))
			return
		}

		encodedContent := base64.StdEncoding.EncodeToString(content)
		req := Request{
			Action:  "import",
			Config:  reader.URI().Name(),
			Content: encodedContent,
		}

		go func() {
			if _, err := sendCommand(req); err != nil {
				ui.showError(err)
				return
			}
			ui.refreshConfigList()
			fyne.Do(func() {
				dialog.ShowInformation("Erfolg", "Datei "+req.Config+" importiert.", ui.window)
			})
		}()
	}, ui.window)

	fileDialog.Resize(fyne.NewSize(700, 500))
	fileDialog.SetFilter(storage.NewExtensionFileFilter([]string{".ovpn"}))
	fileDialog.Show()
}

func (ui *AppUI) handleDelete() {
	if ui.configSelect.Selected == "" {
		dialog.ShowError(errors.New("bitte wähle zuerst eine Konfiguration aus"), ui.window)
		return
	}

	dialog.ShowConfirm("Konfiguration löschen",
		fmt.Sprintf("Möchtest du die Datei \"%s\" wirklich löschen?", ui.configSelect.Selected),
		func(confirmed bool) {
			if !confirmed {
				return
			}
			req := Request{Action: "delete", Config: ui.configSelect.Selected}
			go func() {
				if _, err := sendCommand(req); err != nil {
					ui.showError(err)
					return
				}
				fyne.Do(func() {
					ui.configSelect.ClearSelected()
					ui.selectedConfig = ""
					dialog.ShowInformation("Erfolg", "Konfiguration gelöscht.", ui.window)
				})
				ui.refreshConfigList()
			}()
		}, ui.window)
}

func (ui *AppUI) handleStart() {
	if ui.selectedConfig == "" {
		dialog.ShowError(errors.New("bitte wähle eine Config-Datei aus"), ui.window)
		return
	}
	if ui.userEntry.Text == "" || ui.passEntry.Text == "" {
		dialog.ShowError(errors.New("benutzername und Passwort sind erforderlich"), ui.window)
		return
	}

	req := Request{
		Action:   "start",
		Config:   ui.selectedConfig,
		Username: ui.userEntry.Text,
		Password: ui.passEntry.Text,
	}

	go func() {
		if _, err := sendCommand(req); err != nil {
			ui.showError(err)
			return
		}
		fyne.Do(func() { ui.setUIState(true) })
	}()
}

func (ui *AppUI) handleStop() {
	go func() {
		if _, err := sendCommand(Request{Action: "stop"}); err != nil {
			ui.showError(err)
			return
		}
		fyne.Do(func() { ui.setUIState(false) })
	}()
}

// -----------------------------------------------------------------------------
// TRAY & STATUS LOOP
// -----------------------------------------------------------------------------

func (ui *AppUI) setupTray() {
	if desk, ok := ui.app.(desktop.App); ok {
		ui.trayApp = desk

		ui.disconnectItem = fyne.NewMenuItem("Trennen", ui.handleStop)
		ui.disconnectItem.Disabled = true

		ui.trayMenu = fyne.NewMenu("VPN Manager",
			fyne.NewMenuItem("Anzeigen", func() { ui.window.Show() }),
			ui.disconnectItem,
			fyne.NewMenuItem("Beenden", func() { ui.app.Quit() }),
		)
		ui.trayApp.SetSystemTrayMenu(ui.trayMenu)
		ui.trayApp.SetSystemTrayIcon(ui.iconRed)
	}
}

func (ui *AppUI) startStatusLoop() {
	lastState := "INIT"
	lastIP := "INIT"

	for {
		status := getStatus()

		if status.State != lastState || status.LocalIP != lastIP {
			_ = ui.statusData.Set("Status: " + status.State)
			if status.LocalIP != "" {
				_ = ui.ipData.Set("IP: " + status.LocalIP)
			} else {
				_ = ui.ipData.Set("")
			}

			if ui.trayApp != nil {
				if status.State == "CONNECTED" && lastState != "CONNECTED" {
					_ = ui.connectedBinding.Set(true)
				} else if status.State != "CONNECTED" {
					_ = ui.connectedBinding.Set(false)
				}

				switch status.State {
				case "CONNECTED":
					fyne.Do(func() { ui.setUIState(true) })
					ui.trayApp.SetSystemTrayIcon(ui.iconGreen)
				case "GET_CONFIG", "ASSIGN_IP", "RECONNECTING", "AUTH", "WAIT", "TCP_CONNECT", "WAITING":
					fyne.Do(func() { ui.setUIState(true) })
					ui.trayApp.SetSystemTrayIcon(ui.iconYellow)
				default:
					fyne.Do(func() { ui.setUIState(false) })
					ui.trayApp.SetSystemTrayIcon(ui.iconRed)
				}
			}
			lastState = status.State
			lastIP = status.LocalIP
		}
		time.Sleep(2 * time.Second)
	}
}

func (ui *AppUI) refreshConfigList() {
	respData, err := sendCommand(Request{Action: "list"})
	if err != nil {
		log.Printf("Fehler beim Abrufen der Config-Liste: %v", err)
		return
	}

	var configs []string
	if err := json.Unmarshal(respData, &configs); err != nil {
		log.Printf("Fehler beim Parsen der Config-Liste: %v", err)
		return
	}

	fyne.Do(func() {
		ui.configSelect.Options = configs
		ui.configSelect.Refresh()
	})
}

func (ui *AppUI) setUIState(connected bool) {
	if connected {
		ui.startBtn.Disable()
		ui.stopBtn.Enable()
		ui.userEntry.Disable()
		ui.passEntry.Disable()
		ui.passEntry.SetText("")
		ui.configSelect.Disable()
		if ui.disconnectItem != nil {
			ui.disconnectItem.Disabled = false
		}
	} else {
		ui.startBtn.Enable()
		ui.stopBtn.Disable()
		ui.userEntry.SetText("")
		ui.userEntry.Enable()
		ui.passEntry.Enable()
		ui.configSelect.Enable()
		if ui.disconnectItem != nil {
			ui.disconnectItem.Disabled = true
		}
	}
	ui.passEntry.Refresh()
	if ui.trayApp != nil && ui.trayMenu != nil {
		ui.trayApp.SetSystemTrayMenu(ui.trayMenu)
	}
}

func (ui *AppUI) showError(err error) {
	fyne.Do(func() {
		dialog.ShowError(err, ui.window)
	})
}

// -----------------------------------------------------------------------------
// IPC CLIENT (UNIX SOCKET)
// -----------------------------------------------------------------------------

func sendCommand(req Request) (json.RawMessage, error) {
	conn, err := net.DialTimeout("unix", SocketPath, 3*time.Second)
	if err != nil {
		return nil, errors.New("daemon nicht erreichbar")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, err
	}

	var resp DaemonResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, errors.New("fehler beim Lesen der Server-Antwort")
	}

	if resp.Status == "error" {
		return nil, errors.New(resp.Message)
	}

	return resp.Data, nil
}

func getStatus() VPNStatus {
	data, err := sendCommand(Request{Action: "status"})
	if err != nil {
		return VPNStatus{State: err.Error()}
	}

	var status VPNStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return VPNStatus{State: "Fehler beim Parsen"}
	}
	return status
}