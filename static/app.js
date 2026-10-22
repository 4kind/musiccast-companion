let availableInputs = [];
let statusTimer = null;
let statusBusy = false;
// Name des aktuell laufenden Titels (aus getPlayInfo). Dient dazu, den Titel in
// den Suchergebnissen zu markieren; leer = nichts läuft.
let nowPlayingTrack = "";

// Deutsche Beschriftungen für die Eingangs-IDs des Geräts. Unbekannte IDs
// werden unverändert angezeigt.
const INPUT_LABELS = {
    server: "Server (Jellyfin)",
    net_radio: "Internetradio",
    airplay: "AirPlay",
    bluetooth: "Bluetooth",
    usb: "USB",
    tuner: "Radio (Tuner)",
    optical1: "Optisch 1",
    line1: "Line 1",
};

// --- Kleine API-Helfer --------------------------------------------------------

async function apiGet(path) {
    const response = await fetch(path);
    if (!response.ok) throw new Error(`GET ${path} fehlgeschlagen (${response.status})`);
    return response.json();
}

async function apiPost(path) {
    const response = await fetch(path, { method: "POST" });
    if (!response.ok) throw new Error(`POST ${path} fehlgeschlagen (${response.status})`);
    return response.json();
}

// --- Steuerung ----------------------------------------------------------------

async function sendCommand(action) {
    try {
        const data = await apiPost(`/api/${action}`);
        console.log("Antwort:", data);
        await fetchStatus();
    } catch (error) {
        console.error("Fehler beim Senden des Befehls:", error);
    }
}

async function changeInput(inputId) {
    if (!inputId) return;
    try {
        await apiPost(`/api/input?id=${encodeURIComponent(inputId)}`);
        await fetchStatus();
    } catch (error) {
        console.error("Fehler beim Umschalten des Eingangs:", error);
    }
}

async function setVolume(volumeVal) {
    document.getElementById("val-volume").textContent = volumeVal;
    try {
        await apiPost(`/api/volume?val=${encodeURIComponent(volumeVal)}`);
    } catch (error) {
        console.error("Fehler beim Ändern der Lautstärke:", error);
    }
}

// --- Status & Medien-Info -----------------------------------------------------

async function fetchInputs() {
    try {
        availableInputs = await apiGet("/api/inputs");
        updateInputDropdown();
    } catch (error) {
        console.error("Fehler beim Laden der Eingänge:", error);
    }
}

function updateInputDropdown(currentInput = "") {
    const select = document.getElementById("input-select");
    if (availableInputs.length === 0) return;

    select.textContent = "";
    availableInputs.forEach((input) => {
        const option = document.createElement("option");
        option.value = input;
        option.textContent = INPUT_LABELS[input] || input;
        if (input === currentInput) option.selected = true;
        select.appendChild(option);
    });
}

async function fetchStatus() {
    // Überlappende Läufe vermeiden, falls das Gerät langsamer antwortet als
    // das Poll-Intervall.
    if (statusBusy) return;
    statusBusy = true;
    try {
        const data = await apiGet("/api/status");

        document.getElementById("val-power").textContent = data.power || "Unbekannt";

        // Lautstärkebereich kommt vom Gerät (max_volume), nicht aus dem Code.
        const slider = document.getElementById("volume-slider");
        if (data.max_volume) slider.max = data.max_volume;

        const volumeEl = document.getElementById("val-volume");
        if (data.volume !== undefined) {
            volumeEl.textContent = data.volume;
            if (document.activeElement !== slider) slider.value = data.volume;
        } else {
            volumeEl.textContent = "Unbekannt";
        }

        if (data.input) {
            document.getElementById("input-select").value = data.input;
        }

        await updateMediaInfo(data.input);
    } catch (error) {
        console.error("Fehler beim Laden des Status:", error);
    } finally {
        statusBusy = false;
    }
}

// Vergleicht Titelnamen tolerant (Groß-/Kleinschreibung, Mehrfach-Leerzeichen).
function normalizeTitle(name) {
    return (name || "").trim().toLowerCase().replace(/\s+/g, " ");
}

