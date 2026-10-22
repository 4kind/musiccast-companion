// Command musiccast-companion ist ein kleiner LAN-Proxy für einen Yamaha-
// Netzwerk-Receiver plus Jellyfin-Musiksuche. Er liefert static/ aus und reicht
// alle Geräteaufrufe weiter; der Browser spricht nie direkt mit den Geräten.
//
// Aufteilung im Paket:
//
//	main.go      Konfiguration, HTTP-Client und Routen
//	yamaha.go    MusicCast/Yamaha Extended Control (YXC)
//	jellyfin.go  Jellyfin-API und Titel-Auflösung
//	upnp.go      Wiedergabe über UPnP AVTransport
package main

import (
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Feste Vorgaben. Geräteadressen und API-Key stehen bewusst nicht hier: Sie
// kommen ausschließlich aus der Umgebung (siehe README.md), damit im Repo keine
// echten Werte landen. Ein anderer Receiver oder Jellyfin-Server braucht so
// keine Neukompilierung.
const (
	// Port des Webservers, wenn PORT nicht gesetzt ist.
	defaultPort = 8080

	// Obergrenze der Lautstärke laut getStatus → max_volume. Der maßgebliche
	// Bereich gelangt zur Laufzeit über /api/status ans Frontend; diese
	// Konstante schützt das Backend nur gegen offensichtlichen Unsinn.
	yamahaMaxVolume = 161

	upstreamTimeout = 5 * time.Second
)

// envOr liest eine Umgebungsvariable und fällt auf einen Vorgabewert zurück.
// Leere oder nur aus Leerzeichen bestehende Werte gelten als "nicht gesetzt".
func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// Laufzeit-Konfiguration, ausschließlich aus der Umgebung. yamahaIP und
// jellyfinURL existieren bewusst nur im Backend: Das Frontend spricht nie
// direkt mit den Geräten (auch Cover-Art läuft über /api/albumart), deshalb
// gibt es keine CORS- und kein Mixed-Content-Problem.
var (
	yamahaIP       = envOr("YAMAHA_IP", "")
	yamahaBase     = "http://" + yamahaIP + "/YamahaExtendedControl/v1/"
	jellyfinURL    = strings.TrimRight(envOr("JELLYFIN_URL", ""), "/")
	jellyfinAPIKey = envOr("JELLYFIN_API_KEY", "")

	// Nur der Port ist einstellbar, die Bindung bleibt bei allen Schnittstellen
	// (":<port>"): Die App ist eine LAN-App, localhost allein wäre nutzlos.
	listenAddr = ":" + strconv.Itoa(envPort("PORT", defaultPort))
)

// checkConfig bricht mit einer klaren Meldung ab, wenn eine Pflichtvariable
// fehlt — alle fehlenden auf einmal, statt den Dienst dreimal neu zu starten.
// Ohne Geräteadresse würde die App sonst laufen und jede Anfrage mit 502 enden.
func checkConfig() {
	var missing []string
	for _, item := range []struct {
		key   string
		value string
	}{
		{"YAMAHA_IP", yamahaIP},
		{"JELLYFIN_URL", jellyfinURL},
		{"JELLYFIN_API_KEY", jellyfinAPIKey},
	} {
		if item.value == "" {
			missing = append(missing, item.key)
		}
	}
	if len(missing) == 0 {
		return
	}
	log.Fatalf("Fehlende Konfiguration: %s. Bitte per Umgebungsvariable setzen, z. B.\n"+
		"  YAMAHA_IP=<receiver-ip> JELLYFIN_URL=<jellyfin-url> JELLYFIN_API_KEY=<key> ./musiccast-companion\n"+
		"Hinweise dazu im README.md, Abschnitt „Konfiguration“.",
		strings.Join(missing, ", "))
}

// envPort liest den Port aus der Umgebung und prüft ihn. Ein Tippfehler soll
// nicht still auf dem Vorgabeport landen — sonst sucht man den Fehler im
// falschen Netz. Lieber sofort mit einer klaren Meldung abbrechen.
func envPort(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		log.Fatalf("Ungültiger %s-Wert %q: erwartet wird eine Portnummer von 1 bis 65535 (z. B. %d).", key, value, fallback)
	}
	return port
}

