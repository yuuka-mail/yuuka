package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/openapi"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// servedTypes are the declared media types an attachment keeps; every
// other one is served as application/octet-stream. The response is always
// a download (Content-Disposition: attachment, nosniff, a sandbox CSP), so
// the type never decides whether the browser renders it; it only lets the
// saved file be named and opened as what it is when the file name has no
// extension. The list is the common attachment types that are not active
// content: no HTML, SVG, XML or script, which a later "open" could run,
// and no text/*, whose charset the declared type may get wrong.
var servedTypes = map[string]bool{
	"application/pdf": true,
	"image/png":       true,
	"image/jpeg":      true,
	"image/gif":       true,
	"image/webp":      true,
}

// attachment serves one attachment of a message as a download: the part
// the structured view lists under content.attachments, by its section.
// Mail is untrusted input (§8.3), so the bytes are never rendered here.
func (a *API) attachment(w http.ResponseWriter, r *http.Request, v *auth.Viewer, p AttachmentParams) error {
	part := p.Part
	d, err := a.db.MessageByID(r.Context(), v, p.ID)
	if err != nil {
		return err
	}
	// d.Raw is a visible delivery's raw, or a visible sent copy's (§13.8).
	raw, err := a.raw.ReadRaw(r.Context(), d.Raw)
	if err != nil {
		return err
	}
	ent, body, err := mimeparse.Attachment(raw, part)
	if errors.Is(err, mimeparse.ErrNoAttachment) {
		return openapi.Errorf(http.StatusNotFound, openapi.CodeNotFound, "no attachment %s in this message", part)
	}
	if err != nil {
		a.log.Warn("api: attachment", "message", d.ID, "part", part, "err", err)
		return openapi.Errorf(http.StatusUnprocessableEntity, openapi.CodeUndecodable, "the attachment cannot be decoded")
	}
	// Downloads are recorded like the agent's reads (§8.8): who, which
	// message and part, never the content or the sender's file name. No
	// record, no download.
	now := time.Now()
	err = a.db.RecordAction(r.Context(), store.Action{
		ID: ulid.New(now), Actor: v.PrincipalID, CredentialID: v.CredentialID, Tool: "api.attachment.download", At: now,
		Args:   map[string]any{"message_id": d.ID, "part": part},
		Result: map[string]any{"bytes": len(body)},
	})
	if err != nil {
		return err
	}
	typ := "application/octet-stream"
	if servedTypes[ent.MediaType] {
		typ = ent.MediaType
	}
	h := w.Header()
	h.Set("Content-Type", typ)
	h.Set("Content-Disposition", contentDisposition(ent.Filename, part))
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return nil
}

// maxFilename bounds the file name in bytes; file systems commonly allow
// 255.
const maxFilename = 200

// safeFilename turns the name the sender chose into one that is safe to
// save: no directories, no control or bidirectional formatting characters
// (which can make "txt.exe" display as "exe.txt"), no leading dot that
// would hide it, bounded in length with its extension kept. An empty
// result becomes attachment-<part>.
func safeFilename(name, part string) string {
	name = name[strings.LastIndexAny(name, `/\`)+1:]
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError {
			return -1
		}
		if unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	if len(name) > maxFilename {
		ext := ""
		if i := strings.LastIndexByte(name, '.'); i > 0 && len(name)-i <= 16 {
			ext = name[i:]
		}
		stem := name[:maxFilename-len(ext)]
		for !utf8.ValidString(stem) {
			stem = stem[:len(stem)-1]
		}
		name = strings.TrimRight(stem, " .") + ext
	}
	if name == "" {
		name = "attachment-" + strings.ReplaceAll(part, ".", "-")
	}
	return name
}

// contentDisposition is "attachment" with the file name twice (RFC 6266):
// an ASCII fallback in filename= and the exact name, percent-encoded
// UTF-8, in filename*= (RFC 8187), which every current browser prefers.
func contentDisposition(name, part string) string {
	name = safeFilename(name, part)
	var ascii, ext strings.Builder
	for _, r := range name {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' || r == '%' {
			ascii.WriteByte('_')
		} else {
			ascii.WriteRune(r)
		}
	}
	for _, b := range []byte(name) {
		if isAttrChar(b) {
			ext.WriteByte(b)
		} else {
			fmt.Fprintf(&ext, "%%%02X", b)
		}
	}
	return `attachment; filename="` + ascii.String() + `"; filename*=UTF-8''` + ext.String()
}

// isAttrChar is attr-char of RFC 8187.
func isAttrChar(b byte) bool {
	switch {
	case 'a' <= b && b <= 'z', 'A' <= b && b <= 'Z', '0' <= b && b <= '9':
		return true
	}
	return strings.IndexByte("!#$&+-.^_`|~", b) >= 0
}
