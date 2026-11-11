#!/usr/bin/env bash
# Deploy the current working copy to NAS and restart the service.
#
#   NAS_HOST=<ssh-host> ./deploy.sh
#
# Built on the NAS (its architecture, no cross-compile). For /opt and
# systemctl, sudo password is requested once.
#
# Environment options:
#   NAS_HOST  SSH target (alias from ~/.ssh/config or user@host), default: nas
#   APP_PORT  App port on NAS, default: 8765
set -euo pipefail

host=${NAS_HOST:-nas}
app_port=${APP_PORT:-8765}
staging=/tmp/musiccast-companion
target=/opt/musiccast-companion
files=(main.go yamaha.go jellyfin.go upnp.go go.mod static)

# 0. Local check: no unformatted or obviously broken code.
if [ -n "$(gofmt -l .)" ]; then
	echo "gofmt: these files are not formatted:" >&2
	gofmt -l . >&2
	exit 1
fi
go vet ./...

# 1. Transfer sources. Without pty, the tar stream remains binary-safe.
echo "==> transferring to $host:$staging"
tar -czf - "${files[@]}" |
	ssh "$host" "rm -rf $staging && mkdir -p $staging && tar -xzf - -C $staging"

# 2. Build, install, restart - one sudo prompt (-t for password).
echo "==> building and restarting service"
ssh -t "$host" "set -e
cd $staging
go build -trimpath .
sudo install -d $target
sudo cp $staging/main.go $staging/yamaha.go $staging/jellyfin.go $staging/upnp.go $staging/go.mod $target/
sudo rm -rf $target/static
sudo cp -r $staging/static $target/
# Running binary cannot be overwritten (ETXTBSY/'busy'), so write next to it
# and atomically replace with mv - the process keeps its inode.
sudo install -m 755 $staging/musiccast-companion $target/musiccast-companion.new
sudo mv -f $target/musiccast-companion.new $target/musiccast-companion
sudo systemctl restart musiccast-companion
sleep 2
if curl -sf -o /dev/null http://127.0.0.1:$app_port/; then
	echo 'App responding.'
else
	echo 'App NOT responding - check logs: journalctl -u musiccast-companion -n 30' >&2
	exit 1
fi
systemctl --no-pager -n 3 status musiccast-companion | sed -n '1,5p'"
