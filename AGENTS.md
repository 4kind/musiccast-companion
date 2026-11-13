# AGENTS.md

Context for coding agents in this repository.

## Project

Private LAN web app for **Yamaha MusicCast receivers** plus Jellyfin music search.
The Go **backend** (package `main`, split across `main.go`/`yamaha.go`/`jellyfin.go`/`upnp.go`)
is a **proxy**: it serves `static/` and forwards all device calls.
The browser **never** communicates directly with Yamaha or Jellyfin — this avoids
CORS and mixed-content issues. Please do not "optimize" by having JS point directly to
the device IP.

## Commands

```bash
go run .      # from repo root only! → http://localhost:8080
go vet ./...
gofmt -l .    # must be empty
```

- **Always start from the repo root.** `http.FileServer(http.Dir("./static"))` is relative;
  from another directory, the page will be empty. (The systemd service sets `WorkingDirectory`,
  see `README.md`.)
- Port: `listenAddr` is `":" + PORT` (default **8080**, configurable via environment variable `PORT`,
  validated as `1..65535`). If the port is still in use, an old process is blocking it:
  `pkill -f musiccast-companion` or `fuser -k 8080/tcp`. (Caution: `pkill -f` may hit your
  own shell — if unsure, use `fuser -k <port>/tcp`.)
- **"Old server" trap:** `static/` is read from disk at runtime. An **old** binary still
  running will serve the **new frontend** (e.g., `app.js` with `/api/albumart`) with the
  **old backend** (missing route) → cover art broken even though the code is correct.
  After backend changes, ensure a fresh start and that only **one** process is listening on
  `:8080`.
