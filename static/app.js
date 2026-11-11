let availableInputs = [];
let availableYamahaDevices = [];
let statusTimer = null;
let statusBusy = false;
// Name of the currently playing track (from getPlayInfo). Used to highlight the track
// in search results; empty = nothing playing.
let nowPlayingTrack = "";
// LocalStorage key for the selected Yamaha IP
const YAMAHA_IP_KEY = "musiccast_selected_ip";

// English labels for input IDs of the device. Unknown IDs are displayed as-is.
const INPUT_LABELS = {
    server: "Server (Jellyfin)",
    net_radio: "Internet Radio",
    airplay: "AirPlay",
    bluetooth: "Bluetooth",
    usb: "USB",
    tuner: "Radio (Tuner)",
    optical1: "Optical 1",
    line1: "Line 1",
};

// --- Small API helpers --------------------------------------------------------

async function apiGet(path) {
    const response = await fetch(path);
    if (!response.ok) throw new Error(`GET ${path} failed (${response.status})`);
    return response.json();
}

async function apiPost(path) {
    const response = await fetch(path, { method: "POST" });
    if (!response.ok) throw new Error(`POST ${path} failed (${response.status})`);
    return response.json();
}

// --- Controls ----------------------------------------------------------------

async function sendCommand(action) {
    try {
        const data = await apiPost(`/api/${action}`);
        console.log("Response:", data);
        await fetchStatus();
    } catch (error) {
        console.error("Error sending command:", error);
    }
}

async function changeInput(inputId) {
    if (!inputId) return;
    try {
        await apiPost(`/api/input?id=${encodeURIComponent(inputId)}`);
        await fetchStatus();
    } catch (error) {
        console.error("Error switching input:", error);
    }
}

async function setVolume(volumeVal) {
    document.getElementById("val-volume").textContent = volumeVal;
    try {
        await apiPost(`/api/volume?val=${encodeURIComponent(volumeVal)}`);
    } catch (error) {
        console.error("Error changing volume:", error);
    }
}

// --- Status & Media Info -----------------------------------------------------

async function fetchInputs() {
    try {
        availableInputs = await apiGet("/api/inputs");
        updateInputDropdown();
    } catch (error) {
        console.error("Error loading inputs:", error);
    }
}

// --- Yamaha device selection ----------------------------------------------------

// Load available MusicCast devices and update the dropdown
async function fetchYamahaDevices() {
    try {
        const response = await apiGet("/api/yamaha/devices");
        availableYamahaDevices = response.devices || [];
        const currentIP = response.current_ip || "";
        const currentName = response.current_name || "";
        
        updateYamahaDropdown(currentIP, currentName);
    } catch (error) {
        console.error("Error loading Yamaha devices:", error);
    }
}

// Mapping for user-friendly device names can be added here,
// but device names primarily come from backend (UPnP friendly_name)

// Update the Yamaha device dropdown
function updateYamahaDropdown(currentIP = "", currentName = "") {
    const select = document.getElementById("yamaha-select");
    if (!select) return;
    
    select.textContent = "";
    
    // Current selected device first
    if (currentIP) {
        const currentOption = document.createElement("option");
        currentOption.value = currentIP;
        
        // Use user-friendly name from device
        const displayName = currentName || currentIP;
        currentOption.textContent = displayName + " (" + currentIP + ")";
        currentOption.selected = true;
        select.appendChild(currentOption);
        
        // Separator
        const separator = document.createElement("option");
        separator.disabled = true;
        separator.textContent = "--- Other devices ---";
        select.appendChild(separator);
    }
    
    // All available devices
    availableYamahaDevices.forEach((device) => {
        if (device.ip_address !== currentIP) {
            const option = document.createElement("option");
            option.value = device.ip_address;
            
            // Use user-friendly name from device
            const displayName = device.friendly_name || 
                               device.model_name || 
                               device.ip_address;
            option.textContent = displayName + " (" + device.ip_address + ")";
            select.appendChild(option);
        }
    });
    
    // If no device is currently selected
    if (!currentIP && availableYamahaDevices.length > 0) {
        const option = document.createElement("option");
        option.value = "";
        option.textContent = "Select device...";
        select.insertBefore(option, select.firstChild);
    }
}

