package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// jellyfinGet calls a Jellyfin path with the required Authorization header
// and returns the body. Non-200 responses are returned as errors.
func jellyfinGet(path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, jellyfinURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", fmt.Sprintf(`MediaBrowser Token="%s"`, jellyfinAPIKey))

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return body, fmt.Errorf("Jellyfin responded with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// jellyfinUserID returns the ID of the first Jellyfin user.
// Known simplification: There is no user selection, it blindly takes the
// first entry.
func jellyfinUserID() (string, error) {
	body, err := jellyfinGet("/Users")
	if err != nil {
		return "", err
	}

	var users []struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(body, &users); err != nil {
		return "", fmt.Errorf("Jellyfin user list unreadable: %w", err)
	}
	if len(users) == 0 {
		return "", fmt.Errorf("no Jellyfin users found")
	}
	return users[0].ID, nil
}

// handleJellyfinSearch returns hits separated by albums, artists and
// tracks. Separate queries instead of a common one: Tracks are much more numerous
// (Jellyfin ranks them first) and would displace albums from a shared limit.
// The separation also allows individual limits per type and the frontend a section display.
func handleJellyfinSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		http.Error(w, "Search term missing", http.StatusBadRequest)
		return
	}

	userID, err := jellyfinUserID()
	if err != nil {
		log.Printf("Jellyfin user fetch failed: %v", err)
		http.Error(w, "Jellyfin not reachable", http.StatusInternalServerError)
		return
	}

	albums, err := jellyfinSearchItems(userID, query, "MusicAlbum", 12)
	if err != nil {
		log.Printf("Jellyfin album search failed: %v", err)
		http.Error(w, "Jellyfin search failed", http.StatusInternalServerError)
		return
	}
	artists, err := jellyfinSearchItems(userID, query, "MusicArtist", 8)
	if err != nil {
		log.Printf("Jellyfin artist search failed: %v", err)
		http.Error(w, "Jellyfin search failed", http.StatusInternalServerError)
		return
	}
	tracks, err := jellyfinSearchItems(userID, query, "Audio", 25)
	if err != nil {
		log.Printf("Jellyfin track search failed: %v", err)
		http.Error(w, "Jellyfin search failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"albums":  albums,
		"artists": artists,
		"tracks":  tracks,
	}); err != nil {
		log.Printf("Jellyfin search result could not be written: %v", err)
	}
}

// jellyfinSearchItems returns the raw Jellyfin hits of a type filter.
func jellyfinSearchItems(userID, query, types string, limit int) ([]json.RawMessage, error) {
	q := url.Values{}
	q.Set("searchTerm", query)
	q.Set("IncludeItemTypes", types)
	q.Set("Recursive", "true")
	q.Set("Limit", strconv.Itoa(limit))
	return jellyfinQueryItems(userID, q)
}

// jellyfinQueryItems calls /Users/{id}/Items with arbitrary parameters and
// returns the raw hits. Never nil so JSON produces an array instead of null.
func jellyfinQueryItems(userID string, q url.Values) ([]json.RawMessage, error) {
	items, _, err := jellyfinPage("/Users/"+url.PathEscape(userID)+"/Items", q)
	return items, err
}

// jellyfinPage fetches a Jellyfin list and returns hits plus total count
// (TotalRecordCount). The total count is needed for browsing to know when
// to stop loading more. Never nil so JSON produces [] instead of null.
func jellyfinPage(path string, q url.Values) ([]json.RawMessage, int, error) {
	body, err := jellyfinGet(path + "?" + q.Encode())
	if err != nil {
		return nil, 0, err
	}
	var res struct {
		Items            []json.RawMessage `json:"Items"`
		TotalRecordCount int               `json:"TotalRecordCount"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, 0, fmt.Errorf("Jellyfin response unreadable: %w", err)
	}
	if res.Items == nil {
		res.Items = []json.RawMessage{}
	}
	return res.Items, res.TotalRecordCount, nil
}

// parseIntParam reads a number parameter. Unreadable values fall back to fallback,
// everything outside min..max is limited to that.
func parseIntParam(value string, fallback, min, max int) int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fallback
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

// handleJellyfinTracks returns the tracks of an album/artist for expanding
// in the results list.
func handleJellyfinTracks(w http.ResponseWriter, r *http.Request) {
	parentID := r.URL.Query().Get("parent_id")
	if parentID == "" {
		http.Error(w, "Parameter parent_id missing", http.StatusBadRequest)
		return
	}

	userID, err := jellyfinUserID()
	if err != nil {
		log.Printf("Jellyfin user fetch failed: %v", err)
		http.Error(w, "Jellyfin not reachable", http.StatusInternalServerError)
		return
	}

	tracks, err := jellyfinAudioChildren(userID, parentID, 300)
	if err != nil {
		log.Printf("Track list for %q failed: %v", parentID, err)
		http.Error(w, "Tracks could not be loaded", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(tracks); err != nil {
		log.Printf("Track list could not be written: %v", err)
	}
}

// jellyfinBrowsePageSize is the page size when browsing. Jellyfin delivers it
// in one call (measured: 200 albums in ~98 ms), so it is much larger than the 8 rows
// enforced by the YXC list.
const jellyfinBrowsePageSize = 100

// jellyfinMusicView is a music library (Jellyfin view).
type jellyfinMusicView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// jellyfinMusicViews returns the music libraries of the API key user,
// filtered to CollectionType "music": Movies, series and books do not belong in
// a music interface. Only visible what this user is allowed to see.
func jellyfinMusicViews(userID string) ([]jellyfinMusicView, error) {
	body, err := jellyfinGet("/Users/" + url.PathEscape(userID) + "/Views")
	if err != nil {
		return nil, err
	}

	var res struct {
		Items []struct {
			ID             string `json:"Id"`
			Name           string `json:"Name"`
			CollectionType string `json:"CollectionType"`
		} `json:"Items"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("Jellyfin library list unreadable: %w", err)
	}

	views := make([]jellyfinMusicView, 0, len(res.Items))
	for _, it := range res.Items {
		if it.CollectionType == "music" {
			views = append(views, jellyfinMusicView{ID: it.ID, Name: it.Name})
		}
	}
	return views, nil
}

