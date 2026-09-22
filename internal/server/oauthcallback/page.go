package oauthcallback

import (
	"crypto/sha256"
	"encoding/base64"
	"html"
	"net/http"
)

// resultStyle is the stylesheet of the self-contained result page. It is
// covered by its own CSP hash instead of 'unsafe-inline'.
const resultStyle = `:root{color-scheme:light dark}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
font-family:system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;
background:#f6f7f9;color:#111827}
main{max-width:26rem;margin:1.5rem;padding:2rem;border-radius:0.75rem;background:#fff;
box-shadow:0 1px 3px rgba(0,0,0,.1),0 8px 24px rgba(0,0,0,.06);text-align:center}
h1{margin:0 0 .75rem;font-size:1.125rem;font-weight:600}
p{margin:0;font-size:.875rem;line-height:1.6;color:#4b5563}
.mark{width:2.5rem;height:2.5rem;margin:0 auto 1rem;border-radius:999px;
display:flex;align-items:center;justify-content:center;font-size:1.25rem;
font-weight:700;color:#fff}
.ok{background:#16a34a}
.fail{background:#dc2626}
@media(prefers-color-scheme:dark){
body{background:#0b0f19;color:#f9fafb}
main{background:#111827;box-shadow:0 1px 3px rgba(0,0,0,.4)}
p{color:#9ca3af}
}`

// resultScript forwards the callback URL back to this listener and closes the
// window on success.
//
// Providers that put OAuth state in the URL fragment (Claude Code) never send
// the fragment to the server, so the fragment must be read in the browser and
// posted back. The server validates the origin, port and path before accepting
// it, and the value is stored only in memory for a few minutes.
const resultScript = `(function(){
var CLOSE = document.body.getAttribute('data-autoclose') === '1';
function finish(){
if (!CLOSE) { return; }
setTimeout(function(){ window.close(); }, 1200);
}
var hash = window.location.hash || '';
if (!hash) { finish(); return; }
var callback = window.location.origin + window.location.pathname +
window.location.search + hash;
fetch('/_capture', {
method: 'POST',
headers: { 'Content-Type': 'application/json' },
body: JSON.stringify({ url: callback }),
keepalive: true
}).then(finish).catch(finish);
})();`

// styleHash and scriptHash are the base64 SHA-256 digests CSP requires for the
// inline style and script blocks.
var (
	styleHash  = cspHash(resultStyle)
	scriptHash = cspHash(resultScript)
)

func cspHash(content string) string {
	sum := sha256.Sum256([]byte(content))

	return base64.StdEncoding.EncodeToString(sum[:])
}

// writeResultPage renders a self-contained page that reports the outcome of an
// OAuth callback. It never echoes request data.
func writeResultPage(w http.ResponseWriter, ok bool, message string) {
	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	header.Set("Pragma", "no-cache")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set(
		"Content-Security-Policy",
		"default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; "+
			"style-src 'sha256-"+styleHash+"'; script-src 'sha256-"+scriptHash+"'",
	)

	mark := "ok"
	icon := "&#10003;"
	title := "Authorization complete"
	autoClose := "1"

	if !ok {
		mark = "fail"
		icon = "!"
		title = "Authorization failed"
		autoClose = "0"
	}

	_, _ = w.Write([]byte("<!DOCTYPE html>\n" +
		`<html lang="en">` + "\n<head>\n" +
		`<meta charset="utf-8">` + "\n" +
		`<meta name="viewport" content="width=device-width,initial-scale=1">` + "\n" +
		`<meta name="referrer" content="no-referrer">` + "\n" +
		"<title>" + html.EscapeString(title) + "</title>\n" +
		"<style>" + resultStyle + "</style>\n" +
		"</head>\n" +
		"<body data-autoclose=\"" + autoClose + "\">\n<main>\n" +
		"<div class=\"mark " + mark + "\">" + icon + "</div>\n" +
		"<h1>" + html.EscapeString(title) + "</h1>\n" +
		"<p>" + html.EscapeString(message) + "</p>\n" +
		"</main>\n" +
		"<script>" + resultScript + "</script>\n" +
		"</body>\n</html>\n"))
}
