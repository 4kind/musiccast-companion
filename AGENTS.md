# AGENTS.md

Kontext für Coding-Agents in diesem Repository.

## Projekt

Private LAN-WebApp für **einen** Yamaha-Netzwerk-Receiver plus Jellyfin-Musiksuche.
Das Go-**Backend** (Paket `main`, aufgeteilt auf `main.go`/`yamaha.go`/`jellyfin.go`/`upnp.go`)
ist ein **Proxy**: es liefert `static/` aus und reicht alle Geräteaufrufe weiter.
Der Browser spricht **nie** direkt mit der Yamaha oder Jellyfin — deshalb gibt es hier keine
CORS- und kein Mixed-Content-Problem. Bitte nicht "optimieren", indem JS direkt auf die Geräte-IP zeigt.

## Commands

```bash
go run .      # aus dem Repo-Root! → http://localhost:8080
go vet ./...
gofmt -l .    # muss leer bleiben
```

- **Immer aus dem Repo-Root starten.** `http.FileServer(http.Dir("./static"))` ist relativ;
  aus einem anderen Verzeichnis ist die Seite leer. (Der systemd-Dienst setzt dafür
  `WorkingDirectory`, siehe `README.md`.)
- Port: `listenAddr` ist `":" + PORT` (Vorgabe **8080**, per Umgebungsvariable `PORT` änderbar,
  geprüft auf `1..65535`). Läuft der Port noch, blockiert ein alter Prozess:
  `pkill -f musiccast-companion` bzw. `fuser -k 8080/tcp`. (Achtung: `pkill -f` kann die eigene
  Shell treffen — im Zweifel `fuser -k <port>/tcp`.)
- **Stolperfalle „alter Server“:** `static/` wird zur Laufzeit von der Platte gelesen. Ein noch
  laufender **alter** Binary liefert deshalb das **neue Frontend** (z. B. `app.js` mit
  `/api/albumart`) mit dem **alten Backend** (Route fehlt) → Cover kaputt, obwohl der Code stimmt.
  Nach Backend-Änderungen sicherstellen, dass wirklich neu gestartet wurde und nur **ein**
  Prozess auf `:8080` lauscht.
- Keine Dependencies (`go.mod` ohne `require`, kein `go.sum`), kein Build-Schritt, kein npm.
  `go.mod` verlangt `go 1.22` (älter kann die Method-Patterns des `ServeMux` nicht); neuere
  Werkzeuge bauen unverändert.
- Keine Tests, kein Lint-/CI-/Formatter-Setup. Ein `go build .` legt im Repo-Root ein Binary
  `musiccast-companion` ab — Build-Artefakt, gehört nicht ins Repo (`.gitignore`).
- Das Repo hat ein Git-Remote (`origin`, Branch `main`); vor Änderungen `git status` prüfen.

## LAN-Betrieb

`listenAddr` (aus `PORT`, Vorgabe `:8080`) lauscht auf **allen** Schnittstellen, nicht nur auf
`localhost` — die App ist damit im LAN erreichbar, solange der Prozess läuft. Die Adresse muss
man nicht raten: `main()`
gibt sie beim Start aus (`im LAN erreichbar unter http://192.168.1.10:8080/`), ermittelt von
`lanURLs()` aus `net.InterfaceAddrs()` (ohne Loopback- und Link-Local-Adressen, IPv4 zuerst).
Das ist reine Anzeige — die Bindung bleibt bei allen Schnittstellen, nur der Port ist
einstellbar.

- **Kein Auth:** Wer im LAN ist, kann Receiver und Jellyfin steuern. Das ist für ein privates
  Heimnetz gewollt, aber der Grund, die App nicht per Portfreigabe ins Internet zu hängen.
- **Styling kommt aus dem Repo** (`static/style.css`, Winamp-2-Optik). Kein CDN-Abruf, die Seite
  sieht also auch ohne Internet vollständig aus.
- Ein Prozess pro Port: Läuft noch ein alter, blockiert er den neuen (`*:8080` belegt).
- **Firewall:** Ist auf dem Rechner eine Firewall aktiv (z. B. ufw mit
  `DEFAULT_INPUT_POLICY=DROP`), scheitert ein Test vom Handy an der Firewall, **nicht** an der
  Bindung: `:8080` lauscht bereits auf allen Adressen (`ss -ltnp | grep 8080` → `*:8080`),
  `0.0.0.0` wäre dieselbe Bindung ohne IPv6. Freigabe braucht Root, z. B.
  `sudo ufw allow from <lan>/24 to any port 8080 proto tcp`. Ein Test vom selben Rechner auf die
  eigene LAN-IP beweist nichts — solche Pakete laufen über `lo` und werden von ufw nicht gefiltert.
- **Dauerbetrieb auf dem NAS:** `README.md` beschreibt die Installation als systemd-Dienst
  (`/opt/musiccast-companion`, `Environment=YAMAHA_IP/JELLYFIN_URL/JELLYFIN_API_KEY`,
  `Restart=always`, Journal-Log). Hier im Repo gibt es **keine** Unit und keinen Installer —
  bewusst, die Installation gehört auf den Zielrechner. `Restart=always` statt `on-failure`,
  weil SIGTERM für systemd ein sauberes Ende ist und `on-failure` dann nicht greift.

## Verifiziertes Gerät

```
GET http://<receiver-ip>/YamahaExtendedControl/v1/system/getDeviceInfo
→ model_name R-N602, api_version 2.08, system_version 1.36, eine Zone ("main")
```

