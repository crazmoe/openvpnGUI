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

	"openvpnGUI/internal/ipc"
)

//go:embed assets/*.png
var assets embed.FS

type AppUI struct {
	app              fyne.App
	window           fyne.Window
	prefs            fyne.Preferences
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
	iconAppBig fyne.Resource
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

	ui := &AppUI{
		app:              myApp,
		window:           win,
		prefs:            myApp.Preferences(),
		statusData:       binding.NewString(),
		ipData:           binding.NewString(),
		connectedBinding: binding.NewBool(),
	}

	ui.iconRed = ui.loadIcon("red")
	ui.iconYellow = ui.loadIcon("yellow")
	ui.iconGreen = ui.loadIcon("green")
	ui.iconAppBig = ui.loadIcon("red64")

	// App-/Fenster-Icon bleibt statisch; der Verbindungsstatus wird
	// ausschließlich über das Tray-Icon signalisiert.
	myApp.SetIcon(ui.iconAppBig)
	win.SetIcon(ui.iconAppBig)

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
		ui.prefs.SetString("lastConfig", selected)
	})
	ui.configSelect.PlaceHolder = "Bitte wählen..."

	ui.userEntry = widget.NewEntry()
	ui.userEntry.SetPlaceHolder("Benutzername")
	ui.userEntry.SetText(ui.prefs.String("lastUsername"))
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

	importBtn := widget.NewButtonWithIcon("Importieren", theme.FolderOpenIcon(), ui.handleImport)
	deleteBtn := widget.NewButtonWithIcon("Löschen", theme.DeleteIcon(), ui.handleDelete)

	ui.startBtn = widget.NewButtonWithIcon("Verbinden", theme.MediaPlayIcon(), ui.handleStart)
	ui.startBtn.Importance = widget.HighImportance
	ui.stopBtn = widget.NewButtonWithIcon("Trennen", theme.MediaStopIcon(), ui.handleStop)

	ui.connectedBinding.AddListener(binding.NewDataListener(func() {
		if val, _ := ui.connectedBinding.Get(); val {
			ui.window.Hide()
		}
	}))

	ui.setUIState(false)

	form := widget.NewForm(
		widget.NewFormItem("Server", ui.configSelect),
		widget.NewFormItem("Benutzername", ui.userEntry),
		widget.NewFormItem("Passwort", ui.passEntry),
	)

	spacer := layout.NewSpacer()
	importBtns := container.NewHBox(importBtn, spacer, deleteBtn)
	startBtns := container.NewHBox(ui.startBtn, spacer, ui.stopBtn)
	statusRow := container.NewHBox(statusLabel, layout.NewSpacer(), ipLabel)

	// Anordnung optimiert für die TAB-Fokus-Reihenfolge (Top-Down)
	content := container.NewVBox(
		form,
		startBtns,
		widget.NewSeparator(),
		statusRow,
		importBtns,
	)

	ui.window.SetContent(container.NewPadded(content))
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
		req := ipc.Request{
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
			req := ipc.Request{Action: "delete", Config: ui.configSelect.Selected}
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

	req := ipc.Request{
		Action:   "start",
		Config:   ui.selectedConfig,
		Username: ui.userEntry.Text,
		Password: ui.passEntry.Text,
	}

	// Sofortiges Feedback, damit der Nutzer nicht mehrfach klickt während
	// auf die Antwort des Daemons gewartet wird.
	ui.startBtn.Disable()
	_ = ui.statusData.Set("Status: Verbinde...")
	if ui.trayApp != nil {
		ui.trayApp.SetSystemTrayIcon(ui.iconYellow)
	}

	go func() {
		if _, err := sendCommand(req); err != nil {
			ui.showError(err)
			fyne.Do(func() { ui.setUIState(false) })
			return
		}
		ui.prefs.SetString("lastUsername", req.Username)
		fyne.Do(func() { ui.setUIState(true) })
	}()
}