// Merkt sich den laufenden Titel und aktualisiert die Markierung in der Trefferliste.
function setNowPlaying(track) {
    nowPlayingTrack = track || "";
    updatePlayingHighlights();
}

// Markiert alle Treffer/Albumtitel, deren Name zum laufenden Titel passt.
// Der laufende Titel stammt aus getPlayInfo (kein Jellyfin-Item-ID-Abgleich),
// damit die Markierung auch nach einem Reload oder fremd gestarteter Wiedergabe stimmt.
function updatePlayingHighlights() {
    const key = normalizeTitle(nowPlayingTrack);
    document.querySelectorAll("#search-results [data-track-name], #browse-results [data-track-name]").forEach((el) => {
        const match = key !== "" && normalizeTitle(el.dataset.trackName) === key;
        el.classList.toggle("track-playing", match);
    });
}

// --- Laufzeit-Anzeige der laufenden Wiedergabe --------------------------------

// Yamaha liefert play_time/total_time in Sekunden. Zwischen den 5-s-Polls zählt
// die Anzeige lokal weiter, damit sie nicht springt; jeder Poll setzt sie mit
// dem Gerätewert neu. Bei Pause steht sie still (progressRunning = false).
let progressTimer = null;
let progressRunning = false;
let progressElapsed = 0;
let progressTotal = 0;
let progressSyncedAt = 0;

function renderProgress() {
    const extra = progressRunning ? Math.floor((performance.now() - progressSyncedAt) / 1000) : 0;
    const elapsed = progressTotal > 0
        ? Math.min(progressElapsed + extra, progressTotal)
        : progressElapsed + extra;
    document.getElementById("val-elapsed").textContent = formatSeconds(elapsed);
    document.getElementById("val-total").textContent = formatSeconds(progressTotal);
    const percent = progressTotal > 0 ? Math.min((elapsed / progressTotal) * 100, 100) : 0;
    document.getElementById("val-progress").style.width = `${percent}%`;
}

function stopProgress() {
    if (progressTimer !== null) {
        clearInterval(progressTimer);
        progressTimer = null;
    }
    progressRunning = false;
}

// Zeitzeile synchronisieren; running=false friert die Anzeige ein (Pause).
function syncProgress(elapsed, total, running) {
    progressElapsed = elapsed;
    progressTotal = total;
    progressSyncedAt = performance.now();
    if (running) {
        progressRunning = true;
        if (progressTimer === null) {
            progressTimer = setInterval(renderProgress, 1000);
        }
    } else {
        stopProgress();
    }
    renderProgress();
}

// Zeitzeile ausblenden (kein Net/USB-Eingang, gestoppt oder keine Dauer bekannt,
// z. B. Radio ohne total_time).
function hideTimeRow() {
    document.getElementById("time-row").classList.add("hidden");
    stopProgress();
}