- No dependencies (`go.mod` without `require`, no `go.sum`), no build step, no npm.
  `go.mod` requires `go 1.22` (older versions don't support the `ServeMux` method patterns);
  newer tools build unchanged.
- No tests, no lint/CI/formatter setup. A `go build .` creates a `musiccast-companion`
  binary in the repo root — build artifact, not checked into git (`.gitignore`).
- The repo has a git remote (`origin`, branch `main`); check `git status` before changes.
- **Deploy to NAS:** `./deploy-to-nas.sh <nas-host>` runs `gofmt`/`go vet`, transfers
  sources, builds there, installs to `/opt/musiccast-companion`, and restarts the service
  (single sudo password prompt). `APP_PORT` (default `8765`) only affects the reachability check.

## LAN Operation

`listenAddr` (from `PORT`, default `:8080`) listens on **all** interfaces, not just
`localhost` — the app is thus accessible on the LAN as long as the process is running.
You don't need to guess the address: `main()` prints it on startup, determined by `lanURLs()`
from `net.InterfaceAddrs()` (excluding loopback and link-local addresses, IPv4 first).
This is display only — the binding remains on all interfaces, only the port is configurable.

- **No Auth:** Anyone on the LAN can control the receiver and Jellyfin. This is intentional
  for a private home network, but the reason not to expose the app to the internet via port
  forwarding.
- **Styling comes from the repo** (`static/style.css`, Winamp 2.x look). No CDN requests —
  the page looks complete even without internet access.
- One process per port: if an old one is still running, it blocks the new one (`*:8080` occupied).
- **Firewall:** If a firewall is active on the machine (e.g., ufw with
  `DEFAULT_INPUT_POLICY=DROP`), a test from a phone will fail at the firewall, **not** at
  the binding: `:8080` already listens on all addresses (`ss -ltnp | grep 8080` → `*:8080`),
  `0.0.0.0` would be the same binding without IPv6. Opening requires root, e.g.
  `sudo ufw allow from <lan>/24 to any port 8080 proto tcp`. Testing from the same machine
  to its own LAN IP proves nothing — such packets go through `lo` and are not filtered by ufw.
- **Continuous operation on NAS:** `README.md` describes installation as a systemd service
  (`/opt/musiccast-companion`, `Environment=YAMAHA_IP/JELLYFIN_URL/JELLYFIN_API_KEY`,
  `Restart=always`, journal logging). This repo contains **no** unit file or installer —
  intentionally, installation belongs on the target machine. `Restart=always` instead of
  `on-failure` because SIGTERM is a clean exit for systemd and `on-failure` would not trigger.

## Verified Device

```
GET http://<receiver-ip>/YamahaExtendedControl/v1/system/getDeviceInfo
→ model_name R-N602, api_version 2.08, system_version 1.36, one zone ("main")
```

- The device is accessible on the LAN. **Read-only GETs for verification are okay**,
  e.g., `curl -s http://<receiver-ip>/YamahaExtendedControl/v1/main/getStatus`.
  `POST /api/play`, `/api/stop`, `/api/input`, `/api/volume` change the actual playback state —
  do not use for testing.
- **No Auth.** The Basic Spec v2.0 does not mention `authKey`/`authID`/`authPwd`
  (0 matches in the document), and the device responds without credentials. Do not
  "retrofit" the auth session flow.
- **Volume: min 0, max 161, step 1** — the device provides this itself:
  `getStatus → max_volume` or `getFeatures → zone[].range_step[id=volume]`.
  The slider gets its `max` at runtime from `getStatus.max_volume` (`fetchStatus()` in
  `app.js`); the HTML attribute is just an initial value. Whoever touches the volume
  reads the range from the device, not from the code. It is **not** 0-100.
- 22 inputs; `/api/inputs` returns 8 of them (`airplay`, `server`, `net_radio`, `bluetooth`,
  `usb`, `tuner`, `optical1`, `line1`). Selection is controlled solely by the
  `hiddenInputs` blacklist in `main.go`; `deezer` is now also blacklisted there.
  `POST /api/input` rejects blacklisted inputs with `400`.
- Additionally, the device is a **UPnP MediaRenderer** (`DMR-1.50`, service `AVTransport:1`).
  The device description is on port `49154` (`/MediaRenderer/desc.xml`), the control endpoint
  at `/AVTransport/ctrl`. SSDP multicast is not needed (partially blocked in dev network),
  the port is hardwired. Jellyfin playback runs through this (see below).

## Yamaha Integration: Code Over Documentation

All upstream calls go to `/YamahaExtendedControl/v1/...`:

| Route | Upstream (all GET) |
|---|---|
| `GET /api/status` | `main/getStatus` |
| `GET /api/inputs` | `system/getFeatures`, filtered |
| `GET /api/playinfo` | `netusb/getPlayInfo` (+ UPnP `GetPositionInfo`, only for missing `total_time`) |
| `GET /api/albumart?path=` | `http://{host}{path}` (cover art, path from `playinfo`) |
| `POST /api/play` | if Jellyfin context: UPnP `Play`; else `netusb/setPlayback?playback=play` |
| `POST /api/stop` | if `input == "server"`: UPnP `Stop`; else `netusb/setPlayback?playback=stop` |
| `POST /api/next` / `prev` | Jellyfin context (see below); else `netusb/setPlayback?playback=next`/`previous` |
| `POST /api/input?id=` | `main/setInput?input=` |
| `POST /api/volume?val=` | `main/setVolume?volume=` |

- **Setters are also GET with query parameters** — the spec explicitly states `Method: GET`
  for `setVolume`, `setInput`, `setPlayback`. There is no PUT/POST against the device,
  even for "write" actions. `http.Get` in `main.go` is therefore correct, not sloppy.
- The `PUT /volume` table from old notes is wrong — it does not come from this API.
- `getPlayInfo` provides its **own** `input` field, which may differ from the zone input
  (observed: zone `optical1`, playInfo `airplay`). However, the frontend decides on media
  display based on the **zone** input — for net/USB inputs, both match.
- **`/api/playinfo` enriches missing times from the renderer:** For UPnP playback
  (Jellyfin, `input: "server"`), `getPlayInfo` provides `play_time` but **`total_time = 0`**
  (observed: `play_time` 45, `total_time` 0, although the renderer knows `TrackDuration 0:04:18`).
  `handlePlayInfo` in `yamaha.go` therefore, when `input == "server"` and `total_time == 0`,
  queries UPnP `GetPositionInfo` (`upnpPositionInfo`) for `TrackDuration`/`RelTime` and fills them
  (`fillUPnPTimes`); `play_time` is only replaced if the device itself returns 0.
  Decoding uses `json.Decoder.UseNumber()` to keep all other fields exactly as received.
  The SOAP response is **not well-formed XML** (TrackURI contains a bare `&`, e.g.
  `...&static=true`), which breaks `encoding/xml` — the two values are therefore read by
  `soapValue` via text search.
- `albumart_url` is an absolute path with leading slash
  (observed: `/YamahaRemoteControl/AlbumART/AlbumART<n>.jpg`). When no title is present,
  it is not `""`. The frontend no longer prefixes with the device IP itself, but calls
  `/api/albumart?path=...`; there `yamaha.go` prepends `yamahaIP` and proxies the image.
  Paths without leading slash or with `..` are rejected with `400`.
- `/api/albumart` only forwards **successful** images: if the device does not respond with `200`
  (e.g., outdated albumART ID → `404`), it returns `502`. Otherwise, a device error would
  become an empty `200`, which the browser renders as a broken cover and may cache.
  The frontend only reloads the image when `albumart_url` changes and hides it on a load
  `error` (next poll tries again), instead of showing a "broken image" icon.
  The `<img id="val-art">` deliberately has **no** `src=""`: an empty `src` is resolved by
  the browser to the page URL and would otherwise trigger a false error on initial load;
  the `error` handler ignores errors without a real (set via `dataset.src`) cover.
- **Cover after automatic advance:** `albumart_url` is **not** reliable after transition
  via `SetNextAVTransportURI`. Observed: The renderer assigns a new `albumart_id` (e.g., `8930`),
  but returns `albumart_url: ""`; the resulting file
  `/YamahaRemoteControl/AlbumART/AlbumART<n>.jpg` is then `404`. With manual start/skip,
  the device creates the cover immediately. Therefore, for Jellyfin titles, the backend sends
  `jellyfin_album_id` and `jellyfin_image_tag` in `/api/playinfo` (`jellyfinCoverFor` in
  `jellyfin.go`: `AlbumId`/`AlbumPrimaryImageTag`, otherwise `Id`/`ImageTags.Primary`) —
  recognized by **track name** against the queue context (list or random `current`, additionally
  `next`, because the position only updates every 2 seconds). This ensures that a track started
  externally (MusicCast app) does not get a wrong cover. The frontend uses the device cover
  if available, otherwise `/api/jellyfin/image?id=...&tag=...`; without either, the image
  remains hidden.

## Jellyfin Search

`GET /api/jellyfin/search?q=` searches in **three separate** queries: `MusicAlbum`
(limit 12), `MusicArtist` (limit 8), and `Audio` (limit 25), and returns them
**structured** (`{"albums":[...],"artists":[...],"tracks":[...]}`), not as a flat
`Items` list. Reason: Jellyfin ranks tracks first; with a shared limit, the many tracks
would displace the albums, and for common terms (e.g., `the`) **no** albums would remain.
The separation allows individual limits per type and the frontend a section display.
`GET /api/jellyfin/tracks?parent_id=` provides the tracks of an album for expanding
in the results list.

## Jellyfin Browsing

`GET /api/jellyfin/views` provides the music **libraries** (`/Users/{id}/Views`, filtered
to `CollectionType == "music"` — movies/series/books do not belong in a music interface).
Only libraries of the **API key user** are visible; private libraries of other users are
missing here, which would require an admin key or a second user key.

`GET /api/jellyfin/browse` is the single list for browsing:
`?kind=artists|albums&parent_id=&artist_id=&letter=&sort=name|newest&start=&limit=`
and returns `{"items":[...],"total":n}` (`total` = Jellyfin `TotalRecordCount`).

- `kind=artists` → `/Artists/AlbumArtists` (with `userId`), **not** `IncludeItemTypes=MusicArtist`:
  The latter returns all track features as well (observed 5287 instead of 1275 entries,
  because every "feat." appearance counts as a separate artist).
- `kind=albums` → `/Users/{id}/Items` with `IncludeItemTypes=MusicAlbum`; `artist_id` sets
  `AlbumArtistIds` (+ `SortBy=ProductionYear,SortName`, as when expanding), `sort=newest`
  sets `SortBy=DateCreated&SortOrder=Descending`, otherwise `SortBy=SortName`.
- `letter` sets `NameStartsWithOrGreater` (A-Z jump). Scrolling continues **within**
  this filtered list — so no index calculations are needed, and no worry that name and
  sort order diverge. Observed: albums ≥M = 1418, artists ≥M = 553.
- `start` (0...1000000) and `limit` (1...200, default **100**) are enforced server-side;
  unknown `kind` and `letter` not A-Z are rejected with `400`.
- `/api/jellyfin/artist-albums` **no longer exists**: Expanding an artist in search results
  calls the same endpoint (`browse?kind=albums&artist_id=...&limit=100`) and reads
  `data.items`. One route instead of two ways for the same question.

**Why 100 instead of 8:** The YXC list enforces 8 rows per request (~84-111 ms, 3197 albums =
400 requests ≈ 38 s). Jellyfin delivers the same amount in **one** response in ~60-98 ms
(observed: `Limit=50` → 62 ms, `Limit=200` → 98 ms), a full run would be 16 requests.
Therefore, browsing is implemented against Jellyfin, not against the device list (YXC browse
only offers 8 rows and no search).

## Jellyfin Playback (UPnP AVTransport)

`POST /api/jellyfin/play?item_id=` starts a specific track — the MusicCast API
cannot do this ("play URL X" does not exist). Flow in `handleJellyfinPlay`:

1. Load Jellyfin item for `item_id`; if `Type != "Audio"` (album/artist), take the first
   `Audio` track under it.
2. Build static stream URL: `{jellyfinURL}/Audio/{id}/stream?static=true&api_key=...`
   (the key must be in the URL — the **device** loads it, not our client).
3. Create DIDL-Lite with matching `protocolInfo` (`audio/flac`, `audio/mpeg`, ...) **and**
   the track duration as `res@duration` (`H:MM:SS.mmm`, from `RunTimeTicks/10^7`). Without this
   attribute, the renderer reports `MediaDuration`/`TrackDuration` = `0:00:00` (observed via
   UPnP `GetMediaInfo`) and thus also `total_time` = 0 — the duration display would be empty.
4. `SetAVTransportURI` + `Play` via SOAP to `/AVTransport/ctrl` of the renderer, then
   `primeNext()` for the next track (see "Automatic continuation").

- **Setters here are actual HTTP POSTs with SOAP** — unlike the YXC API (GET there).
  Do not "unify".
- The input automatically jumps to `server` (UPnP), playback reports `playback: "play"`.
- **Stopping** (`POST /api/stop`, `handleStop` in `yamaha.go`) has two paths: If `input == "server"`,
  playback is the UPnP session of the renderer — there an **UPnP `Stop`** ends it immediately
  (YXC reports `playback: "stop"` afterwards). Only for other inputs does the YXC path
  `netusb/setPlayback?playback=stop` run. Reason (observed): The YXC stop blocks the renderer
  for about **4 seconds**; during this time, any `Play` fails with SOAP **501 Action Failed**
  (even after `SetInput` or an additional UPnP `Stop`). If stopped via the YXC path,
  any quick "Stop, then start another track" would have returned `502`.
  `handlePlay` reflects this: If there is a Jellyfin context (`hasJellyfinContext()`),
  UPnP `Play` continues the renderer session and `primeNext()` sets the next track again;
  without context (radio/USB, MusicCast's own queue), it uses YXC play. A YXC play on
  `server` would otherwise start the device's own MusicCast queue (observed: wrong track,
  renderer remained `STOPPED`).
- **Automatic continuation** (`SetNextAVTransportURI`, the core against "album ends after track 1"):
  After each start, `primeNext()` (`jellyfin.go`) sets the next track in the renderer — in the
  album, the next list entry; in random mode, a new random draw. The **device** switches
  automatically at the end of the track (live verified: after ~10s from A to B), without
  stop/start jump. `autoAdvanceLoop()` polls `GetPositionInfo` every `autoAdvanceInterval`
  (2s) for this: If `TrackURI` contains the ID of the set track, the device has advanced →
  update position (`playQueue.index` or random `current`) and set the next one.
  - At the end of the list, the next track is **deleted** (empty `NextURI`), then playback
    stops after the last track (verified: state `STOPPED`, no loop).
  - The metadata (`NextURIMetaData` with DIDL incl. `res@duration`) is mandatory: Without it,
    the renderer does not know the duration of the next track and reports `TrackDuration` = `0:00:00`.
  - `clearJellyfinNext()` (stop, source change ≠ `server`) deletes the entry in the device **and**
    in the backend; otherwise, a later `Play` would jump to the old next track. Skip/Prev and
    the random button call `primeNext()` again afterwards.
  - The loop only polls while a next track is set. If the renderer reports ~30 rounds
    (~1 minute) without playback (device off, source changed on device), the next track
    is forgotten instead of polling endlessly; only the first failure is logged.
  - `Seek` is **ignored** by the device (SOAP response ok, `RelTime` continues unchanged) —
    do not use for testing track endings, use short tracks instead.
- **Skip/Prev** (`POST /api/next` / `/api/prev`, buttons ⏮/⏭): The backend remembers the
  ordered track list of the album/artist (`jellyfinPlayContext`) including position when
  starting, and plays the neighbor track via UPnP when skipping. Reason: `netusb/setPlayback?playback=next`
  jumps to the **device's own** queue (observed: foreign tracks not belonging to the album),
  not to the played Jellyfin album. Without Jellyfin context (radio, USB), `next`/`previous`
  are forwarded to the device; a source change (`POST /api/input` ≠ `server`) clears the context.
  At the beginning/end of the list, no jump occurs (`{"moved": false}`).
- **Random playback** (`POST /api/jellyfin/random`, button 🎲 Random): rolls a track and thus
  switches from list mode to **random mode** — ⏭ then rolls anew each time. The button
  is not a single start: Without the switch, ⏭ would jump to the device's own queue after a
  random track (see above), so random playback would end after one song.
  - Rolling uses Jellyfin `SortBy=Random&Limit=1` (observed **~250-340 ms**). Deliberately
    **not** "get total count, select random `StartIndex`": `TotalRecordCount` costs in
    this library **~42s** (43985 tracks) when measured.
  - The library must always have `parentId` set. Without `parentId`, Jellyfin searches
    **all** libraries (including audiobooks) and runs into a **timeout**. Therefore,
    `?parent_id=` takes the library selected in the frontend (fallback: first music library),
    verified against `/Users/{id}/Views` (`jellyfinLibraryID`).
  - `planShuffleStep()` plans the skip **without network access** (the roll happens outside
    the lock), `history` (max. `shuffleHistoryLimit` = 50) makes ⏮ possible: ⏭ puts the
    current track in history, ⏮ takes the last one out. At the start of the random sequence:
    `{"moved": false}`. A second click on 🎲 in the same library keeps the history,
    a different library starts a fresh session. Each start via `/api/jellyfin/play` ends
    random mode.
  - `GET /api/playinfo` gets a field **`jellyfin_shuffle`** (bool). The name is deliberately
    custom: `shuffle` is already used in `getPlayInfo` — this is the shuffle of the **device's own**
    queue (there is also `shuffle_available`) and must not be overwritten.
    The 🎲 button shows its state (`aria-pressed`) via this, fed from the 5s poll.
  - Live verified: Random → `shuffle=true`, ⏭ = different track, ⏮ = previous one
    (24 ms, without Jellyfin request), ⏮ at start = `moved:false`, album start = `shuffle=false`.
  - Random also **auto-advances**: `primeNext()` already rolls the next track at start
    and sets it in the renderer; after the switch, the played track moves to history and
    the next one is rolled. ⏮ thus also works after automatically advanced tracks.
    After a ⏮, the future is re-rolled (history is then empty).
- Simplifications: Clicking on album/artist starts the **first** track; then the album
  auto-advances (see automatic continuation) and Skip/Prev browse it. `static=true`
  (no transcoding — exotic codecs may fail).
- **Track list order:** `jellyfinAudioChildren` sorts with
  `SortBy=ParentIndexNumber,IndexNumber`. `ParentIndexNumber` is the **disc** number; sorting
  only by `IndexNumber` makes Jellyfin sort multi-CD albums incorrectly (observed: track 1
  of disc 2 ends up directly after track 1 of disc 1).
- The results list is divided into **sections** "Albums / Artists / Tracks" (with count)
  and allows **albums** to be expanded/collapsed by clicking on the name (native `<details>/<summary>`,
  see search UI below). The tracks come via
  `GET /api/jellyfin/tracks?parent_id=...` (audio children, sorted by track number, max 300);
  clicking a track line plays exactly that track. **Artists** also expand, but to their **albums**
  (`GET /api/jellyfin/browse?kind=albums&artist_id=...`) — the recursive track list of an
  artist would be too large; these albums are themselves expandable.
- Rows show cover thumbnails via `GET /api/jellyfin/image?id=&tag=` (Jellyfin
  `Items/{id}/Images/Primary?maxHeight=96`, `Cache-Control` 1 day; `id` is checked for `/`
  and `..`). Albums show artist · year and total runtime, tracks show track number · artist ·
  album and duration. The proxy keeps the browser same-origin (like `/api/albumart`);
  Jellyfin requires the `Authorization` **header** — `api_key` as query returns `401` on `/Users`.
- **Favorite heart:** Tracks marked as favorite in Jellyfin (`UserData.IsFavorite` of the
  API key user) get a red heart in the track line (`tpl-album-track` in `index.html`,
  filled by `createAlbumTrackLine` in `app.js`); non-favorites show a dimmed heart —
  the heart is **clickable** and toggles the flag via
  `POST /api/jellyfin/favorite?id=<item>&favorite=1|0` (`handleJellyfinFavorite` in
  `jellyfin.go`, Jellyfin: POST/DELETE on `/Users/{id}/FavoriteItems/{itemId}` for the
  API key user). State is a `data-fav` attribute on the heart (`aria-pressed` follows),
  not a CSS class. Click handling is in `bindResultList` **before** the play match
  because the heart sits inside the track line (which is itself the play button).
  `/Users/{id}/Items` includes `UserData` by default; the typed `jellyfinItem` struct carries
  the flag through `/api/jellyfin/tracks` (search/browse results are raw passthrough
  and contain it anyway). The heart also appears in the playback area (LCD, `#val-fav`
  in `index.html`, filled by `setLcdFavorite` in `app.js` from `jellyfin_item_id` +
  `jellyfin_favorite` in `/api/playinfo` — `jellyfinFavoriteFor` in `jellyfin.go`, same
  track-name matching as `jellyfinCoverFor`), so it can be toggled there too.

## API Reference

`docs/YXC_API_Spec_Basic_v2.0.pdf` (Rev. 2.00, 129 pages),
`docs/YXC_API_Spec_Advanced_v2.0.pdf` for zone/group topics.
A text extract is **not** in the repo — if needed, create it yourself; then the spec
is searchable instead of 129 pages of PDF:

```bash
pdftotext -layout docs/YXC_API_Spec_Basic_v2.0.pdf /tmp/opencode/yxc.txt
```

## Backend / Frontend

- The backend is a `net/http.ServeMux` with Go 1.22 method patterns (`"GET /api/status"`).
  No router, no framework, no layers. Handlers are thin, named functions;
  the common Yamaha proxy (build URL, `http.Get`, copy body) is bundled in `proxyYamaha()`,
  Jellyfin runs via `jellyfinGet()`. All upstream calls share `httpClient` with timeout.
  Add new endpoints in the same direct way. **Package `main` split:**
  `main.go` (configuration, client, routes), `yamaha.go` (YXC), `jellyfin.go` (Jellyfin API +
  track resolution + `/api/jellyfin/play`), `upnp.go` (AVTransport/SOAP).
- **Parameters always run via query string** (`?id=`, `?val=`, `?q=`). POST bodies are
  **never** read; `app.js` calls `POST /api/<action>` without body. Do not change to JSON bodies.
  Values are encoded server-side (`net/url`) and validated: `val` must be integer in `0..161`,
  `id` must not be a blacklisted input.
- Errors are not forwarded: upstream failures are logged and responded with `502`/`500`
  with an error message, not with the raw Go error message.
- Frontend: `static/index.html` + `static/app.js` + `static/style.css`, vanilla JS, UI
  in Winamp 2.x look (gray chrome with 1px bevels, blue title bars, green LCD,
  black playlist areas with monospace text). **No Tailwind/CDN** — the theme is fixed CSS
  with custom classes; changes to markup and CSS go together. No inline handlers in HTML —
  `bindControls()` in `app.js` wires everything via `addEventListener`. `app.js` polls
  `/api/status` every 5s via `setInterval`; in background tabs (`visibilitychange`),
  polling is paused and a running request is not overlapped by a `statusBusy` flag.
  Search results are built via `createElement`/`textContent` (external data, no `innerHTML`).
  - CSS classes that **JS** sets/toggles: `hidden` (hide everything), `track-playing`
    (current track), `sr-only` (radio inputs), `cover-img` (image in a cover tile),
    `list-hint`/`list-hint-warn` (message in a list), `row-body-hint` (message in the
    expanded area). Renaming these classes requires updating `app.js`.
  - Layout: `.app` is a grid (302px left column + rest), `.stack` stacks main window and
    search, the browse list ("Playlist Editor") is on the right. Below 820px, everything
    stacks vertically. Row markup for lists is in the `<template>` elements.
- **Search UI (as native as possible):** Sections "Albums / Artists / Tracks" with counts.
  Visibility is controlled by **pure CSS** (`#search-panel:has(#filter-...:checked) #search-results ...`);
  chips are hidden radios + labels (`#search-filters`), there is **no** JS filter state.
  Row markup is in `<template>` elements (`tpl-section`/`tpl-album`/`tpl-artist`/`tpl-track`/
  `tpl-album-track`); `app.js` clones them (`fromTemplate`) and only fills `textContent`
  (external data, no `innerHTML`). Search also triggers on typing (`scheduleSearch`, 300ms;
  Enter/Search immediately).
- **Browse UI:** Second list below search (`#browse-panel`): type (album artists/albums),
  sorting (A-Z/newest), and A-Z jump. Controls use the same patterns as search — hidden
  radios + labels in `.chip-group`, the A-Z bar are static
  `<button class="chip" data-letter=...>` in HTML (no JS-generated markup), active letter
  via `aria-pressed` (CSS `.chip[aria-pressed="true"]`). Sort chips are hidden by **CSS**
  while artists are selected (`#browse-panel:has(#browse-kind-artists:checked) #browse-sorts`).
  Libraries come as chips from `/api/jellyfin/views` (JS, because the count is only known
  at runtime) and are completely hidden when there is **only one** library.
- **Infinite scroll on scroll (`IntersectionObserver`, no scroll listener):**
  `#browse-sentinel` at the list end is observed against `root: #browse-results` with
  `rootMargin: 200px`, thus loading one page **before** it becomes visible (hides the ~60-100ms
  response time) and saves throttling/scroll calculations. After each load, the sentinel
  is re-observed — an element reports itself once initially on `observe`, and without this,
  a page that does not yet fill the space would not trigger further loading.
- **Two pitfalls with infinite scroll:** (1) The end check (`browse.start >= browse.total`)
  is in `loadBrowsePage` and **not** only in the observer callback: The sentinel remains
  visible at the list end, otherwise it would request endlessly (for an empty list, infinite
  loop). (2) A `browseRequest` counter discards responses from overtaken requests, so
  that fast switching (library/type/sort/letter) during an ongoing request does not waste
  it and does not insert an outdated page. Status line: "x of y loaded" or "y entries"
  (`y` = `TotalRecordCount`, thus honest unlike the YXC list).
- **Expand/collapse native:** Album/artist rows are `<details>/<summary>`; lazy loading
  and playing run via **event delegation** (`bindResultList(container)`; `click` and `toggle`
  in **capture** phase, because `toggle` does not bubble). The function gets the container
  as argument and is registered for `#search-results` **and** `#browse-results` — hence
  the common CSS rules for `#search-results, #browse-results` (triangle, `.track-playing`).
  The triangle comes via CSS (`summary [data-slot="title"]::before`), not from JS.
  The "Play" button is in `<summary>` and calls `event.preventDefault()`, so it does not
  trigger expansion.
- **Nested rows pitfall:** `el.querySelector('[data-slot="body"] button')` also matches
  buttons whose `[data-slot="body"]` ancestor is **outside** of `el` (e.g., the album button
  in the artist body). Use `:scope > ...` within a row. The app code does not use descendant
  selectors; only tests/extensions need to consider this.
- **"Now playing" highlight:** The current track (`getPlayInfo.track`) is highlighted in
  search results (hit rows with `Type === "Audio"` and expanded album tracks, attribute
  `data-track-name`, CSS class `.track-playing` in `index.html`). Matching is done via
  **track name** (`normalizeTitle`, tolerant), not via a Jellyfin item ID — this ensures
  the highlight also works after reload or externally started playback.
  `setNowPlaying()` in `updateMediaInfo()` updates the highlight on each poll.
- **Track duration:** Results and album track lists show duration on the right. Jellyfin
  provides it as `RunTimeTicks` (100-ns ticks); `formatDuration()` in `app.js` converts to
  `m:ss`/`h:mm:ss`. The search forwards raw fields, so `item.RunTimeTicks` is sufficient there;
  the album track endpoint uses `jellyfinItem`, which has a `RunTimeTicks` field for this.
- **Playback duration display:** Below the cover, `#media-info` shows a bar with
  `val-elapsed`/`val-total`. Source is `getPlayInfo.play_time`/`total_time` in **seconds**
  (`play_time` is negative for invalid position, e.g., `-60000` → clamped to 0). Since only
  polled every 5s, a 1s `setInterval` (`syncProgress`/`renderProgress` in `app.js`) continues
  counting locally; each poll resets the display to the device value (`performance.now()` as anchor).
  The timer only runs when `playback === "play"` (paused display stands still), and the
  whole line remains hidden when `total_time` is 0 (radio/streams), on `stop`, and for
  non-net inputs. `formatSeconds()` is the common seconds formatter;
  `formatDuration(ticks)` uses it for Jellyfin ticks.
- Changes to `static/` take effect after reload, without restarting the Go process.

## Configuration (Four Values, via Environment)

`YAMAHA_IP`, `JELLYFIN_URL`, `JELLYFIN_API_KEY`, and `PORT` are read in `main.go`
(`envOr()`, for port `envPort()` with validation). The three device values are **required**
and deliberately have **no** built-in defaults — the repo does not contain real addresses
or keys because it is public. `checkConfig()` collects all missing variables and aborts
with `log.Fatalf`; only `PORT` falls back to `defaultPort` = `8080`. Designed for
`Environment=` in the systemd service (see `README.md`): A different network thus
**requires no recompilation**.

- `PORT` is deliberately **only** a port number (`1..65535`), not an address: `listenAddr`
  is built as `":" + PORT`, binding remains on all interfaces (LAN app). An invalid value
  aborts startup via `log.Fatalf` instead of silently listening on 8080 — otherwise you
  look for the error in the wrong network. Therefore, there is no flag or frontend value:
  The frontend uses relative URLs and knows nothing about the port.
- `JELLYFIN_URL` is **not** just the address for API calls: `jellyfinStreamURL()` builds
  from it the URL that the **receiver** itself fetches via `SetAVTransportURI`. If it
  contains `localhost`/`127.0.0.1` (natural if Jellyfin runs on the same machine), the
  device loads from itself — playback starts briefly and then fails. Always enter the
  LAN-reachable address. For the same reason, the API key must be in the stream URL
  (`api_key=...`), as the device does not send an `Authorization` header.

- The values still exist **only** in the backend: The frontend does not duplicate the
  device IP (cover art runs via `/api/albumart`), the Jellyfin URL is also built in
  the search handler via `jellyfinURL`.
- `yamahaIP`, `yamahaBase`, `jellyfinURL`, `jellyfinAPIKey`, and `listenAddr` are therefore
  **variables**, not constants. The UPnP URL is built at request time via
  `getUpnpAVTransportControlURL()` from the currently selected device; only the
  device-specific port `49154` remains fixed.
- On startup, `main()` logs the active configuration (key only with last four characters,
  `maskSecret()`). If a service line is missing in `Environment=`, you see it there immediately.
- The UPnP port `49154` remains **non-configurable**.

## Known Issues (Not Surprising)

- `playJellyfinItem()` in `app.js` **actually plays now** (via the UPnP path above). Limitations:
  clicking on album/artist starts the first track, then the album auto-advances via
  `SetNextAVTransportURI` on its own; no transcoding (`static=true`),
  playback progress/artist in `getPlayInfo` are sometimes empty (tracks without `RunTimeTicks`
  in Jellyfin also provide no duration in DIDL).
- Jellyfin search fetches `/Users` first and blindly takes `users[0]`; `/api/jellyfin/views`
  thus only shows libraries of this user (no private libraries of other accounts).
- When browsing, library, type, sorting, and letter **do not** go into the URL (no deep link,
  no browser back button); the list starts at the top again after reload.
  A letter with empty result simply shows "No entries." — which letters are occupied
  is not pre-checked (that would be 26 requests).