- Das Gerät ist im LAN erreichbar. **Read-only-GETs zum Verifizieren sind okay**,
  z. B. `curl -s http://<receiver-ip>/YamahaExtendedControl/v1/main/getStatus`.
  `POST /api/play`, `/api/stop`, `/api/input`, `/api/volume` ändern dagegen den echten
  Wiedergabezustand — nicht zum Testen verwenden.
- **Kein Auth.** Die Basic-Spec v2.0 kennt kein `authKey`/`authID`/`authPwd`
  (0 Treffer im Dokument), und das Gerät antwortet ohne Credentials. Den Auth-Session-Flow
  nicht "nachrüsten".
- **Volume: min 0, max 161, step 1** — das Gerät liefert das selbst:
  `getStatus → max_volume` bzw. `getFeatures → zone[].range_step[id=volume]`.
  Der Slider bekommt seinen `max` zur Laufzeit aus `getStatus.max_volume` gesetzt
  (`fetchStatus()` in `app.js`); das HTML-Attribut ist nur ein Startwert. Wer die Lautstärke
  anfasst, liest den Bereich aus dem Gerät, nicht aus dem Code. Es ist **nicht** 0–100.
- 22 Inputs; `/api/inputs` liefert davon 8 (`airplay`, `server`, `net_radio`, `bluetooth`,
  `usb`, `tuner`, `optical1`, `line1`). Die Auswahl steuert allein die `hiddenInputs`-Blacklist
  in `main.go`; `deezer` steht mittlerweile ebenfalls darin. `POST /api/input` weist geblacklistete
  Eingänge mit `400` zurück.
- Zusätzlich ist das Gerät ein **UPnP-MediaRenderer** (`DMR-1.50`, Service `AVTransport:1`).
  Die Gerätebeschreibung liegt auf Port `49154` (`/MediaRenderer/desc.xml`), der Control-Endpunkt
  unter `/AVTransport/ctrl`. SSDP-Multicast braucht es nicht (im Dev-Netz teils geblockt), der
  Port ist fest verdrahtet. Darüber läuft die Jellyfin-Wiedergabe (siehe unten).

## Yamaha-Anbindung: Code schlägt Doku

Alle Upstreams sind `/YamahaExtendedControl/v1/…`:

| Route | Upstream (alle GET) |
|---|---|
| `GET /api/status` | `main/getStatus` |
| `GET /api/inputs` | `system/getFeatures`, gefiltert |
| `GET /api/playinfo` | `netusb/getPlayInfo` (+ UPnP `GetPositionInfo`, nur für fehlendes `total_time`) |
| `GET /api/albumart?path=` | `http://{host}{path}` (Cover-Art, Pfad aus `playinfo`) |
| `POST /api/play` | bei Jellyfin-Kontext: UPnP `Play`; sonst `netusb/setPlayback?playback=play` |
| `POST /api/stop` | bei `input == "server"`: UPnP `Stop`; sonst `netusb/setPlayback?playback=stop` |
| `POST /api/next` / `prev` | Jellyfin-Kontext (siehe unten); sonst `netusb/setPlayback?playback=next`/`previous` |
| `POST /api/input?id=` | `main/setInput?input=` |
| `POST /api/volume?val=` | `main/setVolume?volume=` |

- **Auch Setter sind GET mit Query-Parametern** — die Spec sagt für `setVolume`, `setInput`,
  `setPlayback` ausdrücklich `Method: GET`. Es gibt hier kein PUT/POST gegen das Gerät, auch
  nicht bei "schreibenden" Aktionen. `http.Get` in `main.go` ist also korrekt, nicht schlampig.
- Die `PUT /volume`-Tabelle aus alten Notizen ist falsch — sie stammt nicht aus dieser API.
- `getPlayInfo` liefert ein **eigenes** `input`-Feld, das vom Zone-Input abweichen kann
  (gemessen: Zone `optical1`, PlayInfo `airplay`). Das Frontend entscheidet über die
  Media-Anzeige aber anhand des **Zone**-Inputs — bei Net/USB-Eingängen stimmen beide überein.
- **`/api/playinfo` reichert fehlende Zeiten aus dem Renderer an:** Für UPnP-Wiedergabe
  (Jellyfin, `input: "server"`) liefert `getPlayInfo` zwar `play_time`, aber **`total_time` = 0**
  (gemessen: `play_time` 45, `total_time` 0, obwohl der Renderer `TrackDuration 0:04:18` kennt).
  `handlePlayInfo` in `yamaha.go` fragt deshalb bei `input == "server"` und `total_time == 0`
  per UPnP `GetPositionInfo` (`upnpPositionInfo`) `TrackDuration`/`RelTime` ab und trägt sie nach
  (`fillUPnPTimes`); `play_time` wird nur ersetzt, wenn das Gerät selbst 0 liefert. Dekodiert wird
  mit `json.Decoder.UseNumber()`, damit alle übrigen Felder exakt erhalten bleiben.
  Die SOAP-Antwort ist **kein wohlgeformtes XML** (TrackURI enthält ein nacktes `&`, z. B.
  `…&static=true`), daran scheitert `encoding/xml` — die zwei Werte liest deshalb `soapValue`
  per Textsuche.
