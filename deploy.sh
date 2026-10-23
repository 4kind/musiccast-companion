#!/usr/bin/env bash
# Spielt den aktuellen Arbeitsstand auf den NAS und startet den Dienst neu.
#
#   NAS_HOST=nas-andreas ./deploy.sh
#
# Gebaut wird auf dem NAS (dortige Architektur, kein Cross-Compile). Für /opt
# und systemctl wird einmal das sudo-Passwort abgefragt.
#
# Optionen per Umgebung:
#   NAS_HOST  SSH-Ziel (Alias aus ~/.ssh/config oder user@host), Vorgabe: nas
#   APP_PORT  Port der App auf dem NAS, Vorgabe: 8765
set -euo pipefail

host=${NAS_HOST:-nas}
app_port=${APP_PORT:-8765}
staging=/tmp/musiccast-companion
target=/opt/musiccast-companion
files=(main.go yamaha.go jellyfin.go upnp.go go.mod static)

# 0. Lokal prüfen: kein unformatierter oder offensichtlich kaputter Stand.
if [ -n "$(gofmt -l .)" ]; then
	echo "gofmt: diese Dateien sind nicht formatiert:" >&2
	gofmt -l . >&2
	exit 1
fi
go vet ./...

# 1. Quellen übertragen. Ohne pty, damit der tar-Stream binärsicher bleibt.
echo "==> übertrage nach $host:$staging"
tar -czf - "${files[@]}" |
	ssh "$host" "rm -rf $staging && mkdir -p $staging && tar -xzf - -C $staging"

# 2. Bauen, installieren, neu starten — ein sudo-Prompt (-t für das Passwort).
echo "==> baue und starte den Dienst neu"
ssh -t "$host" "set -e
cd $staging
go build -trimpath .
sudo install -d $target
sudo cp $staging/main.go $staging/yamaha.go $staging/jellyfin.go $staging/upnp.go $staging/go.mod $target/
sudo rm -rf $target/static
sudo cp -r $staging/static $target/
# Das laufende Binary lässt sich nicht überschreiben (ETXTBSY/'busy'), deshalb
# daneben schreiben und per mv atomar austauschen — der Prozess behält sein Inode.
sudo install -m 755 $staging/musiccast-companion $target/musiccast-companion.new
sudo mv -f $target/musiccast-companion.new $target/musiccast-companion
sudo systemctl restart musiccast-companion
sleep 2
if curl -sf -o /dev/null http://127.0.0.1:$app_port/; then
	echo 'App antwortet.'
else
	echo 'App antwortet NICHT — Log prüfen: journalctl -u musiccast-companion -n 30' >&2
	exit 1
fi
systemctl --no-pager -n 3 status musiccast-companion | sed -n '1,5p'"