func (ui *AppUI) handleStop() {
	ui.stopBtn.Disable()
	go func() {
		if _, err := sendCommand(ipc.Request{Action: "stop"}); err != nil {
			ui.showError(err)
			fyne.Do(func() { ui.stopBtn.Enable() })
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

// stateCategory ordnet einen rohen OpenVPN-Status einer von drei
// UI-Kategorien zu, die Icon-Farbe und Bedienbarkeit bestimmen.
func stateCategory(state string) string {
	switch state {
	case "CONNECTED":
		return "connected"
	case "GET_CONFIG", "ASSIGN_IP", "RECONNECTING", "AUTH", "WAIT", "TCP_CONNECT", "WAITING":
		return "connecting"
	default:
		return "disconnected"
	}
}

// friendlyState übersetzt die technischen OpenVPN-Statuscodes in
// verständliche deutsche Meldungen für die Statusanzeige.
func friendlyState(state string) string {
	switch state {
	case "CONNECTED":
		return "Verbunden"
	case "DISCONNECTED":
		return "Getrennt"
	case "AUTH":
		return "Authentifiziere..."
	case "GET_CONFIG":
		return "Lade Konfiguration..."
	case "ASSIGN_IP":
		return "Weise IP-Adresse zu..."
	case "TCP_CONNECT":
		return "Verbinde zum Server..."
	case "WAIT", "WAITING":
		return "Warte auf Server..."
	case "RECONNECTING":
		return "Verbindung wird wiederhergestellt..."
	default:
		return state
	}
}

func (ui *AppUI) startStatusLoop() {
	lastState := "INIT"
	lastIP := "INIT"

	for {
		status := getStatus()

		if status.State != lastState || status.LocalIP != lastIP {
			category := stateCategory(status.State)

			_ = ui.statusData.Set("Status: " + friendlyState(status.State))
			if status.LocalIP != "" {
				_ = ui.ipData.Set("IP: " + status.LocalIP)
			} else {
				_ = ui.ipData.Set("")
			}

			if status.State == "CONNECTED" && lastState != "CONNECTED" {
				_ = ui.connectedBinding.Set(true)
			} else if status.State != "CONNECTED" {
				_ = ui.connectedBinding.Set(false)
			}

			trayIcon := ui.iconRed
			switch category {
			case "connected":
				trayIcon = ui.iconGreen
			case "connecting":
				trayIcon = ui.iconYellow
			}

			fyne.Do(func() {
				ui.setUIState(category != "disconnected")
			})
			if ui.trayApp != nil {
				ui.trayApp.SetSystemTrayIcon(trayIcon)
			}

			lastState = status.State
			lastIP = status.LocalIP
		}
		time.Sleep(2 * time.Second)
	}
}

func (ui *AppUI) refreshConfigList() {
	respData, err := sendCommand(ipc.Request{Action: "list"})
	if err != nil {
		log.Printf("Fehler beim Abrufen der Config-Liste: %v", err)
		return
	}

	var configs []string
	if err := json.Unmarshal(respData, &configs); err != nil {
		log.Printf("Fehler beim Parsen der Config-Liste: %v", err)
		return
	}

	lastConfig := ui.prefs.String("lastConfig")

	fyne.Do(func() {
		ui.configSelect.Options = configs
		ui.configSelect.Refresh()

		for _, c := range configs {
			if c == lastConfig {
				ui.configSelect.SetSelected(lastConfig)
				ui.selectedConfig = lastConfig
				break
			}
		}
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

func sendCommand(req ipc.Request) (json.RawMessage, error) {
	conn, err := net.DialTimeout("unix", ipc.SocketPath, 3*time.Second)
	if err != nil {
		return nil, errors.New("daemon nicht erreichbar")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, err
	}

	var resp ipc.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, errors.New("fehler beim Lesen der Server-Antwort")
	}

	if resp.Status == "error" {
		return nil, errors.New(resp.Message)
	}

	return resp.Data, nil
}

func getStatus() ipc.VPNStatus {
	data, err := sendCommand(ipc.Request{Action: "status"})
	if err != nil {
		return ipc.VPNStatus{State: err.Error()}
	}

	var status ipc.VPNStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return ipc.VPNStatus{State: "Fehler beim Parsen"}
	}
	return status
}
