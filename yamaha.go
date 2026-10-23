package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// FeaturesResponse ist der Ausschnitt aus system/getFeatures, den wir brauchen.
type FeaturesResponse struct {
	ResponseCode int `json:"response_code"`
	System       struct {
		InputList []struct {
			ID string `json:"id"`
		} `json:"input_list"`
	} `json:"system"`
}

// hiddenInputs sind Eingänge, die das Gerät anbietet, die wir aber nicht zeigen.
var hiddenInputs = map[string]bool{
	"napster":  true,
	"spotify":  true,
	"juke":     true,
	"qobuz":    true,
	"tidal":    true,
	"deezer":   true, // fehlte zuvor und rutschte dadurch bis in die UI durch
	"coaxial1": true,
	"coaxial2": true,
	"optical2": true,
	"line2":    true,
	"line3":    true,
	"mc_link":  true,
	"phono":    true,
	"line_cd":  true,
}

// encodeQuery baut einen einzelnen Query-Parameter URL-kodiert. Wichtig, weil
// die übergebenen Werte (Suchbegriffe, Eingänge) sonst die URL zerlegen können.
func encodeQuery(key, value string) string {
	q := url.Values{}
	q.Set(key, value)
	return q.Encode()
}

// yamahaGet holt einen Pfad unter /YamahaExtendedControl/v1/. Auch die
// "schreibenden" Aktionen (setVolume, setInput, setPlayback) sind laut Spec
// GET mit Query-Parametern.
func yamahaGet(path string) ([]byte, error) {
	resp, err := httpClient.Get(yamahaBase + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// proxyYamaha reicht eine Yamaha-Antwort unverändert weiter.
func proxyYamaha(w http.ResponseWriter, path string) {
	body, err := yamahaGet(path)
	if err != nil {
		log.Printf("Yamaha-Aufruf %q fehlgeschlagen: %v", path, err)
		http.Error(w, "Yamaha nicht erreichbar", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(body); err != nil {
		log.Printf("Antwort von %q konnte nicht weitergegeben werden: %v", path, err)
	}
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	proxyYamaha(w, "main/getStatus")
}

func handlePlay(w http.ResponseWriter, r *http.Request) {
	// Bei "server" gehört die Wiedergabe dem UPnP-Renderer. Ein YXC-Play startet
	// dort die geräteeigene MusicCast-Queue (gemessen: falscher Titel, Renderer
	// bleibt STOPPED), deshalb hier UPnP-Play — und danach den Folgetitel wieder
	// hinterlegen, den /api/stop entfernt hat.
	if hasJellyfinContext() {
		if err := upnpAVTransport("Play", "<InstanceID>0</InstanceID><Speed>1</Speed>"); err != nil {
			log.Printf("UPnP-Play fehlgeschlagen, versuche YXC-Play: %v", err)
		} else {
			primeNext()
			writeResponseCode(w)
			return
		}
	}
	proxyYamaha(w, "netusb/setPlayback?"+encodeQuery("playback", "play"))
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	// Kein automatisches Weiterschalten mehr: Der im Renderer hinterlegte
	// Folgetitel würde sonst nach einem späteren Play weiterlaufen.
	clearJellyfinNext()

	// Bei "server" ist die Wiedergabe die UPnP-Session des Renderers selbst. Ein
	// UPnP-Stop beendet sie sofort. Der YXC-Stop (netusb/setPlayback) sperrt den
	// Renderer dagegen für rund vier Sekunden, in denen das nächste Play mit SOAP
	// 501 scheitert (gemessen) — deshalb hier der direkte Weg.
	if currentInput() == "server" {
		if err := upnpAVTransport("Stop", "<InstanceID>0</InstanceID>"); err != nil {
			log.Printf("UPnP-Stop fehlgeschlagen: %v", err)
		}
		writeResponseCode(w)
		return
	}

	proxyYamaha(w, "netusb/setPlayback?"+encodeQuery("playback", "stop"))
}

// hasJellyfinContext sagt, ob die laufende Wiedergabe von uns (per UPnP)
// gestartet wurde. Nur dann gehört der Play-Knopf dem Renderer.
func hasJellyfinContext() bool {
	playQueue.mu.Lock()
	defer playQueue.mu.Unlock()
	return len(playQueue.items) > 0 || playQueue.current.ID != ""
}

// writeResponseCode meldet Erfolg im Format der YXC-Antworten.
func writeResponseCode(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(`{"response_code":0}`)); err != nil {
		log.Printf("Antwort konnte nicht geschrieben werden: %v", err)
	}
}

// currentInput liefert den gerade gewählten Eingang der Zone. Bei einem Fehler
// bleibt der Wert leer; der Aufrufer verhält sich dann wie ohne Treffer.
func currentInput() string {
	body, err := yamahaGet("main/getStatus")
	if err != nil {
		log.Printf("Eingang konnte nicht gelesen werden: %v", err)
		return ""
	}
	var status struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal(body, &status); err != nil {
		log.Printf("Status unlesbar: %v", err)
		return ""
	}
	return status.Input
}

func handlePlayInfo(w http.ResponseWriter, r *http.Request) {
	body, err := yamahaGet("netusb/getPlayInfo")
	if err != nil {
		log.Printf("Yamaha-Aufruf %q fehlgeschlagen: %v", "netusb/getPlayInfo", err)
		http.Error(w, "Yamaha nicht erreichbar", http.StatusBadGateway)
		return
	}

	// UseNumber erhält die exakte Zahlendarstellung beim erneuten Kodieren.
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var info map[string]any
	if err := decoder.Decode(&info); err != nil {
		// Unerwartetes Format: lieber unverändert durchreichen als zerlegen.
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
		return
	}

	fillUPnPTimes(info)

	// Eigener Feldname: `shuffle` ist bei getPlayInfo schon belegt (der Shuffle
	// der geräteeigenen Queue) und darf nicht überschrieben werden. jellyfin_shuffle
	// ist der Zustand unserer Zufallswiedergabe.
	info["jellyfin_shuffle"] = shuffleActive()

	// Cover-Ersatz: Läuft der gemeldete Titel nachweislich aus unserem
	// Jellyfin-Kontext, geben wir Album-ID und Image-Tag mit. Das Frontend nimmt
	// dieses Cover nur, wenn das Gerät keine eigene albumart_url liefert — die
	// bleibt nach dem automatischen Weiterschalten teils leer (siehe jellyfinCoverFor).
	if input, _ := info["input"].(string); input == "server" {
		if playback, _ := info["playback"].(string); playback != "stop" {
			running, _ := info["track"].(string)
			if albumID, imageTag := jellyfinCoverFor(running); albumID != "" {
				info["jellyfin_album_id"] = albumID
				info["jellyfin_image_tag"] = imageTag
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(info); err != nil {
		log.Printf("PlayInfo konnte nicht gesendet werden: %v", err)
	}
}

// fillUPnPTimes ergänzt fehlende Zeitangaben aus dem UPnP-Renderer. Für die
// Jellyfin-Wiedergabe liefert getPlayInfo zwar play_time, aber total_time = 0
// (gemessen); die Gesamtdauer kennt nur der Renderer, aus dem DIDL res@duration.
func fillUPnPTimes(info map[string]any) {
	if input, _ := info["input"].(string); input != "server" {
		return
	}
	if playback, _ := info["playback"].(string); playback == "stop" {
		return
	}
	if jsonInt(info["total_time"]) > 0 {
		return
	}

	duration, position, err := upnpPositionInfo()
	if err != nil {
		log.Printf("UPnP GetPositionInfo fehlgeschlagen: %v", err)
		return
	}
	if duration <= 0 {
		return
	}
	info["total_time"] = json.Number(strconv.FormatInt(duration, 10))
	if jsonInt(info["play_time"]) <= 0 && position > 0 {
		info["play_time"] = json.Number(strconv.FormatInt(position, 10))
	}
}

// jsonInt liest einen Zahlenwert aus dem dekodierten JSON (UseNumber → json.Number).
func jsonInt(value any) int64 {
	switch v := value.(type) {
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0
		}
		return n
	case float64:
		return int64(v)
	}
	return 0
}

func handleInput(w http.ResponseWriter, r *http.Request) {
	inputID := r.URL.Query().Get("id")
	if inputID == "" {
		http.Error(w, "Parameter id fehlt", http.StatusBadRequest)
		return
	}
	if hiddenInputs[inputID] {
		http.Error(w, "Eingang nicht erlaubt", http.StatusBadRequest)
		return
	}
	// Ein Quellenwechsel beendet den Jellyfin-Skip-Kontext und den im Renderer
	// hinterlegten Folgetitel.
	if inputID != "server" {
		clearJellyfinQueue()
		clearJellyfinNext()
	}
	proxyYamaha(w, "main/setInput?"+encodeQuery("input", inputID))
}

func handleVolume(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("val")
	val, err := strconv.Atoi(raw)
	if err != nil {
		http.Error(w, "Parameter val muss eine Zahl sein", http.StatusBadRequest)
		return
	}
	if val < 0 || val > yamahaMaxVolume {
		http.Error(w, fmt.Sprintf("Lautstärke muss zwischen 0 und %d liegen", yamahaMaxVolume), http.StatusBadRequest)
		return
	}
	proxyYamaha(w, "main/setVolume?"+encodeQuery("volume", strconv.Itoa(val)))
}

func handleInputs(w http.ResponseWriter, r *http.Request) {
	resp, err := httpClient.Get(yamahaBase + "system/getFeatures")
	if err != nil {
		log.Printf("getFeatures fehlgeschlagen: %v", err)
		http.Error(w, "Yamaha nicht erreichbar", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	var features FeaturesResponse
	if err := json.NewDecoder(resp.Body).Decode(&features); err != nil {
		log.Printf("getFeatures konnte nicht gelesen werden: %v", err)
		http.Error(w, "Yamaha-Antwort unlesbar", http.StatusBadGateway)
		return
	}

	inputIDs := make([]string, 0, len(features.System.InputList))
	for _, input := range features.System.InputList {
		if !hiddenInputs[input.ID] {
			inputIDs = append(inputIDs, input.ID)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(inputIDs); err != nil {
		log.Printf("Eingangsliste konnte nicht geschrieben werden: %v", err)
	}
}

// handleAlbumArt reicht Cover-Art durch den Go-Proxy, damit der Browser die
// Yamaha nie direkt anspricht. Der Pfad ist der relative albumart_url-Wert aus
// getPlayInfo (z. B. "xxx/yyy.jpg"); absolute URLs werden bewusst nicht erlaubt.
func handleAlbumArt(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
		http.Error(w, "Ungültiger Pfad", http.StatusBadRequest)
		return
	}

	resp, err := albumArtClient.Get("http://" + yamahaIP + path)
	if err != nil {
		log.Printf("Cover-Art-Abruf %q fehlgeschlagen: %v", path, err)
		http.Error(w, "Cover-Art nicht erreichbar", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Nur erfolgreiche Bilder weiterreichen. Sonst würde aus einem 400 des
	// Geräts (z. B. veraltete AlbumART-ID) ein leeres 200, das der Browser als
	// kaputtes/leeres Cover anzeigt und unter Umständen zwischenspeichert.
	if resp.StatusCode != http.StatusOK {
		log.Printf("Cover-Art %q: Gerät antwortet mit Status %d", path, resp.StatusCode)
		http.Error(w, "Cover-Art nicht verfügbar", http.StatusBadGateway)
		return
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		// Bricht der Browser ab (Seite geschlossen, neues Cover), ist das kein Fehler.
		if r.Context().Err() == nil {
			log.Printf("Cover-Art %q konnte nicht weitergegeben werden: %v", path, err)
		}
	}
}
