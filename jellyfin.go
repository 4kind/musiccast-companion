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

// jellyfinGet ruft einen Jellyfin-Pfad mit dem nötigen Authorization-Header ab
// und liefert den Body. Nicht-200-Antworten werden als Fehler zurückgegeben.
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
		return body, fmt.Errorf("Jellyfin antwortet mit Status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// jellyfinUserID liefert die ID des ersten Jellyfin-Benutzers.
// Bekannte Vereinfachung: Es gibt keine Benutzerauswahl, es wird blind der
// erste Eintrag genommen.
func jellyfinUserID() (string, error) {
	body, err := jellyfinGet("/Users")
	if err != nil {
		return "", err
	}

	var users []struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(body, &users); err != nil {
		return "", fmt.Errorf("Jellyfin-Benutzerliste unlesbar: %w", err)
	}
	if len(users) == 0 {
		return "", fmt.Errorf("keine Jellyfin-Benutzer gefunden")
	}
	return users[0].ID, nil
}

// handleJellyfinSearch liefert die Treffer getrennt nach Alben, Interpreten und
// Titeln. Getrennte Abfragen statt einer gemeinsamen: Titel sind viel zahlreicher
// (Jellyfin rankt sie zuerst) und würden die Alben aus einem gemeinsamen Limit
// verdrängen. Die Trennung erlaubt zudem eigene Limits je Typ und dem Frontend
// eine Abschnitts-Darstellung.
func handleJellyfinSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		http.Error(w, "Suchbegriff fehlt", http.StatusBadRequest)
		return
	}

	userID, err := jellyfinUserID()
	if err != nil {
		log.Printf("Jellyfin-Benutzerabruf fehlgeschlagen: %v", err)
		http.Error(w, "Jellyfin nicht erreichbar", http.StatusInternalServerError)
		return
	}

	albums, err := jellyfinSearchItems(userID, query, "MusicAlbum", 12)
	if err != nil {
		log.Printf("Jellyfin-Albensuche fehlgeschlagen: %v", err)
		http.Error(w, "Jellyfin-Suche fehlgeschlagen", http.StatusInternalServerError)
		return
	}
	artists, err := jellyfinSearchItems(userID, query, "MusicArtist", 8)
	if err != nil {
		log.Printf("Jellyfin-Interpretensuche fehlgeschlagen: %v", err)
		http.Error(w, "Jellyfin-Suche fehlgeschlagen", http.StatusInternalServerError)
		return
	}
	tracks, err := jellyfinSearchItems(userID, query, "Audio", 25)
	if err != nil {
		log.Printf("Jellyfin-Titelsuche fehlgeschlagen: %v", err)
		http.Error(w, "Jellyfin-Suche fehlgeschlagen", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"albums":  albums,
		"artists": artists,
		"tracks":  tracks,
	}); err != nil {
		log.Printf("Jellyfin-Suchergebnis konnte nicht geschrieben werden: %v", err)
	}
}

// jellyfinSearchItems liefert die rohen Jellyfin-Treffer eines Types-Filters.
func jellyfinSearchItems(userID, query, types string, limit int) ([]json.RawMessage, error) {
	q := url.Values{}
	q.Set("searchTerm", query)
	q.Set("IncludeItemTypes", types)
	q.Set("Recursive", "true")
	q.Set("Limit", strconv.Itoa(limit))
	return jellyfinQueryItems(userID, q)
}

// jellyfinQueryItems ruft /Users/{id}/Items mit beliebigen Parametern ab und
// liefert die Roh-Treffer. Nie nil, damit JSON ein Array statt null ergibt.
func jellyfinQueryItems(userID string, q url.Values) ([]json.RawMessage, error) {
	items, _, err := jellyfinPage("/Users/"+url.PathEscape(userID)+"/Items", q)
	return items, err
}

// jellyfinPage holt eine Jellyfin-Liste und liefert Treffer plus Gesamtzahl
// (TotalRecordCount). Die Gesamtzahl braucht das Stöbern, um zu wissen, wann
// nicht mehr nachgeladen werden muss. Nie nil, damit JSON [] statt null ergibt.
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
		return nil, 0, fmt.Errorf("Jellyfin-Antwort unlesbar: %w", err)
	}
	if res.Items == nil {
		res.Items = []json.RawMessage{}
	}
	return res.Items, res.TotalRecordCount, nil
}