// Ein gemeinsamer Client mit Timeout statt des Default-Clients ohne Timeout.
var httpClient = &http.Client{Timeout: upstreamTimeout}

// lanURLs liefert die Adressen, unter denen der Server im LAN erreichbar ist.
// listenAddr lauscht auf allen Schnittstellen, deshalb sind das alle nicht-
// lokalen IPs der Netzwerkkarten. Die LAN-IP ist die interessante: localhost
// hilft nur auf diesem Rechner, Handy und Tablet brauchen die andere.
func lanURLs(addr string) []string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		log.Printf("Netzwerkadressen nicht lesbar: %v", err)
		return nil
	}

	var v4, v6 []string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		url := "http://" + net.JoinHostPort(ipnet.IP.String(), port) + "/"
		if ipnet.IP.To4() != nil {
			v4 = append(v4, url)
		} else {
			v6 = append(v6, url)
		}
	}
	sort.Strings(v4)
	sort.Strings(v6)

	// IPv4 zuerst: im Heimnetz der Normalfall und leichter zu tippen.
	return append(v4, v6...)
}

// maskSecret zeigt nur die letzten vier Zeichen — genug, um den richtigen Key im
// Log zu erkennen, ohne ihn vollständig ins Journal zu schreiben.
func maskSecret(secret string) string {
	if len(secret) <= 4 {
		return "…"
	}
	return "…" + secret[len(secret)-4:]
}

func main() {
	checkConfig()

	mux := http.NewServeMux()

	// Statische Dateien (Frontend) aus dem Ordner "static" ausliefern.
	mux.Handle("/", http.FileServer(http.Dir("./static")))

	mux.HandleFunc("GET /api/status", handleStatus)
	mux.HandleFunc("GET /api/inputs", handleInputs)
	mux.HandleFunc("GET /api/playinfo", handlePlayInfo)
	mux.HandleFunc("GET /api/albumart", handleAlbumArt)
	mux.HandleFunc("POST /api/play", handlePlay)
	mux.HandleFunc("POST /api/stop", handleStop)
	mux.HandleFunc("POST /api/next", handleNext)
	mux.HandleFunc("POST /api/prev", handlePrev)
	mux.HandleFunc("POST /api/input", handleInput)
	mux.HandleFunc("POST /api/volume", handleVolume)
	mux.HandleFunc("GET /api/jellyfin/search", handleJellyfinSearch)
	mux.HandleFunc("GET /api/jellyfin/tracks", handleJellyfinTracks)
	mux.HandleFunc("GET /api/jellyfin/views", handleJellyfinViews)
	mux.HandleFunc("GET /api/jellyfin/browse", handleJellyfinBrowse)
	mux.HandleFunc("GET /api/jellyfin/image", handleJellyfinImage)
	mux.HandleFunc("POST /api/jellyfin/play", handleJellyfinPlay)
	mux.HandleFunc("POST /api/jellyfin/random", handleJellyfinRandom)

	log.Printf("🚀 Go-Backend läuft auf http://localhost%s", listenAddr)
	if urls := lanURLs(listenAddr); len(urls) > 0 {
		log.Printf("   im LAN erreichbar unter %s", urls[0])
		for _, url := range urls[1:] {
			log.Printf("                          oder %s", url)
		}
	} else {
		log.Printf("   im LAN erreichbar unter http://<IP-dieses-Rechners>%s", listenAddr)
	}
	// Konfiguration einmal beim Start ausgeben: Wenn im Dienst eine
	// Environment-Zeile fehlt, sieht man hier sofort, was wirklich gilt.
	log.Printf("   Yamaha %s · Jellyfin %s · API-Key %s", yamahaIP, jellyfinURL, maskSecret(jellyfinAPIKey))

	// Der Renderer schaltet selbst weiter; diese Schleife legt den jeweils
	// nächsten Titel nach (siehe autoAdvanceLoop in jellyfin.go).
	go autoAdvanceLoop()

	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatalf("Server fehlgeschlagen: %v", err)
	}
}