// Called when user selects a different Yamaha device
async function changeYamahaDevice() {
    const select = document.getElementById("yamaha-select");
    const selectedIP = select.value;
    
    if (!selectedIP) return;
    
    // Save selection in localStorage
    localStorage.setItem(YAMAHA_IP_KEY, selectedIP);
    
    // Try to send selection to backend
    try {
        const response = await apiPost(`/api/yamaha/select?ip=${encodeURIComponent(selectedIP)}`);
        console.log("Yamaha device changed:", response);
        
        // Show a short notification
        showNotification("Device changed!");
        
        // Refresh inputs as they depend on the device
        await fetchInputs();
        
        // Update status
        await fetchStatus();
        
    } catch (error) {
        console.error("Error changing Yamaha device:", error);
        showNotification("Error changing device - please try again");
    }
}

// Load saved IP from localStorage
function getSavedYamahaIP() {
    return localStorage.getItem(YAMAHA_IP_KEY) || "";
}

// Show a short notification
function showNotification(message, duration = 2000) {
    // Create notification element
    const notification = document.createElement("div");
    notification.className = "notification";
    notification.textContent = message;
    notification.style.cssText = `
        position: fixed;
        top: 50%;
        left: 50%;
        transform: translate(-50%, -50%);
        background: #1f9c4a;
        color: white;
        padding: 12px 24px;
        border-radius: 4px;
        font-size: 14px;
        z-index: 10000;
        opacity: 0;
        transition: opacity 0.3s;
        box-shadow: 0 2px 10px rgba(0,0,0,0.3);
    `;
    
    document.body.appendChild(notification);
    
    // Animation: fade in
    setTimeout(() => {
        notification.style.opacity = "1";
    }, 10);
    
    // After duration, fade out and remove
    setTimeout(() => {
        notification.style.opacity = "0";
        setTimeout(() => {
            notification.remove();
        }, 300);
    }, duration);
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
    // Avoid overlapping calls if device responds slower than polling interval
    if (statusBusy) return;
    statusBusy = true;
    try {
        const data = await apiGet("/api/status");

        document.getElementById("val-power").textContent = data.power || "Unknown";

        // Volume range comes from device (max_volume), not from code
        const slider = document.getElementById("volume-slider");
        if (data.max_volume) slider.max = data.max_volume;

        const volumeEl = document.getElementById("val-volume");
        if (data.volume !== undefined) {
            volumeEl.textContent = data.volume;
            if (document.activeElement !== slider) slider.value = data.volume;
        } else {
            volumeEl.textContent = "Unknown";
        }

        if (data.input) {
            document.getElementById("input-select").value = data.input;
        }

        await updateMediaInfo(data.input);
    } catch (error) {
        console.error("Error loading status:", error);
    } finally {
        statusBusy = false;
    }
}

// Compare track names tolerantly (case, multiple spaces).
function normalizeTitle(name) {
    return (name || "").trim().toLowerCase().replace(/\s+/g, " ");
}

// Remember the playing track and update highlighting in the result list.
function setNowPlaying(track) {
    nowPlayingTrack = track || "";
    updatePlayingHighlights();
}

// Highlight all results/album tracks whose name matches the playing track.
// The playing track comes from getPlayInfo (no Jellyfin item ID matching),
// so the highlighting also works after reload or externally started playback.
function updatePlayingHighlights() {
    const key = normalizeTitle(nowPlayingTrack);
    document.querySelectorAll("#search-results [data-track-name], #browse-results [data-track-name]").forEach((el) => {
        const match = key !== "" && normalizeTitle(el.dataset.trackName) === key;
        el.classList.toggle("track-playing", match);
    });
}

// --- Playback time display ------------------------------------------------

// Yamaha provides play_time/total_time in seconds. Between the 5-second polls,
// the local display continues counting so it doesn't jump; each poll resets it
// with the device value. When paused, it stays still (progressRunning = false).
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

// Sync time display; running=false freezes the display (Pause).
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

