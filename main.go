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

// Fixed defaults. Device addresses and API keys are intentionally not here:
// they come exclusively from the environment (see README.md), so no real values
// end up in the repo. A different receiver or Jellyfin server thus requires
// no recompilation.
const (
	// Port of the web server if PORT is not set.
	defaultPort = 8080

	// Volume upper limit according to getStatus -> max_volume. The actual
	// range is provided to the frontend at runtime via /api/status; this
	// constant only protects the backend against obvious nonsense.
	yamahaMaxVolume = 161

	upstreamTimeout = 5 * time.Second

	// Cover art is a large body (measured ~680 KB) and the device occasionally
	// delivers it slowly. The general timeout applies to API JSON and is too
	// tight here — it would break in the middle of the image (context deadline exceeded).
	albumArtTimeout = 20 * time.Second
)

// envOr reads an environment variable and falls back to a default value.
// Empty or whitespace-only values are considered "not set".
func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// Runtime configuration, exclusively from the environment. yamahaIP and
// jellyfinURL intentionally exist only in the backend: The frontend never
// communicates directly with the devices (even cover art goes through /api/albumart),
// so there is no CORS or mixed content problem.
var (
	yamahaIP       = envOr("YAMAHA_IP", "")
	yamahaBase     = "http://" + yamahaIP + "/YamahaExtendedControl/v1/"
	jellyfinURL    = strings.TrimRight(envOr("JELLYFIN_URL", ""), "/")
	jellyfinAPIKey = envOr("JELLYFIN_API_KEY", "")

	// Runtime IP can be changed via cookie or API
	currentYamahaIP   = yamahaIP
	currentYamahaIPMx sync.RWMutex

	// Only the port is configurable, binding remains on all interfaces
	// (":<port>"): The app is a LAN app, localhost alone would be useless.
	listenAddr = ":" + strconv.Itoa(envPort("PORT", defaultPort))
)

// getCurrentYamahaIP returns the currently selected Yamaha IP
// (can be changed at runtime via API)
func getCurrentYamahaIP() string {
	currentYamahaIPMx.RLock()
	defer currentYamahaIPMx.RUnlock()
	return currentYamahaIP
}

// setCurrentYamahaIP sets the currently selected Yamaha IP
func setCurrentYamahaIP(ip string) {
	currentYamahaIPMx.Lock()
	defer currentYamahaIPMx.Unlock()
	currentYamahaIP = ip
	// Also update the base URL
	yamahaBase = "http://" + ip + "/YamahaExtendedControl/v1/"
}

// currentYamahaBase returns the base URL of the currently selected device
// ("http://<ip>/YamahaExtendedControl/v1/"). The selected IP can change at
// runtime via the API, so every caller must read it through this function.
func currentYamahaBase() string {
	currentYamahaIPMx.RLock()
	defer currentYamahaIPMx.RUnlock()
	return yamahaBase
}

// checkConfig aborts with a clear message if a required variable is missing
// — all missing at once, instead of restarting the service three times.
// YAMAHA_IP is optional: If not set, the app tries to find devices via UPnP discovery.
func checkConfig() {
	var missing []string
	for _, item := range []struct {
		key      string
		value    string
		optional bool
	}{
		{"YAMAHA_IP", yamahaIP, true}, // Optional - found via discovery
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
	log.Fatalf("Missing configuration: %s. Please set via environment variables, e.g.\n  JELLYFIN_URL=<jellyfin-url> JELLYFIN_API_KEY=<key> ./musiccast-companion\n  See README.md, section Configuration for details.",
		strings.Join(missing, ", "))
}

// envPort reads the port from the environment and validates it. A typo should
// not silently fall back to the default port — otherwise you look for the error
// in the wrong network. Better to abort immediately with a clear message.
func envPort(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		log.Fatalf("Invalid %s value %q: expected is a port number from 1 to 65535 (e.g., %d).", key, value, fallback)
	}
	return port
}

// A common client with timeout instead of the default client without timeout.
var httpClient = &http.Client{Timeout: upstreamTimeout}

// Separate client for cover art: the same timeout applies to the entire body,
// so it is much more generous here than for JSON calls.
var albumArtClient = &http.Client{Timeout: albumArtTimeout}

// MusicCast device cache and mutex
type MusicCastDevice struct {
	FriendlyName string `json:"friendly_name"`
	ModelName    string `json:"model_name"`
	IPAddress    string `json:"ip_address"`
	UDN          string `json:"udn"`
}

var (
	musicCastDevices   []MusicCastDevice
	musicCastDevicesMx sync.RWMutex

	// last discovery timestamp
	lastDiscoveryTime time.Time
	lastDiscoveryMx   sync.Mutex
)