async function updateMediaInfo(input) {
    const mediaContainer = document.getElementById("media-info");
    const netInputs = ["server", "net_radio", "usb"];

    if (!netInputs.includes(input)) {
        mediaContainer.classList.add("hidden");
        hideTimeRow();
        setNowPlaying("");
        // Kein Net-Eingang: ein Quellenwechsel löscht den Jellyfin-Kontext,
        // also läuft auch keine Zufallswiedergabe mehr.
        setShuffleIndicator(false);
        return;
    }

    try {
        const dataPlay = await apiGet("/api/playinfo");
        setShuffleIndicator(dataPlay.jellyfin_shuffle === true);
        if (dataPlay.response_code === 0 && dataPlay.playback !== "stop") {
            mediaContainer.classList.remove("hidden");
            setNowPlaying(dataPlay.track);
            document.getElementById("val-track").textContent = dataPlay.track || "Unbekannter Titel";
            document.getElementById("val-artist").textContent = dataPlay.artist || "";
            document.getElementById("val-album").textContent = dataPlay.album || "";

            const imgEl = document.getElementById("val-art");
            // Cover-Quelle: Bevorzugt das Gerät (albumart_url) — das ist das Cover
            // der laufenden Datei. Nach dem automatischen Weiterschalten liefert das
            // Gerät die URL aber teils gar nicht (neue albumart_id, Datei 404).
            // Für eigene Jellyfin-Titel schickt das Backend deshalb Album-ID und
            // Image-Tag mit; damit erscheint das Cover auch ohne Geräte-URL.
            const artSrc = dataPlay.albumart_url
                ? `/api/albumart?path=${encodeURIComponent(dataPlay.albumart_url)}`
                : dataPlay.jellyfin_album_id
                  ? `/api/jellyfin/image?id=${encodeURIComponent(dataPlay.jellyfin_album_id)}` +
                    `&tag=${encodeURIComponent(dataPlay.jellyfin_image_tag || "")}`
                  : "";
            if (artSrc && imgEl.dataset.src !== artSrc) {
                // Cover-Art läuft über den Go-Proxy, nicht direkt zur Yamaha.
                // Nur bei Wechsel setzen, sonst lädt der Browser das Bild bei
                // jedem 5-s-Poll neu.
                imgEl.dataset.src = artSrc;
                imgEl.src = artSrc;
                imgEl.classList.remove("hidden");
            } else if (!artSrc) {
                delete imgEl.dataset.src;
                imgEl.classList.add("hidden");
            }

            // play_time ist negativ, wenn die Position ungültig ist (-60000).
            const elapsed = Math.max(dataPlay.play_time || 0, 0);
            const total = Math.max(dataPlay.total_time || 0, 0);
            if (total > 0) {
                document.getElementById("time-row").classList.remove("hidden");
                // Nur bei laufender Wiedergabe weiterzählen, nicht bei Pause.
                syncProgress(elapsed, total, dataPlay.playback === "play");
            } else {
                hideTimeRow();
            }
        } else {
            mediaContainer.classList.add("hidden");
            hideTimeRow();
            setNowPlaying("");
        }
    } catch (error) {
        console.error("Fehler beim Laden der Wiedergabe-Infos:", error);
    }
}

// --- Jellyfin-Suche -----------------------------------------------------------

// Debounce fürs Suchen beim Tippen.
let searchTimer = null;

function scheduleSearch() {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(searchJellyfin, 300);
}

async function searchJellyfin() {
    const query = document.getElementById("search-input").value.trim();

    if (!query) {
        document.getElementById("search-results").classList.add("hidden");
        document.getElementById("search-filters").classList.add("hidden");
        return;
    }

    try {
        const data = await apiGet(`/api/jellyfin/search?q=${encodeURIComponent(query)}`);
        renderSearchResults({
            albums: data.albums || [],
            artists: data.artists || [],
            tracks: data.tracks || [],
        }, query);
    } catch (error) {
        console.error("Fehler bei der Jellyfin-Suche:", error);
        renderSearchMessage("Suche fehlgeschlagen.");
    }
}

// Zentrierte Meldung statt Trefferliste (leer, Fehler, …).
function renderSearchMessage(message) {
    const resultsContainer = document.getElementById("search-results");
    document.getElementById("search-filters").classList.add("hidden");
    resultsContainer.textContent = "";
    resultsContainer.classList.remove("hidden");
    const p = document.createElement("p");
    p.className = "list-hint";
    p.textContent = message;
    resultsContainer.appendChild(p);
}

// Sekunden als m:ss bzw. h:mm:ss (leer bei ungültigen Werten).
function formatSeconds(seconds) {
    if (!Number.isFinite(seconds) || seconds < 0) return "";
    const total = Math.floor(seconds);
    const hours = Math.floor(total / 3600);
    const minutes = Math.floor((total % 3600) / 60);
    const secs = total % 60;
    const pad = (n) => String(n).padStart(2, "0");
    return hours > 0 ? `${hours}:${pad(minutes)}:${pad(secs)}` : `${minutes}:${pad(secs)}`;
}