// parseIntParam liest einen Zahlenparameter. Unlesbares fällt auf fallback
// zurück, alles außerhalb von min..max wird darauf begrenzt.
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

// handleJellyfinTracks liefert die Titel eines Albums/Interpreten zum Aufklappen
// in der Trefferliste.
func handleJellyfinTracks(w http.ResponseWriter, r *http.Request) {
	parentID := r.URL.Query().Get("parent_id")
	if parentID == "" {
		http.Error(w, "Parameter parent_id fehlt", http.StatusBadRequest)
		return
	}

	userID, err := jellyfinUserID()
	if err != nil {
		log.Printf("Jellyfin-Benutzerabruf fehlgeschlagen: %v", err)
		http.Error(w, "Jellyfin nicht erreichbar", http.StatusInternalServerError)
		return
	}

	tracks, err := jellyfinAudioChildren(userID, parentID, 300)
	if err != nil {
		log.Printf("Titelliste für %q fehlgeschlagen: %v", parentID, err)
		http.Error(w, "Titel konnten nicht geladen werden", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(tracks); err != nil {
		log.Printf("Titelliste konnte nicht geschrieben werden: %v", err)
	}
}

// jellyfinBrowsePageSize ist die Seitengröße beim Stöbern. Jellyfin liefert sie
// in einem Aufruf (gemessen: 200 Alben in ~98 ms), deshalb ist sie deutlich
// größer als die 8 Zeilen, die die YXC-Liste erzwingt.
const jellyfinBrowsePageSize = 100

// jellyfinMusicView ist eine Musik-Bibliothek (Jellyfin-View).
type jellyfinMusicView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// jellyfinMusicViews liefert die Musik-Bibliotheken des API-Key-Benutzers,
// gefiltert auf CollectionType "music": Filme, Serien und Bücher gehören nicht in
// eine Musik-Oberfläche. Sichtbar ist nur, was dieser Benutzer sehen darf.
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
		return nil, fmt.Errorf("Jellyfin-Bibliotheksliste unlesbar: %w", err)
	}

	views := make([]jellyfinMusicView, 0, len(res.Items))
	for _, it := range res.Items {
		if it.CollectionType == "music" {
			views = append(views, jellyfinMusicView{ID: it.ID, Name: it.Name})
		}
	}
	return views, nil
}

