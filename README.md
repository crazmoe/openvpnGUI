# openvpnGUI

Eine schlanke Desktop-Oberfläche für OpenVPN unter Linux, bestehend aus
zwei Komponenten:

- **`cmd/daemon`** – ein root-privilegierter Hintergrunddienst, der
  `openvpn` startet/stoppt, Config-Dateien verwaltet und die DNS-Einstellungen
  beim Verbinden/Trennen anpasst.
- **`cmd/gui`** – eine Fyne-basierte Desktop-App (System-Tray-fähig), die als
  normaler Benutzer läuft und über einen Unix-Socket mit dem Daemon spricht.

Die beiden Teile sind bewusst getrennt: Nur der Daemon braucht Root-Rechte
(für `openvpn`, das `tun`-Device und DNS-Änderungen), die GUI läuft mit den
Rechten des angemeldeten Benutzers.

## Architektur

```
┌──────────────┐   Unix-Socket    ┌──────────────┐
│  cmd/gui     │ ───────────────▶ │  cmd/daemon  │──▶ openvpn (root)
│ (User-Rechte)│  /run/vpn-       │ (root)       │──▶ resolvectl/DNS
└──────────────┘  manager.sock    └──────────────┘
```

Das gemeinsame Nachrichtenformat (`Request`/`Response`/`VPNStatus`) liegt in
[`internal/ipc`](internal/ipc/ipc.go).

## Voraussetzungen

- Go 1.25+
- Linux mit `openvpn` installiert (`/usr/sbin/openvpn`)
- Für die GUI: X11/Wayland, sowie die üblichen Fyne-Build-Abhängigkeiten
  (siehe [Fyne-Dokumentation](https://docs.fyne.io/started/), z. B. unter
  Ubuntu/Debian: `gcc`, `libgl1-mesa-dev`, `xorg-dev`)
- Für DNS-Handling eines von: `resolvectl` (systemd-resolved), `systemd-resolve`
  oder `resolvconf`

## Build

```sh
go build -o vpn-daemon ./cmd/daemon
go build -o vpn-gui ./cmd/gui
```

### Debian-Paket bauen

Auf Debian/Ubuntu (und Abkömmlingen) baut `build.sh` beide Programme und
packt sie zusammen mit systemd-Dienst, Startmenü-Eintrag und Icon in ein
`.deb`:

```sh
./build.sh                    # Version aus dem git-Tag bzw. Ordnernamen
./build.sh --version 1.0.15   # Version explizit setzen
sudo apt install ./dist/openvpngui_*.deb
sudo usermod -aG vpnusers "$USER"   # danach neu anmelden
```

Fehlen Build-Abhängigkeiten, nennt das Skript den passenden
`sudo apt install …`-Befehl. Das Paket legt die Gruppe `vpnusers` an und
aktiviert den Dienst `vpn-manager.service` (Daemon unter
`/usr/sbin/vpn-daemon`); die manuelle Einrichtung unten entfällt damit.
Wer vorher schon manuell installiert hat, sollte
`/etc/systemd/system/vpn-manager.service` und `/usr/local/bin/vpn-daemon`
entfernen, sonst startet weiterhin die alte Version.

## Einrichtung

### 1. Daemon

Der Daemon muss als `root` laufen (z. B. via systemd-Service) und legt beim
Start folgende Pfade an:

| Pfad                         | Zweck                                  |
|------------------------------|-----------------------------------------|
| `/run/vpn-manager.sock`      | IPC-Socket für die GUI                  |
| `/run/vpn-openvpn-mgmt.sock` | OpenVPN-Management-Interface            |
| `/etc/vpn-manager/configs`   | Abgelegte `.ovpn`-Konfigurationsdateien |
| `/run/vpn-manager-temp`      | Temporäre Auth-Dateien (0700, root-only)|

Damit die GUI (als normaler Benutzer) mit dem Daemon sprechen darf, wird eine
Gruppe `vpnusers` erwartet; der Socket wird auf Gruppenrechte `0660` gesetzt.
Lege die Gruppe an und füge deinen Benutzer hinzu:

```sh
sudo groupadd vpnusers
sudo usermod -aG vpnusers "$USER"
```

Beispiel für einen systemd-Service (`/etc/systemd/system/vpn-manager.service`):

```ini
[Unit]
Description=VPN Manager Daemon
After=network.target

[Service]
ExecStart=/usr/local/bin/vpn-daemon
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

```sh
sudo cp vpn-daemon /usr/local/bin/vpn-daemon
sudo systemctl enable --now vpn-manager.service
```

### 2. GUI

Die GUI kann direkt gestartet werden:

```sh
./vpn-gui
```

Optional minimiert im System-Tray starten (z. B. per Autostart-Eintrag):

```sh
./vpn-gui --minimized
```

Die GUI übernimmt die Skalierung des Desktops (z. B. 150 % oder 200 % auf
HiDPI-Bildschirmen) automatisch aus `Xft.dpi` bzw. `GDK_SCALE`. Passt die
Grösse trotzdem nicht, lässt sie sich von Hand festlegen:

```sh
./vpn-gui --scale 2
```

Alternativ wirkt auch die Fyne-Umgebungsvariable `FYNE_SCALE`.

## Bedienung

1. `.ovpn`-Konfiguration über **Importieren** hinzufügen.
2. Server, Benutzername und Passwort eingeben.
3. **Verbinden** klicken – die App minimiert sich bei erfolgreichem Verbindungsaufbau
   automatisch ins Tray-Icon.
4. Der Verbindungsstatus (Getrennt/Verbinde/Verbunden) wird per Farbe im
   Tray-Icon angezeigt sowie als Text im Fenster.
5. Trennen über den Button im Fenster oder das Tray-Menü.

Benutzername und zuletzt gewählte Konfiguration werden lokal gespeichert
(Fyne-Preferences) und beim nächsten Start vorausgefüllt. Das Passwort wird
nicht gespeichert.

## Sicherheitshinweise

- Zugangsdaten werden nur temporär (0700, ausschließlich root-lesbar) als
  Auth-Datei für `openvpn --auth-user-pass` abgelegt und nach Verbindungsende
  wieder gelöscht.
- Konfigurationsnamen werden beim Import/Löschen bereinigt (nur alphanumerische
  Zeichen, `.` und `-`), um Path-Traversal zu verhindern.
- Der Daemon nimmt ausschließlich Verbindungen über den lokalen Unix-Socket an.