// Hide time row (no Net/USB input, stopped or no duration known,
// e.g., radio without total_time).
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
        // No Net input: a source change clears the Jellyfin context,
        // so no random playback is running either.
        setShuffleIndicator(false);
        return;
    }

    try {
        const dataPlay = await apiGet("/api/playinfo");
        setShuffleIndicator(dataPlay.jellyfin_shuffle === true);
        if (dataPlay.response_code === 0 && dataPlay.playback !== "stop") {
            mediaContainer.classList.remove("hidden");
            setNowPlaying(dataPlay.track);
            document.getElementById("val-track").textContent = dataPlay.track || "Unknown track";
            document.getElementById("val-artist").textContent = dataPlay.artist || "";
            document.getElementById("val-album").textContent = dataPlay.album || "";

            const imgEl = document.getElementById("val-art");
            // Cover source: Prefer device (albumart_url) - this is the cover
            // of the current file. After automatic switch, the device
            // sometimes doesn't provide the URL (new albumart_id, file 404).
            // For own Jellyfin tracks, the backend sends Album ID and
            // Image tag; this ensures the cover appears even without device URL.
            const artSrc = dataPlay.albumart_url
                ? `/api/albumart?path=${encodeURIComponent(dataPlay.albumart_url)}`
                : dataPlay.jellyfin_album_id
                  ? `/api/jellyfin/image?id=${encodeURIComponent(dataPlay.jellyfin_album_id)}` +
                    `&tag=${encodeURIComponent(dataPlay.jellyfin_image_tag || "")}`
                  : "";
            if (artSrc && imgEl.dataset.src !== artSrc) {
                // Cover art goes through the Go proxy, not directly to Yamaha.
                // Only set on change, otherwise browser reloads the image on
                // every 5-second poll.
                imgEl.dataset.src = artSrc;
                imgEl.src = artSrc;
                imgEl.classList.remove("hidden");
            } else if (!artSrc) {
                delete imgEl.dataset.src;
                imgEl.classList.add("hidden");
            }

            // play_time is negative when position is invalid (-60000).
            const elapsed = Math.max(dataPlay.play_time || 0, 0);
            const total = Math.max(dataPlay.total_time || 0, 0);
            if (total > 0) {
                document.getElementById("time-row").classList.remove("hidden");
                // Only continue counting on playback, not on pause.
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
        console.error("Error loading playback info:", error);
    }
}

// --- Jellyfin Search -----------------------------------------------------------

// Debounce for search while typing.
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
        console.error("Error searching Jellyfin:", error);
        renderSearchMessage("Search failed.");
    }
}

// Centered message instead of result list (empty, error, etc.).
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

// Format seconds as m:ss or h:mm:ss (empty for invalid values).
function formatSeconds(seconds) {
    if (!Number.isFinite(seconds) || seconds < 0) return "";
    const total = Math.floor(seconds);
    const hours = Math.floor(total / 3600);
    const minutes = Math.floor((total % 3600) / 60);
    const secs = total % 60;
    const pad = (n) => String(n).padStart(2, "0");
    return hours > 0 ? `${hours}:${pad(minutes)}:${pad(secs)}` : `${minutes}:${pad(secs)}`;
}

// Jellyfin provides durations in 100-ns ticks (10,000,000 = 1 second).
function formatDuration(ticks) {
    if (!ticks || ticks <= 0) return "";
    return formatSeconds(Math.round(ticks / 10000000));
}

// Clone template and then only fill with text (no innerHTML).
function fromTemplate(id) {
    return document.getElementById(id).content.firstElementChild.cloneNode(true);
}

// Add cover to tile; without img tag, the gray placeholder remains.
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

// Display duration on the right; without duration, the column is omitted entirely.
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
    row.querySelector('[data-slot="title"]').textContent = album.Name || "Unknown";
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
    row.querySelector('[data-slot="title"]').textContent = artist.Name || "Unknown";
    row.querySelector('[data-slot="sub"]').textContent = "Artist";
    const play = row.querySelector('[data-action="play"]');
    play.dataset.id = artist.Id;
    play.dataset.type = artist.Type || "MusicArtist";
    return row;
}

function createTrackRow(track, index) {
    const row = fromTemplate("tpl-track");
    // The track line itself is the "play" hit (data-action="play"),
    // therefore set directly on the line instead of via querySelector.
    row.dataset.id = track.Id;
    row.dataset.type = "Audio";
    // Only real track lines are candidates for "currently playing" highlighting.
    row.dataset.trackName = track.Name || "";
    row.querySelector('[data-slot="number"]').textContent = track.IndexNumber || index + 1;
    fillCover(row.querySelector('[data-slot="cover"]'), track);
    row.querySelector('[data-slot="title"]').textContent = track.Name || "Unknown";
    row.querySelector('[data-slot="sub"]').textContent = subtitle([artistNames(track), track.Album]);
    setDuration(row, track.RunTimeTicks);
    return row;
}

function createAlbumTrackLine(track, index) {
    const line = fromTemplate("tpl-album-track");
    line.dataset.id = track.Id;
    line.dataset.trackName = track.Name || "";
    const number = track.IndexNumber ? `${track.IndexNumber}. ` : `${index + 1}. `;
    line.querySelector('[data-slot="title"]').textContent = number + (track.Name || "Unknown");
    setDuration(line, track.RunTimeTicks);
    return line;
}