// handleJellyfinViews liefert die Musik-Bibliotheken als Einstieg zum Stöbern.
func handleJellyfinViews(w http.ResponseWriter, r *http.Request) {
	userID, err := jellyfinUserID()
	if err != nil {
		log.Printf("Jellyfin-Benutzerabruf fehlgeschlagen: %v", err)
		http.Error(w, "Jellyfin nicht erreichbar", http.StatusInternalServerError)
		return
	}

	views, err := jellyfinMusicViews(userID)
	if err != nil {
		log.Printf("Jellyfin-Bibliotheken fehlgeschlagen: %v", err)
		http.Error(w, "Bibliotheken konnten nicht geladen werden", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(views); err != nil {
		log.Printf("Bibliotheksliste konnte nicht geschrieben werden: %v", err)
	}
}

// handleJellyfinBrowse liefert eine Seite zum Stöbern: Album-Künstler oder Alben.
// Gelesen wird per StartIndex/Limit (Nachladen beim Scrollen); TotalRecordCount
// kommt mit, damit das Frontend weiß, wann Schluss ist. `letter` springt per
// NameStartsWithOrGreater zu einem Anfangsbuchstaben — weitergescrollt wird dann
// innerhalb dieser gefilterten Liste, deshalb braucht es keine Index-Rechnerei.
//
// kind=artists → /Artists/AlbumArtists (echte Album-Künstler; IncludeItemTypes=
// MusicArtist liefert dagegen alle Titel-Features, gemessen 5287 statt 1275).
// kind=albums  → Alben, wahlweise einer Bibliothek (parent_id), eines
// Album-Künstlers (artist_id) oder sortiert (name|newest).
func handleJellyfinBrowse(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	kind := query.Get("kind")
	if kind != "artists" && kind != "albums" {
		http.Error(w, "Parameter kind muss artists oder albums sein", http.StatusBadRequest)
		return
	}

	letter := strings.ToUpper(strings.TrimSpace(query.Get("letter")))
	if letter != "" && (len(letter) != 1 || letter < "A" || letter > "Z") {
		http.Error(w, "Parameter letter muss ein Buchstabe A-Z sein", http.StatusBadRequest)
		return
	}

	userID, err := jellyfinUserID()
	if err != nil {
		log.Printf("Jellyfin-Benutzerabruf fehlgeschlagen: %v", err)
		http.Error(w, "Jellyfin nicht erreichbar", http.StatusInternalServerError)
		return
	}

	params := url.Values{}
	params.Set("StartIndex", strconv.Itoa(parseIntParam(query.Get("start"), 0, 0, 1_000_000)))
	params.Set("Limit", strconv.Itoa(parseIntParam(query.Get("limit"), jellyfinBrowsePageSize, 1, 200)))
	if letter != "" {
		params.Set("NameStartsWithOrGreater", letter)
	}

	// Beide Arten lassen sich auf eine Bibliothek einschränken.
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
			// AlbumArtistIds passt besser als ArtistIds: Gastauftritte und
			// Kompilationen würden sonst mitgezählt. Chronologisch wie bisher
			// beim Aufklappen eines Interpreten.
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
		log.Printf("Stöbern (%s) fehlgeschlagen: %v", kind, err)
		http.Error(w, "Liste konnte nicht geladen werden", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"items": items,
		"total": total,
	}); err != nil {
		log.Printf("Stöber-Liste konnte nicht geschrieben werden: %v", err)
	}
}

// handleJellyfinImage reicht ein Cover-Thumbnail aus Jellyfin durch, damit der
// Browser same-origin bleibt (wie /api/albumart). Die id kommt aus dem jeweiligen
// Jellyfin-Item, der tag aus dessen ImageTags.Primary und dient nur der
// Cache-Validierung; die Größe ist bewusst klein (Listendarstellung).
func handleJellyfinImage(w http.ResponseWriter, r *http.Request) {
	itemID := strings.TrimSpace(r.URL.Query().Get("id"))
	if itemID == "" {
		http.Error(w, "Parameter id fehlt", http.StatusBadRequest)
		return
	}
	if strings.Contains(itemID, "/") || strings.Contains(itemID, "..") {
		http.Error(w, "Ungültige id", http.StatusBadRequest)
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
		log.Printf("Jellyfin-Cover %q fehlgeschlagen: %v", itemID, err)
		http.Error(w, "Cover nicht verfügbar", http.StatusBadGateway)
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Type", http.DetectContentType(body))
	if _, err := w.Write(body); err != nil {
		log.Printf("Cover %q konnte nicht geschrieben werden: %v", itemID, err)
	}
}

// --- Titel-Auflösung für die Wiedergabe ---------------------------------------

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
	// Laufzeit in 100-ns-Ticks (Jellyfin-Standard), für die Zeitanzeige.
	RunTimeTicks int64 `json:"RunTimeTicks"`
	// Cover des Albums bzw. (falls das Album keins hat) des Titels selbst.
	// Wird als Ersatz für die Geräte-Cover-URL gebraucht, siehe jellyfinCoverFor.
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
		return jellyfinItem{}, fmt.Errorf("Jellyfin-Item unlesbar: %w", err)
	}
	return item, nil
}

// jellyfinAudioChildren liefert bis zu limit Audio-Titel unter parentID,
// sortiert nach Tracknummer.
func jellyfinAudioChildren(userID, parentID string, limit int) ([]jellyfinItem, error) {
	if parentID == "" {
		return nil, fmt.Errorf("leere parentID")
	}

	q := url.Values{}
	q.Set("parentId", parentID)
	q.Set("Recursive", "true")
	q.Set("IncludeItemTypes", "Audio")
	// Mehr-CD-Alben: ParentIndexNumber ist die Disc-Nummer. Ohne sie sortiert
	// Jellyfin nur nach IndexNumber und schiebt Track 1 von Disc 2 direkt
	// hinter Track 1 von Disc 1.
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
		return nil, fmt.Errorf("Jellyfin-Trackliste unlesbar: %w", err)
	}
	return res.Items, nil
}