// discoverMusicCastDevices finds MusicCast devices on the local network.
// SSDP/UPnP delivers candidate addresses (and friendly names), the subnet
// scan contributes addresses directly. Every candidate is then verified with
// a strict getDeviceInfo probe, so only real MusicCast devices survive.
// Results are cached for 30 seconds.
func discoverMusicCastDevices() []MusicCastDevice {
	lastDiscoveryMx.Lock()
	defer lastDiscoveryMx.Unlock()

	// Cache for 30 seconds
	if time.Since(lastDiscoveryTime) < 30*time.Second {
		musicCastDevicesMx.RLock()
		defer musicCastDevicesMx.RUnlock()
		return musicCastDevices
	}

	// 1. SSDP discovery (fast, ~2-3 seconds, may fail silently)
	log.Println("Starting UPnP Discovery...")
	ssdpChan := make(chan []MusicCastDevice, 1)
	go func() {
		ssdpChan <- discoverViaSSDP()
	}()

	// 2. In parallel: scan the configured/local subnet
	log.Println("Starting direct IP check...")
	ipChan := make(chan []string, 1)
	go func() {
		ipChan <- discoverCandidateIPs()
	}()

	ssdpDevices := <-ssdpChan
	log.Printf("UPnP: %d candidates", len(ssdpDevices))

	var scanIPs []string
	select {
	case scanIPs = <-ipChan:
		log.Printf("IP check: %d candidates", len(scanIPs))
	case <-time.After(30 * time.Second):
		log.Println("IP Discovery Timeout")
	}

	// 3. Merge candidates (SSDP first, so its friendly names win), dedupe
	// and verify each address strictly. SSDP answers come from any UPnP
	// device (routers included), so an unverified address must not reach
	// the device list.
	names := make(map[string]MusicCastDevice)
	seen := make(map[string]bool)
	var candidates []string
	for _, d := range append(append([]MusicCastDevice{}, ssdpDevices...), devicesFromIPs(scanIPs)...) {
		if d.IPAddress == "" || seen[d.IPAddress] {
			continue
		}
		seen[d.IPAddress] = true
		candidates = append(candidates, d.IPAddress)
		names[d.IPAddress] = d
	}

	uniqueDevices := verifyDevices(candidates, names)

	// Save in cache
	musicCastDevicesMx.Lock()
	musicCastDevices = uniqueDevices
	musicCastDevicesMx.Unlock()

	lastDiscoveryTime = time.Now()

	log.Printf("Total %d MusicCast devices found:", len(uniqueDevices))
	for _, d := range uniqueDevices {
		log.Printf("  - %s (%s) at %s", d.FriendlyName, d.ModelName, d.IPAddress)
	}

	// If no Yamaha IP is set and we found devices, use the first one as default
	if yamahaIP == "" && len(uniqueDevices) > 0 {
		setCurrentYamahaIP(uniqueDevices[0].IPAddress)
	}

	return uniqueDevices
}

// discoverViaSSDP searches for devices via SSDP/UPnP
func discoverViaSSDP() []MusicCastDevice {
	// Search multiple service types
	serviceTypes := []string{
		"urn:schemas-upnp-org:device:MediaRenderer:1",
		"urn:schemas-upnp-org:device:MediaServer:1",
		"urn:schemas-upnp-org:device:WANDevice:1",
		"urn:schemas-upnp-org:service:AVTransport:1",
		"urn:schemas-yamaha-com:device:MusicCast:1",
		"ssdp:all",
	}

	// UDP connection for SSDP
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		log.Printf("SSDP listen failed: %v", err)
		return nil
	}
	defer conn.Close()

	ssdpAddr := "239.255.255.250:1900"
	addr, err := net.ResolveUDPAddr("udp4", ssdpAddr)
	if err != nil {
		log.Printf("SSDP address cannot be resolved: %v", err)
		return nil
	}

	// Collect responses
	var devices []MusicCastDevice

	// Send M-SEARCH for each service type
	for _, st := range serviceTypes {
		mx := 2 // Max-Wait in seconds
		msg := fmt.Sprintf("M-SEARCH * HTTP/1.1\r\nHOST: %s\r\nMAN: \"ssdp:discover\"\r\nMX: %d\r\nST: %s\r\n\r\n",
			ssdpAddr, mx, st)

		_, err = conn.WriteTo([]byte(msg), addr)
		if err != nil {
			log.Printf("SSDP M-SEARCH failed for %s: %v", st, err)
			continue
		}

		// Wait for responses
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
				device := parseSSDPResponse(response)
				if device.IPAddress != "" {
					// Accept all devices, we filter later
					devices = append(devices, device)
				}
			}
		}
	}

	return devices
}