// Jellyfin liefert Laufzeiten in 100-ns-Ticks (10.000.000 = 1 Sekunde).
function formatDuration(ticks) {
    if (!ticks || ticks <= 0) return "";
    return formatSeconds(Math.round(ticks / 10000000));
}

// Vorlage klonen und anschließend nur mit Text füllen (kein innerHTML).
function fromTemplate(id) {
    return document.getElementById(id).content.firstElementChild.cloneNode(true);
}

// Cover in die Kachel setzen; ohne Bild-Tag bleibt der graue Platzhalter.
function fillCover(slot, item) {
    const tag = item.ImageTags && item.ImageTags.Primary;
    if (!tag) return;
    const img = document.createElement("img");
    img.className = "cover-img";
    img.alt = "";
    img.loading = "lazy";
    img.src = `/api/jellyfin/image?id=${encodeURIComponent(item.Id)}&tag=${encodeURIComponent(tag)}`;
    img.addEventListener("error", () => img.remove());
    slot.appendChild(img);
}

function subtitle(parts) {
    return parts.filter(Boolean).join(" · ");
}

function artistNames(item) {
    return item.Artists ? item.Artists.join(", ") : (item.AlbumArtist || "");
}

// Dauer rechts ausgeben; ohne Dauer entfällt die Spalte ganz.
function setDuration(row, ticks) {
    const el = row.querySelector('[data-slot="duration"]');
    const text = formatDuration(ticks);
    if (text) el.textContent = text;
    else el.remove();
}

function createAlbumRow(album) {
    const row = fromTemplate("tpl-album");
    row.dataset.id = album.Id;
    fillCover(row.querySelector('[data-slot="cover"]'), album);
    row.querySelector('[data-slot="title"]').textContent = album.Name || "Unbekannt";
    row.querySelector('[data-slot="sub"]').textContent = subtitle([artistNames(album), album.ProductionYear]);
    setDuration(row, album.RunTimeTicks);
    const play = row.querySelector('[data-action="play"]');
    play.dataset.id = album.Id;
    play.dataset.type = album.Type || "MusicAlbum";
    return row;
}

function createArtistRow(artist) {
    const row = fromTemplate("tpl-artist");
    row.dataset.id = artist.Id;
    fillCover(row.querySelector('[data-slot="cover"]'), artist);
    row.querySelector('[data-slot="title"]').textContent = artist.Name || "Unbekannt";
    row.querySelector('[data-slot="sub"]').textContent = "Interpret";
    const play = row.querySelector('[data-action="play"]');
    play.dataset.id = artist.Id;
    play.dataset.type = artist.Type || "MusicArtist";
    return row;
}

function createTrackRow(track, index) {
    const row = fromTemplate("tpl-track");
    // Die Titelzeile ist selbst der "Abspielen"-Treffer (data-action="play"),
    // daher direkt auf der Zeile setzen statt per querySelector.
    row.dataset.id = track.Id;
    row.dataset.type = "Audio";
    // Nur echte Titelzeilen kommen für die "läuft gerade"-Markierung in Frage.
    row.dataset.trackName = track.Name || "";
    row.querySelector('[data-slot="number"]').textContent = track.IndexNumber || index + 1;
    fillCover(row.querySelector('[data-slot="cover"]'), track);
    row.querySelector('[data-slot="title"]').textContent = track.Name || "Unbekannt";
    row.querySelector('[data-slot="sub"]').textContent = subtitle([artistNames(track), track.Album]);
    setDuration(row, track.RunTimeTicks);
    return row;
}

function createAlbumTrackLine(track, index) {
    const line = fromTemplate("tpl-album-track");
    line.dataset.id = track.Id;
    line.dataset.trackName = track.Name || "";
    const number = track.IndexNumber ? `${track.IndexNumber}. ` : `${index + 1}. `;
    line.querySelector('[data-slot="title"]').textContent = number + (track.Name || "Unbekannt");
    setDuration(line, track.RunTimeTicks);
    return line;
}