// jellyfinFirstAudioChild liefert den ersten Titel eines Albums/Interpreten.
func jellyfinFirstAudioChild(userID, parentID string) (jellyfinItem, error) {
	items, err := jellyfinAudioChildren(userID, parentID, 1)
	if err != nil {
		return jellyfinItem{}, err
	}
	if len(items) == 0 {
		return jellyfinItem{}, fmt.Errorf("keine abspielbaren Titel gefunden")
	}
	return items[0], nil
}

// resolveJellyfinTrack bestimmt den konkret abspielbaren Audio-Titel: bei einem
// Audio-Item dieses selbst, bei Album/Interpret den ersten enthaltenen Titel.
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

// jellyfinLibraryID bestimmt die Bibliothek zum Würfeln: die gewünschte, wenn sie
// eine Musik-Bibliothek des Benutzers ist, sonst die erste Musik-Bibliothek.
// Die Prüfung ist nicht kosmetisch: Ohne parentId durchsucht Jellyfin ALLE
// Bibliotheken (inklusive Hörbücher) und läuft dabei in einen Timeout (gemessen).
func jellyfinLibraryID(userID, wanted string) (string, error) {
	views, err := jellyfinMusicViews(userID)
	if err != nil {
		return "", err
	}
	if len(views) == 0 {
		return "", fmt.Errorf("keine Musik-Bibliothek gefunden")
	}
	for _, view := range views {
		if view.ID == wanted {
			return wanted, nil
		}
	}
	return views[0].ID, nil
}

// jellyfinRandomTrack würfelt einen Titel aus der Bibliothek. Bewusst
// SortBy=Random statt "Gesamtzahl holen und zufälligen StartIndex wählen": Die
// Gesamtzahl kostet in dieser Bibliothek gemessen ~42 s (43985 Titel),
// SortBy=Random liefert in ~250 ms.
func jellyfinRandomTrack(userID, libraryID string) (jellyfinItem, error) {
	if libraryID == "" {
		return jellyfinItem{}, fmt.Errorf("leere Bibliotheks-ID")
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
		return jellyfinItem{}, fmt.Errorf("Jellyfin-Zufallstitel unlesbar: %w", err)
	}
	if len(res.Items) == 0 {
		return jellyfinItem{}, fmt.Errorf("keine abspielbaren Titel gefunden")
	}
	return res.Items[0], nil
}

// jellyfinStreamURL ist die Static-Stream-URL, die der Renderer selbst abruft.
// Deshalb muss der API-Key in der URL stehen — der Aufrufer (das Gerät) schickt
// keinen Authorization-Header.
func jellyfinStreamURL(trackID string) string {
	q := url.Values{}
	q.Set("static", "true")
	q.Set("api_key", jellyfinAPIKey)
	return jellyfinURL + "/Audio/" + url.PathEscape(trackID) + "/stream?" + q.Encode()
}

// --- Wiedergabe über UPnP AVTransport -----------------------------------------
//
// Die MusicCast-API kann keinen konkreten Titel starten. Der Receiver ist aber
// ein DLNA/UPnP-MediaRenderer (DMR-1.50): Wir geben ihm die Jellyfin-Stream-URL
// per SetAVTransportURI und starten sie mit Play (siehe upnp.go). Den Rest holt
// sich das Gerät selbst aus dem LAN; Stoppen läuft weiter über netusb/setPlayback.
// trackDIDL baut Stream-URL und DIDL-Metadaten eines Titels. Beides braucht der
// Renderer für den laufenden Titel (SetAVTransportURI) und für den folgenden
// (SetNextAVTransportURI).
func trackDIDL(track jellyfinItem) (streamURL, didl string) {
	streamURL = jellyfinStreamURL(track.ID)
	// RunTimeTicks sind 100-ns-Ticks; die Sekunden gehören ins DIDL, damit der
	// Renderer die Gesamtdauer kennt (sonst total_time = 0).
	didl = buildDIDL(track.Name, track.artistName(), track.Album, mimeForContainer(track.Container), streamURL, track.RunTimeTicks/10000000)
	return streamURL, didl
}

// startJellyfinTrack gibt dem Renderer die Jellyfin-Stream-URL per UPnP
// AVTransport und startet die Wiedergabe.
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

// jellyfinPlayContext bestimmt die geordnete Titelliste des gestarteten Albums/
// Interpreten und die Position des Titels darin. Bei einem einzelnen Titel dient
// dessen Album als Liste.
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

