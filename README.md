# MusicCast Companion

Kleine LAN-Web-App für **einen** Yamaha-Netzwerk-Receiver (getestet: R-N602) plus
Jellyfin-Musiksuche. Das Go-Backend liefert das Frontend aus `static/` aus und ist
gleichzeitig Proxy für alle Geräteaufrufe: Der Browser spricht **nie** direkt mit Receiver
oder Jellyfin, deshalb gibt es weder CORS- noch Mixed-Content-Probleme. Die Oberfläche ist im
Stil von **Winamp 2.x** gehalten (eigenes CSS in `static/style.css`, kein CDN).

Diese Anleitung beschreibt die Installation als **systemd-Dienst auf einem Debian-NAS**.

---

## Voraussetzungen

| Was | Details |
|---|---|
| Go | **1.22 oder neuer**, nur zum Bauen. Keine Abhängigkeiten (`go.mod` ohne `require`), kein npm, kein Build-Schritt |
| Netz | Der NAS muss Receiver und Jellyfin per HTTP erreichen |
| Port | **8080/tcp** muss frei sein (per `PORT` änderbar, siehe [Konfiguration](#konfiguration)) |

---

## 1. Dateien auf den NAS kopieren

Zum Betrieb nötig sind nur diese Dateien:

```
main.go  yamaha.go  jellyfin.go  upnp.go  go.mod  static/
```

`static/` (`index.html`, `app.js`, `style.css`) muss **neben dem Binary** liegen — das Backend liefert
`./static` relativ zum Arbeitsverzeichnis aus.

Im Beispiel heißt der SSH-Zielhost `nas` — dort deinen Alias aus `~/.ssh/config` (oder
`benutzer@ip`) einsetzen.

```bash
# vom Entwicklungsrechner aus
ssh nas 'mkdir -p /tmp/musiccast-companion'
scp -O -r main.go yamaha.go jellyfin.go upnp.go go.mod static nas:/tmp/musiccast-companion/

# auf dem NAS
ssh nas
sudo install -d /opt/musiccast-companion
sudo cp -r /tmp/musiccast-companion/. /opt/musiccast-companion/
```

**`-O` ist Absicht:** Seit OpenSSH 9 spricht `scp` standardmäßig SFTP und braucht dafür einen
`sftp-server` auf der Gegenseite. Läuft dort **Dropbear** (z. B. auf einem NAS), fehlt der und es
kommt zu `bash: /usr/lib/sftp-server: Datei oder Verzeichnis nicht gefunden`. `-O` erzwingt das
alte SCP-Protokoll, das Dropbear kann. Alternativ ohne `scp`:

```bash
tar -czf - main.go yamaha.go jellyfin.go upnp.go go.mod static \
  | ssh nas 'mkdir -p /tmp/musiccast-companion && tar -xzf - -C /tmp/musiccast-companion'
```

## 2. Bauen

Auf dem NAS im Quellverzeichnis (als normaler Benutzer, nicht als root):

```bash
cd /tmp/musiccast-companion
go build -o musiccast-companion .
sudo install -m 755 musiccast-companion /opt/musiccast-companion/
```

## 3. Dienstbenutzer

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin musiccast
sudo chown -R musiccast:musiccast /opt/musiccast-companion
```

Wer keinen eigenen Benutzer will, kann in der Unit stattdessen `User=` weglassen oder einen
vorhandenen Benutzer eintragen.

## 4. systemd-Unit anlegen

`/etc/systemd/system/musiccast-companion.service`:

```ini
[Unit]
Description=MusicCast Companion (Yamaha-Proxy + Jellyfin)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=musiccast
Group=musiccast
WorkingDirectory=/opt/musiccast-companion
ExecStart=/opt/musiccast-companion/musiccast-companion

# Die drei Pflichtwerte für dein Netz (Platzhalter ersetzen):
Environment=YAMAHA_IP=<receiver-ip>
Environment=JELLYFIN_URL=http://<jellyfin-host>:8096
Environment=JELLYFIN_API_KEY=<api-key>

# Nur nötig, wenn 8080 belegt ist oder etwas anderes davor steht:
#Environment=PORT=8080

Restart=always
RestartSec=2

# Die App schreibt nichts, sie liest nur ihren Ordner.
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

Aktivieren:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now musiccast-companion
```

## 5. Prüfen

```bash
systemctl status musiccast-companion
journalctl -u musiccast-companion -n 20 --no-pager
curl -s http://<nas-ip>:8080/api/status
```

Im Log stehen die erkannte LAN-Adresse und die **tatsächlich benutzten** Werte:

```
🚀 Go-Backend läuft auf http://localhost:8080
   im LAN erreichbar unter http://192.168.1.10:8080/
   Yamaha 192.168.1.20 · Jellyfin http://192.168.1.30:8096 · API-Key …a1b2
```

Danach im Browser öffnen: `http://<nas-ip>:8080/`

---

## Konfiguration

Alle Gerätewerte kommen aus der Umgebung — im Code stehen **keine** echten Adressen oder Keys.
Fehlt einer der drei Pflichtwerte, bricht der Start mit einer Meldung ab, die alle fehlenden
Variablen nennt:

```
Fehlende Konfiguration: YAMAHA_IP, JELLYFIN_API_KEY. Bitte per Umgebungsvariable setzen, z. B.
  YAMAHA_IP=<receiver-ip> JELLYFIN_URL=<jellyfin-url> JELLYFIN_API_KEY=<key> ./musiccast-companion
```

| Variable | Pflicht | Bedeutung |
|---|---|---|
| `YAMAHA_IP` | ja | IP des Receivers (ohne `http://`, ohne Port) |
| `JELLYFIN_URL` | ja | Basis-URL des Jellyfin-Servers, mit Schema (ein abschließender `/` stört nicht). **Muss vom Receiver aus erreichbar sein** — `localhost`/`127.0.0.1` funktioniert nicht, auch wenn Jellyfin auf demselben Rechner läuft |
| `JELLYFIN_API_KEY` | ja | API-Key des Jellyfin-Benutzers |
| `PORT` | nein | Port des Webservers (`1`–`65535`, Vorgabe `8080`) |

Leere Werte (`YAMAHA_IP=`) gelten als nicht gesetzt. Ein ungültiger `PORT`-Wert (z. B.
`PORT=abc`) bricht den Start ebenfalls mit einer Meldung ab — ein Tippfehler soll nicht still auf
dem Vorgabeport landen.

Der Server lauscht immer auf **allen** Schnittstellen (`:<PORT>`), nicht nur auf `localhost`:
Die App ist im LAN gedacht. Ein anderer Port heißt also auch: Firewall-Freigabe und ggf. die
aufgerufene URL anpassen (`http://<nas>:<PORT>/`). Im Frontend steht kein Port — es benutzt
relative URLs.

**Nicht konfigurierbar:** Der UPnP-Control-Port des Receivers ist `49154`
(gerätespezifisch, `upnp.go`). Ändern heißt Code anpassen und neu bauen.

### API-Key statt im Unit-File in einer eigenen Datei

Unit-Dateien sind für alle lesbar. Wem der Key dort zu offen liegt:

```bash
sudo install -m 600 /dev/null /etc/musiccast-companion.env
sudo tee /etc/musiccast-companion.env >/dev/null <<'EOF'
YAMAHA_IP=<receiver-ip>
JELLYFIN_URL=http://<jellyfin-host>:8096
JELLYFIN_API_KEY=<api-key>
EOF
```

In der Unit dann die drei `Environment=`-Zeilen durch ersetzen:

```ini
EnvironmentFile=/etc/musiccast-companion.env
```

### Jellyfin-API-Key erzeugen

Jellyfin → **Dashboard → API Keys → +** (Name z. B. `musiccast`). Der Key gehört zu dem
Benutzer, der ihn anlegt: Sichtbar sind nur dessen **Musik**-Bibliotheken. Ein Admin-Key
sieht mehr Bibliotheken, ändert aber nichts an der Filterung auf `CollectionType=music`.

---

## Aktualisieren

Ein Paketmanager ist nicht im Spiel, aber das Projekt liegt als Git-Repo. Vom
Entwicklungsrechner aus genügt ein Befehl — `deploy.sh` prüft `gofmt`/`go vet`, überträgt die
Quellen, baut **auf dem NAS** (passende Architektur), installiert nach `/opt` und startet den
Dienst neu:

```bash
NAS_HOST=nas-andreas ./deploy.sh
```

`NAS_HOST` ist der SSH-Alias (oder `user@host`), `APP_PORT` überschreibt den Vorgabeport 8765
für die abschließende Erreichbarkeitsprüfung. Für `/opt` und `systemctl` wird einmal das
sudo-Passwort abgefragt.

Von Hand geht es genauso:

```bash
# neue Quellen nach /tmp/musiccast-companion kopieren, dann:
cd /tmp/musiccast-companion
go build -o musiccast-companion .
sudo install -m 755 musiccast-companion /opt/musiccast-companion/
sudo rm -rf /opt/musiccast-companion/static
sudo cp -r static /opt/musiccast-companion/
sudo systemctl restart musiccast-companion
```

Nur Frontend-Änderungen (`static/`) brauchen **keinen** Neustart — die Dateien werden bei
jedem Aufruf von der Platte gelesen, ein Browser-Reload genügt. Backend-Änderungen dagegen
immer mit `restart` (nicht `start`), sonst läuft der alte Prozess weiter.

Logs:

```bash
journalctl -u musiccast-companion -f        # live
journalctl -u musiccast-companion -n 50     # letzte Zeilen
```

---

## Firewall

Nur nötig, wenn auf dem NAS eine Firewall läuft:

```bash
# Debian/Ubuntu mit ufw — nur das lokale Netz (empfohlen, die App hat keine Anmeldung)
sudo ufw allow from <dein-lan>/24 to any port 8080 proto tcp comment 'MusicCast Companion'

# alternativ pauschal
sudo ufw allow 8080/tcp

# firewalld
sudo firewall-cmd --add-port=8080/tcp --permanent && sudo firewall-cmd --reload
```

**Häufigste Ursache für „vom Handy nicht erreichbar“:** ufw ist aktiv, `DEFAULT_INPUT_POLICY=DROP`
und kein 8080 freigegeben. Das Backend lauscht mit `:8080` bereits auf allen Adressen — an der
Bindung (`0.0.0.0`) liegt es dann nicht. Prüfen mit `sudo ufw status` bzw. Hinweise auf
abgewiesene Pakete mit `sudo dmesg | grep 'UFW BLOCK'`.

Läuft der Dienst auf einem anderen `PORT`, in den Regeln oben die Zahl entsprechend ersetzen.

## Sicherheit

Die App hat **keine Anmeldung**. Wer den Port erreicht, kann den Receiver steuern, die
Lautstärke ändern und in Jellyfin suchen/abspielen. Fürs private Heimnetz ist das gewollt —
deshalb aber **keine Portfreigabe ins Internet** und kein Reverse Proxy ohne Auth davor.

## Fehlersuche

| Symptom | Ursache / Prüfung |
|---|---|
| Start bricht mit „Fehlende Konfiguration“ ab | `YAMAHA_IP`, `JELLYFIN_URL` oder `JELLYFIN_API_KEY` fehlt in der Umgebung/Unit |
| Wiedergabe startet, aber kein Ton, springt zurück | `JELLYFIN_URL` zeigt auf `localhost`/`127.0.0.1`. Den Stream lädt der **Receiver**, nicht der Server — dort die LAN-Adresse eintragen |
| Seite leer, `404` für `/` | `WorkingDirectory` falsch oder `static/` fehlt daneben. `ls /opt/musiccast-companion/static` |
| `502` bei `/api/status`, keine Reaktion | `YAMAHA_IP` falsch. Test: `curl http://<ip>/YamahaExtendedControl/v1/system/getDeviceInfo` |
| Suche/Stöbern leer, Cover kaputt | `JELLYFIN_URL` oder `JELLYFIN_API_KEY` falsch. Test: `curl -H 'Authorization: MediaBrowser Token="<key>"' <url>/Users` |
| Kein Ton, aber Status geht | Wiedergabe läuft über UPnP auf Port `49154`; Receiver-Firmware/Port prüfen (`curl http://<ip>:49154/MediaRenderer/desc.xml`) |
| Start scheitert: `address already in use` | Port belegt: `ss -ltnp \| grep 8080` |
| Alles funktioniert, aber ohne Styling | `static/style.css` fehlt oder wurde nicht mitkopiert (z. B. beim Deployment nur `index.html`/`app.js` übertragen) |
| Dienst startet nicht | `journalctl -u musiccast-companion -n 30`; Unit prüfen mit `systemd-analyze verify /etc/systemd/system/musiccast-companion.service` |

## Ohne systemd testen

```bash
cd /opt/musiccast-companion
YAMAHA_IP=<receiver-ip> \
JELLYFIN_URL=http://<jellyfin-host>:8096 \
JELLYFIN_API_KEY=<api-key> \
./musiccast-companion
```

Beenden mit `Strg+C`. Der Port muss dafür frei sein (`sudo systemctl stop musiccast-companion`).
