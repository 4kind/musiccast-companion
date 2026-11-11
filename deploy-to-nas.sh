#!/bin/bash
# =============================================================================
# MusicCast Companion - Deploy to NAS
# Copy current files to NAS and restart the service
# Usage: ./deploy-to-nas.sh
# =============================================================================

set -euo pipefail

# Configuration
NAS_HOST="nas"
TARGET_DIR="/opt/musiccast-companion"
STAGING_DIR="/tmp/musiccast-deploy"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

echo -e "${BLUE}==========================================${NC}"
echo -e "${BLUE}  MusicCast Companion - Deploy to NAS${NC}"
echo -e "${BLUE}==========================================${NC}"
echo ""

# Step 1: Check local files
echo -e "${YELLOW}✓ Step 1: Checking local files...${NC}"
REQUIRED_FILES=("main.go" "yamaha.go" "jellyfin.go" "upnp.go" "go.mod" "static")
for file in "${REQUIRED_FILES[@]}"; do
    if [ ! -e "$file" ]; then
        echo -e "${RED}✗ ERROR: File '$file' not found!${NC}"
        exit 1
    fi
done
echo -e "${GREEN}✓ All required files present${NC}"
echo ""

# Step 2: Package everything into a tar archive and extract on NAS
echo -e "${YELLOW}✓ Step 2: Packaging and copying to NAS...${NC}"
tar -czf - main.go yamaha.go jellyfin.go upnp.go go.mod static | ssh "$NAS_HOST" "mkdir -p $STAGING_DIR && tar -xzf - -C $STAGING_DIR/"
echo -e "${GREEN}✓ Files copied${NC}"
echo ""

# Step 3: Build binary
echo -e "${YELLOW}✓ Step 3: Building binary on NAS...${NC}"
ssh "$NAS_HOST" "cd $STAGING_DIR && go build -trimpath ."
echo -e "${GREEN}✓ Binary built successfully${NC}"
echo ""

# Step 4: Install and restart (single sudo password)
echo -e "${YELLOW}✓ Step 4: Installing and restarting (single password)...${NC}"
echo ""

# Create a script on NAS
ssh "$NAS_HOST" "cat > /tmp/deploy.sh" << 'ENDOFSCRIPT'
#!/bin/bash
set -e
STAGING_DIR="/tmp/musiccast-deploy"
TARGET_DIR="/opt/musiccast-companion"

echo "  -> Installing to $TARGET_DIR..."
install -d "$TARGET_DIR"
cp "$STAGING_DIR"/main.go "$STAGING_DIR"/yamaha.go "$STAGING_DIR"/jellyfin.go "$STAGING_DIR"/upnp.go "$STAGING_DIR"/go.mod "$TARGET_DIR/"
rm -rf "$TARGET_DIR/static"
cp -r "$STAGING_DIR/static" "$TARGET_DIR/"
install -m 755 "$STAGING_DIR/musiccast-companion" "$TARGET_DIR/musiccast-companion.new"
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

# Make script executable
ssh "$NAS_HOST" "chmod +x /tmp/deploy.sh"

# Execute with sudo and single password prompt
# Using ssh with -t to force pseudo-terminal
# and then sudo with the script

echo "Please enter sudo password for NAS (only once):"
ssh -t "$NAS_HOST" "sudo /tmp/deploy.sh"

echo ""
echo -e "${GREEN}✓ Deploy to NAS completed${NC}"
echo ""

# Step 5: Verification
echo -e "${YELLOW}✓ Step 5: Verifying installation...${NC}"
sleep 2

if ssh "$NAS_HOST" "systemctl is-active --quiet musiccast-companion" 2>/dev/null; then
    echo -e "${GREEN}✓ Service running${NC}"
    if ssh "$NAS_HOST" "curl -sf -o /dev/null http://127.0.0.1:8765/" 2>/dev/null; then
        echo -e "${GREEN}✓ Web app responding on port 8765${NC}"
    else
        echo -e "${RED}⚠ Web app not responding - check logs with: journalctl -u musiccast-companion -n 30${NC}"
    fi
else
    echo -e "${RED}✗ Service not running - check status with: systemctl status musiccast-companion${NC}"
fi

echo ""
echo -e "${BLUE}==========================================${NC}"
echo -e "${GREEN}  Deploy completed!${NC}"
echo -e "${BLUE}==========================================${NC}"
echo ""
echo "The app is now available at http://<NAS-IP>:8765/"
echo ""
echo "If there are problems:"
echo "  - Logs: journalctl -u musiccast-companion -n 50"
echo "  - Status: systemctl status musiccast-companion"
echo ""