// handleJellyfinViews returns the music libraries as entry point for browsing.
func handleJellyfinViews(w http.ResponseWriter, r *http.Request) {
	userID, err := jellyfinUserID()
	if err != nil {
		log.Printf("Jellyfin user fetch failed: %v", err)
		http.Error(w, "Jellyfin not reachable", http.StatusInternalServerError)
		return
	}

	views, err := jellyfinMusicViews(userID)
	if err != nil {
		log.Printf("Jellyfin libraries failed: %v", err)
		http.Error(w, "Libraries could not be loaded", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(views); err != nil {
		log.Printf("Library list could not be written: %v", err)
	}
}

// handleJellyfinBrowse returns a page for browsing: album artists or albums.
// Read via StartIndex/Limit (loading on scroll); TotalRecordCount is included
// so the frontend knows when to stop. `letter` filters via NameStartsWith
// to show only items starting with that letter.
//
// kind=artists → /Artists/AlbumArtists (real album artists; IncludeItemTypes=
// MusicArtist returns all track features instead, measured 5287 instead of 1275).
// kind=albums  → albums, optionally from a library (parent_id), an album artist
// (artist_id) or sorted (name|newest).
func handleJellyfinBrowse(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	kind := query.Get("kind")
	if kind != "artists" && kind != "albums" {
		http.Error(w, "Parameter kind must be artists or albums", http.StatusBadRequest)
		return
	}

	letter := strings.ToUpper(strings.TrimSpace(query.Get("letter")))
	if letter != "" && (len(letter) != 1 || letter < "A" || letter > "Z") {
		http.Error(w, "Parameter letter must be a letter A-Z", http.StatusBadRequest)
		return
	}

	userID, err := jellyfinUserID()
	if err != nil {
		log.Printf("Jellyfin user fetch failed: %v", err)
		http.Error(w, "Jellyfin not reachable", http.StatusInternalServerError)
		return
	}

	params := url.Values{}
	params.Set("StartIndex", strconv.Itoa(parseIntParam(query.Get("start"), 0, 0, 1_000_000)))
	params.Set("Limit", strconv.Itoa(parseIntParam(query.Get("limit"), jellyfinBrowsePageSize, 1, 200)))
	if letter != "" {
		params.Set("NameStartsWith", letter)
	}

	// Both types can be restricted to a library.
	if parentID := strings.TrimSpace(query.Get("parent_id")); parentID != "" {
		params.Set("parentId", parentID)
	}

	var path string
	switch kind {
	case "artists":
		path = "/Artists/AlbumArtists"
		params.Set("userId", userID)
		params.Set("SortBy", "SortName")
	case "albums":
		path = "/Users/" + url.PathEscape(userID) + "/Items"
		params.Set("IncludeItemTypes", "MusicAlbum")
		params.Set("Recursive", "true")
		if artistID := strings.TrimSpace(query.Get("artist_id")); artistID != "" {
			// AlbumArtistIds fits better than ArtistIds: Guest appearances and
			// compilations would otherwise be counted. Chronologically as before
			// when expanding an artist.
			params.Set("AlbumArtistIds", artistID)
			params.Set("SortBy", "ProductionYear,SortName")
		} else if query.Get("sort") == "newest" {
			params.Set("SortBy", "DateCreated")
			params.Set("SortOrder", "Descending")
		} else {
			params.Set("SortBy", "SortName")
		}
	}

	items, total, err := jellyfinPage(path, params)
	if err != nil {
		log.Printf("Browse (%s) failed: %v", kind, err)
		http.Error(w, "List could not be loaded", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"items": items,
		"total": total,
	}); err != nil {
		log.Printf("Browse list could not be written: %v", err)
	}
}

// handleJellyfinImage forwards a cover thumbnail from Jellyfin so the
// browser stays same-origin (like /api/albumart). The id comes from the respective
// Jellyfin item, the tag from its ImageTags.Primary and is only used for
// cache validation; the size is intentionally small (list display).
func handleJellyfinImage(w http.ResponseWriter, r *http.Request) {
	itemID := strings.TrimSpace(r.URL.Query().Get("id"))
	if itemID == "" {
		http.Error(w, "Parameter id missing", http.StatusBadRequest)
		return
	}
	if strings.Contains(itemID, "/") || strings.Contains(itemID, "..") {
		http.Error(w, "Invalid id", http.StatusBadRequest)
		return
	}

	q := url.Values{}
	q.Set("maxHeight", "96")
	q.Set("quality", "80")
	if tag := r.URL.Query().Get("tag"); tag != "" {
		q.Set("tag", tag)
	}

	body, err := jellyfinGet("/Items/" + url.PathEscape(itemID) + "/Images/Primary?" + q.Encode())
	if err != nil {
		log.Printf("Jellyfin cover %q failed: %v", itemID, err)
		http.Error(w, "Cover not available", http.StatusBadGateway)
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Type", http.DetectContentType(body))
	if _, err := w.Write(body); err != nil {
		log.Printf("Cover %q could not be written: %v", itemID, err)
	}
}

// --- Track resolution for playback ---------------------------------------

type jellyfinItem struct {
	ID          string   `json:"Id"`
	Name        string   `json:"Name"`
	Type        string   `json:"Type"`
	Container   string   `json:"Container"`
	IndexNumber int      `json:"IndexNumber"`
	Artists     []string `json:"Artists"`
	AlbumArtist string   `json:"AlbumArtist"`
	Album       string   `json:"Album"`
	AlbumID     string   `json:"AlbumId"`
	// Duration in 100-ns ticks (Jellyfin standard), for time display.
	RunTimeTicks int64 `json:"RunTimeTicks"`
	// Cover of the album or (if the album has none) of the track itself.
	// Used as replacement for the device cover URL, see jellyfinCoverFor.
	AlbumPrimaryImageTag string            `json:"AlbumPrimaryImageTag"`
	ImageTags            map[string]string `json:"ImageTags"`
}

func (it jellyfinItem) artistName() string {
	if len(it.Artists) > 0 {
		return strings.Join(it.Artists, ", ")
	}
	return it.AlbumArtist
}

func jellyfinItemByID(userID, itemID string) (jellyfinItem, error) {
	body, err := jellyfinGet("/Users/" + url.PathEscape(userID) + "/Items/" + url.PathEscape(itemID))
	if err != nil {
		return jellyfinItem{}, err
	}
	var item jellyfinItem
	if err := json.Unmarshal(body, &item); err != nil {
		return jellyfinItem{}, fmt.Errorf("Jellyfin item unreadable: %w", err)
	}
	return item, nil
}

// jellyfinAudioChildren returns up to limit audio tracks under parentID,
// sorted by track number.
func jellyfinAudioChildren(userID, parentID string, limit int) ([]jellyfinItem, error) {
	if parentID == "" {
		return nil, fmt.Errorf("empty parentID")
	}

	q := url.Values{}
	q.Set("parentId", parentID)
	q.Set("Recursive", "true")
	q.Set("IncludeItemTypes", "Audio")
	// Multi-CD albums: ParentIndexNumber is the disc number. Without it, Jellyfin
	// only sorts by IndexNumber and places track 1 of disc 2 directly after
	// track 1 of disc 1.
	q.Set("SortBy", "ParentIndexNumber,IndexNumber")
	q.Set("Limit", strconv.Itoa(limit))

	body, err := jellyfinGet("/Users/" + url.PathEscape(userID) + "/Items?" + q.Encode())
	if err != nil {
		return nil, err
	}
	var res struct {
		Items []jellyfinItem `json:"Items"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("Jellyfin track list unreadable: %w", err)
	}
	return res.Items, nil
}

// jellyfinFirstAudioChild returns the first track of an album/artist.
func jellyfinFirstAudioChild(userID, parentID string) (jellyfinItem, error) {
	items, err := jellyfinAudioChildren(userID, parentID, 1)
	if err != nil {
		return jellyfinItem{}, err
	}
	if len(items) == 0 {
		return jellyfinItem{}, fmt.Errorf("no playable tracks found")
	}
	return items[0], nil
}

// resolveJellyfinTrack determines the concrete playable audio track: for an
// audio item, itself; for album/artist, the first contained track.
func resolveJellyfinTrack(userID, itemID string) (jellyfinItem, error) {
	item, err := jellyfinItemByID(userID, itemID)
	if err != nil {
		return jellyfinItem{}, err
	}
	if item.Type == "Audio" {
		return item, nil
	}
	return jellyfinFirstAudioChild(userID, itemID)
}

// jellyfinLibraryID determines the library for random selection: the desired one
// if it is a music library of the user, otherwise the first music library.
// The check is not cosmetic: Without parentId, Jellyfin searches ALL libraries
// (including audiobooks) and times out (observed).
func jellyfinLibraryID(userID, wanted string) (string, error) {
	views, err := jellyfinMusicViews(userID)
	if err != nil {
		return "", err
	}
	if len(views) == 0 {
		return "", fmt.Errorf("no music library found")
	}
	for _, view := range views {
		if view.ID == wanted {
			return wanted, nil
		}
	}
	return views[0].ID, nil
}

// jellyfinRandomTrack rolls a random track from the library. Deliberately
// SortBy=Random instead of "get total count and select random StartIndex":
// The total count costs ~42s in this library (43985 tracks), measured;
// SortBy=Random delivers in ~250ms.
func jellyfinRandomTrack(userID, libraryID string) (jellyfinItem, error) {
	if libraryID == "" {
		return jellyfinItem{}, fmt.Errorf("empty library ID")
	}

	q := url.Values{}
	q.Set("parentId", libraryID)
	q.Set("Recursive", "true")
	q.Set("IncludeItemTypes", "Audio")
	q.Set("SortBy", "Random")
	q.Set("Limit", "1")

	body, err := jellyfinGet("/Users/" + url.PathEscape(userID) + "/Items?" + q.Encode())
	if err != nil {
		return jellyfinItem{}, err
	}
	var res struct {
		Items []jellyfinItem `json:"Items"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return jellyfinItem{}, fmt.Errorf("Jellyfin random track unreadable: %w", err)
	}
	if len(res.Items) == 0 {
		return jellyfinItem{}, fmt.Errorf("no playable tracks found")
	}
	return res.Items[0], nil
}

// jellyfinStreamURL is the static stream URL that the renderer fetches itself.
// Therefore, the API key must be in the URL - the caller (the device) does not send
// an Authorization header.
func jellyfinStreamURL(trackID string) string {
	q := url.Values{}
	q.Set("static", "true")
	q.Set("api_key", jellyfinAPIKey)
	return jellyfinURL + "/Audio/" + url.PathEscape(trackID) + "/stream?" + q.Encode()
}

// --- Playback via UPnP AVTransport -----------------------------------------
//
// The MusicCast API cannot start a specific track. The receiver is, however,
// a DLNA/UPnP MediaRenderer (DMR-1.50): We give it the Jellyfin stream URL
// via SetAVTransportURI and start it with Play (see upnp.go). The device fetches
// the rest from the LAN by itself; stopping continues via netusb/setPlayback.
// trackDIDL builds stream URL and DIDL metadata of a track. Both are needed by the
// renderer for the current track (SetAVTransportURI) and for the next one (SetNextAVTransportURI).
func trackDIDL(track jellyfinItem) (streamURL, didl string) {
	streamURL = jellyfinStreamURL(track.ID)
	// RunTimeTicks are 100-ns ticks; the seconds belong in the DIDL so the
	// renderer knows the total duration (otherwise total_time = 0).
	didl = buildDIDL(track.Name, track.artistName(), track.Album, mimeForContainer(track.Container), streamURL, track.RunTimeTicks/10000000)
	return streamURL, didl
}

// startJellyfinTrack gives the renderer the Jellyfin stream URL via UPnP
// AVTransport and starts playback.
func startJellyfinTrack(track jellyfinItem) error {
	streamURL, didl := trackDIDL(track)

	setURI := "<InstanceID>0</InstanceID><CurrentURI>" + xmlEscape(streamURL) + "</CurrentURI>" +
		"<CurrentURIMetaData>" + xmlEscape(didl) + "</CurrentURIMetaData>"
	if err := upnpAVTransport("SetAVTransportURI", setURI); err != nil {
		return fmt.Errorf("SetAVTransportURI: %w", err)
	}
	if err := upnpAVTransport("Play", "<InstanceID>0</InstanceID><Speed>1</Speed>"); err != nil {
		return fmt.Errorf("Play: %w", err)
	}
	return nil
}

// jellyfinPlayContext determines the ordered track list of the started album/
// artist and the position of the track in it. For a single track, its album serves
// as the list.
func jellyfinPlayContext(userID, requestedID string, track jellyfinItem) ([]jellyfinItem, int) {
	parentID := requestedID
	if track.Type == "Audio" {
		parentID = track.AlbumID
	}
	if parentID == "" {
		return []jellyfinItem{track}, 0
	}

	items, err := jellyfinAudioChildren(userID, parentID, 300)
	if err != nil || len(items) == 0 {
		return []jellyfinItem{track}, 0
	}
	for i, it := range items {
		if it.ID == track.ID {
			return items, i
		}
	}
	return []jellyfinItem{track}, 0
}

// --- Skip/Prev within the current Jellyfin list -------------------------
//
// For our UPnP single tracks, the device cannot meaningfully skip:
// netusb/setPlayback?playback=next follows the device own queue, not the
// played Jellyfin album (observed: jumps to foreign tracks). Therefore the
// backend remembers the ordered track list and the current position.

type jellyfinQueue struct {
	mu    sync.Mutex
	items []jellyfinItem
	index int

	// Random mode: no fixed list context, each skip rolls again.
	// current is the current track, history is the history for ⏮.
	shuffle bool
	library string
	current jellyfinItem
	history []jellyfinItem

	// next is the track that the renderer has set as the next track. We
	// remember it to detect when the device automatically advances.
	next jellyfinItem
}

// shuffleHistoryLimit limits the history so ⏮ cannot go back arbitrarily far.
const shuffleHistoryLimit = 50

var playQueue jellyfinQueue

// setJellyfinQueue sets an ordered list context (album/artist) and thus
// ends any running random mode.
func setJellyfinQueue(items []jellyfinItem, index int) {
	playQueue.mu.Lock()
	defer playQueue.mu.Unlock()
	playQueue.items = items
	playQueue.index = index
	playQueue.shuffle = false
	playQueue.library = ""
	playQueue.current = jellyfinItem{}
	playQueue.history = nil
	playQueue.next = jellyfinItem{}
}

// setJellyfinShuffle switches to random playback. If a
// random session in the same library, the current track moves to the
// history — otherwise a fresh session starts without history.
func setJellyfinShuffle(library string, track jellyfinItem) {
	playQueue.mu.Lock()
	defer playQueue.mu.Unlock()
	sameSession := playQueue.shuffle && playQueue.library == library
	if sameSession {
		pushShuffleHistoryLocked()
	} else {
		playQueue.history = nil
	}
	playQueue.items = nil
	playQueue.index = 0
	playQueue.shuffle = true
	playQueue.library = library
	playQueue.current = track
	playQueue.next = jellyfinItem{}
}

// clearJellyfinQueue ends the Jellyfin context (e.g., when changing source).
func clearJellyfinQueue() {
	setJellyfinQueue(nil, 0)
}

// --- Automatic continuation -------------------------------------------------
//
// The renderer only knows the one track we give it. So that an album
// does not end after track 1, we set the next track via
// SetNextAVTransportURI: The device advances automatically at the end of a track
// (verified live). Our task is only to set the next track — for this, autoAdvanceLoop
// polls the current TrackURI every few seconds.

// autoAdvanceInterval determines how often the renderer is polled after a track
// change. The change itself happens on the device.
const autoAdvanceInterval = 2 * time.Second

// setQueueNext remembers the next track set in the renderer.
func setQueueNext(track jellyfinItem) {
	playQueue.mu.Lock()
	defer playQueue.mu.Unlock()
	playQueue.next = track
}

// clearJellyfinNext removes the next track in the renderer and in memory. Required
// when stopping and when changing source: Otherwise the device would continue
// playing after a later Play.
func clearJellyfinNext() {
	setQueueNext(jellyfinItem{})
	if err := upnpClearNext(); err != nil {
		log.Printf("Removing next track failed: %v", err)
	}
}

// primeNext sets the next track in the renderer: in the list the next entry,
// in random mode a new roll. At the end of the list, the next track is deleted,
// then playback stops after the last track.
func primeNext() {
	playQueue.mu.Lock()
	shuffle, library, index := playQueue.shuffle, playQueue.library, playQueue.index
	next := jellyfinItem{}
	if !shuffle && index+1 < len(playQueue.items) {
		next = playQueue.items[index+1]
	}
	playQueue.mu.Unlock()

	if shuffle {
		// The roll accesses the network, therefore outside the lock.
		userID, err := jellyfinUserID()
		if err != nil {
			log.Printf("Next track: Jellyfin user fetch failed: %v", err)
			return
		}
		next, err = jellyfinRandomTrack(userID, library)
		if err != nil {
			log.Printf("Next track: Random track could not be determined: %v", err)
			return
		}
	}

	// In the meantime, a different context (skip, stop, new selection)? Then no longer
	// change on the renderer — otherwise a next track from the old album would remain.
	if !queueUnchanged(shuffle, library, index, next) {
		return
	}

	if next.ID == "" {
		clearJellyfinNext()
		return
	}

	streamURL, didl := trackDIDL(next)
	log.Printf("DEBUG primeNext: Setting next track %q (ID: %s) with URL: %s", next.Name, next.ID, streamURL)
	if err := upnpSetNext(streamURL, didl); err != nil {
		log.Printf("Setting next track %q failed: %v", next.Name, err)
		setQueueNext(jellyfinItem{})
		return
	}
	setQueueNext(next)
}

// queueUnchanged indicates whether the playback context is still the same
// since the start of primeNext.
func queueUnchanged(shuffle bool, library string, index int, next jellyfinItem) bool {
	playQueue.mu.Lock()
	defer playQueue.mu.Unlock()
	if playQueue.shuffle != shuffle {
		return false
	}
	if shuffle {
		return playQueue.library == library
	}
	if playQueue.index != index {
		return false
	}
	if next.ID == "" {
		return index+1 >= len(playQueue.items)
	}
	return index+1 < len(playQueue.items) && playQueue.items[index+1].ID == next.ID
}

// autoAdvanceLoop keeps playback running: As soon as the renderer has
// automatically switched to the set next track, the position in the Jellyfin
// list advances and the next track is set.
func autoAdvanceLoop() {
	ticker := time.NewTicker(autoAdvanceInterval)
	defer ticker.Stop()
	for range ticker.C {
		advanceIfNeeded()
	}
}

// advanceIdle counts consecutive requests without useful TrackURI. Only
// the ticker loop reads and writes the value.
var advanceIdle int

// advanceIdleLimit is reached when the renderer reports no session for 30 requests
// (~1 minute) or is unreachable; then the next track is forgotten instead of
// polling endlessly.
const advanceIdleLimit = 30

// advanceIfNeeded checks once if the device has advanced.
func advanceIfNeeded() {
	playQueue.mu.Lock()
	expected := playQueue.next
	playQueue.mu.Unlock()
	if expected.ID == "" {
		advanceIdle = 0
		return // no next track set
	}

	current, err := upnpCurrentTrackURI()
	if err != nil || current == "" {
		// Device off, source changed on device or session ended: The next
		// track is then no longer useful. Don't log every round.
		advanceIdle++
		if advanceIdle == 1 {
			if err != nil {
				log.Printf("Transport query failed, next track remains set for now: %v", err)
			} else {
				log.Printf("Renderer reports no playback, next track remains set for now")
			}
		}
		if advanceIdle >= advanceIdleLimit {
			log.Printf("Renderer no longer reports playback — next track is forgotten")
			setQueueNext(jellyfinItem{})
			advanceIdle = 0
		}
		return
	}
	advanceIdle = 0

	// DEBUG: Logging for troubleshooting
	if advanceIdle <= 2 {
		log.Printf("DEBUG advanceIfNeeded: currentURI=%q, expected.ID=%q, contains=%v", current, expected.ID, strings.Contains(current, expected.ID))
	}

	if !strings.Contains(current, expected.ID) {
		return // the device is still playing the current track
	}

	// The device has advanced: update position. The comparison under the lock
	// catches that a Skip/Stop/source change happened in the meantime.
	playQueue.mu.Lock()
	if playQueue.next.ID != expected.ID {
		playQueue.mu.Unlock()
		return
	}
	playQueue.next = jellyfinItem{}
	if playQueue.shuffle {
		pushShuffleHistoryLocked()
		playQueue.current = expected
	} else {
		for i, item := range playQueue.items {
			if item.ID == expected.ID {
				playQueue.index = i
				break
			}
		}
	}
	playQueue.mu.Unlock()

	log.Printf("Continuing with %q by %q", expected.Name, expected.artistName())
	primeNext()
}

// shuffleActive indicates whether random playback is currently active. The frontend
// uses this to display the state of the random button (field jellyfin_shuffle in /api/playinfo).
func shuffleActive() bool {
	playQueue.mu.Lock()
	defer playQueue.mu.Unlock()
	return playQueue.shuffle
}

// jellyfinCoverFor returns album ID and image tag of the current track — but only
// if it verifiably belongs to our Jellyfin context. The track name is compared,
// because the renderer can also be started externally (MusicCast app); a foreign
// track must not get a Jellyfin cover.
//
// Reason for the detour: After automatic advance (SetNextAVTransportURI),
// getPlayInfo sometimes reports a new albumart_id but an empty albumart_url,
// and the AlbumART file for it is 404 (observed). With manual start/skip, the
// device creates the cover immediately. Without replacement, the browser would
// have no cover.
func jellyfinCoverFor(running string) (albumID, imageTag string) {
	running = strings.TrimSpace(running)
	if running == "" {
		return "", ""
	}

	playQueue.mu.Lock()
	candidates := make([]jellyfinItem, 0, 2)
	if playQueue.shuffle {
		candidates = append(candidates, playQueue.current)
	} else if playQueue.index >= 0 && playQueue.index < len(playQueue.items) {
		candidates = append(candidates, playQueue.items[playQueue.index])
	}
	// When advancing, the device already reports the new track, while our
	// Position only updates in the next 2-second cycle — therefore also the next track.
	candidates = append(candidates, playQueue.next)
	playQueue.mu.Unlock()

	for _, track := range candidates {
		if strings.TrimSpace(track.Name) != running {
			continue
		}
		if track.AlbumID != "" && track.AlbumPrimaryImageTag != "" {
			return track.AlbumID, track.AlbumPrimaryImageTag
		}
		if tag := track.ImageTags["Primary"]; tag != "" {
			return track.ID, tag
		}
	}
	return "", ""
}

// pushShuffleHistoryLocked adds the current track to the history (under mu).
func pushShuffleHistoryLocked() {
	if playQueue.current.ID == "" {
		return
	}
	playQueue.history = append(playQueue.history, playQueue.current)
	if len(playQueue.history) > shuffleHistoryLimit {
		playQueue.history = playQueue.history[1:]
	}
}

// shuffleStep describes what a skip should do in random mode.
type shuffleStep struct {
	active  bool   // random playback active
	moved   bool   // there is something to play
	roll    bool   // track is empty: caller must roll a new track
	library string // library from which to roll
	track   jellyfinItem
}

// planShuffleStep prepares a skip in random mode without accessing the network
// (the network call happens outside the lock). ⏭ moves the current
// track to history and requests a new one; ⏮ takes the last played.
func planShuffleStep(delta int) shuffleStep {
	playQueue.mu.Lock()
	defer playQueue.mu.Unlock()
	if !playQueue.shuffle {
		return shuffleStep{}
	}

	step := shuffleStep{active: true, library: playQueue.library}
	if delta < 0 {
		if len(playQueue.history) == 0 {
			return step // start of random sequence: no jump
		}
		step.track = playQueue.history[len(playQueue.history)-1]
		playQueue.history = playQueue.history[:len(playQueue.history)-1]
		playQueue.current = step.track
		step.moved = true
		return step
	}

	pushShuffleHistoryLocked()
	step.moved = true
	step.roll = true
	return step
}

// setShuffleCurrent remembers the newly rolled track.
func setShuffleCurrent(track jellyfinItem) {
	playQueue.mu.Lock()
	defer playQueue.mu.Unlock()
	playQueue.current = track
}

// stepJellyfinQueue moves the position by delta. hasQueue indicates if
// a Jellyfin list is active at all; moved indicates if the new position was valid.
func stepJellyfinQueue(delta int) (item jellyfinItem, moved bool, hasQueue bool) {
	playQueue.mu.Lock()
	defer playQueue.mu.Unlock()
	if len(playQueue.items) == 0 {
		return jellyfinItem{}, false, false
	}
	next := playQueue.index + delta
	if next < 0 || next >= len(playQueue.items) {
		return jellyfinItem{}, false, true
	}
	playQueue.index = next
	return playQueue.items[next], true, true
}

func handleNext(w http.ResponseWriter, r *http.Request) { handleJellyfinSkip(w, 1) }
func handlePrev(w http.ResponseWriter, r *http.Request) { handleJellyfinSkip(w, -1) }

// handleJellyfinSkip switches within the active Jellyfin list. If random
// playback is active, it rolls again instead; without Jellyfin context (e.g.,
// internet radio, USB), the command is forwarded to the device.
func handleJellyfinSkip(w http.ResponseWriter, delta int) {
	if step := planShuffleStep(delta); step.active {
		handleShuffleSkip(w, step)
		return
	}

	track, moved, hasQueue := stepJellyfinQueue(delta)
	if !hasQueue {
		action := "next"
		if delta < 0 {
			action = "previous"
		}
		proxyYamaha(w, "netusb/setPlayback?"+encodeQuery("playback", action))
		return
	}

	if !moved {
		// Start/end of list reached: do not jump.
		writeSkipResult(w, false, jellyfinItem{})
		return
	}

	if err := startJellyfinTrack(track); err != nil {
		log.Printf("Track change to %q failed: %v", track.Name, err)
		http.Error(w, "Track change failed", http.StatusBadGateway)
		return
	}
	primeNext()
	log.Printf("Switching to %q by %q", track.Name, track.artistName())
	writeSkipResult(w, true, track)
}

// handleShuffleSkip handles ⏭/⏮ in random mode. The new track is only rolled
// here, so outside the lock in planShuffleStep.
func handleShuffleSkip(w http.ResponseWriter, step shuffleStep) {
	if !step.moved {
		writeSkipResult(w, false, jellyfinItem{})
		return
	}

	track := step.track
	if step.roll {
		userID, err := jellyfinUserID()
		if err != nil {
			log.Printf("Jellyfin user fetch failed: %v", err)
			http.Error(w, "Jellyfin not reachable", http.StatusInternalServerError)
			return
		}
		track, err = jellyfinRandomTrack(userID, step.library)
		if err != nil {
			log.Printf("Random track could not be determined: %v", err)
			http.Error(w, "Random track could not be determined", http.StatusBadGateway)
			return
		}
		setShuffleCurrent(track)
	}

	if err := startJellyfinTrack(track); err != nil {
		log.Printf("Track change to %q failed: %v", track.Name, err)
		http.Error(w, "Track change failed", http.StatusBadGateway)
		return
	}
	primeNext()
	log.Printf("Random: %q by %q", track.Name, track.artistName())
	writeSkipResult(w, true, track)
}

// handleJellyfinRandom starts a random track and switches to
// random playback, so that further ⏭ rolls again. The library can be specified via
// parent_id (the one currently browsed), otherwise the first
// music library.
func handleJellyfinRandom(w http.ResponseWriter, r *http.Request) {
	userID, err := jellyfinUserID()
	if err != nil {
		log.Printf("Jellyfin user fetch failed: %v", err)
		http.Error(w, "Jellyfin not reachable", http.StatusInternalServerError)
		return
	}

	library, err := jellyfinLibraryID(userID, strings.TrimSpace(r.URL.Query().Get("parent_id")))
	if err != nil {
		log.Printf("Music library could not be determined: %v", err)
		http.Error(w, "No music library found", http.StatusBadGateway)
		return
	}

	track, err := jellyfinRandomTrack(userID, library)
	if err != nil {
		log.Printf("Random track could not be determined: %v", err)
		http.Error(w, "Random track could not be determined", http.StatusBadGateway)
		return
	}

	setJellyfinShuffle(library, track)

	if err := startJellyfinTrack(track); err != nil {
		log.Printf("Playback of %q failed: %v", track.Name, err)
		http.Error(w, "Playback could not be started", http.StatusBadGateway)
		return
	}
	primeNext()

	log.Printf("Playing random track %q by %q", track.Name, track.artistName())
	writeTrackResponse(w, track)
}

// writeSkipResult reports whether a skip moved the playback.
func writeSkipResult(w http.ResponseWriter, moved bool, track jellyfinItem) {
	result := map[string]any{"response_code": 0, "moved": moved}
	if moved {
		result["track"] = track.Name
		result["artist"] = track.artistName()
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Printf("Response could not be written: %v", err)
	}
}

func handleJellyfinPlay(w http.ResponseWriter, r *http.Request) {
	itemID := r.URL.Query().Get("item_id")
	if itemID == "" {
		http.Error(w, "Parameter item_id missing", http.StatusBadRequest)
		return
	}

	userID, err := jellyfinUserID()
	if err != nil {
		log.Printf("Jellyfin user fetch failed: %v", err)
		http.Error(w, "Jellyfin not reachable", http.StatusInternalServerError)
		return
	}

	track, err := resolveJellyfinTrack(userID, itemID)
	if err != nil {
		log.Printf("Track %q could not be resolved: %v", itemID, err)
		http.Error(w, "Track not found", http.StatusNotFound)
		return
	}

	// Remember context for Skip/Prev: ordered track list + position. This also ends
	// any running random mode.
	queue, index := jellyfinPlayContext(userID, itemID, track)
	setJellyfinQueue(queue, index)

	if err := startJellyfinTrack(track); err != nil {
		log.Printf("Playback of %q failed: %v", track.Name, err)
		http.Error(w, "Playback could not be started", http.StatusBadGateway)
		return
	}
	primeNext()

	log.Printf("Playing Jellyfin track %q by %q", track.Name, track.artistName())
	writeTrackResponse(w, track)
}

// writeTrackResponse reports the started track as JSON.
func writeTrackResponse(w http.ResponseWriter, track jellyfinItem) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"response_code": 0,
		"track":         track.Name,
		"artist":        track.artistName(),
	}); err != nil {
		log.Printf("Response could not be written: %v", err)
	}
}
