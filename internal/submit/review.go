package submit

import (
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/extract"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/notify"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// What the review tier reads of an outgoing message (§8.9): the bytes
// that go out, parsed like incoming mail, every text part read (both
// alternatives, the markup and its text, text attachments, attached
// messages), cut into candidate values, linked hosts and references.
// Anything it cannot read as text is named, and the review escalates it.

// Bounds of what the review reads; beyond them the part is unreadable.
const (
	// maxReviewText bounds one text part. Mail written by people and
	// agents is far below it; more is a dump, not a reply.
	maxReviewText = 2 * mimeparse.MaxBodyText
	// maxNesting bounds attached messages inside attached messages.
	maxNesting = 2
	// maxLinks is mimeparse's bound on the links of a message: a message
	// that reaches it may have more the review never saw.
	maxLinks = 200
)

// anchorPattern finds yuuka:// message URIs written in text.
var anchorPattern = regexp.MustCompile(`(?i)yuuka://message/([0-9A-HJKMNP-TV-Z]{26})`)

// outgoing is a message as the review and the held card read it.
type outgoing struct {
	review      store.ReviewInput
	subject     string
	excerpt     string
	attachments []notify.Attachment
}

// readOutgoing reads raw, with cites the draft's cites (nil for SMTP).
func readOutgoing(raw []byte, cites json.RawMessage) *outgoing {
	c := &collector{tokens: map[string]bool{}, hosts: map[string]bool{}, refs: map[string]bool{}}
	m := c.message(raw, "", 0)
	o := &outgoing{subject: m.Subject, excerpt: m.BodyText, attachments: c.attachments}
	var list []struct {
		URI string `json:"uri"`
	}
	if len(cites) > 0 {
		if err := json.Unmarshal(cites, &list); err != nil {
			c.out.Unreadable = append(c.out.Unreadable, "the cites")
		}
	}
	for _, ct := range list {
		ref := store.ReviewRef{As: ct.URI, Where: "cites"}
		if id, _, err := agentview.ParseURI(ct.URI); err == nil {
			ref.MessageID = id
		}
		c.ref(ref)
	}
	o.review = c.out
	return o
}

type collector struct {
	out                 store.ReviewInput
	tokens, hosts, refs map[string]bool
	attachments         []notify.Attachment
}

// message reads one message, the outgoing one or one attached to it
// (where names it then), and returns what mimeparse made of it.
func (c *collector) message(raw []byte, where string, depth int) mimeparse.Message {
	m, root := mimeparse.ParseTree(raw)
	if m.Problem != "" {
		c.unreadable(where + "the message structure (" + m.Problem + ")")
	}
	c.text(where+"subject", m.Subject, false)
	if len(m.Links) >= maxLinks {
		c.unreadable(fmt.Sprintf("%smore than %d links", where, maxLinks-1))
	}
	for _, l := range m.Links {
		c.token(where+"body", l.Href)
		c.host(where+"body", l.Href)
	}
	c.entity(raw, root, where, depth)
	return m
}

func (c *collector) entity(raw []byte, e mimeparse.Entity, where string, depth int) {
	if strings.HasPrefix(e.MediaType, "multipart/") {
		for _, ch := range e.Children {
			c.entity(raw, ch, where, depth)
		}
		return
	}
	place := where + "body"
	if e.Attachment {
		place = where + "attachment " + e.Section
		if e.Filename != "" {
			place += " (" + e.Filename + ")"
		}
	}
	switch {
	case !e.Attachment && (e.MediaType == "text/plain" || e.MediaType == "text/html"):
		c.body(place, e.MediaType, e.Text)
	case e.Attachment && (strings.HasPrefix(e.MediaType, "text/") || e.MediaType == "message/rfc822"):
		_, b, err := mimeparse.Attachment(raw, e.Section)
		if err != nil {
			c.unreadable(place)
			return
		}
		if depth == 0 {
			c.attachments = append(c.attachments, notify.Attachment{Name: e.Filename, Type: e.MediaType, Size: len(b)})
		}
		if e.MediaType != "message/rfc822" {
			c.body(place, e.MediaType, strings.ToValidUTF8(string(b), "�"))
			return
		}
		if depth >= maxNesting {
			c.unreadable(place + ": attached messages nested too deep")
			return
		}
		inner := c.message(b, place+": ", depth+1)
		ref := store.ReviewRef{As: "an attached message without a Message-ID", Where: place}
		if inner.MessageID != "" {
			ref.As, ref.HeaderID = "<"+inner.MessageID+">", inner.MessageID
		}
		c.ref(ref)
	default:
		if depth == 0 && e.Attachment {
			c.attachments = append(c.attachments, notify.Attachment{Name: e.Filename, Type: e.MediaType})
		}
		c.unreadable(fmt.Sprintf("%s: %s is not text", place, e.MediaType))
	}
}

// body reads one text part; HTML both as markup (what is hidden in it
// still goes out) and as the text a reader sees.
func (c *collector) body(place, mediaType, s string) {
	if len(s) > maxReviewText {
		c.unreadable(place + ": too long")
		return
	}
	c.text(place, s, true)
	if mediaType == "text/html" {
		c.text(place, html.UnescapeString(s), true)
		c.text(place, mimeparse.HTMLToText(s), true)
	}
}

func (c *collector) text(place, s string, anchors bool) {
	for _, t := range extract.Tokens(s) {
		c.token(place, t)
	}
	for _, u := range extract.URLs(s) {
		c.host(place, u)
	}
	if anchors {
		for _, m := range anchorPattern.FindAllStringSubmatch(s, -1) {
			id := strings.ToUpper(m[1])
			c.ref(store.ReviewRef{As: agentview.MessageURI(id), Where: place, MessageID: id})
		}
	}
}

func (c *collector) token(place, v string) {
	// One past the bound is enough for the store to know it is over.
	if v == "" || c.tokens[v] || len(c.out.Tokens) > extract.MaxTokens {
		return
	}
	c.tokens[v] = true
	c.out.Tokens = append(c.out.Tokens, store.ReviewItem{Value: v, Where: place})
}

// host records the host of link u. A URL that does not parse is kept by
// what stands where its host would, which no domain check will know.
func (c *collector) host(place, u string) {
	h := ""
	if p, err := url.Parse(strings.TrimSpace(u)); err == nil {
		h = p.Hostname()
	} else if _, rest, ok := strings.Cut(u, "://"); ok {
		h, _, _ = strings.Cut(rest, "/")
	}
	if h == "" || c.hosts[h] {
		return
	}
	c.hosts[h] = true
	c.out.Hosts = append(c.out.Hosts, store.ReviewItem{Value: h, Where: place})
}

func (c *collector) ref(r store.ReviewRef) {
	if c.refs[r.As] {
		return
	}
	c.refs[r.As] = true
	c.out.Refs = append(c.out.Refs, r)
}

func (c *collector) unreadable(what string) {
	c.out.Unreadable = append(c.out.Unreadable, what)
}
