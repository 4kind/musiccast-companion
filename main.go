// Command musiccast-companion is a small LAN proxy for Yamaha MusicCast devices
// with Jellyfin music library support. It serves static/ and proxies all device
// calls; the browser never communicates directly with the devices.
//
// Package structure:
//
//	main.go      Configuration, HTTP client and routes
//	yamaha.go    MusicCast/Yamaha Extended Control (YXC)
//	jellyfin.go  Jellyfin API and track resolution
//	upnp.go      Playback via UPnP AVTransport
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
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

	// Cover-Art ist ein großer Body (gemessen ~680 KB) und das Gerät liefert ihn
	// gelegentlich zäh. Der allgemeine Timeout gilt für API-JSON und ist hier zu
	// knapp — er würde mitten im Bild abbrechen (context deadline exceeded).
	albumArtTimeout = 20 * time.Second
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

	// Laufzeit-IP kann durch Cookie oder API geändert werden
	currentYamahaIP   = yamahaIP
	currentYamahaIPMx sync.RWMutex

	// Nur der Port ist einstellbar, die Bindung bleibt bei allen Schnittstellen
	// (":<port>"): Die App ist eine LAN-App, localhost allein wäre nutzlos.
	listenAddr = ":" + strconv.Itoa(envPort("PORT", defaultPort))
)

// getCurrentYamahaIP liefert die aktuell ausgewählte Yamaha-IP
// (kann zur Laufzeit durch API geändert werden)
func getCurrentYamahaIP() string {
	currentYamahaIPMx.RLock()
	defer currentYamahaIPMx.RUnlock()
	return currentYamahaIP
}

// setCurrentYamahaIP setzt die aktuell ausgewählte Yamaha-IP
func setCurrentYamahaIP(ip string) {
	currentYamahaIPMx.Lock()
	defer currentYamahaIPMx.Unlock()
	currentYamahaIP = ip
	// Aktualisiere auch die Base-URL
	yamahaBase = "http://" + ip + "/YamahaExtendedControl/v1/"
}

// GetCurrentYamahaBase liefert die aktuelle Yamaha Base-URL
func GetCurrentYamahaBase() string {
	currentYamahaIPMx.RLock()
	defer currentYamahaIPMx.RUnlock()
	return yamahaBase
}

// GetCurrentYamahaIP liefert die aktuelle Yamaha-IP für externe Pakete
func GetCurrentYamahaIP() string {
	return getCurrentYamahaIP()
}