// Hinweistext im aufgeklappten Bereich (wird geladen, leer, Fehler).
function setBodyMessage(body, text) {
    body.textContent = "";
    const p = document.createElement("p");
    p.className = "row-body-hint";
    p.textContent = text;
    body.appendChild(p);
}

function renderSearchResults(data, query) {
    const resultsContainer = document.getElementById("search-results");
    const total = data.albums.length + data.artists.length + data.tracks.length;

    if (total === 0) {
        renderSearchMessage(`Keine Treffer für „${query}“.`);
        return;
    }

    resultsContainer.textContent = "";
    resultsContainer.classList.remove("hidden");

    // Abschnitte in fester Reihenfolge: Alben, Interpreten, Titel. Leere Gruppen
    // bekommen keine Überschrift. Welcher Abschnitt sichtbar ist, regelt das CSS.
    const groups = [
        ["albums", "Alben", data.albums, createAlbumRow],
        ["artists", "Interpreten", data.artists, createArtistRow],
        ["tracks", "Titel", data.tracks, createTrackRow],
    ];
    groups.forEach(([key, label, items, createRow]) => {
        if (items.length === 0) return;
        const section = fromTemplate("tpl-section");
        section.dataset.section = key;
        section.querySelector("[data-section-label]").textContent = `${label} (${items.length})`;
        const list = section.querySelector("[data-section-list]");
        items.forEach((item, i) => list.appendChild(createRow(item, i)));
        resultsContainer.appendChild(section);
    });

    updateFilterChips(data);
    updatePlayingHighlights();
}

// Chips sind Radios + Labels im HTML; hier nur Anzahl und Ausblenden leerer Gruppen.
// Die Sichtbarkeit der Abschnitte regelt CSS, deshalb gibt es hier kein Filtern.
function updateFilterChips(data) {
    document.getElementById("filter-all").checked = true;

    [["albums", data.albums.length], ["artists", data.artists.length], ["tracks", data.tracks.length]]
        .forEach(([key, count]) => {
            const input = document.getElementById("filter-" + key);
            const label = document.querySelector(`label[for="filter-${key}"]`);
            const empty = count === 0;
            input.disabled = empty;
            label.hidden = empty;
            label.querySelector("[data-count]").textContent = `(${count})`;
        });

    document.getElementById("search-filters").classList.remove("hidden");
}

// Klick-Delegation für eine Trefferliste (Suche oder Stöbern): „Abspielen" (oder
// eine Titelzeile) startet die Wiedergabe; ein Klick daneben klappt das <details>
// nativ auf.
function bindResultList(container) {
    container.addEventListener("click", (event) => {
        const play = event.target.closest('[data-action="play"]');
        if (!play) return;
        // Kein Auf-/Zuklappen, wenn im <summary> auf „Abspielen" geklickt wird.
        event.preventDefault();
        event.stopPropagation();
        if (play.dataset.id) playJellyfinItem(play.dataset.id, play.dataset.type || "Audio", container);
    });

    // Lazy-Load beim Aufklappen. Das toggle-Ereignis blubbert nicht, deshalb
    // lauscht die Delegation in der Capture-Phase.
    container.addEventListener("toggle", (event) => {
        const details = event.target;
        if (!details.open || details.dataset.loaded === "1") return;
        if (details.dataset.kind === "album") loadAlbumTracks(details);
        else if (details.dataset.kind === "artist") loadArtistAlbums(details);
    }, true);
}

async function loadAlbumTracks(details) {
    const body = details.querySelector('[data-slot="body"]');
    setBodyMessage(body, "Lade Titel…");
    try {
        const tracks = await apiGet(`/api/jellyfin/tracks?parent_id=${encodeURIComponent(details.dataset.id)}`);
        details.dataset.loaded = "1";
        if (tracks.length === 0) {
            setBodyMessage(body, "Keine Titel enthalten.");
            return;
        }
        body.textContent = "";
        tracks.forEach((track, i) => body.appendChild(createAlbumTrackLine(track, i)));
        updatePlayingHighlights();
    } catch (error) {
        console.error("Fehler beim Laden der Album-Titel:", error);
        setBodyMessage(body, "Titel konnten nicht geladen werden.");
    }
}