// --- Skip/Prev innerhalb der laufenden Jellyfin-Liste -------------------------
//
// Für unsere UPnP-Einzeltitel kann das Gerät nicht sinnvoll springen:
// netusb/setPlayback?playback=next folgt der geräteeigenen Queue, nicht dem
// gespielten Jellyfin-Album (gemessen: springt in fremde Titel). Deshalb merkt
// sich das Backend die geordnete Titelliste und die aktuelle Position.

type jellyfinQueue struct {
	mu    sync.Mutex
	items []jellyfinItem
	index int

	// Zufallsmodus: kein fester Listenkontext, jeder Skip würfelt neu.
	// current ist der laufende Titel, history der Verlauf für ⏮.
	shuffle bool
	library string
	current jellyfinItem
	history []jellyfinItem

	// next ist der Titel, den der Renderer als Folgetitel hinterlegt hat. Wir
	// merken ihn uns, um zu erkennen, wann das Gerät von selbst weiterschaltet.
	next jellyfinItem
}

// shuffleHistoryLimit begrenzt den Verlauf, damit ⏮ nicht beliebig weit zurückgeht.
const shuffleHistoryLimit = 50

var playQueue jellyfinQueue

// setJellyfinQueue setzt einen geordneten Listenkontext (Album/Interpret) und
// beendet damit einen laufenden Zufallsmodus.
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

// setJellyfinShuffle schaltet auf Zufallswiedergabe um. Läuft schon eine
// Zufallssitzung in derselben Bibliothek, wandert der bisherige Titel in den
// Verlauf — sonst beginnt eine frische Sitzung ohne Verlauf.
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

// clearJellyfinQueue beendet den Jellyfin-Kontext (z. B. beim Quellenwechsel).
func clearJellyfinQueue() {
	setJellyfinQueue(nil, 0)
}

// --- Automatische Fortsetzung -------------------------------------------------
//
// Der Renderer kennt nur den einen Titel, den wir ihm geben. Damit ein Album
// nicht nach Track 1 endet, hinterlegen wir den Folgetitel per
// SetNextAVTransportURI: Das Gerät schaltet am Titelende selbst weiter (live
// verifiziert). Unsere Aufgabe ist nur, den jeweils nächsten Titel nachzulegen —
// dafür fragt autoAdvanceLoop alle paar Sekunden die aktuelle TrackURI ab.

// autoAdvanceInterval bestimmt, wie oft der Renderer nach einem Titelwechsel
// gefragt wird. Der Wechsel selbst passiert im Gerät.
const autoAdvanceInterval = 2 * time.Second

// setQueueNext merkt sich den im Renderer hinterlegten Folgetitel.
func setQueueNext(track jellyfinItem) {
	playQueue.mu.Lock()
	defer playQueue.mu.Unlock()
	playQueue.next = track
}

// clearJellyfinNext entfernt den Folgetitel im Renderer und im Speicher. Nötig
// beim Stoppen und beim Quellenwechsel: Sonst würde das Gerät nach einem
// späteren Play weiterlaufen.
func clearJellyfinNext() {
	setQueueNext(jellyfinItem{})
	if err := upnpClearNext(); err != nil {
		log.Printf("Folgetitel entfernen fehlgeschlagen: %v", err)
	}
}

// primeNext hinterlegt den Folgetitel im Renderer: in der Liste den nächsten
// Eintrag, im Zufallsmodus einen neuen Würfelwurf. Am Listenende wird der
// Folgetitel gelöscht, dann hört die Wiedergabe nach dem letzten Titel auf.
func primeNext() {
	playQueue.mu.Lock()
	shuffle, library, index := playQueue.shuffle, playQueue.library, playQueue.index
	next := jellyfinItem{}
	if !shuffle && index+1 < len(playQueue.items) {
		next = playQueue.items[index+1]
	}
	playQueue.mu.Unlock()

	if shuffle {
		// Der Würfelwurf geht ins Netz, deshalb außerhalb der Sperre.
		userID, err := jellyfinUserID()
		if err != nil {
			log.Printf("Folgetitel: Jellyfin-Benutzerabruf fehlgeschlagen: %v", err)
			return
		}
		next, err = jellyfinRandomTrack(userID, library)
		if err != nil {
			log.Printf("Folgetitel: Zufallstitel konnte nicht ermittelt werden: %v", err)
			return
		}
	}

	// Inzwischen ein anderer Kontext (Skip, Stop, neue Auswahl)? Dann nicht mehr
	// am Renderer drehen — sonst bliebe ein Folgetitel aus dem alten Album übrig.
	if !queueUnchanged(shuffle, library, index, next) {
		return
	}

	if next.ID == "" {
		clearJellyfinNext()
		return
	}

	streamURL, didl := trackDIDL(next)
	if err := upnpSetNext(streamURL, didl); err != nil {
		log.Printf("Folgetitel %q hinterlegen fehlgeschlagen: %v", next.Name, err)
		setQueueNext(jellyfinItem{})
		return
	}
	setQueueNext(next)
}