// parseSSDPResponse parses an SSDP response and extracts device information
func parseSSDPResponse(response string) MusicCastDevice {
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

	// Fetch the XML description from the LOCATION URL
	resp, err := httpClient.Get(location)
	if err != nil {
		log.Printf("Could not fetch device description: %v", err)
		return device
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return device
	}

	// Parse XML manually (simpler than XML parsing for these specific values)
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// Extract IP address from the URL
	if u, err := url.Parse(location); err == nil {
		if host, _, err := net.SplitHostPort(u.Host); err == nil {
			if ip := net.ParseIP(host); ip != nil {
				device.IPAddress = ip.String()
			} else {
				// Resolve hostname
				if ips, err := net.LookupIP(host); err == nil && len(ips) > 0 {
					device.IPAddress = ips[0].String()
				}
			}
		}
	}

	// Extract FriendlyName and ModelName
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

// lanURLs returns the addresses where the server is reachable in the LAN.
// listenAddr listens on all interfaces, so these are all non-local IPs of the
// network interfaces. The LAN IP is the interesting one: localhost only helps
// on this machine, phone and tablet need the other.
func lanURLs(addr string) []string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		log.Printf("Cannot read network addresses: %v", err)
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

	// IPv4 first: standard in home networks and easier to type.
	return append(v4, v6...)
}

// maskSecret shows only the last four characters — enough to recognize the
// correct key in the log without writing it completely to the journal.
func maskSecret(secret string) string {
	if len(secret) <= 4 {
		return "…"
	}
	return "…" + secret[len(secret)-4:]
}

// discoverCandidateIPs collects addresses worth probing: the configured
// YAMAHA_IP (if any) plus the local subnet and common private ranges. It does
// not verify anything by itself — verification happens in verifyDevices.
func discoverCandidateIPs() []string {
	var ips []string

	if yamahaIP != "" {
		ips = append(ips, yamahaIP)
	}

	// Ranges to scan: the local subnet first, then common private ranges as
	// fallback (deduplicated, the local prefix may repeat a fallback range).
	ranges := []string{}
	if p := getLocalNetworkPrefix(); p != "" {
		ranges = append(ranges, p)
	}
	ranges = append(ranges, "192.168.178.", "192.168.1.", "192.168.0.", "10.0.0.")

	seenRange := map[string]bool{}
	uniq := ranges[:0]
	for _, p := range ranges {
		if !seenRange[p] {
			seenRange[p] = true
			uniq = append(uniq, p)
		}
	}

	seenIP := map[string]bool{}
	for _, p := range uniq {
		for i := 1; i <= 254; i++ {
			ip := p + strconv.Itoa(i)
			if !seenIP[ip] {
				seenIP[ip] = true
				ips = append(ips, ip)
			}
		}
	}
	return ips
}

// verifyDevices probes candidate addresses in parallel and returns only real
// MusicCast devices. Verified addresses keep the metadata (friendly name, UDN)
// that SSDP provided, if available. Probing runs with 64 workers: a sequential
// walk with a 500 ms timeout per address would take minutes on a /24.
func verifyDevices(candidates []string, ssdpMeta map[string]MusicCastDevice) []MusicCastDevice {
	var mu sync.Mutex
	var wg sync.WaitGroup
	devices := make([]MusicCastDevice, 0, len(candidates))
	sem := make(chan struct{}, 64)

	for _, ip := range candidates {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if device := checkYamahaDeviceWithTimeout(ip, 500*time.Millisecond); device.IPAddress != "" {
				if meta, ok := ssdpMeta[ip]; ok && device.FriendlyName == device.ModelName {
					// Prefer the SSDP-provided name over the fallback name.
					device.FriendlyName = yamahaDisplayName(ip, firstNonEmpty(meta.FriendlyName, device.ModelName))
				}
				mu.Lock()
				devices = append(devices, device)
				mu.Unlock()
			}
		}(ip)
	}
	wg.Wait()

	sort.Slice(devices, func(i, j int) bool { return devices[i].IPAddress < devices[j].IPAddress })
	return devices
}

// devicesFromIPs wraps plain addresses as metadata-less candidates.
func devicesFromIPs(ips []string) []MusicCastDevice {
	out := make([]MusicCastDevice, 0, len(ips))
	for _, ip := range ips {
		out = append(out, MusicCastDevice{IPAddress: ip})
	}
	return out
}

// firstNonEmpty returns the first argument that is not empty.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// getLocalNetworkPrefix returns the network prefix (e.g., "192.168.178.") of the local machine
func getLocalNetworkPrefix() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}

	for _, addr := range addrs {
		// Check for IPv4 addresses
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				// Get the first three octets
				ipStr := ipnet.IP.String()
				if idx := strings.LastIndex(ipStr, "."); idx >= 0 {
					return ipStr[:idx+1]
				}
			}
		}
	}
	return ""
}