- `albumart_url` ist ein absoluter Pfad mit führendem Slash
  (gemessen: `/YamahaRemoteControl/AlbumART/AlbumART<n>.jpg`). Bei keinem Titel ist es `""`.
  Das Frontend präfixiert **nicht** mehr selbst mit der Geräte-IP, sondern ruft
  `/api/albumart?path=…` auf; dort hängt `yamaha.go` die `yamahaIP` an und reicht das Bild durch.
  Pfade ohne führenden Slash oder mit `..` werden mit `400` abgelehnt.
- `/api/albumart` reicht nur **erfolgreiche** Bilder durch: antwortet das Gerät nicht mit `200`
  (z. B. veraltete AlbumART-ID → `404`), kommt `502` zurück. Sonst würde aus dem Geräte-Fehler ein
  leeres `200`, das der Browser als kaputtes Cover rendert und ggf. zwischenspeichert.
  Das Frontend lädt das Bild nur bei geändertem `albumart_url` neu und blendet es bei einem
  Lade-`error` aus (nächster Poll versucht erneut), statt ein „Bild kaputt“-Icon zu zeigen.
  Das `<img id="val-art">` hat bewusst **kein** `src=""`: ein leerer `src` wird vom Browser zur
  Seiten-URL aufgelöst und löst sonst beim Initialladen einen falschen Fehler aus; der
  `error`-Handler ignoriert Fehler ohne echtes (per `dataset.src` gesetztes) Cover.
- **Cover nach dem automatischen Weiterschalten:** `albumart_url` ist nach dem Übergang per
  `SetNextAVTransportURI` **nicht** verlässlich. Gemessen: Der Renderer vergibt eine neue
  `albumart_id` (z. B. `8930`), liefert aber `albumart_url: ""`; die daraus gebaute Datei
  `/YamahaRemoteControl/AlbumART/AlbumART<n>.jpg` ist dann `404`. Beim manuellen Start/Skip legt
  das Gerät das Cover dagegen sofort an. Das Backend schickt für eigene Jellyfin-Titel deshalb
  `jellyfin_album_id` und `jellyfin_image_tag` in `/api/playinfo` mit (`jellyfinCoverFor` in
  `jellyfin.go`: `AlbumId`/`AlbumPrimaryImageTag`, sonst `Id`/`ImageTags.Primary`) — erkannt am
  **Titelnamen** gegen den Queue-Kontext (Liste bzw. Zufalls-`current`, zusätzlich `next`, weil
  die Position erst im 2-s-Takt nachzieht). Nur so bekommt ein von außen (MusicCast-App)
  gestarteter Titel kein fremdes Cover. Das Frontend nimmt das Geräte-Cover, wenn es eins gibt,
  sonst `/api/jellyfin/image?id=…&tag=…`; ohne beides bleibt das Bild ausgeblendet.

## Jellyfin-Suche

`GET /api/jellyfin/search?q=` sucht in **drei getrennten** Abfragen: `MusicAlbum`
(Limit 12), `MusicArtist` (Limit 8) und `Audio` (Limit 25), und liefert sie
**strukturiert** zurück (`{"albums":[…],"artists":[…],"tracks":[…]}`), nicht mehr als
eine flache `Items`-Liste. Grund: Jellyfin rankt Titel zuerst; mit einem gemeinsamen
Limit verdrängen die vielen Titel die Alben, bei häufigen Begriffen (z. B. `the`) bleiben
sonst **gar keine** Alben übrig. Die Trennung erlaubt eigene Limits je Typ und dem
Frontend eine Abschnitts-Darstellung. `GET /api/jellyfin/tracks?parent_id=` liefert die
Titel eines Albums fürs Aufklappen in der Trefferliste.

## Jellyfin-Stöbern

`GET /api/jellyfin/views` liefert die Musik-**Bibliotheken** (`/Users/{id}/Views`, gefiltert
auf `CollectionType == "music"` — Filme/Serien/Bücher gehören nicht in eine Musik-Oberfläche).
Sichtbar sind nur Bibliotheken des **API-Key-Benutzers**; die private Bibliothek eines anderen
Benutzers fehlt hier, dafür bräuchte es einen Admin-Key oder einen zweiten Benutzer-Key.

`GET /api/jellyfin/browse` ist die eine Liste zum Stöbern:
`?kind=artists|albums&parent_id=&artist_id=&letter=&sort=name|newest&start=&limit=`
und liefert `{"items":[…],"total":n}` (`total` = Jellyfin `TotalRecordCount`).