// queueUnchanged sagt, ob der Wiedergabekontext seit dem Beginn von primeNext
// noch derselbe ist.
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

// autoAdvanceLoop hält die Wiedergabe am Laufen: Sobald der Renderer von selbst
// auf den hinterlegten Folgetitel umgeschaltet hat, rückt die Position in der
// Jellyfin-Liste weiter und der nächste Titel wird hinterlegt.
func autoAdvanceLoop() {
	ticker := time.NewTicker(autoAdvanceInterval)
	defer ticker.Stop()
	for range ticker.C {
		advanceIfNeeded()
	}
}

// advanceIdle zählt aufeinanderfolgende Abfragen ohne brauchbare TrackURI. Nur
// die Ticker-Schleife liest und schreibt den Wert.
var advanceIdle int

// advanceIdleLimit ist erreicht, wenn der Renderer 30 Abfragen lang (~1 min)
// keine Session mehr meldet oder nicht erreichbar ist; dann wird der Folgetitel
// vergessen, statt endlos weiterzufragen.
const advanceIdleLimit = 30

// advanceIfNeeded prüft einmal, ob das Gerät weitergeschaltet hat.
func advanceIfNeeded() {
	playQueue.mu.Lock()
	expected := playQueue.next
	playQueue.mu.Unlock()
	if expected.ID == "" {
		advanceIdle = 0
		return // kein Folgetitel hinterlegt
	}

	current, err := upnpCurrentTrackURI()
	if err != nil || current == "" {
		// Gerät aus, Quelle am Gerät gewechselt oder Session beendet: Der
		// Folgetitel ist dann nicht mehr zu gebrauchen. Nicht jede Runde loggen.
		advanceIdle++
		if advanceIdle == 1 {
			if err != nil {
				log.Printf("Transportabfrage fehlgeschlagen, Folgetitel bleibt vorerst hinterlegt: %v", err)
			} else {
				log.Printf("Renderer meldet keine Wiedergabe, Folgetitel bleibt vorerst hinterlegt")
			}
		}
		if advanceIdle >= advanceIdleLimit {
			log.Printf("Renderer meldet keine Wiedergabe mehr — Folgetitel wird vergessen")
			setQueueNext(jellyfinItem{})
			advanceIdle = 0
		}
		return
	}
	advanceIdle = 0

	if !strings.Contains(current, expected.ID) {
		return // das Gerät spielt noch den laufenden Titel
	}

	// Das Gerät ist weitergeschaltet: Position nachziehen. Der Vergleich unter
	// der Sperre fängt ab, dass inzwischen ein Skip/Stop/Quellenwechsel lief.
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

	log.Printf("Weiter mit %q von %q", expected.Name, expected.artistName())
	primeNext()
}

// shuffleActive sagt, ob gerade Zufallswiedergabe läuft. Das Frontend zeigt damit
// den Zustand des Zufall-Knopfes an (Feld jellyfin_shuffle in /api/playinfo).
func shuffleActive() bool {
	playQueue.mu.Lock()
	defer playQueue.mu.Unlock()
	return playQueue.shuffle
}

// jellyfinCoverFor liefert Album-ID und Image-Tag des laufenden Titels — aber nur,
// wenn er nachweislich zu unserem Jellyfin-Kontext gehört. Verglichen wird der
// Titelname, weil der Renderer auch von außen (MusicCast-App) gestartet werden kann;
// ein fremder Titel darf kein Jellyfin-Cover bekommen.
//
// Grund für den Umweg: Nach dem automatischen Weiterschalten (SetNextAVTransportURI)
// meldet getPlayInfo teils eine neue albumart_id, aber eine leere albumart_url, und
// die AlbumART-Datei dazu ist 404 (gemessen). Beim manuellen Start/Skip legt das
// Gerät das Cover dagegen sofort an. Ohne Ersatz bliebe der Browser ohne Cover.
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
	// Beim Weiterschalten meldet das Gerät schon den neuen Titel, während unsere
	// Position erst im nächsten 2-s-Takt nachzieht — deshalb auch der Folgetitel.
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