// Klappt unter einem Interpreten dessen Alben auf — selbst wieder aufklappbar.
// Statt der rekursiven Titelliste eines Interpreten, die viel zu groß wäre.
async function loadArtistAlbums(details) {
    const body = details.querySelector('[data-slot="body"]');
    setBodyMessage(body, "Lade Alben…");
    try {
        const data = await apiGet(`/api/jellyfin/browse?kind=albums&artist_id=${encodeURIComponent(details.dataset.id)}&limit=100`);
        const albums = data.items || [];
        details.dataset.loaded = "1";
        if (albums.length === 0) {
            setBodyMessage(body, "Keine Alben gefunden.");
            return;
        }
        body.textContent = "";
        albums.forEach((album) => body.appendChild(createAlbumRow(album)));
        updatePlayingHighlights();
    } catch (error) {
        console.error("Fehler beim Laden der Alben:", error);
        setBodyMessage(body, "Alben konnten nicht geladen werden.");
    }
}

// Startet einen konkreten Jellyfin-Titel auf dem Receiver. Das Backend gibt dem
// Gerät dazu die Jellyfin-Stream-URL über UPnP AVTransport (SetAVTransportURI +
// Play); Album/Interpret werden dort auf ihren ersten Titel aufgelöst.
async function playJellyfinItem(itemId, itemType, container) {
    try {
        const data = await apiPost(`/api/jellyfin/play?item_id=${encodeURIComponent(itemId)}`);
        console.log(`Wiedergabe gestartet: ${data.track || itemId} (${itemType})`);
        await fetchStatus();
    } catch (error) {
        console.error("Fehler beim Starten der Wiedergabe:", error);
        if (container) showHint(container, "Wiedergabe konnte nicht gestartet werden.");
    }
}

// Kurzer Hinweis unter einer Liste (Fehler beim Abspielen).
function showHint(container, message) {
    const hint = document.createElement("p");
    hint.className = "list-hint list-hint-warn";
    hint.textContent = message;
    container.appendChild(hint);
}

// Startet ein zufälliges Lied und schaltet die Zufallswiedergabe ein; ⏭ würfelt
// danach weiter. Gewürfelt wird in der gerade durchstöberten Bibliothek (der
// Endpunkt nimmt sonst die erste Musik-Bibliothek).
async function playRandomTrack() {
    const library = browse.library ? `?parent_id=${encodeURIComponent(browse.library)}` : "";
    try {
        const data = await apiPost(`/api/jellyfin/random${library}`);
        console.log(`Zufallswiedergabe: ${data.track} — ${data.artist}`);
        await fetchStatus();
    } catch (error) {
        console.error("Zufallswiedergabe fehlgeschlagen:", error);
        showHint(document.getElementById("browse-results"), "Zufallslied konnte nicht gestartet werden.");
    }
}

// Zustand des Zufall-Knopfes. Quelle ist /api/playinfo; das Feld heißt
// jellyfin_shuffle, weil `shuffle` dort schon der Shuffle der Geräte-Queue ist.
function setShuffleIndicator(active) {
    document.getElementById("btn-random").setAttribute("aria-pressed", active ? "true" : "false");
}

// --- Jellyfin Stöbern ---------------------------------------------------------

// Jellyfin liefert große Seiten in einem Aufruf (gemessen: 200 Alben in ~98 ms),
// deshalb 100 Zeilen pro Nachladeschritt statt der 8, die die YXC-Liste erzwingt.
const BROWSE_PAGE_SIZE = 100;

// Zustand der Stöber-Liste. `start` ist der Offset der nächsten Seite, `total`
// die Gesamtzahl aus TotalRecordCount (nur so weiß das Nachladen, wann Schluss ist).
const browse = {
    library: "",
    kind: "artists",
    sort: "name",
    letter: "",
    start: 0,
    total: 0,
    loading: false,
};