// checkYamahaDevice checks if a specific IP is a MusicCast device
func checkYamahaDevice(ip string) MusicCastDevice {
	return checkYamahaDeviceWithTimeout(ip, upstreamTimeout)
}

// checkYamahaDeviceWithTimeout probes a single address with a short timeout.
// A host counts as a MusicCast device only if system/getDeviceInfo returns
// valid Yamaha JSON (response_code 0 and a model name). This strict check
// keeps non-MusicCast responders (routers, NAS web servers) out of the list.
func checkYamahaDeviceWithTimeout(ip string, timeout time.Duration) MusicCastDevice {
	client := &http.Client{Timeout: timeout}

	resp, err := client.Get("http://" + ip + "/YamahaExtendedControl/v1/system/getDeviceInfo")
	if err != nil {
		return MusicCastDevice{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return MusicCastDevice{}
	}

	var info struct {
		ResponseCode int    `json:"response_code"`
		ModelName    string `json:"model_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return MusicCastDevice{}
	}
	if info.ResponseCode != 0 || info.ModelName == "" {
		return MusicCastDevice{}
	}

	return MusicCastDevice{
		FriendlyName: yamahaDisplayName(ip, info.ModelName),
		ModelName:    info.ModelName,
		IPAddress:    ip,
	}
}

// yamahaDisplayName resolves a human-friendly name for a device IP. Names are
// optionally mapped via YAMAHA_NAMES (e.g. "192.168.178.23=Kitchen"); without
// a mapping the MusicCast model name is shown.
func yamahaDisplayName(ip, modelName string) string {
	if mapping := os.Getenv("YAMAHA_NAMES"); mapping != "" {
		for _, pair := range strings.Split(mapping, ",") {
			kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
			if len(kv) == 2 && kv[0] == ip && kv[1] != "" {
				return kv[1]
			}
		}
	}
	return modelName
}

// handleYamahaDevices returns the list of found MusicCast devices
func handleYamahaDevices(w http.ResponseWriter, r *http.Request) {
	devices := discoverMusicCastDevices()

	musicCastDevicesMx.RLock()
	cachedDevices := musicCastDevices
	musicCastDevicesMx.RUnlock()

	// If discovery is running or no devices were found,
	// return the cached devices
	if len(devices) == 0 && len(cachedDevices) > 0 {
		devices = cachedDevices
	}

	w.Header().Set("Content-Type", "application/json")

	// Add the currently selected IP
	currentIP := getCurrentYamahaIP()

	type devicesResponse struct {
		Devices     []MusicCastDevice `json:"devices"`
		CurrentIP   string            `json:"current_ip"`
		CurrentName string            `json:"current_name"`
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

// handleYamahaSelect sets the selected Yamaha IP at runtime
func handleYamahaSelect(w http.ResponseWriter, r *http.Request) {
	ip := r.URL.Query().Get("ip")

	// Validation
	if ip == "" {
		http.Error(w, "IP address missing", http.StatusBadRequest)
		return
	}

	// Check if it's a valid MusicCast device
	devices := discoverMusicCastDevices()
	validIP := false
	for _, d := range devices {
		if d.IPAddress == ip {
			validIP = true
			break
		}
	}

	// The environment variable IP is also valid
	if ip == yamahaIP {
		validIP = true
	}

	if !validIP {
		http.Error(w, "Invalid IP address", http.StatusBadRequest)
		return
	}

	// Set the new IP at runtime
	setCurrentYamahaIP(ip)

	// Also store in a cookie for the UI
	http.SetCookie(w, &http.Cookie{
		Name:     "yamaha_ip",
		Value:    ip,
		Expires:  time.Now().Add(365 * 24 * time.Hour), // 1 year
		Path:     "/",
		HttpOnly: false, // so JavaScript can read it
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

	log.Printf("🚀 Go backend running at http://localhost%s", listenAddr)
	if urls := lanURLs(listenAddr); len(urls) > 0 {
		log.Printf("   reachable in LAN at %s", urls[0])
		for _, url := range urls[1:] {
			log.Printf("                          or %s", url)
		}
	} else {
		log.Printf("   reachable in LAN at http://<this-machine-IP>%s", listenAddr)
	}
	// Print configuration once at startup: If a service line is missing
	// in Environment=, you see what's actually active here.
	currentIP := getCurrentYamahaIP()
	if currentIP == "" {
		currentIP = "(will be searched via UPnP Discovery)"
	}
	log.Printf("   Yamaha %s · Jellyfin %s · API-Key %s", currentIP, jellyfinURL, maskSecret(jellyfinAPIKey))

	// The renderer advances automatically; this loop sets the next track
	// (see autoAdvanceLoop in jellyfin.go).
	go autoAdvanceLoop()

	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