// pushShuffleHistoryLocked legt den laufenden Titel in den Verlauf (unter mu).
func pushShuffleHistoryLocked() {
	if playQueue.current.ID == "" {
		return
	}
	playQueue.history = append(playQueue.history, playQueue.current)
	if len(playQueue.history) > shuffleHistoryLimit {
		playQueue.history = playQueue.history[1:]
	}
}

// shuffleStep beschreibt, was ein Skip im Zufallsmodus tun soll.
type shuffleStep struct {
	active  bool   // Zufallswiedergabe läuft
	moved   bool   // es gibt etwas zu spielen
	roll    bool   // track ist leer: der Aufrufer muss einen neuen Titel würfeln
	library string // Bibliothek, aus der gewürfelt wird
	track   jellyfinItem
}

// planShuffleStep bereitet einen Skip im Zufallsmodus vor, ohne dabei ins Netz zu
// gehen (der Netzaufruf passiert außerhalb der Sperre). ⏭ schiebt den laufenden
// Titel in den Verlauf und verlangt einen neuen; ⏮ nimmt den zuletzt gespielten.
func planShuffleStep(delta int) shuffleStep {
	playQueue.mu.Lock()
	defer playQueue.mu.Unlock()
	if !playQueue.shuffle {
		return shuffleStep{}
	}

	step := shuffleStep{active: true, library: playQueue.library}
	if delta < 0 {
		if len(playQueue.history) == 0 {
			return step // Anfang der Zufallsreihe: nicht springen
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

// setShuffleCurrent merkt sich den neu gewürfelten Titel.
func setShuffleCurrent(track jellyfinItem) {
	playQueue.mu.Lock()
	defer playQueue.mu.Unlock()
	playQueue.current = track
}

// stepJellyfinQueue verschiebt die Position um delta. hasQueue sagt, ob
// überhaupt eine Jellyfin-Liste aktiv ist; moved, ob die neue Position gültig war.
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

// handleJellyfinSkip wechselt innerhalb der aktiven Jellyfin-Liste. Läuft die
// Zufallswiedergabe, würfelt sie stattdessen neu; ohne Jellyfin-Kontext (z. B.
// Internetradio, USB) wird der Befehl an das Gerät durchgereicht.
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
		// Anfang/Ende der Liste erreicht: nicht springen.
		writeSkipResult(w, false, jellyfinItem{})
		return
	}

	if err := startJellyfinTrack(track); err != nil {
		log.Printf("Titelwechsel auf %q fehlgeschlagen: %v", track.Name, err)
		http.Error(w, "Titelwechsel fehlgeschlagen", http.StatusBadGateway)
		return
	}
	primeNext()
	log.Printf("Wechsle zu %q von %q", track.Name, track.artistName())
	writeSkipResult(w, true, track)
}

// handleShuffleSkip bedient ⏭/⏮ im Zufallsmodus. Der neue Titel wird erst hier
// gewürfelt, also außerhalb der Sperre in planShuffleStep.
func handleShuffleSkip(w http.ResponseWriter, step shuffleStep) {
	if !step.moved {
		writeSkipResult(w, false, jellyfinItem{})
		return
	}

	track := step.track
	if step.roll {
		userID, err := jellyfinUserID()
		if err != nil {
			log.Printf("Jellyfin-Benutzerabruf fehlgeschlagen: %v", err)
			http.Error(w, "Jellyfin nicht erreichbar", http.StatusInternalServerError)
			return
		}
		track, err = jellyfinRandomTrack(userID, step.library)
		if err != nil {
			log.Printf("Zufallstitel konnte nicht ermittelt werden: %v", err)
			http.Error(w, "Zufallstitel konnte nicht ermittelt werden", http.StatusBadGateway)
			return
		}
		setShuffleCurrent(track)
	}

	if err := startJellyfinTrack(track); err != nil {
		log.Printf("Titelwechsel auf %q fehlgeschlagen: %v", track.Name, err)
		http.Error(w, "Titelwechsel fehlgeschlagen", http.StatusBadGateway)
		return
	}
	primeNext()
	log.Printf("Zufall: %q von %q", track.Name, track.artistName())
	writeSkipResult(w, true, track)
}

// handleJellyfinRandom startet einen zufälligen Titel und schaltet auf
// Zufallswiedergabe um, damit weitere ⏭ wieder würfeln. Die Bibliothek kann per
// parent_id mitgegeben werden (die gerade durchstöberte), sonst gilt die erste
// Musik-Bibliothek.
func handleJellyfinRandom(w http.ResponseWriter, r *http.Request) {
	userID, err := jellyfinUserID()
	if err != nil {
		log.Printf("Jellyfin-Benutzerabruf fehlgeschlagen: %v", err)
		http.Error(w, "Jellyfin nicht erreichbar", http.StatusInternalServerError)
		return
	}

	library, err := jellyfinLibraryID(userID, strings.TrimSpace(r.URL.Query().Get("parent_id")))
	if err != nil {
		log.Printf("Musik-Bibliothek nicht bestimmbar: %v", err)
		http.Error(w, "Keine Musik-Bibliothek gefunden", http.StatusBadGateway)
		return
	}

	track, err := jellyfinRandomTrack(userID, library)
	if err != nil {
		log.Printf("Zufallstitel konnte nicht ermittelt werden: %v", err)
		http.Error(w, "Zufallstitel konnte nicht ermittelt werden", http.StatusBadGateway)
		return
	}

	setJellyfinShuffle(library, track)

	if err := startJellyfinTrack(track); err != nil {
		log.Printf("Wiedergabe von %q fehlgeschlagen: %v", track.Name, err)
		http.Error(w, "Wiedergabe konnte nicht gestartet werden", http.StatusBadGateway)
		return
	}
	primeNext()

	log.Printf("Spiele Zufallstitel %q von %q", track.Name, track.artistName())
	writeTrackResponse(w, track)
}

// writeSkipResult meldet, ob ein Skip die Wiedergabe verschoben hat.
func writeSkipResult(w http.ResponseWriter, moved bool, track jellyfinItem) {
	result := map[string]any{"response_code": 0, "moved": moved}
	if moved {
		result["track"] = track.Name
		result["artist"] = track.artistName()
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Printf("Antwort konnte nicht geschrieben werden: %v", err)
	}
}

func handleJellyfinPlay(w http.ResponseWriter, r *http.Request) {
	itemID := r.URL.Query().Get("item_id")
	if itemID == "" {
		http.Error(w, "Parameter item_id fehlt", http.StatusBadRequest)
		return
	}

	userID, err := jellyfinUserID()
	if err != nil {
		log.Printf("Jellyfin-Benutzerabruf fehlgeschlagen: %v", err)
		http.Error(w, "Jellyfin nicht erreichbar", http.StatusInternalServerError)
		return
	}

	track, err := resolveJellyfinTrack(userID, itemID)
	if err != nil {
		log.Printf("Titel %q konnte nicht aufgelöst werden: %v", itemID, err)
		http.Error(w, "Titel nicht gefunden", http.StatusNotFound)
		return
	}

	// Kontext für Skip/Prev merken: geordnete Titelliste + Position. Das beendet
	// zugleich einen laufenden Zufallsmodus.
	queue, index := jellyfinPlayContext(userID, itemID, track)
	setJellyfinQueue(queue, index)

	if err := startJellyfinTrack(track); err != nil {
		log.Printf("Wiedergabe von %q fehlgeschlagen: %v", track.Name, err)
		http.Error(w, "Wiedergabe konnte nicht gestartet werden", http.StatusBadGateway)
		return
	}
	primeNext()

	log.Printf("Spiele Jellyfin-Titel %q von %q", track.Name, track.artistName())
	writeTrackResponse(w, track)
}

// writeTrackResponse meldet den gestarteten Titel als JSON.
func writeTrackResponse(w http.ResponseWriter, track jellyfinItem) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"response_code": 0,
		"track":         track.Name,
		"artist":        track.artistName(),
	}); err != nil {
		log.Printf("Antwort konnte nicht geschrieben werden: %v", err)
	}
}