let browseObserver = null;
let browseRequest = 0;

// Einstieg ins Stöbern: Bibliotheken holen und die erste Seite laden. Die
// Bibliotheks-Auswahl entfällt bei nur einer Bibliothek — sie wäre eine Auswahl
// ohne Alternative.
async function initBrowse() {
    try {
        const views = await apiGet("/api/jellyfin/views");
        if (views.length > 0) browse.library = views[0].id;
        if (views.length > 1) {
            // Chips aus Radios + Labels erst zur Laufzeit: Die Zahl der
            // Bibliotheken steht erst nach dem Abruf fest.
            const container = document.getElementById("browse-libraries");
            views.forEach((view, index) => {
                const input = document.createElement("input");
                input.type = "radio";
                input.name = "browse-library";
                input.id = `browse-lib-${view.id}`;
                input.className = "sr-only";
                input.value = view.id;
                input.checked = index === 0;

                const label = document.createElement("label");
                label.htmlFor = input.id;
                label.textContent = view.name;

                container.append(input, label);
            });
            container.classList.remove("hidden");
        }
    } catch (error) {
        console.error("Bibliotheken konnten nicht geladen werden:", error);
    }
    await loadBrowsePage(true);
}

function bindBrowseControls() {
    document.getElementById("browse-libraries").addEventListener("change", (event) => {
        browse.library = event.target.value;
        loadBrowsePage(true);
    });
    document.getElementById("browse-kinds").addEventListener("change", (event) => {
        browse.kind = event.target.value;
        loadBrowsePage(true);
    });
    document.getElementById("browse-sorts").addEventListener("change", (event) => {
        browse.sort = event.target.value;
        loadBrowsePage(true);
    });
    document.getElementById("browse-letters").addEventListener("click", (event) => {
        const chip = event.target.closest("button[data-letter]");
        if (!chip) return;
        browse.letter = chip.dataset.letter;
        document.querySelectorAll("#browse-letters .chip").forEach((button) => {
            button.setAttribute("aria-pressed", button === chip ? "true" : "false");
        });
        loadBrowsePage(true);
    });

    bindResultList(document.getElementById("browse-results"));

    // Nachladen ohne Scroll-Listener: der Sentinel am Listenende wird per
    // IntersectionObserver beobachtet. rootMargin lädt eine Seite voraus, damit
    // die ~60-100 ms Antwortzeit nicht als Ruckeln sichtbar wird.
    const results = document.getElementById("browse-results");
    browseObserver = new IntersectionObserver((entries) => {
        if (entries.some((entry) => entry.isIntersecting)) loadBrowsePage(false);
    }, { root: results, rootMargin: "200px 0px" });
}

// Lädt die nächste Seite; reset beginnt die Liste neu (andere Bibliothek, Art,
// Sortierung oder Buchstabe). Über browseRequest wird eine veraltete Antwort
// verworfen, wenn zwischendurch umgeschaltet wurde.
async function loadBrowsePage(reset) {
    if (browse.loading && !reset) return;
    // Am Listenende gibt es nichts mehr zu holen. Die Prüfung sitzt hier und nicht
    // im Observer, weil der Sentinel am Ende sichtbar bleibt und sonst immer
    // wieder auslösen würde (bei leerer Liste endlos).
    if (!reset && browse.start >= browse.total) return;

    const results = document.getElementById("browse-results");
    const sentinel = document.getElementById("browse-sentinel");
    const requestId = ++browseRequest;

    if (reset) {
        browse.start = 0;
        browse.total = 0;
        results.querySelectorAll("details, p").forEach((el) => el.remove());
        setBrowseStatus("Lade…");
    }

    const params = new URLSearchParams({
        kind: browse.kind,
        sort: browse.sort,
        start: String(browse.start),
        limit: String(BROWSE_PAGE_SIZE),
    });
    if (browse.library) params.set("parent_id", browse.library);
    if (browse.letter) params.set("letter", browse.letter);

    browse.loading = true;
    try {
        const data = await apiGet(`/api/jellyfin/browse?${params}`);
        if (requestId !== browseRequest) return;
        browse.total = data.total || 0;
        const items = data.items || [];
        browse.start += items.length;

        items.forEach((item) => {
            const row = browse.kind === "artists" ? createArtistRow(item) : createAlbumRow(item);
            results.insertBefore(row, sentinel);
        });
        updatePlayingHighlights();
        renderBrowseStatus(items.length === 0 && browse.start === 0);
    } catch (error) {
        if (requestId !== browseRequest) return;
        console.error("Stöbern fehlgeschlagen:", error);
        setBrowseStatus("Liste konnte nicht geladen werden.");
    } finally {
        if (requestId !== browseRequest) return;
        browse.loading = false;
        // Erneut beobachten: das löst eine erste Prüfung aus. Ohne das würde eine
        // Seite, die den Platz noch nicht füllt, kein weiteres Nachladen anstoßen
        // (der Sentinel bleibt sichtbar, das Ereignis feuert nur beim Wechsel).
        browseObserver.unobserve(sentinel);
        browseObserver.observe(sentinel);
    }
}

