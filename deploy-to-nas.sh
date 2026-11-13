#!/bin/bash
# =============================================================================
# MusicCast Companion - Deploy to NAS
# Usage: ./deploy-to-nas.sh <nas-host> [path-to-binary]
#        <nas-host>:     SSH target (alias from ~/.ssh/config or user@host)
#        [binary]:       optional prebuilt Linux binary (e.g. from a GitHub
#                        release). Without it, the binary is built locally,
#                        cross-compiled for the NAS architecture (detected
#                        via ssh). The binary is static (CGO_ENABLED=0).
#                        static/ is always deployed alongside - the app reads
#                        it from disk at runtime.
# =============================================================================

set -euo pipefail

# Configuration
NAS_HOST="${1:-${NAS_HOST:-}}"
BINARY="${2:-}"
TARGET_DIR="/opt/musiccast-companion"
STAGING_DIR="/tmp/musiccast-deploy"

if [ -z "$NAS_HOST" ]; then
    echo "ERROR: No NAS host given."
    echo ""
    echo "Usage: $0 <nas-host> [path-to-binary]"
    echo "       <nas-host> is an SSH target: an alias from ~/.ssh/config"
    echo "       or a user@host / IP address."
    echo ""
    echo "Example: $0 nas"
    echo "         $0 nas musiccast-companion-linux-arm64   # binary from release"
    exit 1
fi

# Colors for output
RED='\033[0m\033[31m'
GREEN='\033[0m\033[32m'
YELLOW='\033[0m\033[33m'
BLUE='\033[0m\033[34m'
NC='\033[0m'

echo -e "${BLUE}==========================================${NC}"
echo -e "${BLUE}  MusicCast Companion - Deploy to NAS${NC}"
echo -e "${BLUE}==========================================${NC}"
echo ""

# Step 1: Check local files (static/ is always needed, sources only for a local build)
echo -e "${YELLOW}Step 1: Checking local files...${NC}"
if [ ! -d static ]; then
    echo -e "${RED}ERROR: Directory 'static' not found! Run from the repo root.${NC}"
    exit 1
fi
if [ -z "$BINARY" ]; then
    for file in main.go yamaha.go jellyfin.go upnp.go go.mod; do
        if [ ! -e "$file" ]; then
            echo -e "${RED}ERROR: File '$file' not found (needed for local build)!${NC}"
            exit 1
        fi
    done
else
    if [ ! -f "$BINARY" ]; then
        echo -e "${RED}ERROR: Binary '$BINARY' not found!${NC}"
        exit 1
    fi
fi
echo -e "${GREEN}OK${NC}"
echo ""

# Step 2: Determine the binary — given one, or cross-compile locally for the NAS arch
echo -e "${YELLOW}Step 2: Preparing binary...${NC}"
if [ -n "$BINARY" ]; then
    cp "$BINARY" ./musiccast-companion.deploy
    echo -e "${GREEN}Using given binary: $BINARY${NC}"
else
    NAS_ARCH_RAW=$(ssh "$NAS_HOST" "uname -m")
    case "$NAS_ARCH_RAW" in
        x86_64)  NAS_ARCH=amd64 ;;
        aarch64|arm64) NAS_ARCH=arm64 ;;
        *) echo -e "${RED}ERROR: Unsupported NAS architecture '$NAS_ARCH_RAW'${NC}"; exit 1 ;;
    esac
    echo "NAS architecture: $NAS_ARCH_RAW -> GOARCH=$NAS_ARCH"
    CGO_ENABLED=0 GOOS=linux GOARCH="$NAS_ARCH" go build -trimpath -o musiccast-companion.deploy .
    echo -e "${GREEN}Built locally (static, $NAS_ARCH)${NC}"
fi
echo ""

# Step 3: Package binary + static/ and copy to NAS
echo -e "${YELLOW}Step 3: Packaging and copying to NAS...${NC}"
tar -czf - musiccast-companion.deploy static | ssh "$NAS_HOST" "mkdir -p $STAGING_DIR && tar -xzf - -C $STAGING_DIR/"
echo -e "${GREEN}Files copied${NC}"
echo ""

# Step 4: Install and restart (single sudo password)
echo -e "${YELLOW}Step 4: Installing and restarting (single password)...${NC}"
echo ""

ssh "$NAS_HOST" "cat > /tmp/deploy.sh" << 'ENDOFSCRIPT'
#!/bin/bash
set -e
STAGING_DIR="/tmp/musiccast-deploy"
TARGET_DIR="/opt/musiccast-companion"

echo "  -> Installing to $TARGET_DIR..."
install -d "$TARGET_DIR"
rm -rf "$TARGET_DIR/static"
cp -r "$STAGING_DIR/static" "$TARGET_DIR/"
install -m 755 "$STAGING_DIR/musiccast-companion.deploy" "$TARGET_DIR/musiccast-companion.new"
mv -f "$TARGET_DIR/musiccast-companion.new" "$TARGET_DIR/musiccast-companion"

echo "  -> Setting permissions..."
chown -R jellyfin:jellyfin "$TARGET_DIR"

echo "  -> Restarting service..."
systemctl restart musiccast-companion

echo "  -> Cleaning up..."
rm -rf "$STAGING_DIR"
rm -f /tmp/deploy.sh

echo "  -> Done!"
ENDOFSCRIPT

ssh "$NAS_HOST" "chmod +x /tmp/deploy.sh"

echo "Please enter sudo password for NAS (only once):"
ssh -t "$NAS_HOST" "sudo /tmp/deploy.sh"

rm -f ./musiccast-companion.deploy

echo ""
echo -e "${GREEN}Deploy to NAS completed${NC}"
echo ""

# Step 5: Verification
echo -e "${YELLOW}Step 5: Verifying installation...${NC}"
sleep 2

if ssh "$NAS_HOST" "systemctl is-active --quiet musiccast-companion" 2>/dev/null; then
    echo -e "${GREEN}Service running${NC}"
    if ssh "$NAS_HOST" "curl -sf -o /dev/null http://127.0.0.1:8765/" 2>/dev/null; then
        echo -e "${GREEN}Web app responding on port 8765${NC}"
    else
        echo -e "${RED}Web app not responding - check logs with: journalctl -u musiccast-companion -n 30${NC}"
    fi
else
    echo -e "${RED}Service not running - check status with: systemctl status musiccast-companion${NC}"
fi

echo ""
echo -e "${BLUE}==========================================${NC}"
echo -e "${GREEN}  Deploy completed!${NC}"
echo -e "${BLUE}==========================================${NC}"
echo ""
echo "The app is now available at http://$NAS_HOST:8765/"
