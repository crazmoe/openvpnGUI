#!/usr/bin/env bash
#
# build.sh – kompiliert openvpnGUI (Daemon + GUI) und baut daraus ein
# Debian-Paket (.deb), das sich mit `sudo apt install ./dist/<paket>.deb`
# installieren lässt.
#
# Aufruf:
#   ./build.sh                    # Version automatisch (git-Tag bzw. Ordnername)
#   ./build.sh --version 1.0.15   # Version explizit setzen
#   ./build.sh --help
#
# Das Paket enthält:
#   /usr/sbin/vpn-daemon                       Daemon (läuft als root)
#   /usr/bin/vpn-gui                           Desktop-App
#   /usr/lib/systemd/system/vpn-manager.service systemd-Dienst für den Daemon
#   /usr/share/applications/vpn-gui.desktop    Startmenü-Eintrag
#   /usr/share/icons/hicolor/64x64/apps/vpn-gui.png
#
# Bei der Installation wird die Gruppe `vpnusers` angelegt und der Dienst
# aktiviert. Der eigene Benutzer muss danach noch in die Gruppe aufgenommen
# werden (siehe Hinweis am Ende der Installation).

set -euo pipefail

PKG_NAME="openvpngui"
MAINTAINER="openvpnGUI <noreply@localhost>"
DESCRIPTION_SHORT="Schlanke Desktop-Oberfläche für OpenVPN"

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIST_DIR="$ROOT_DIR/dist"
VERSION=""

# ---------------------------------------------------------------------------
# Hilfsfunktionen
# ---------------------------------------------------------------------------

if [[ -t 1 ]]; then
	C_RED=$'\e[31m' C_GRN=$'\e[32m' C_YEL=$'\e[33m' C_BLD=$'\e[1m' C_RST=$'\e[0m'
else
	C_RED="" C_GRN="" C_YEL="" C_BLD="" C_RST=""
fi

info() { printf '%s==>%s %s\n' "$C_GRN$C_BLD" "$C_RST" "$*"; }
warn() { printf '%sWarnung:%s %s\n' "$C_YEL$C_BLD" "$C_RST" "$*" >&2; }
die() {
	printf '%sFehler:%s %s\n' "$C_RED$C_BLD" "$C_RST" "$*" >&2
	exit 1
}

usage() {
	sed -n '2,/^$/{s/^# \{0,1\}//;p}' "${BASH_SOURCE[0]}"
	exit 0
}

# ---------------------------------------------------------------------------
# Argumente
# ---------------------------------------------------------------------------

while [[ $# -gt 0 ]]; do
	case "$1" in
	-v | --version)
		[[ $# -ge 2 ]] || die "--version braucht einen Wert, z. B. --version 1.0.15"
		VERSION="$2"
		shift 2
		;;
	--version=*)
		VERSION="${1#*=}"
		shift
		;;
	-h | --help) usage ;;
	*) die "Unbekannte Option: $1 (siehe ./build.sh --help)" ;;
	esac
done

# ---------------------------------------------------------------------------
# Abhängigkeiten prüfen
# ---------------------------------------------------------------------------

