package main

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Der Receiver ist ein UPnP-MediaRenderer (AVTransport). Nur über diesen Weg
// lässt sich ein konkreter Titel abspielen — die MusicCast-API kennt kein
// "spiele URL X". Die IP kommt aus yamahaIP (also aus YAMAHA_IP), der Port 49154
// ist gerätespezifisch (R-N602) und bleibt fest.
// Hinweis: Die YXC-Setter sind GET, diese UPnP-Setter sind echte POSTs mit SOAP.
var upnpAVTransportControlURL = "http://" + yamahaIP + ":49154/AVTransport/ctrl"

const upnpAVTransportService = "urn:schemas-upnp-org:service:AVTransport:1"

// mimeForContainer bildet den Jellyfin-Container auf einen MIME-Typ für die
// DLNA-protocolInfo ab. Unbekannte Formate fallen auf mp3 zurück.
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

// upnpDuration formatiert Sekunden als UPnP-res-Attribut (H:MM:SS.mmm).
// 0 oder negativ heißt "unbekannt" → leer, dann entfällt das Attribut ganz.
func upnpDuration(seconds int64) string {
	if seconds <= 0 {
		return ""
	}
	return fmt.Sprintf("%d:%02d:%02d.000", seconds/3600, (seconds%3600)/60, seconds%60)
}

// buildDIDL erzeugt die DIDL-Lite-Metadaten, die der Renderer zum URI erwartet.
// Die Dauer gehört als res@duration dazu: nur damit kennt der Renderer die
// Gesamtlänge und meldet später brauchbare play_time/total_time — ohne das
// Attribut bleiben MediaDuration/TrackDuration bei 0:00:00.
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

// upnpAVTransport ruft eine AVTransport-Action per SOAP auf. Der Aufruf ist
// synchron und liefert nur einen Fehler, keine Nutzdaten.
func upnpAVTransport(action, inner string) error {
	_, err := upnpAVTransportRaw(action, inner)
	return err
}

// upnpAVTransportRaw wie upnpAVTransport, gibt aber zusätzlich die SOAP-Antwort
// zurück — für lesende Actions wie GetPositionInfo.
func upnpAVTransportRaw(action, inner string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, upnpAVTransportControlURL, strings.NewReader(soapEnvelope(action, inner)))
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

// upnpSetNext hinterlegt den Folgetitel im Renderer (Action
// SetNextAVTransportURI). Der Renderer schaltet dann am Ende des laufenden
// Titels von selbst weiter — ohne diesen Eintrag bleibt er nach dem ersten Titel
// stehen (genau das war der Fehler "Album endet nach Track 1").
// Die Metadaten sind Pflicht: Ohne DIDL kennt der Renderer die Dauer des
// Folgetitels nicht und meldet TrackDuration = 0:00:00.
func upnpSetNext(streamURL, didl string) error {
	return upnpAVTransport("SetNextAVTransportURI",
		"<InstanceID>0</InstanceID>"+
			"<NextURI>"+xmlEscape(streamURL)+"</NextURI>"+
			"<NextURIMetaData>"+xmlEscape(didl)+"</NextURIMetaData>")
}

// upnpClearNext löscht den hinterlegten Folgetitel (leere URI), damit die
// Wiedergabe am Ende des laufenden Titels aufhört statt weiterzulaufen.
func upnpClearNext() error {
	return upnpAVTransport("SetNextAVTransportURI",
		"<InstanceID>0</InstanceID><NextURI></NextURI><NextURIMetaData></NextURIMetaData>")
}

// upnpCurrentTrackURI liefert die URI des Titels, den der Renderer gerade spielt.
// Daran erkennen wir, dass er von selbst auf den hinterlegten Folgetitel
// umgeschaltet hat.
func upnpCurrentTrackURI() (string, error) {
	body, err := upnpAVTransportRaw("GetPositionInfo", "<InstanceID>0</InstanceID>")
	if err != nil {
		return "", err
	}
	return soapValue(body, "TrackURI"), nil
}

// upnpPositionInfo liest Gesamtdauer und Position der aktuellen AVTransport-Session.
// Der Renderer kennt die Dauer aus dem DIDL (res@duration) und liefert sie hier als
// "H:MM:SS". Nötig, weil die YXC-API für UPnP-Wiedergabe play_time liefert, aber
// total_time = 0 lässt (gemessen) — dann bliebe die Laufzeit-Anzeige leer.
func upnpPositionInfo() (duration, position int64, err error) {
	body, err := upnpAVTransportRaw("GetPositionInfo", "<InstanceID>0</InstanceID>")
	if err != nil {
		return 0, 0, err
	}
	return parseUPnPTime(soapValue(body, "TrackDuration")), parseUPnPTime(soapValue(body, "RelTime")), nil
}

// soapValue liest den Textinhalt eines einfachen Elements aus einer SOAP-Antwort.
// Bewusst kein XML-Parser: Die Antwort des Receivers ist nicht wohlgeformt
// (TrackURI enthält ein nacktes "&", z. B. "…&static=true"), daran scheitert
// encoding/xml. TrackDuration/RelTime sind reine Zeitangaben ohne Sonderzeichen.
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

// parseUPnPTime wandelt "H:MM:SS" (ggf. mit Sekundenbruchteilen) in Sekunden um.
// Unlesbare Werte (leer, "NOT_IMPLEMENTED") ergeben 0.
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
