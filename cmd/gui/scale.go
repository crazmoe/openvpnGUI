package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	scaleEnvKey        = "FYNE_SCALE"
	disableDPIEnvKey   = "FYNE_DISABLE_DPI_DETECTION"
	platformEnvKey     = "FYNE_PLATFORM"
	standardDPI        = 96.0
	minScale, maxScale = 0.5, 4.0
)

// configureScale legt fest, mit welchem Faktor Fyne Schrift und Bedienelemente
// skaliert. Muss vor dem Erzeugen der App aufgerufen werden.
//
// Fyne erkennt unter Linux/X11 (auch XWayland) die Desktop-Skalierung nicht
// zuverlässig: Es rechnet nur mit der physischen Monitorgröße und ignoriert die
// Einstellung der Desktop-Umgebung (z. B. 200 % unter KDE Plasma). Auf
// HiDPI-Bildschirmen erscheinen Fenster und Schrift dann winzig. Deshalb wird
// hier die Skalierung des Desktops (Xft.dpi bzw. GDK_SCALE) übernommen.
//
// Reihenfolge: Kommandozeilen-Flag > gesetztes FYNE_SCALE > Desktop-Einstellung.
func configureScale(flagScale float64) {
	if flagScale > 0 {
		applyScale(flagScale, "Kommandozeile")
		return
	}
	if os.Getenv(scaleEnvKey) != "" {
		return // Benutzer hat selbst etwas vorgegeben
	}
	// Nur eingreifen, wenn der Desktop tatsächlich vergrößert; bei 100 %
	// bleibt alles beim Fyne-Standardverhalten.
	if s, source := detectDesktopScale(); s > 1.05 {
		applyScale(s, source)
	}
}

func applyScale(s float64, source string) {
	if s < minScale {
		s = minScale
	}
	if s > maxScale {
		s = maxScale
	}
	_ = os.Setenv(scaleEnvKey, strconv.FormatFloat(s, 'f', 2, 64))
	// Die eigene DPI-Erkennung von Fyne abschalten, sonst würde der Faktor
	// doppelt angewendet.
	_ = os.Setenv(disableDPIEnvKey, "true")
	// In einer Wayland-Sitzung würde Fyne sonst je nach Compositor nativ unter
	// Wayland laufen und dort zusätzlich selbst skalieren. Mit XWayland wird
	// der Faktor genau einmal angewendet.
	if os.Getenv("WAYLAND_DISPLAY") != "" && os.Getenv("DISPLAY") != "" && os.Getenv(platformEnvKey) == "" {
		_ = os.Setenv(platformEnvKey, "x11")
	}
	log.Printf("Skalierung %.2f (Quelle: %s)", s, source)
}

// detectDesktopScale liefert die von der Desktop-Umgebung gewünschte
// Skalierung, oder 0, wenn nichts Brauchbares gefunden wurde.
func detectDesktopScale() (float64, string) {
	if dpi := xftDPI(); dpi > 0 {
		return dpi / standardDPI, fmt.Sprintf("Xft.dpi=%.0f", dpi)
	}
	if v := os.Getenv("GDK_SCALE"); v != "" {
		if s, err := strconv.ParseFloat(v, 64); err == nil && s > 0 {
			return s, "GDK_SCALE=" + v
		}
	}
	return 0, ""
}

// xftDPI liest Xft.dpi aus der X-Resource-Datenbank. Dort hinterlegen KDE,
// GNOME, Xfce & Co. die eingestellte Skalierung für X11-Programme.
func xftDPI() float64 {
	if os.Getenv("DISPLAY") == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "xrdb", "-query").Output()
	if err != nil {
		return 0
	}
	return parseXftDPI(out)
}

func parseXftDPI(out []byte) float64 {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if !ok || strings.TrimSpace(key) != "Xft.dpi" {
			continue
		}
		dpi, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err == nil && dpi >= 48 && dpi <= 480 {
			return dpi
		}
	}
	return 0
}