function renderBrowseStatus(empty) {
    if (empty) {
        setBrowseStatus("Keine Einträge.");
        return;
    }
    if (browse.start >= browse.total) {
        setBrowseStatus(`${browse.total} Einträge`);
        return;
    }
    setBrowseStatus(`${browse.start} von ${browse.total} geladen`);
}

function setBrowseStatus(text) {
    const status = document.getElementById("browse-status");
    status.textContent = text;
    status.classList.toggle("hidden", !text);
}

// --- Polling & Initialisierung ------------------------------------------------

function startStatusPolling() {
    if (statusTimer !== null) return;
    statusTimer = setInterval(fetchStatus, 5000);
}

function stopStatusPolling() {
    if (statusTimer === null) return;
    clearInterval(statusTimer);
    statusTimer = null;
}

function bindControls() {
    document.getElementById("input-select").addEventListener("change", (event) => changeInput(event.target.value));
    document.getElementById("volume-slider").addEventListener("input", (event) => setVolume(event.target.value));
    document.getElementById("btn-play").addEventListener("click", () => sendCommand("play"));
    document.getElementById("btn-stop").addEventListener("click", () => sendCommand("stop"));
    document.getElementById("btn-prev").addEventListener("click", () => sendCommand("prev"));
    document.getElementById("btn-next").addEventListener("click", () => sendCommand("next"));
    document.getElementById("btn-random").addEventListener("click", () => playRandomTrack());
    document.getElementById("btn-search").addEventListener("click", () => searchJellyfin());
    document.getElementById("search-input").addEventListener("input", () => scheduleSearch());
    document.getElementById("search-input").addEventListener("keydown", (event) => {
        if (event.key === "Enter") {
            clearTimeout(searchTimer);
            searchJellyfin();
        }
    });
    bindResultList(document.getElementById("search-results"));
    bindBrowseControls();

    // Kaputtes/leeres Cover (z. B. veraltete AlbumART-ID am Gerät) ohne
    // "Bild"-Icon ausblenden und beim nächsten Poll erneut versuchen.
    const artEl = document.getElementById("val-art");
    artEl.addEventListener("error", () => {
        // Nur echte Cover-Ladefehler behandeln: ein leerer src wird vom Browser
        // zur Seiten-URL aufgelöst und würde sonst eine falsche Meldung erzeugen.
        if (!artEl.dataset.src) return;
        console.warn("Cover-Art konnte nicht geladen werden:", artEl.src);
        delete artEl.dataset.src;
        artEl.classList.add("hidden");
    });
}

async function init() {
    await fetchInputs();
    await fetchStatus();
    startStatusPolling();
    await initBrowse();

    // Im Hintergrund-Tab nicht weiter pollen.
    document.addEventListener("visibilitychange", () => {
        if (document.hidden) {
            stopStatusPolling();
        } else {
            fetchStatus();
            startStatusPolling();
        }
    });
}

bindControls();
init();
