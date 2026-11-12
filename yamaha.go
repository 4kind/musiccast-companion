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

// FeaturesResponse is the excerpt from system/getFeatures that we need.
type FeaturesResponse struct {
	ResponseCode int `json:"response_code"`
	System       struct {
		InputList []struct {
			ID string `json:"id"`
		} `json:"input_list"`
	} `json:"system"`
}

// hiddenInputs are inputs offered by the device that we don't want to show.
var hiddenInputs = map[string]bool{
	"napster":  true,
	"spotify":  true,
	"juke":     true,
	"qobuz":    true,
	"tidal":    true,
	"deezer":   true, // was missing and thus appeared in the UI
	"coaxial1": true,
	"coaxial2": true,
	"optical2": true,
	"line2":    true,
	"line3":    true,
	"mc_link":  true,
	"phono":    true,
	"line_cd":  true,
}

// encodeQuery builds a single URL-encoded query parameter. Important because
// the passed values (search terms, inputs) could otherwise break the URL.
func encodeQuery(key, value string) string {
	q := url.Values{}
	q.Set(key, value)
	return q.Encode()
}

// yamahaGet fetches a path under /YamahaExtendedControl/v1/. Even the
// "writing" actions (setVolume, setInput, setPlayback) are GET with query
// parameters according to the spec.
func yamahaGet(path string) ([]byte, error) {
	// Use the current IP at runtime
	currentBase := currentYamahaBase()
	resp, err := httpClient.Get(currentBase + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// proxyYamaha forwards a Yamaha response unchanged.
func proxyYamaha(w http.ResponseWriter, path string) {
	body, err := yamahaGet(path)
	if err != nil {
		log.Printf("Yamaha call %q failed: %v", path, err)
		http.Error(w, "Yamaha not reachable", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(body); err != nil {
		log.Printf("Response from %q could not be forwarded: %v", path, err)
	}
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	proxyYamaha(w, "main/getStatus")
}

func handlePlay(w http.ResponseWriter, r *http.Request) {
	// For "server", playback belongs to the UPnP renderer. A YXC Play would
	// start the device's own MusicCast queue there (observed: wrong track, renderer
	// remains STOPPED), so use UPnP Play here — and then set the next track again,
	// which /api/stop has removed.
	if hasJellyfinContext() {
		if err := upnpAVTransport("Play", "<InstanceID>0</InstanceID><Speed>1</Speed>"); err != nil {
			log.Printf("UPnP Play failed, trying YXC Play: %v", err)
		} else {
			primeNext()
			writeResponseCode(w)
			return
		}
	}
	proxyYamaha(w, "netusb/setPlayback?"+encodeQuery("playback", "play"))
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	// No more automatic continuation: The next track set in the renderer
	// would otherwise continue playing after a later Play.
	clearJellyfinNext()

	if currentInput() == "server" {
		// "server" playback has two owners: our renderer session (Jellyfin)
		// or the device's netusb queue (e.g., started via the MusicCast app).
		// Stopping the wrong layer is a silent no-op — observed: renderer
		// reported STOPPED while play_time kept advancing. So ask the
		// renderer first: a non-idle transport state means the UPnP session
		// owns playback; an idle one means the netusb queue does. Only a
		// query error keeps the UPnP path (the primary use case here is
		// our own Jellyfin session, and a YXC Stop would block the renderer
		// for ~4s with SOAP 501 on the next Play).
		state, err := upnpTransportState()
		if err != nil {
			log.Printf("GetTransportInfo failed, assuming UPnP session: %v", err)
		}
		if err != nil || (state != "" && state != "STOPPED" && state != "NO_MEDIA_PRESENT") {
			if err := upnpAVTransport("Stop", "<InstanceID>0</InstanceID>"); err != nil {
				log.Printf("UPnP Stop failed: %v", err)
			}
			writeResponseCode(w)
			return
		}
	}

	proxyYamaha(w, "netusb/setPlayback?"+encodeQuery("playback", "stop"))
}

// hasJellyfinContext indicates if the current playback was started by us (via UPnP).
// Only then does the Play button belong to the renderer.
func hasJellyfinContext() bool {
	playQueue.mu.Lock()
	defer playQueue.mu.Unlock()
	return len(playQueue.items) > 0 || playQueue.current.ID != ""
}

// writeResponseCode reports success in the format of YXC responses.
func writeResponseCode(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(`{"response_code":0}`)); err != nil {
		log.Printf("Response could not be written: %v", err)
	}
}

// currentInput returns the currently selected input of the zone. On error,
// the value remains empty; the caller then behaves as if no match was found.
func currentInput() string {
	body, err := yamahaGet("main/getStatus")
	if err != nil {
		log.Printf("Input could not be read: %v", err)
		return ""
	}
	var status struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal(body, &status); err != nil {
		log.Printf("Status unreadable: %v", err)
		return ""
	}
	return status.Input
}

func handlePlayInfo(w http.ResponseWriter, r *http.Request) {
	body, err := yamahaGet("netusb/getPlayInfo")
	if err != nil {
		log.Printf("Yamaha call %q failed: %v", "netusb/getPlayInfo", err)
		http.Error(w, "Yamaha not reachable", http.StatusBadGateway)
		return
	}

	// UseNumber preserves the exact number representation when re-encoding.
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var info map[string]any
	if err := decoder.Decode(&info); err != nil {
		// Unexpected format: better to forward unchanged than to break.
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
		return
	}

	fillUPnPTimes(info)

	// Custom field name: `shuffle` is already used in getPlayInfo (the shuffle
	// of the device's own queue) and must not be overwritten. jellyfin_shuffle
	// is the state of our random playback.
	info["jellyfin_shuffle"] = shuffleActive()

	// Cover replacement: If the reported track is verifiably from our
	// Jellyfin context, we provide album ID and image tag. The frontend uses
	// this cover only if the device does not provide its own albumart_url — which
	// sometimes remains empty after automatic continuation (see jellyfinCoverFor).
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
		log.Printf("PlayInfo could not be sent: %v", err)
	}
}

// fillUPnPTimes adds missing time information from the UPnP renderer. For
// Jellyfin playback, getPlayInfo provides play_time but total_time = 0
// (observed); the total duration is only known by the renderer, from the DIDL res@duration.
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
		log.Printf("UPnP GetPositionInfo failed: %v", err)
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

// jsonInt reads a number value from decoded JSON (UseNumber → json.Number).
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
		http.Error(w, "Parameter id missing", http.StatusBadRequest)
		return
	}
	if hiddenInputs[inputID] {
		http.Error(w, "Input not allowed", http.StatusBadRequest)
		return
	}
	// A source change ends the Jellyfin skip context and the next track set in the renderer.
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
		http.Error(w, "Parameter val must be a number", http.StatusBadRequest)
		return
	}
	if val < 0 || val > yamahaMaxVolume {
		http.Error(w, fmt.Sprintf("Volume must be between 0 and %d", yamahaMaxVolume), http.StatusBadRequest)
		return
	}
	proxyYamaha(w, "main/setVolume?"+encodeQuery("volume", strconv.Itoa(val)))
}

