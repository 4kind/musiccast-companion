package main

import (
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// The receiver is a UPnP MediaRenderer (AVTransport). Only via this method
// can a specific track be played - the MusicCast API doesn't have a
// "play URL X" command. The IP comes from getCurrentYamahaIP(), port 49154
// is device-specific (R-N602) and remains fixed.
// Note: The YXC setters are GET, these UPnP setters are actual POSTs with SOAP.
func getUpnpAVTransportControlURL() string {
	return "http://" + getCurrentYamahaIP() + ":49154/AVTransport/ctrl"
}

const upnpAVTransportService = "urn:schemas-upnp-org:service:AVTransport:1"

// mimeForContainer maps the Jellyfin container to a MIME type for
// DLNA protocolInfo. Unknown formats fall back to mp3.
func mimeForContainer(container string) string {
	switch strings.ToLower(container) {
	case "mp3":
		return "audio/mpeg"
	case "flac":
		return "audio/flac"
	case "m4a", "aac", "alac":
		return "audio/mp4"
	case "ogg", "oga", "opus":
		return "audio/ogg"
	case "wav":
		return "audio/wav"
	case "wma":
		return "audio/x-ms-wma"
	default:
		return "audio/mpeg"
	}
}

func xmlEscape(s string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return s
	}
	return b.String()
}

// upnpDuration formats seconds as UPnP res attribute (H:MM:SS.mmm).
// 0 or negative means "unknown" - empty, then the attribute is omitted.
func upnpDuration(seconds int64) string {
	if seconds <= 0 {
		return ""
	}
	return fmt.Sprintf("%d:%02d:%02d.000", seconds/3600, (seconds%3600)/60, seconds%60)
}

// buildDIDL generates the DIDL-Lite metadata that the renderer expects for the URI.
// The duration belongs as res@duration: only this way does the renderer know the
// total length and later reports useful play_time/total_time - without it the
// attribute MediaDuration/TrackDuration remain at 0:00:00.
func buildDIDL(title, artist, album, mime, streamURL string, durationSeconds int64) string {
	resAttrs := ` protocolInfo="http-get:*:` + mime + `:DLNA.ORG_OP=01;DLNA.ORG_CI=0"`
	if duration := upnpDuration(durationSeconds); duration != "" {
		resAttrs += ` duration="` + duration + `"`
	}
	return `<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" ` +
		`xmlns:dc="http://purl.org/dc/elements/1.1/" ` +
		`xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">` +
		`<item id="1" parentID="0" restricted="1">` +
		`<dc:title>` + xmlEscape(title) + `</dc:title>` +
		`<dc:creator>` + xmlEscape(artist) + `</dc:creator>` +
		`<upnp:album>` + xmlEscape(album) + `</upnp:album>` +
		`<upnp:class>object.item.audioItem.musicTrack</upnp:class>` +
		`<res` + resAttrs + `>` +
		xmlEscape(streamURL) +
		`</res></item></DIDL-Lite>`
}

func soapEnvelope(action, inner string) string {
	return `<?xml version="1.0" encoding="utf-8"?>` +
		`<s:Envelope s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/" ` +
		`xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
		`<u:` + action + ` xmlns:u="` + upnpAVTransportService + `">` + inner +
		`</u:` + action + `></s:Body></s:Envelope>`
}

// upnpAVTransport calls an AVTransport action via SOAP. The call is
// synchronous and only returns an error, no payload.
func upnpAVTransport(action, inner string) error {
	_, err := upnpAVTransportRaw(action, inner)
	return err
}

// upnpAVTransportRaw like upnpAVTransport, but additionally returns the SOAP response
// - for read actions like GetPositionInfo.
func upnpAVTransportRaw(action, inner string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, getUpnpAVTransportControlURL(), strings.NewReader(soapEnvelope(action, inner)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPACTION", `"`+upnpAVTransportService+`#`+action+`"`)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("UPnP %s: Status %d: %s", action, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// upnpSetNext sets the next track in the renderer (action
// SetNextAVTransportURI). The renderer then advances at the end of the current
// track automatically - without this entry it stops after the first track
// (that was exactly the error "album ends after track 1").
// Metadata is required: Without DIDL, the renderer does not know the duration of the
// next track and reports TrackDuration = 0:00:00.
func upnpSetNext(streamURL, didl string) error {
	return upnpAVTransport("SetNextAVTransportURI",
		"<InstanceID>0</InstanceID>"+
			"<NextURI>"+xmlEscape(streamURL)+"</NextURI>"+
			"<NextURIMetaData>"+xmlEscape(didl)+"</NextURIMetaData>")
}

// upnpClearNext deletes the set next track (empty URI), so that the
// playback stops at the end of the current track instead of continuing.
func upnpClearNext() error {
	return upnpAVTransport("SetNextAVTransportURI",
		"<InstanceID>0</InstanceID><NextURI></NextURI><NextURIMetaData></NextURIMetaData>")
}

// upnpCurrentTrackURI returns the URI of the track that the renderer is currently playing.
// This is how we recognize that it has automatically switched to the set next track.
// umgeschaltet hat.
func upnpCurrentTrackURI() (string, error) {
	body, err := upnpAVTransportRaw("GetPositionInfo", "<InstanceID>0</InstanceID>")
	if err != nil {
		return "", err
	}
	uri := soapValue(body, "TrackURI")
	log.Printf("DEBUG upnpCurrentTrackURI: URI=%q", uri)
	return uri, nil
}

// upnpPositionInfo reads total duration and position of the current AVTransport session.
// The renderer knows the duration from the DIDL (res@duration) and returns it here as
// "H:MM:SS". Needed because the YXC API for UPnP playback provides play_time, but
// total_time = 0 (observed) - otherwise the duration display would be empty.
func upnpPositionInfo() (duration, position int64, err error) {
	body, err := upnpAVTransportRaw("GetPositionInfo", "<InstanceID>0</InstanceID>")
	if err != nil {
		return 0, 0, err
	}
	return parseUPnPTime(soapValue(body, "TrackDuration")), parseUPnPTime(soapValue(body, "RelTime")), nil
}

// soapValue reads the text content of a simple element from a SOAP response.
// Intentionally no XML parser: The receiver response is not well-formed
// (TrackURI contains a bare "// (TrackURI enthält ein nacktes "&", z. B. "…&static=true"), daran scheitert", e.g., "...// (TrackURI enthält ein nacktes "&", z. B. "…&static=true"), daran scheitertstatic=true"), which fails
// encoding/xml. TrackDuration/RelTime are pure time specifications without special characters.
func soapValue(body []byte, element string) string {
	text := string(body)
	open, close := "<"+element+">", "</"+element+">"
	start := strings.Index(text, open)
	if start < 0 {
		return ""
	}
	start += len(open)
	end := strings.Index(text[start:], close)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(text[start : start+end])
}

// parseUPnPTime converts "H:MM:SS" (optionally with fractional seconds) to seconds.
// Unreadable values (empty, "NOT_IMPLEMENTED") result in 0.
func parseUPnPTime(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if dot := strings.IndexByte(value, '.'); dot >= 0 {
		value = value[:dot]
	}
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return 0
	}
	var total int64
	for _, part := range parts {
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return 0
		}
		total = total*60 + n
	}
	return total
}