- `kind=artists` → `/Artists/AlbumArtists` (mit `userId`), **nicht** `IncludeItemTypes=MusicArtist`:
  Letzteres liefert alle Titel-Features mit (gemessen 5287 statt 1275 Einträge, weil jeder
  „feat."-Auftritt als eigener Künstler zählt).
- `kind=albums` → `/Users/{id}/Items` mit `IncludeItemTypes=MusicAlbum`; `artist_id` setzt
  `AlbumArtistIds` (+ `SortBy=ProductionYear,SortName`, wie beim Aufklappen), `sort=newest`
  setzt `SortBy=DateCreated&SortOrder=Descending`, sonst `SortBy=SortName`.
- `letter` setzt `NameStartsWithOrGreater` (A–Z-Sprung). Weitergescrollt wird **innerhalb**
  dieser gefilterten Liste — deshalb braucht es keine Index-Rechnerei und keine Sorge, dass
  Namens- und Sortierreihenfolge auseinanderlaufen. Gemessen: Alben ≥M = 1418, Künstler ≥M = 553.
- `start` (0…1000000) und `limit` (1…200, Vorgabe **100**) werden serverseitig begrenzt;
  unbekanntes `kind` und `letter` ≠ A–Z werden mit `400` abgewiesen.
- `/api/jellyfin/artist-albums` gibt es **nicht mehr**: Das Aufklappen eines Interpreten in den
  Suchergebnissen ruft denselben Endpunkt (`browse?kind=albums&artist_id=…&limit=100`) und liest
  `data.items`. Eine Route statt zweier Wege für dieselbe Frage.

**Warum 100 statt 8:** Die YXC-Liste erzwingt 8 Zeilen pro Abruf (~84–111 ms, 3197 Alben =
400 Abrufe ≈ 38 s). Jellyfin liefert in ~60–98 ms dieselbe Menge in **einer** Antwort
(gemessen: `Limit=50` → 62 ms, `Limit=200` → 98 ms), ein voller Durchlauf wären 16 Abrufe.
Deshalb ist Stöbern gegen Jellyfin gebaut und nicht gegen die Geräteliste (die YXC-Browse
könnte nur 8 Zeilen und bietet keine Suche).

## Jellyfin-Wiedergabe (UPnP AVTransport)

`POST /api/jellyfin/play?item_id=` startet einen konkreten Titel — das kann die MusicCast-API
nicht ("spiele URL X" existiert nicht). Ablauf in `handleJellyfinPlay`:

1. Jellyfin-Item zu `item_id` laden; bei `Type != "Audio"` (Album/Interpret) den ersten
   `Audio`-Titel darunter nehmen.
2. Static-Stream-URL bauen: `{jellyfinURL}/Audio/{id}/stream?static=true&api_key=…`
   (der Key muss in der URL stehen — das **Gerät** lädt sie, nicht unser Client).
3. DIDL-Lite mit passender `protocolInfo` (`audio/flac`, `audio/mpeg`, …) **und** der Titeldauer
   als `res@duration` (`H:MM:SS.mmm`, aus `RunTimeTicks/10^7`) erzeugen. Ohne dieses Attribut
   meldet der Renderer `MediaDuration`/`TrackDuration` = `0:00:00` (gemessen per UPnP
   `GetMediaInfo`) und damit auch `total_time` = 0 — die Laufzeit-Anzeige bliebe leer.
4. `SetAVTransportURI` + `Play` per SOAP an `/AVTransport/ctrl` des Renderers, danach
   `primeNext()` für den Folgetitel (siehe „Automatische Fortsetzung").

- **Die Setter sind hier echte HTTP-POSTs mit SOAP** — anders als die YXC-API (dort GET).
  Nicht "vereinheitlichen".
- Der Eingang springt automatisch auf `server` (UPnP), Wiedergabe meldet `playback: "play"`.
- **Stoppen** (`POST /api/stop`, `handleStop` in `yamaha.go`) hat zwei Wege: Bei `input == "server"`
  ist die Wiedergabe die UPnP-Session des Renderers — dort beendet ein **UPnP-`Stop`** sie sofort
  (YXC meldet danach `playback: "stop"`). Nur bei anderen Eingängen läuft der YXC-Weg
  `netusb/setPlayback?playback=stop`. Grund (gemessen): Der YXC-Stop sperrt den Renderer rund
  **4 Sekunden** lang; in dieser Zeit scheitert jedes `Play` mit SOAP **501 Action Failed** (auch
  nach `SetInput` oder einem zusätzlichen UPnP-`Stop`). Über den YXC-Weg gestoppt, hätte jedes
  schnelle „Stop, dann anderen Titel starten" ein `502` geliefert.
  `handlePlay` spiegelt das: Gibt es einen Jellyfin-Kontext (`hasJellyfinContext()`), startet
  UPnP-`Play` die Renderer-Session weiter und `primeNext()` legt den Folgetitel wieder nach;
  ohne Kontext (Radio/USB, MusicCast-eigene Queue) bleibt es beim YXC-Play. Ein YXC-Play bei
  `server` startet sonst die geräteeigene MusicCast-Queue (gemessen: falscher Titel, Renderer
  blieb `STOPPED`).
- **Automatische Fortsetzung** (`SetNextAVTransportURI`, der Kern gegen „Album endet nach Track 1"):
  Nach jedem Start hinterlegt `primeNext()` (`jellyfin.go`) den Folgetitel im Renderer — im Album
  den nächsten Listeneintrag, im Zufallsmodus einen neuen Würfelwurf. Das **Gerät** schaltet am
  Titelende selbst weiter (live verifiziert: nach ~10 s von A auf B), ohne Stop/Start-Sprung.
  `autoAdvanceLoop()` fragt dafür alle `autoAdvanceInterval` (2 s) `GetPositionInfo` ab: Enthält
  `TrackURI` die ID des hinterlegten Titels, ist das Gerät weitergeschaltet → Position
  (`playQueue.index` bzw. Zufalls-`current`) nachziehen und den nächsten hinterlegen.
  - Am Listenende wird der Folgetitel **gelöscht** (leere `NextURI`), dann bleibt die Wiedergabe
    nach dem letzten Titel stehen (verifiziert: Zustand `STOPPED`, kein Weiterlaufen).
  - Die Metadaten (`NextURIMetaData` mit DIDL inkl. `res@duration`) sind Pflicht: Ohne sie kennt
    der Renderer die Dauer des Folgetitels nicht und meldet `TrackDuration` = `0:00:00`.
  - `clearJellyfinNext()` (Stop, Quellenwechsel ≠ `server`) löscht den Eintrag im Gerät **und** im
    Backend; sonst würde ein späteres `Play` in den alten Folgetitel springen. Skip/Prev und der
    Zufallsknopf rufen danach `primeNext()` erneut auf.
  - Die Schleife fragt nur, solange ein Folgetitel hinterlegt ist. Meldet der Renderer ~30 Runden
    (~1 min) keine Wiedergabe (Gerät aus, am Gerät die Quelle gewechselt), wird der Folgetitel
    vergessen statt endlos weiterzupollen; geloggt wird nur der erste Fehlversuch.
  - `Seek` wird vom Gerät **ignoriert** (SOAP-Antwort ok, `RelTime` läuft unverändert weiter) —
    nicht zum Testen von Titelenden verwenden, dafür kurze Titel nehmen.
- **Skip/Prev** (`POST /api/next` / `/api/prev`, Buttons ⏮/⏭): Das Backend merkt sich beim Start
  die geordnete Titelliste des Albums/Interpreten (`jellyfinPlayContext`) samt Position und spielt
  beim Skip den Nachbar-Titel per UPnP. Grund: `netusb/setPlayback?playback=next` springt in die
  **geräteeigene** Queue (gemessen: fremde, nicht zum Album gehörende Titel), nicht ins gespielte
  Jellyfin-Album. Ohne Jellyfin-Kontext (Radio, USB) werden `next`/`previous` an das Gerät
  durchgereicht; ein Quellenwechsel (`POST /api/input` ≠ `server`) löscht den Kontext. Am
  Listenanfang/-ende wird nicht gesprungen (`{"moved": false}`).
- **Zufallswiedergabe** (`POST /api/jellyfin/random`, Button 🎲 Zufall): würfelt einen Titel und
  schaltet damit vom Listen- auf den **Zufallsmodus** um — ⏭ würfelt dann jedes Mal neu. Der Knopf
  ist kein Einzelstart: Ohne das Umschalten würde ⏭ nach einem Zufallstitel in die geräteeigene
  Queue springen (siehe oben), also wäre die Zufallswiedergabe nach einem Lied zu Ende.
  - Gewürfelt wird mit Jellyfin `SortBy=Random&Limit=1` (gemessen **~250–340 ms**). Bewusst
    **nicht** „Gesamtzahl holen, zufälligen `StartIndex` wählen": `TotalRecordCount` kostet in
    dieser Bibliothek gemessen **~42 s** (43985 Titel).
  - Die Bibliothek muss immer mit `parentId` gesetzt sein. Ohne `parentId` durchsucht Jellyfin
    **alle** Bibliotheken (inklusive Hörbücher) und läuft in einen **Timeout**. Deshalb nimmt
    `?parent_id=` die im Frontend gewählte Bibliothek (Fallback: erste Musik-Bibliothek), geprüft
    gegen `/Users/{id}/Views` (`jellyfinLibraryID`).
  - `planShuffleStep()` plant den Skip **ohne Netzzugriff** (der Würfelwurf passiert außerhalb der
    Sperre), `history` (max. `shuffleHistoryLimit` = 50) macht ⏮ möglich: ⏭ legt den laufenden Titel
    in den Verlauf, ⏮ nimmt den letzten heraus. Am Anfang der Zufallsreihe: `{"moved": false}`.
    Ein zweiter Klick auf 🎲 in derselben Bibliothek behält den Verlauf, eine andere Bibliothek
    startet eine frische Sitzung. Jeder Start über `/api/jellyfin/play` beendet den Zufallsmodus.
  - `GET /api/playinfo` bekommt ein Feld **`jellyfin_shuffle`** (bool). Der Name ist bewusst
    eigen: `shuffle` ist in getPlayInfo schon belegt — das ist der Shuffle der **geräteeigenen**
    Queue (dazu gibt es dort auch `shuffle_available`) und darf nicht überschrieben werden.
    Der 🎲-Knopf zeigt darüber seinen Zustand (`aria-pressed`), gespeist aus dem 5-s-Poll.
  - Live verifiziert: Zufall → `shuffle=true`, ⏭ = anderer Titel, ⏮ = der vorherige
    (24 ms, ohne Jellyfin-Abruf), ⏮ am Anfang = `moved:false`, Albumstart = `shuffle=false`.
  - Der Zufall läuft auch **von selbst** weiter: `primeNext()` würfelt schon beim Start den
    Folgetitel und hinterlegt ihn im Renderer; nach dem Wechsel wandert der gespielte Titel in den
    Verlauf und es wird der nächste gewürfelt. ⏮ funktioniert damit auch nach automatisch
    weitergelaufenen Titeln. Nach einem ⏮ wird die Zukunft neu gewürfelt (der Verlauf ist dann leer).
- Vereinfachungen: Ein Klick auf Album/Interpret startet den **ersten** Titel; danach läuft das
  Album von selbst weiter (siehe automatische Fortsetzung) und Skip/Prev blättert darin. `static=true`
  (keine Transkodierung — exotische Codecs können scheitern).
- **Reihenfolge der Titelliste:** `jellyfinAudioChildren` sortiert mit
  `SortBy=ParentIndexNumber,IndexNumber`. `ParentIndexNumber` ist die **Disc**-Nummer; nur nach
  `IndexNumber` sortiert Jellyfin Mehr-CD-Alben falsch (gemessen: Track 1 von Disc 2 landet
  direkt hinter Track 1 von Disc 1).
- Die Trefferliste ist in **Abschnitte** „Alben / Interpreten / Titel" (mit Anzahl) gegliedert
  und lässt **Alben** per Klick auf den Namen auf-/zuklappen (natives `<details>/<summary>`, siehe
  Suche-UI unten). Die Titel kommen über
  `GET /api/jellyfin/tracks?parent_id=…` (Audio-Kinder, nach Tracknummer sortiert, max. 300);
  ein Klick auf eine Titelzeile spielt genau diesen Titel. **Interpreten** klappen ebenfalls auf,
  aber zu ihren **Alben** (`GET /api/jellyfin/browse?kind=albums&artist_id=…`) — die rekursive
  Titelliste eines Interpreten wäre zu groß; diese Alben sind selbst wieder aufklappbar.
- Zeilen zeigen Cover-Thumbnails über `GET /api/jellyfin/image?id=&tag=` (Jellyfin
  `Items/{id}/Images/Primary?maxHeight=96`, `Cache-Control` 1 Tag; `id` wird auf `/` und `..`
  geprüft). Alben zeigen Interpret · Jahr und Gesamtlaufzeit, Titel Tracknummer · Interpret ·
  Album und Dauer. Der Proxy hält den Browser same-origin (wie `/api/albumart`); Jellyfin
  braucht den `Authorization`-**Header** — `api_key` als Query liefert bei `/Users` ein `401`.

API-Referenz: `docs/YXC_API_Spec_Basic_v2.0.pdf` (Rev. 2.00, 129 Seiten),
`docs/YXC_API_Spec_Advanced_v2.0.pdf` für Zonen-/Gruppen-Themen.
Ein Text-Extrakt liegt **nicht** im Repo — bei Bedarf selbst erzeugen, dann ist die Spec
durchsuchbar statt 129 Seiten PDF:

```bash
pdftotext -layout docs/YXC_API_Spec_Basic_v2.0.pdf /tmp/opencode/yxc.txt
```

## Backend/ Frontend

- Das Backend ist ein `net/http.ServeMux` mit Go-1.22-Method-Patterns (`"GET /api/status"`).
  Kein Router, kein Framework, keine Schichten. Die Handler sind dünne, benannte Funktionen;
  den gemeinsamen Yamaha-Proxy (URL bauen, `http.Get`, Body durchkopieren) bündelt `proxyYamaha()`,
  Jellyfin läuft über `jellyfinGet()`. Alle Upstream-Aufrufe teilen sich `httpClient` mit Timeout.
  Neue Endpunkte genauso direkt ergänzen. **Aufteilung im Paket `main`**:
  `main.go` (Konfiguration, Client, Routen), `yamaha.go` (YXC), `jellyfin.go` (Jellyfin-API +
  Titel-Auflösung + `/api/jellyfin/play`), `upnp.go` (AVTransport/SOAP).
- **Parameter laufen immer über den Query-String** (`?id=`, `?val=`, `?q=`). POST-Bodies werden
  **nie** gelesen; `app.js` ruft `POST /api/<action>` ohne Body auf. Nicht auf JSON-Bodies umbauen.
  Werte werden serverseitig kodiert (`net/url`) und geprüft: `val` muss Integer in `0..161` sein,
  `id` kein geblacklisteter Eingang.
- Fehler werden nicht durchgereicht: Upstream-Ausfälle werden geloggt und als `502`/`500` mit
  deutscher Meldung beantwortet, nicht mit der rohen Go-Fehlermeldung.
- Frontend: `static/index.html` + `static/app.js` + `static/style.css`, Vanilla JS, deutsches UI
  im Look von Winamp 2.x (graues Chrom mit 1-px-Bevels, blaue Titelleisten, grünes LCD,
  schwarze Playlist-Flächen mit Monospace-Text). **Kein Tailwind/CDN** — das Theme ist festes CSS
  mit eigenen Klassen; Änderungen an Markup und CSS gehören zusammen. Keine Inline-Handler im
  HTML — `bindControls()`
  in `app.js` verdrahtet alles per `addEventListener`. `app.js` pollt `/api/status` alle 5 s per
  `setInterval`; im Hintergrund-Tab (`visibilitychange`) wird das Polling pausiert und ein
  laufender Request durch ein `statusBusy`-Flag nicht überlappt. Suchergebnisse werden per
  `createElement`/`textContent` gebaut (Fremddaten, kein `innerHTML`).
  - CSS-Klassen, die **JS** setzt/umschaltet: `hidden` (alles ausblenden), `track-playing`
    (laufender Titel), `sr-only` (Radio-Eingänge), `cover-img` (Bild in einer Cover-Kachel),
    `list-hint`/`list-hint-warn` (Meldung in einer Liste), `row-body-hint` (Meldung im
    aufgeklappten Bereich). Wer diese Klassen umbenennt, muss `app.js` mitändern.
  - Layout: `.app` ist ein Grid (302 px linke Spalte + Rest), `.stack` stapelt Hauptfenster und
    Suche, die Stöber-Liste („Playlist Editor") liegt rechts. Unter 820 px steht alles
    untereinander. Zeilen-Markup der Listen steht in den `<template>`-Elementen.
- **Suche-UI (möglichst nativ statt JS):** Abschnitte „Alben / Interpreten / Titel" mit Anzahl.
  Die Sichtbarkeit regelt **reines CSS** (`#search-panel:has(#filter-…:checked) #search-results …`);
  die Chips sind versteckte Radios + Labels (`#search-filters`), es gibt **keinen** JS-Filterzustand.
  Zeilen-Markup steht in `<template>`-Elementen (`tpl-section`/`tpl-album`/`tpl-artist`/`tpl-track`/
  `tpl-album-track`); `app.js` klont sie (`fromTemplate`) und füllt nur `textContent` (Fremddaten,
  kein `innerHTML`). Gesucht wird auch beim Tippen (`scheduleSearch`, 300 ms; Enter/Suchen sofort).
- **Stöbern-UI:** Zweite Liste unter der Suche (`#browse-panel`): Art (Album-Künstler/Alben),
  Sortierung (A–Z/Neueste) und ein A–Z-Sprung. Bedienelemente sind dieselben Muster wie bei der
  Suche — versteckte Radios + Labels in `.chip-group`, die A–Z-Leiste sind statische
  `<button class="chip" data-letter=…>` im HTML (kein JS-erzeugtes Markup), aktiver Buchstabe
  über `aria-pressed` (CSS `.chip[aria-pressed="true"]`). Die Sortier-Chips blendet **CSS** aus,
  solange Künstler gewählt sind (`#browse-panel:has(#browse-kind-artists:checked) #browse-sorts`).
  Bibliotheken kommen als Chips aus `/api/jellyfin/views` (JS, weil die Anzahl erst zur Laufzeit
  feststeht) und werden bei nur **einer** Bibliothek ganz ausgeblendet.
- **Nachladen beim Scrollen (`IntersectionObserver`, kein Scroll-Listener):** `#browse-sentinel`
  am Listenende wird gegen `root: #browse-results` mit `rootMargin: 200px` beobachtet, lädt also
  eine Seite **vor** dem Sichtbarwerden nach (versteckt die ~60–100 ms Antwortzeit) und erspart
  Throttling/Scroll-Rechnerei. Nach jedem Laden wird der Sentinel neu beobachtet — ein Element
  meldet sich beim `observe` einmal initial, und ohne das würde eine Seite, die den Platz noch
  nicht füllt, kein weiteres Nachladen anstoßen.
- **Zwei Fallen beim Nachladen:** (1) Die Ende-Prüfung (`browse.start >= browse.total`) sitzt in
  `loadBrowsePage` und **nicht** nur im Observer-Callback: Der Sentinel bleibt am Listenende
  sichtbar, sonst würde er endlos weiter anfragen (bei leerer Liste als Endlosschleife).
  (2) Ein `browseRequest`-Zähler verwirft die Antwort eines überholten Abrufs, damit schnelles
  Umschalten (Bibliothek/Art/Sortierung/Buchstabe) während eines laufenden Abrufs nicht verpufft
  und keine veraltete Seite einsortiert. Statuszeile: „x von y geladen" bzw. „y Einträge"
  (`y` = `TotalRecordCount`, deshalb ehrlich im Gegensatz zur YXC-Liste).
- **Auf-/Zuklappen nativ:** Album-/Interpreten-Zeilen sind `<details>/<summary>`; Lazy-Load und
  Abspielen laufen über **Event-Delegation** (`bindResultList(container)`; `click` sowie `toggle`
  in der **Capture**-Phase, weil `toggle` nicht blubbert). Die Funktion bekommt den Container als
  Argument und wird für `#search-results` **und** `#browse-results` registriert — daher die
  gemeinsamen CSS-Regeln für `#search-results, #browse-results` (Dreieck, `.track-playing`).
  Das Dreieck kommt per CSS (`summary [data-slot="title"]::before`), nicht aus JS. Der
  „Abspielen"-Button sitzt im `<summary>` und ruft `event.preventDefault()`, damit er nicht
  mit-aufklappt.
- **Stolperfalle verschachtelte Zeilen:** `el.querySelector('[data-slot="body"] button')` matcht auch
  Buttons, deren `[data-slot="body"]`-Vorfahre **außerhalb** von `el` liegt (z. B. den Album-Button
  im Interpret-Body). Innerhalb einer Zeile `:scope > …` verwenden. Der App-Code nutzt keine
  Nachfahren-Selektoren; nur Tests/Erweiterungen müssen das beachten.
- **„Läuft gerade"-Markierung:** Der laufende Titel (`getPlayInfo.track`) wird in den
  Suchergebnissen hervorgehoben (Trefferzeilen mit `Type === "Audio"` und aufgeklappte
  Album-Titel, Attribut `data-track-name`, CSS-Klasse `.track-playing` in `index.html`).
  Abgeglichen wird über den **Titelnamen** (`normalizeTitle`, tolerant), nicht über eine
  Jellyfin-Item-ID — so stimmt die Markierung auch nach Reload oder fremd gestarteter Wiedergabe.
  `setNowPlaying()` in `updateMediaInfo()` aktualisiert die Markierung bei jedem Poll.
- **Titeldauer:** Treffer- und Album-Titellisten zeigen rechts die Laufzeit. Jellyfin liefert sie
  als `RunTimeTicks` (100-ns-Ticks); `formatDuration()` in `app.js` macht daraus `m:ss`/`h:mm:ss`.
  Die Suche reicht die Rohfelder durch, deshalb genügt dort `item.RunTimeTicks`; der
  Album-Titel-Endpunkt nutzt `jellyfinItem`, das dafür ein `RunTimeTicks`-Feld hat.
- **Laufzeit-Anzeige der laufenden Wiedergabe:** Unter dem Cover zeigt `#media-info` einen Balken
  mit `val-elapsed`/`val-total`. Quelle ist `getPlayInfo.play_time`/`total_time` in **Sekunden**
  (`play_time` ist bei ungültiger Position negativ, z. B. `-60000` → auf 0 geklemmt). Da nur alle
  5 s gepollt wird, zählt ein 1-s-`setInterval` (`syncProgress`/`renderProgress` in `app.js`) lokal
  weiter; jeder Poll setzt die Anzeige auf den Gerätewert zurück (`performance.now()` als Anker).
  Der Timer läuft nur bei `playback === "play"` (bei `pause` steht die Anzeige still), und die
  ganze Zeile bleibt ausgeblendet, wenn `total_time` 0 ist (Radio/Streams), bei `stop` und bei
  Nicht-Net-Eingängen. `formatSeconds()` ist der gemeinsame Sekunden-Formatter;
  `formatDuration(ticks)` nutzt ihn für die Jellyfin-Ticks.
- Änderungen an `static/` wirken nach Reload, ohne den Go-Prozess neu zu starten.

## Konfiguration (vier Werte, per Umgebung)

`YAMAHA_IP`, `JELLYFIN_URL`, `JELLYFIN_API_KEY` und `PORT` werden in `main.go` gelesen
(`envOr()`, für den Port `envPort()` mit Prüfung). Die drei Gerätewerte sind **Pflicht** und
haben bewusst **keine** eingebauten Vorgaben — im Repo stehen keine echten Adressen oder Keys,
weil es öffentlich ist. `checkConfig()` sammelt alle fehlenden Variablen und bricht mit
`log.Fatalf` ab; nur `PORT` fällt auf `defaultPort` = `8080` zurück. Gedacht für
`Environment=` im systemd-Dienst (siehe `README.md`): Ein anderes Netz braucht so **keine
Neukompilierung**.

- `PORT` ist bewusst **nur** eine Portnummer (`1..65535`), keine Adresse: `listenAddr` wird als
  `":" + PORT` gebaut, die Bindung bleibt bei allen Schnittstellen (LAN-App). Ein ungültiger Wert
  bricht den Start per `log.Fatalf` ab, statt still auf 8080 zu lauschen — sonst sucht man den
  Fehler im falschen Netz. Deshalb gibt es auch kein Flag und keinen Frontend-Wert: Das Frontend
  nutzt relative URLs und merkt vom Port nichts.
- `JELLYFIN_URL` ist **nicht** nur die Adresse für die eigenen API-Aufrufe: `jellyfinStreamURL()`
  baut daraus die URL, die der **Receiver** per `SetAVTransportURI` selbst abruft. Steht dort
  `localhost`/`127.0.0.1` (naheliegend, wenn Jellyfin auf demselben Rechner läuft), lädt das Gerät
  von sich selbst — die Wiedergabe startet kurz und bricht dann ab. Immer die im LAN erreichbare
  Adresse eintragen. Aus demselben Grund muss der API-Key in der Stream-URL stehen
  (`api_key=…`), das Gerät schickt keinen `Authorization`-Header.

- Die Werte existieren weiterhin **nur** im Backend: Das Frontend dupliziert die Geräte-IP nicht
  (Cover-Art läuft über `/api/albumart`), die Jellyfin-URL wird auch im Suchhandler über
  `jellyfinURL` gebaut.
- `yamahaIP`, `yamahaBase`, `jellyfinURL`, `jellyfinAPIKey`, `listenAddr` und (in `upnp.go`)
  `upnpAVTransportControlURL` sind deshalb **Variablen**, keine Konstanten. Die UPnP-URL folgt
  jetzt `YAMAHA_IP`; fest bleibt nur der gerätespezifische Port `49154`.
- Beim Start loggt `main()` die wirksame Konfiguration (Key nur mit den letzten vier Zeichen,
  `maskSecret()`). Fehlt im Dienst eine `Environment=`-Zeile, sieht man das dort sofort.
- Der UPnP-Port `49154` bleibt **nicht** konfigurierbar.

## Bekannte Baustellen (nicht überraschend)

- `playJellyfinItem()` in `app.js` **spielt jetzt wirklich ab** (über den UPnP-Weg oben). Grenzen:
  ein Klick auf Album/Interpret startet den ersten Titel, danach läuft das Album über
  `SetNextAVTransportURI` von selbst weiter; keine Transkodierung (`static=true`),
  Wiedergabefortschritt/Artist im `getPlayInfo` bleiben teils leer (Titel ohne `RunTimeTicks` in
  Jellyfin liefern auch keine Dauer im DIDL).
- Die Jellyfin-Suche holt erst `/Users` und nimmt blind `users[0]`; `/api/jellyfin/views` zeigt
  dadurch nur die Bibliotheken dieses Benutzers (keine privaten Bibliotheken anderer Konten).
- Beim Stöbern kommen Bibliothek, Art, Sortierung und Buchstabe **nicht** in die URL (kein
  Deep-Link, kein Zurück-Knopf im Browser); die Liste startet nach einem Reload wieder oben.
  Ein Buchstabe mit leerem Ergebnis zeigt schlicht „Keine Einträge." — welche Buchstaben belegt
  sind, wird nicht vorab abgefragt (das wären 26 Anfragen).
- Deutsche Texte in UI und Log-Ausgaben beibehalten.