// Hint text in expanded area (loading, empty, error).
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
        renderSearchMessage(`No results for "${query".`);
        return;
    }

    resultsContainer.textContent = "";
    resultsContainer.classList.remove("hidden");

    // Sections in fixed order: Albums, Artists, Tracks. Empty groups
    // don't get a header. Which section is visible is controlled by CSS.
    const groups = [
        ["albums", "Albums", data.albums, createAlbumRow],
        ["artists", "Artists", data.artists, createArtistRow],
        ["tracks", "Tracks", data.tracks, createTrackRow],
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

// Chips are radios + labels in HTML; here only count and hide empty groups.
// The visibility of sections is controlled by CSS, so there's no filtering here.
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

// Click delegation for a result list (search or browse): "Play" (or
// a track line) starts playback; a click next to it expands the <details>
// natively.
function bindResultList(container) {
    container.addEventListener("click", (event) => {
        const play = event.target.closest('[data-action="play"]');
        if (!play) return;
        // Don't expand/collapse if clicking "Play" in <summary>
        event.preventDefault();
        event.stopPropagation();
        if (play.dataset.id) playJellyfinItem(play.dataset.id, play.dataset.type || "Audio", container);
    });

    // Lazy load when expanded. The toggle event doesn't bubble, so
    // the delegation listens in the capture phase.
    container.addEventListener("toggle", (event) => {
        const details = event.target;
        if (!details.open || details.dataset.loaded === "1") return;
        if (details.dataset.kind === "album") loadAlbumTracks(details);
        else if (details.dataset.kind === "artist") loadArtistAlbums(details);
    }, true);
}

async function loadAlbumTracks(details) {
    const body = details.querySelector('[data-slot="body"]');
    setBodyMessage(body, "Loading tracks...");
    try {
        const tracks = await apiGet(`/api/jellyfin/tracks?parent_id=${encodeURIComponent(details.dataset.id)}`);
        details.dataset.loaded = "1";
        if (tracks.length === 0) {
            setBodyMessage(body, "No tracks available.");
            return;
        }
        body.textContent = "";
        tracks.forEach((track, i) => body.appendChild(createAlbumTrackLine(track, i)));
        updatePlayingHighlights();
    } catch (error) {
        console.error("Error loading album tracks:", error);
        setBodyMessage(body, "Could not load tracks.");
    }
}

// Expands under an artist their albums - itself expandable again.
// Instead of the recursive track list of an artist, which would be too large.
async function loadArtistAlbums(details) {
    const body = details.querySelector('[data-slot="body"]');
    setBodyMessage(body, "Loading albums...");
    try {
        const data = await apiGet(`/api/jellyfin/browse?kind=albums&artist_id=${encodeURIComponent(details.dataset.id)}&limit=100`);
        const albums = data.items || [];
        details.dataset.loaded = "1";
        if (albums.length === 0) {
            setBodyMessage(body, "No albums found.");
            return;
        }
        body.textContent = "";
        albums.forEach((album) => body.appendChild(createAlbumRow(album)));
        updatePlayingHighlights();
    } catch (error) {
        console.error("Error loading albums:", error);
        setBodyMessage(body, "Could not load albums.");
    }
}

// Start a specific Jellyfin item on the receiver. The backend gives the
// device the Jellyfin stream URL via UPnP AVTransport (SetAVTransportURI +
// Play); Album/Artist are resolved to their first track there.
async function playJellyfinItem(itemId, itemType, container) {
    try {
        const data = await apiPost(`/api/jellyfin/play?item_id=${encodeURIComponent(itemId)}`);
        console.log(`Playback started: ${data.track || itemId} (${itemType})`);
        await fetchStatus();
    } catch (error) {
        console.error("Error starting playback:", error);
        if (container) showHint(container, "Could not start playback.");
    }
}

// Short hint below a list (error when playing).
function showHint(container, message) {
    const hint = document.createElement("p");
    hint.className = "list-hint list-hint-warn";
    hint.textContent = message;
    container.appendChild(hint);
}

// Start a random track and enable shuffle; next will continue shuffling.
// Shuffled from the currently browsed library (the endpoint takes the
// first music library otherwise).
async function playRandomTrack() {
    const library = browse.library ? `?parent_id=${encodeURIComponent(browse.library)}` : "";
    try {
        const data = await apiPost(`/api/jellyfin/random${library}`);
        console.log(`Random playback: ${data.track} - ${data.artist}`);
        await fetchStatus();
    } catch (error) {
        console.error("Random playback failed:", error);
        showHint(document.getElementById("browse-results"), "Could not start random track.");
    }
}

// State of shuffle button. Source is /api/playinfo; the field is called
// jellyfin_shuffle because `shuffle` there is already the shuffle of the device queue.
function setShuffleIndicator(active) {
    document.getElementById("btn-random").setAttribute("aria-pressed", active ? "true" : "false");
}

// --- Jellyfin Browse ---------------------------------------------------------

// Jellyfin delivers large pages in one call (measured: 200 albums in ~98 ms),
// so 100 rows per load step instead of the 8 that the YXC list enforces.
const BROWSE_PAGE_SIZE = 100;

// State of browse list. `start` is the offset of the next page, `total`
// is the total count from TotalRecordCount (only then does loading know when to stop).
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

// Entry point for browsing: get libraries and load the first page. The
// library selection is omitted if there's only one library - it would be a choice
// without alternative.
async function initBrowse() {
    try {
        const views = await apiGet("/api/jellyfin/views");
        if (views.length > 0) browse.library = views[0].id;
        if (views.length > 1) {
            // Chips from radios + labels are created at runtime: The number of
            // libraries is only known after the request.
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
        console.error("Could not load libraries:", error);
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

    // Load more without scroll listener: the sentinel at the list end is observed
    // via IntersectionObserver. rootMargin loads one page ahead so that the ~60-100 ms
    // response time is not visible as jerking.
    const results = document.getElementById("browse-results");
    browseObserver = new IntersectionObserver((entries) => {
        if (entries.some((entry) => entry.isIntersecting)) loadBrowsePage(false);
    }, { root: results, rootMargin: "200px 0px" });
}

// Load the next page; reset starts the list anew (different library, type,
// sort or letter). An outdated response is discarded via browseRequest.
async function loadBrowsePage(reset) {
    if (browse.loading && !reset) return;
    // At the end of the list there's nothing more to load. The check is here
    // instead of in the observer because the sentinel remains visible at the end
    // and would otherwise trigger repeatedly (empty list = infinite loop).
    if (!reset && browse.start >= browse.total) return;

    const results = document.getElementById("browse-results");
    const sentinel = document.getElementById("browse-sentinel");
    const requestId = ++browseRequest;

    if (reset) {
        browse.start = 0;
        browse.total = 0;
        results.querySelectorAll("details, p").forEach((el) => el.remove());
        setBrowseStatus("Loading...");
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
        console.error("Browse failed:", error);
        setBrowseStatus("Could not load list.");
    } finally {
        if (requestId !== browseRequest) return;
        browse.loading = false;
        // Re-observe: this triggers an initial check. Without it, a page that
        // doesn't fill the space wouldn't trigger further loading
        // (the sentinel stays visible, event only fires on change).
        browseObserver.unobserve(sentinel);
        browseObserver.observe(sentinel);
    }
}

function renderBrowseStatus(empty) {
    if (empty) {
        setBrowseStatus("No entries.");
        return;
    }
    if (browse.start >= browse.total) {
        setBrowseStatus(`${browse.total} entries`);
        return;
    }
    setBrowseStatus(`${browse.start} of ${browse.total} loaded`);
}

function setBrowseStatus(text) {
    const status = document.getElementById("browse-status");
    status.textContent = text;
    status.classList.toggle("hidden", !text);
}

// --- Polling & Initialization ------------------------------------------------

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
    
    // Yamaha device selection
    const yamahaSelect = document.getElementById("yamaha-select");
    if (yamahaSelect) {
        yamahaSelect.addEventListener("change", changeYamahaDevice);
    }
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

    // Hide broken/empty cover (e.g., outdated AlbumART ID on device) without
    // "image" icon and try again on next poll.
    const artEl = document.getElementById("val-art");
    artEl.addEventListener("error", () => {
        // Only handle real cover load errors: an empty src would be resolved
        // to the page URL by the browser and would otherwise generate a false error.
        if (!artEl.dataset.src) return;
        console.warn("Could not load cover art:", artEl.src);
        delete artEl.dataset.src;
        artEl.classList.add("hidden");
    });
}

async function init() {
    // If an IP is saved in localStorage, send it to backend
    const savedIP = getSavedYamahaIP();
    if (savedIP) {
        try {
            await apiPost(`/api/yamaha/select?ip=${encodeURIComponent(savedIP)}`);
            console.log("Loaded saved IP:", savedIP);
        } catch (error) {
            console.error("Error loading saved IP:", error);
        }
    }
    
    await fetchInputs();
    await fetchStatus();
    await fetchYamahaDevices();
    startStatusPolling();
    await initBrowse();

    // Don't poll in background tab
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