// checkConfig bricht mit einer klaren Meldung ab, wenn eine Pflichtvariable
// fehlt — alle fehlenden auf einmal, statt den Dienst dreimal neu zu starten.
// YAMAHA_IP ist optional: Falls nicht gesetzt, versucht die App Geräte per UPnP zu finden.
func checkConfig() {
	var missing []string
	for _, item := range []struct {
		key   string
		value string
		optional bool
	}{
		{"YAMAHA_IP", yamahaIP, true},  // Optional - wird per Discovery gefunden
		{"JELLYFIN_URL", jellyfinURL, false},
		{"JELLYFIN_API_KEY", jellyfinAPIKey, false},
	} {
		if !item.optional && item.value == "" {
			missing = append(missing, item.key)
		}
	}
	if len(missing) == 0 {
		return
	}
	log.Fatalf("Fehlende Konfiguration: %s. Bitte per Umgebungsvariable setzen, z. B.\n"+
		"  JELLYFIN_URL=<jellyfin-url> JELLYFIN_API_KEY=<key> ./musiccast-companion\n"+
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

// Eigener Client für Cover-Art: derselbe Timeout gilt für den gesamten Body,
// deshalb hier deutlich großzügiger als bei den JSON-Aufrufen.
var albumArtClient = &http.Client{Timeout: albumArtTimeout}

// MusicCast-Geräte-Cache und Mutex
type MusicCastDevice struct {
	FriendlyName string `json:"friendly_name"`
	ModelName    string `json:"model_name"`
	IPAddress    string `json:"ip_address"`
	UDN          string `json:"udn"`
}

var (
	musicCastDevices   []MusicCastDevice
	musicCastDevicesMx sync.RWMutex
	
	// letzten Discovery-Zeitpunkt
	lastDiscoveryTime time.Time
	lastDiscoveryMx   sync.Mutex
)

// discoverMusicCastDevices durchsucht das lokale Netzwerk nach MusicCast-Geräten
// via SSDP (UPnP Discovery) und direkten IP-Checks.
// Gesamt-Timeout: 5 Sekunden
func discoverMusicCastDevices() []MusicCastDevice {
	lastDiscoveryMx.Lock()
	defer lastDiscoveryMx.Unlock()
	
	// Cache für 30 Sekunden
	if time.Since(lastDiscoveryTime) < 30*time.Second {
		musicCastDevicesMx.RLock()
		defer musicCastDevicesMx.RUnlock()
		return musicCastDevices
	}
	
	var allDevices []MusicCastDevice
	
	// 1. Versuche SSDP Discovery (schnell, ~2-3 Sekunden)
	log.Println("Starte UPnP Discovery...")
	ssdpChan := make(chan []MusicCastDevice, 1)
	go func() {
		devices := discoverViaSSDP()
		ssdpChan <- devices
	}()
	
	// 2. Parallel: Versuche direkte IP-Prüfung (nur bekannte IP aus Umgebung)
	log.Println("Starte direkte IP-Prüfung...")
	ipChan := make(chan []MusicCastDevice, 1)
	go func() {
		devices := discoverMusicCastByIP()
		ipChan <- devices
	}()
	
	// Warte maximal 4 Sekunden auf beide
	select {
	case devices := <-ssdpChan:
		allDevices = append(allDevices, devices...)
		log.Printf("UPnP: %d Geräte gefunden", len(devices))
	case <-time.After(4 * time.Second):
		log.Println("UPnP Discovery Timeout")
	}
	
	select {
	case devices := <-ipChan:
		allDevices = append(allDevices, devices...)
		log.Printf("IP-Check: %d Geräte gefunden", len(devices))
	case <-time.After(1 * time.Second):
		log.Println("IP Discovery Timeout")
	}
	
	// 3. Filtere doppelte Geräte (gleiche IP)
	seen := make(map[string]bool)
	var uniqueDevices []MusicCastDevice
	for _, d := range allDevices {
		if !seen[d.IPAddress] && d.IPAddress != "" {
			seen[d.IPAddress] = true
			uniqueDevices = append(uniqueDevices, d)
		}
	}
	
	// Speichere im Cache
	musicCastDevicesMx.Lock()
	musicCastDevices = uniqueDevices
	musicCastDevicesMx.Unlock()
	
	lastDiscoveryTime = time.Now()
	
	log.Printf("Insgesamt %d MusicCast-Geräte gefunden:", len(uniqueDevices))
	for _, d := range uniqueDevices {
		log.Printf("  - %s (%s) bei %s", d.FriendlyName, d.ModelName, d.IPAddress)
	}
	
	return uniqueDevices
}

// discoverViaSSDP durchsucht via SSDP/UPnP nach Geräten
func discoverViaSSDP() []MusicCastDevice {
	// Mehrere Service Types durchsuchen
	serviceTypes := []string{
		"urn:schemas-upnp-org:device:MediaRenderer:1",
		"urn:schemas-upnp-org:device:MediaServer:1",
		"urn:schemas-upnp-org:device:WANDevice:1",
		"urn:schemas-upnp-org:service:AVTransport:1",
		"urn:schemas-yamaha-com:device:MusicCast:1",
		"ssdp:all",
	}
	
	// UDP-Verbindung für SSDP
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		log.Printf("SSDP Listen fehlgeschlagen: %v", err)
		return nil
	}
	defer conn.Close()
	
	ssdpAddr := "239.255.255.250:1900"
	addr, err := net.ResolveUDPAddr("udp4", ssdpAddr)
	if err != nil {
		log.Printf("SSDP Adresse kann nicht resolved werden: %v", err)
		return nil
	}
	
	// Antworten sammeln
	var devices []MusicCastDevice
	
	// Für jeden Service Type eine M-SEARCH senden
	for _, st := range serviceTypes {
		mx := 2 // Max-Wait in Sekunden
		msg := fmt.Sprintf("M-SEARCH * HTTP/1.1\r\nHOST: %s\r\nMAN: \"ssdp:discover\"\r\nMX: %d\r\nST: %s\r\n\r\n", 
			ssdpAddr, mx, st)
		
		_, err = conn.WriteTo([]byte(msg), addr)
		if err != nil {
			log.Printf("SSDP M-SEARCH fehlgeschlagen für %s: %v", st, err)
			continue
		}
		
		// Warte auf Antworten
		buf := make([]byte, 8192)
		deadline := time.Now().Add(time.Duration(mx+1) * time.Second)
		
		for time.Now().Before(deadline) {
			conn.SetReadDeadline(deadline)
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				break
			}
			
			response := string(buf[:n])
			if strings.HasPrefix(response, "HTTP/1.1 200 OK") {
				device := parseSSDPResponse(response, n)
				if device.IPAddress != "" {
					// Akzeptiere alle Geräte, wir filtern später
					devices = append(devices, device)
				}
			}
		}
	}
	
	return devices
}