check_deps() {
	info "Prüfe Build-Abhängigkeiten …"

	command -v dpkg >/dev/null 2>&1 ||
		die "dpkg nicht gefunden. Dieses Skript baut ein .deb und läuft nur auf Debian/Ubuntu und Abkömmlingen."

	local missing_pkgs=() problems=()

	# Debian-Pakete mit Headern/Werkzeugen, die Fyne zum Kompilieren braucht.
	local pkg
	for pkg in gcc binutils pkg-config libgl1-mesa-dev xorg-dev libwayland-dev libxkbcommon-dev; do
		if ! dpkg-query -W -f='${Status}' "$pkg" 2>/dev/null | grep -q 'install ok installed'; then
			missing_pkgs+=("$pkg")
		fi
	done

	# Go: Mindestversion steht in go.mod.
	local go_required
	go_required="$(awk '$1 == "go" { print $2; exit }' "$ROOT_DIR/go.mod")"
	if ! command -v go >/dev/null 2>&1; then
		problems+=("Go ist nicht installiert (benötigt: Go $go_required oder neuer).")
	else
		local go_have
		go_have="$(go env GOVERSION 2>/dev/null || true)"
		go_have="${go_have#go}"
		if [[ -n "$go_have" ]] && ! version_ge "$go_have" "$go_required"; then
			# Neuere Go-Versionen laden die passende Toolchain selbst nach
			# (GOTOOLCHAIN=auto), ältere als 1.21 können das nicht.
			if version_ge "$go_have" "1.21"; then
				warn "Go $go_have ist älter als $go_required – Go lädt die passende Toolchain automatisch herunter."
			else
				problems+=("Go $go_have ist zu alt (benötigt: Go $go_required oder neuer).")
			fi
		fi
	fi

	if [[ ${#missing_pkgs[@]} -eq 0 && ${#problems[@]} -eq 0 ]]; then
		return
	fi

	printf '\n%sEs fehlen Voraussetzungen zum Bauen:%s\n\n' "$C_RED$C_BLD" "$C_RST" >&2
	if [[ ${#missing_pkgs[@]} -gt 0 ]]; then
		printf '  Fehlende Pakete: %s\n\n' "${missing_pkgs[*]}" >&2
		printf '  Installieren mit:\n    sudo apt install %s\n\n' "${missing_pkgs[*]}" >&2
	fi
	local p
	for p in "${problems[@]}"; do
		printf '  %s\n' "$p" >&2
	done
	if [[ ${#problems[@]} -gt 0 ]]; then
		printf '\n  Go gibt es unter https://go.dev/dl/ (die Version aus den\n' >&2
		printf '  Paketquellen ist oft zu alt). Danach ./build.sh erneut starten.\n\n' >&2
	fi
	exit 1
}

# version_ge A B → wahr, wenn Version A >= B (Punkt-getrennt, z. B. 1.25.0).
version_ge() {
	[[ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" == "$2" ]]
}

# ---------------------------------------------------------------------------
# Version bestimmen
# ---------------------------------------------------------------------------

detect_version() {
	if [[ -n "$VERSION" ]]; then
		VERSION="${VERSION#v}"
	elif git -C "$ROOT_DIR" rev-parse --git-dir >/dev/null 2>&1 &&
		desc="$(git -C "$ROOT_DIR" describe --tags --dirty 2>/dev/null)"; then
		# v1.0.15            → 1.0.15
		# v1.0.15-3-gabc1234 → 1.0.15+git3.abc1234
		desc="${desc#v}"
		if [[ "$desc" =~ ^(.+)-([0-9]+)-g([0-9a-f]+)(-dirty)?$ ]]; then
			VERSION="${BASH_REMATCH[1]}+git${BASH_REMATCH[2]}.${BASH_REMATCH[3]}"
			[[ -n "${BASH_REMATCH[4]}" ]] && VERSION+=".dirty"
		else
			VERSION="${desc/-dirty/+dirty}"
		fi
	else
		# Entpacktes Source-Archiv von GitHub heisst z. B. openvpnGUI-1.0.15
		local dir
		dir="$(basename "$ROOT_DIR")"
		if [[ "$dir" =~ -v?([0-9][0-9A-Za-z.+~]*)$ ]]; then
			VERSION="${BASH_REMATCH[1]}"
		else
			VERSION="0.0.0"
			warn "Version nicht erkennbar, verwende $VERSION (setzen mit --version X.Y.Z)."
		fi
	fi

	[[ "$VERSION" =~ ^[0-9][0-9A-Za-z.+~-]*$ ]] ||
		die "Ungültige Paketversion '$VERSION' (muss mit einer Ziffer beginnen, z. B. 1.0.15)."
}

# ---------------------------------------------------------------------------
# Kompilieren
# ---------------------------------------------------------------------------

build_binaries() {
	local out="$1"
	info "Kompiliere vpn-daemon …"
	CGO_ENABLED=0 go build -C "$ROOT_DIR" -trimpath -ldflags "-s -w" -o "$out/vpn-daemon" ./cmd/daemon
	info "Kompiliere vpn-gui (das dauert beim ersten Mal ein paar Minuten) …"
	CGO_ENABLED=1 go build -C "$ROOT_DIR" -trimpath -ldflags "-s -w" -o "$out/vpn-gui" ./cmd/gui
}

# ---------------------------------------------------------------------------
# Laufzeit-Abhängigkeiten aus den gelinkten Bibliotheken ableiten
# ---------------------------------------------------------------------------

# Gibt für jede direkt gelinkte Shared Library (ELF NEEDED) von $1 das
# Debian-Paket aus, z. B. "libgl1 (>= 1.7.0)".
shlib_depends() {
	local bin="$1" lib path pkg ver
	local -A seen=() needed=()
	while read -r lib; do
		needed[$lib]=1
	done < <(readelf -d "$bin" 2>/dev/null | sed -n 's/.*(NEEDED).*\[\(.*\)\]/\1/p')

	while read -r lib path; do
		[[ -n "${needed[$lib]:-}" && -n "$path" && -e "$path" ]] || continue
		pkg=""
		local candidate
		for candidate in "$path" "$(readlink -f "$path")" "${path#/usr}" "/usr$path"; do
			pkg="$(dpkg-query -S "$candidate" 2>/dev/null | head -n1 | cut -d: -f1)" && [[ -n "$pkg" ]] && break
		done
		[[ -n "$pkg" && -z "${seen[$pkg]:-}" ]] || continue
		seen[$pkg]=1
		ver="$(dpkg-query -W -f='${Version}' "$pkg" 2>/dev/null | sed -E 's/^[0-9]+://; s/[-+~].*$//')"
		if [[ -n "$ver" ]]; then
			printf '%s (>= %s)\n' "$pkg" "$ver"
		else
			printf '%s\n' "$pkg"
		fi
	done < <(ldd "$bin" 2>/dev/null | awk '$2 == "=>" && $3 ~ /^\// { print $1, $3 }')
}

compute_depends() {
	local gui="$1" deps
	deps="$(shlib_depends "$gui" | sort -u | paste -sd, - | sed 's/,/, /g')"
	if [[ -z "$deps" ]]; then
		warn "Konnte Bibliotheks-Abhängigkeiten nicht ermitteln, verwende Standardliste."
		deps="libc6, libgl1, libwayland-client0"
	fi
	# GLFW lädt X11- und Wayland-Bibliotheken erst zur Laufzeit (dlopen),
	# daher tauchen sie nicht in den gelinkten Bibliotheken auf.
	deps+=", libx11-6, libxcursor1, libxrandr2, libxinerama1, libxi6, libxxf86vm1"
	deps+=", libxkbcommon0, libwayland-cursor0, libwayland-egl1"
	printf '%s, openvpn' "$deps"
}

# ---------------------------------------------------------------------------
# Paket zusammenstellen
# ---------------------------------------------------------------------------

write_package_files() {
	local root="$1" bin="$2"

	install -Dm0755 "$bin/vpn-daemon" "$root/usr/sbin/vpn-daemon"
	install -Dm0755 "$bin/vpn-gui" "$root/usr/bin/vpn-gui"
	install -Dm0644 "$ROOT_DIR/cmd/gui/assets/green64.png" \
		"$root/usr/share/icons/hicolor/64x64/apps/vpn-gui.png"
	install -Dm0644 "$ROOT_DIR/LICENSE" "$root/usr/share/doc/$PKG_NAME/copyright"
	install -Dm0644 "$ROOT_DIR/README.md" "$root/usr/share/doc/$PKG_NAME/README.md"

	install -d "$root/usr/lib/systemd/system"
	cat >"$root/usr/lib/systemd/system/vpn-manager.service" <<'EOF'
[Unit]
Description=VPN Manager Daemon (openvpnGUI)
After=network.target

[Service]
ExecStart=/usr/sbin/vpn-daemon
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF

	install -d "$root/usr/share/applications"
	cat >"$root/usr/share/applications/vpn-gui.desktop" <<'EOF'
[Desktop Entry]
Type=Application
Name=VPN Manager
GenericName=OpenVPN-Client
Comment=OpenVPN-Verbindungen verwalten
Exec=vpn-gui
Icon=vpn-gui
Terminal=false
Categories=Network;
Keywords=VPN;OpenVPN;
EOF

	local installed_size
	installed_size="$(du -sk --exclude=DEBIAN "$root" | cut -f1)"

	install -d "$root/DEBIAN"
	cat >"$root/DEBIAN/control" <<EOF
Package: $PKG_NAME
Version: $VERSION
Section: net
Priority: optional
Architecture: $ARCH
Maintainer: $MAINTAINER
Installed-Size: $installed_size
Depends: $DEPENDS
Recommends: systemd-resolved | resolvconf
Description: $DESCRIPTION_SHORT
 Besteht aus einem root-Dienst (vpn-daemon), der openvpn startet und DNS
 anpasst, und einer Fyne-Desktop-App (vpn-gui) mit Tray-Icon, die als
 normaler Benutzer läuft. Benutzer der Gruppe vpnusers dürfen die GUI nutzen.
EOF

	cat >"$root/DEBIAN/postinst" <<'EOF'
#!/bin/sh
set -e

if [ "$1" = "configure" ]; then
	if ! getent group vpnusers >/dev/null; then
		addgroup --system vpnusers >/dev/null 2>&1 || groupadd --system vpnusers
	fi

	if [ -d /run/systemd/system ]; then
		systemctl daemon-reload || true
		systemctl enable vpn-manager.service >/dev/null 2>&1 || true
		systemctl restart vpn-manager.service || true
	fi

	if [ -z "$2" ]; then
		cat <<'MSG'

openvpnGUI ist installiert. Damit die GUI mit dem Dienst sprechen darf,
füge deinen Benutzer zur Gruppe vpnusers hinzu und melde dich neu an:

    sudo usermod -aG vpnusers "$USER"

MSG
	fi
fi

exit 0
EOF

	cat >"$root/DEBIAN/prerm" <<'EOF'
#!/bin/sh
set -e

if [ -d /run/systemd/system ]; then
	case "$1" in
	remove | deconfigure)
		systemctl disable --now vpn-manager.service >/dev/null 2>&1 || true
		;;
	upgrade)
		systemctl stop vpn-manager.service >/dev/null 2>&1 || true
		;;
	esac
fi

exit 0
EOF

	cat >"$root/DEBIAN/postrm" <<'EOF'
#!/bin/sh
set -e

if [ -d /run/systemd/system ]; then
	systemctl daemon-reload >/dev/null 2>&1 || true
fi

# Bei "purge" auch die importierten VPN-Konfigurationen entfernen.
if [ "$1" = "purge" ]; then
	rm -rf /etc/vpn-manager
fi

exit 0
EOF
	chmod 0755 "$root/DEBIAN/postinst" "$root/DEBIAN/prerm" "$root/DEBIAN/postrm"
}

# ---------------------------------------------------------------------------
# Ablauf
# ---------------------------------------------------------------------------

check_deps
detect_version
ARCH="$(dpkg --print-architecture)"

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT
BIN_DIR="$WORK_DIR/bin"
PKG_ROOT="$WORK_DIR/pkg"
mkdir -p "$BIN_DIR" "$PKG_ROOT"

info "Baue $PKG_NAME $VERSION für $ARCH"
build_binaries "$BIN_DIR"

info "Ermittle Laufzeit-Abhängigkeiten …"
DEPENDS="$(compute_depends "$BIN_DIR/vpn-gui")"

info "Stelle Paket zusammen …"
write_package_files "$PKG_ROOT" "$BIN_DIR"

mkdir -p "$DIST_DIR"
DEB="$DIST_DIR/${PKG_NAME}_${VERSION}_${ARCH}.deb"
dpkg-deb --root-owner-group -Zxz --build "$PKG_ROOT" "$DEB" >/dev/null

printf '\n%sFertig:%s %s\n\n' "$C_GRN$C_BLD" "$C_RST" "${DEB#"$ROOT_DIR"/}"
printf 'Installieren mit:\n    sudo apt install ./%s\n\n' "${DEB#"$ROOT_DIR"/}"
printf 'Danach einmalig den eigenen Benutzer berechtigen und neu anmelden:\n'
# shellcheck disable=SC2016 # $USER soll wörtlich ausgegeben werden
printf '    sudo usermod -aG vpnusers "$USER"\n'