func handleInputs(w http.ResponseWriter, r *http.Request) {
	resp, err := httpClient.Get(currentYamahaBase() + "system/getFeatures")
	if err != nil {
		log.Printf("getFeatures failed: %v", err)
		http.Error(w, "Yamaha not reachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	var features FeaturesResponse
	if err := json.NewDecoder(resp.Body).Decode(&features); err != nil {
		log.Printf("getFeatures could not be read: %v", err)
		http.Error(w, "Yamaha response unreadable", http.StatusBadGateway)
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
		log.Printf("Input list could not be written: %v", err)
	}
}

// handleAlbumArt forwards cover art through the Go proxy so the browser never
// communicates directly with Yamaha. The path is the relative albumart_url value
// from getPlayInfo (e.g., "xxx/yyy.jpg"); absolute URLs are deliberately not allowed.
func handleAlbumArt(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	resp, err := albumArtClient.Get("http://" + getCurrentYamahaIP() + path)
	if err != nil {
		log.Printf("Cover art fetch %q failed: %v", path, err)
		http.Error(w, "Cover art not reachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Only forward successful images. Otherwise, a 400 from the device
	// (e.g., outdated AlbumART ID) would become an empty 200, which the browser
	// displays as a broken/empty cover and may cache.
	if resp.StatusCode != http.StatusOK {
		log.Printf("Cover art %q: device responded with status %d", path, resp.StatusCode)
		http.Error(w, "Cover art not available", http.StatusBadGateway)
		return
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		// If the browser aborts (page closed, new cover), this is not an error.
		if r.Context().Err() == nil {
			log.Printf("Cover art %q could not be forwarded: %v", path, err)
		}
	}
}