// isMusicCastDevice prüft, ob ein Gerät ein MusicCast-Gerät ist
func isMusicCastDevice(device MusicCastDevice) bool {
	// Prüfe ob es ein Yamaha-Gerät ist
	lowerName := strings.ToLower(device.FriendlyName)
	lowerModel := strings.ToLower(device.ModelName)
	
	// MusicCast-Geräte haben typischerweise "MusicCast" oder "Yamaha" im Namen
	if strings.Contains(lowerName, "musiccast") || 
	   strings.Contains(lowerModel, "musiccast") ||
	   strings.Contains(lowerName, "yamaha") || 
	   strings.Contains(lowerModel, "yamaha") {
		return true
	}
	
	// Auch prüfen, ob die Beschreibung Yamaha-spezifische URLs enthält
	// Wir können das nicht hier prüfen, aber wir vertrauen den SSDP-Antworten
	// und filtern später im Frontend
	return true
}

// parseSSDPResponse parst eine SSDP-Antwort und extrahiert die Geräte-Informationen
func parseSSDPResponse(response string, length int) MusicCastDevice {
	var device MusicCastDevice
	
	lines := strings.Split(response, "\r\n")
	var location string
	
	for _, line := range lines {
		if strings.HasPrefix(line, "LOCATION: ") {
			location = strings.TrimPrefix(line, "LOCATION: ")
			break
		}
	}
	
	if location == "" {
		return device
	}
	
	// Hole die XML-Beschreibung von der LOCATION-URL
	resp, err := httpClient.Get(location)
	if err != nil {
		log.Printf("Konnte Gerätebeschreibung nicht abrufen: %v", err)
		return device
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return device
	}
	
	// Parse XML manuell (einfacher als XML-Parsing für diese spezifischen Werte)
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	
	// Extrahiere IP-Adresse aus der URL
	if u, err := url.Parse(location); err == nil {
		if host, _, err := net.SplitHostPort(u.Host); err == nil {
			if ip := net.ParseIP(host); ip != nil {
				device.IPAddress = ip.String()
			} else {
				// Hostname auflösen
				if ips, err := net.LookupIP(host); err == nil && len(ips) > 0 {
					device.IPAddress = ips[0].String()
				}
			}
		}
	}
	
	// Extrahiere FriendlyName und ModelName
	if strings.Contains(bodyStr, "<friendlyName>") {
		start := strings.Index(bodyStr, "<friendlyName>") + len("<friendlyName>")
		end := strings.Index(bodyStr[start:], "</friendlyName>")
		if end > 0 {
			device.FriendlyName = strings.TrimSpace(bodyStr[start : start+end])
		}
	}
	
	if strings.Contains(bodyStr, "<modelName>") {
		start := strings.Index(bodyStr, "<modelName>") + len("<modelName>")
		end := strings.Index(bodyStr[start:], "</modelName>")
		if end > 0 {
			device.ModelName = strings.TrimSpace(bodyStr[start : start+end])
		}
	}
	
	if strings.Contains(bodyStr, "<UDN>") {
		start := strings.Index(bodyStr, "<UDN>") + len("<UDN>")
		end := strings.Index(bodyStr[start:], "</UDN>")
		if end > 0 {
			device.UDN = strings.TrimSpace(bodyStr[start : start+end])
		}
	}
	
	return device
}

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

// discoverMusicCastByIP prüft die bekannte IP aus der Umgebung auf MusicCast-Geräte
// Dies ist ein Fallback, falls SSDP nicht funktioniert
func discoverMusicCastByIP() []MusicCastDevice {
	var devices []MusicCastDevice
	
	// Versuche, die bekannte IP aus der Umgebung zu prüfen
	if yamahaIP != "" {
		if device := checkYamahaDeviceWithTimeout(yamahaIP, 500*time.Millisecond); device.IPAddress != "" {
			devices = append(devices, device)
		}
	}
	
	return devices
}

// checkYamahaDevice prüft, ob eine bestimmte IP ein MusicCast-Gerät ist
func checkYamahaDevice(ip string) MusicCastDevice {
	return checkYamahaDeviceWithTimeout(ip, upstreamTimeout)
}

// checkYamahaDeviceWithTimeout prüft mit einem bestimmten Timeout
func checkYamahaDeviceWithTimeout(ip string, timeout time.Duration) MusicCastDevice {
	var device MusicCastDevice
	device.IPAddress = ip
	
	// Eigenen Client mit kurzem Timeout erstellen
	client := &http.Client{Timeout: timeout}
	
	// Versuche, die Yamaha Extended Control API zu erreichen
	url := fmt.Sprintf("http://%s/YamahaExtendedControl/v1/system/getFeatures", ip)
	
	resp, err := client.Get(url)
	if err != nil {
		return MusicCastDevice{}
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return MusicCastDevice{}
	}
	
	// Wenn wir hier sind, ist es ein MusicCast-Gerät
	device.FriendlyName = ip
	device.ModelName = "MusicCast"
	
	// Versuche, System-Info zu holen
	infoURL := fmt.Sprintf("http://%s/YamahaExtendedControl/v1/system/getStatus", ip)
	infoResp, err := client.Get(infoURL)
	if err == nil && infoResp.StatusCode == http.StatusOK {
		defer infoResp.Body.Close()
		body, _ := io.ReadAll(infoResp.Body)
		
		// Parse JSON manuell für den Modellnamen
		bodyStr := string(body)
		if strings.Contains(bodyStr, "model_name") {
			start := strings.Index(bodyStr, "\"model_name\":\"") + len("\"model_name\":\"")
			end := strings.Index(bodyStr[start:], "\"")
			if end > 0 {
				device.ModelName = bodyStr[start : start+end]
			}
		}
		if strings.Contains(bodyStr, "friendly_name") {
			start := strings.Index(bodyStr, "\"friendly_name\":\"") + len("\"friendly_name\":\"")
			end := strings.Index(bodyStr[start:], "\"")
			if end > 0 {
				device.FriendlyName = bodyStr[start : start+end]
			}
		}
	}
	
	return device
}

// handleYamahaDevices liefert die Liste der gefundenen MusicCast-Geräte
func handleYamahaDevices(w http.ResponseWriter, r *http.Request) {
	devices := discoverMusicCastDevices()
	
	musicCastDevicesMx.RLock()
	cachedDevices := musicCastDevices
	musicCastDevicesMx.RUnlock()
	
	// Wenn Discovery gerade läuft oder keine Geräte gefunden wurden, 
	// gib die gecachten Geräte zurück
	if len(devices) == 0 && len(cachedDevices) > 0 {
		devices = cachedDevices
	}
	
	w.Header().Set("Content-Type", "application/json")
	
	// Füge die aktuell ausgewählte IP hinzu
	currentIP := getCurrentYamahaIP()
	
	type devicesResponse struct {
		Devices     []MusicCastDevice `json:"devices"`
		CurrentIP   string             `json:"current_ip"`
		CurrentName string             `json:"current_name"`
	}
	
	currentName := ""
	for _, d := range devices {
		if d.IPAddress == currentIP {
			currentName = d.FriendlyName
			break
		}
	}
	
	if currentName == "" && currentIP != "" {
		currentName = currentIP
	}
	
	resp := devicesResponse{
		Devices:     devices,
		CurrentIP:   currentIP,
		CurrentName: currentName,
	}
	
	json.NewEncoder(w).Encode(resp)
}

// handleYamahaSelect setzt die ausgewählte Yamaha-IP zur Laufzeit
func handleYamahaSelect(w http.ResponseWriter, r *http.Request) {
	ip := r.URL.Query().Get("ip")
	
	// Validierung
	if ip == "" {
		http.Error(w, "IP-Adresse fehlt", http.StatusBadRequest)
		return
	}
	
	// Überprüfe ob es ein gültiges MusicCast-Gerät ist
	devices := discoverMusicCastDevices()
	validIP := false
	for _, d := range devices {
		if d.IPAddress == ip {
			validIP = true
			break
		}
	}
	
	// Auch die Umgebungsvariable-IP ist gültig
	if ip == yamahaIP {
		validIP = true
	}
	
	if !validIP {
		http.Error(w, "Ungültige IP-Adresse", http.StatusBadRequest)
		return
	}
	
	// Setze die neue IP zur Laufzeit
	setCurrentYamahaIP(ip)
	
	// Speichere auch in einem Cookie für die UI
	http.SetCookie(w, &http.Cookie{
		Name:     "yamaha_ip",
		Value:    ip,
		Expires:  time.Now().Add(365 * 24 * time.Hour), // 1 Jahr
		Path:     "/",
		HttpOnly: false, // damit JavaScript es lesen kann
	})
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"selected_ip": ip, "status": "ok"})
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
	mux.HandleFunc("GET /api/yamaha/devices", handleYamahaDevices)
	mux.HandleFunc("POST /api/yamaha/select", handleYamahaSelect)
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
	currentIP := getCurrentYamahaIP()
	if currentIP == "" {
		currentIP = "(wird per UPnP Discovery gesucht)"
	}
	log.Printf("   Yamaha %s · Jellyfin %s · API-Key %s", currentIP, jellyfinURL, maskSecret(jellyfinAPIKey))

	// Der Renderer schaltet selbst weiter; diese Schleife legt den jeweils
	// nächsten Titel nach (siehe autoAdvanceLoop in jellyfin.go).
	go autoAdvanceLoop()

	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatalf("Server fehlgeschlagen: %v", err)
	}
}
