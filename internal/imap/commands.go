package imap

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Capability asks the server for its capabilities.
func (c *Client) Capability(ctx context.Context) (Caps, error) {
	if _, err := c.exec(ctx, "CAPABILITY", "CAPABILITY", false, smallLiteral, nil, nil); err != nil {
		return nil, err
	}
	return c.caps, nil
}

// ID sends the client's identification (RFC 2971) and returns the
// server's. Some providers expect it before login. Fields are sent in
// key order; nil sends ID NIL. Check Caps().Has("ID") first: a server
// without it answers BAD.
func (c *Client) ID(ctx context.Context, fields map[string]string) (map[string]string, error) {
	args := "NIL"
	if len(fields) > 0 {
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		parts := make([]string, 0, 2*len(keys))
		for _, k := range keys {
			qk, err1 := quote(k)
			qv, err2 := quote(fields[k])
			if err := firstErr(err1, err2); err != nil {
				return nil, &Error{Kind: KindRefused, Op: "ID", Err: err}
			}
			parts = append(parts, qk, qv)
		}
		args = "(" + strings.Join(parts, " ") + ")"
	}
	var server map[string]string
	_, err := c.exec(ctx, "ID", "ID "+args, false, smallLiteral, func(r *response) error {
		if r.name != "ID" || r.hasNum || len(r.fields) != 1 {
			return nil
		}
		server = map[string]string{}
		l := r.fields[0].List
		for i := 0; i+1 < len(l); i += 2 {
			k, ok := l[i].Text()
			if !ok {
				continue
			}
			v, _ := l[i+1].Text()
			server[k] = v
		}
		return nil
	}, nil)
	return server, err
}

// Login authenticates with LOGIN. The user name and password are sent as
// quoted strings, so they must be 7-bit and free of line breaks; anything
// else needs AuthenticatePlain. The capabilities are refreshed on success.
func (c *Client) Login(ctx context.Context, user, password string) error {
	if c.caps.Has("LOGINDISABLED") {
		return &Error{Kind: KindRefused, Op: "LOGIN", Err: errors.New("the server disabled LOGIN")}
	}
	qu, err1 := quote(user)
	qp, err2 := quote(password)
	if firstErr(err1, err2) != nil {
		// Not the quote error itself: it would describe the password.
		return &Error{Kind: KindRefused, Op: "LOGIN", Err: errors.New("user name or password cannot be sent with LOGIN; use AUTHENTICATE PLAIN")}
	}
	return c.authenticated(ctx, "LOGIN", "LOGIN "+qu+" "+qp, nil)
}

// AuthenticatePlain authenticates with SASL PLAIN (RFC 4616), sending the
// response with the command when the server has SASL-IR (RFC 4959).
func (c *Client) AuthenticatePlain(ctx context.Context, user, password string) error {
	return c.authenticate(ctx, "PLAIN", "\x00"+user+"\x00"+password)
}

// AuthenticateXOAUTH2 authenticates with Google's and Microsoft's XOAUTH2
// SASL mechanism, given an OAuth 2.0 access token. On a rejected token the
// server's error, a small JSON document, is in the returned error.
func (c *Client) AuthenticateXOAUTH2(ctx context.Context, user, token string) error {
	return c.authenticate(ctx, "XOAUTH2", "user="+user+"\x01auth=Bearer "+token+"\x01\x01")
}

func (c *Client) authenticate(ctx context.Context, mech, ir string) error {
	op := "AUTHENTICATE " + mech
	if !c.caps.Has("AUTH=" + mech) {
		// Checked here, because a NO from the server would read as
		// rejected credentials.
		return &Error{Kind: KindRefused, Op: op, Err: fmt.Errorf("the server does not offer %s", mech)}
	}
	resp := base64.StdEncoding.EncodeToString([]byte(ir))
	cmd := op
	sent := false
	if c.caps.Has("SASL-IR") {
		cmd += " " + resp
		sent = true
	}
	var challenge string
	err := c.authenticated(ctx, op, cmd, func(text string) string {
		if !sent {
			sent = true
			return resp
		}
		if b, err := base64.StdEncoding.DecodeString(text); err == nil {
			challenge = string(b)
		}
		if mech == "XOAUTH2" {
			// The challenge after the response is the error; an empty
			// response asks for the final NO (Google's XOAUTH2 protocol).
			return ""
		}
		// PLAIN has no second step: cancel (RFC 3501 §6.2.2).
		return "*"
	})
	var e *Error
	if challenge != "" && errors.As(err, &e) && e.Err == nil {
		e.Err = fmt.Errorf("server challenge %q", challenge)
	}
	return err
}

// authenticated runs LOGIN or AUTHENTICATE and then makes sure the
// capabilities are the post-login ones.
func (c *Client) authenticated(ctx context.Context, op, cmd string, cont func(string) string) error {
	before := c.caps
	c.caps = nil
	if _, err := c.exec(ctx, op, cmd, true, smallLiteral, nil, cont); err != nil {
		if c.caps == nil {
			c.caps = before
		}
		return err
	}
	if c.caps == nil {
		if _, err := c.Capability(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Mailbox is one LIST entry.
type Mailbox struct {
	// Name is the name as the server sent it, in modified UTF-7 (RFC 3501
	// §5.1.3). Pass it as is to Status and Examine. DisplayName decodes it.
	Name string
	// Delim is the hierarchy delimiter, "" when the server has none.
	Delim string
	// Attrs are the name attributes as sent: \Noselect, \HasChildren, and
	// with SPECIAL-USE, \All, \Junk, \Trash, \Sent, \Drafts ...
	Attrs []string
	// Status is set when the server returned it with the LIST (LIST-STATUS).
	Status *MailboxStatus
}

// HasAttr reports whether the attribute is set, ignoring case.
func (m *Mailbox) HasAttr(attr string) bool {
	for _, a := range m.Attrs {
		if strings.EqualFold(a, attr) {
			return true
		}
	}
	return false
}

// Selectable reports whether the mailbox can be opened.
func (m *Mailbox) Selectable() bool {
	return !m.HasAttr(`\Noselect`) && !m.HasAttr(`\NonExistent`)
}

// DisplayName decodes Name for people. A name that is not valid modified
// UTF-7, as some servers send raw UTF-8, is returned as is.
func (m *Mailbox) DisplayName() string {
	if s, err := DecodeMailbox(m.Name); err == nil {
		return s
	}
	return m.Name
}

// MailboxStatus holds the STATUS items. An item that was not asked for or
// not returned is zero; a server sends a zero UIDVALIDITY or UIDNEXT
// only if it is broken, and HIGHESTMODSEQ is never zero (RFC 7162).
type MailboxStatus struct {
	Messages      uint32
	UIDNext       uint32
	UIDValidity   uint32
	Unseen        uint32
	HighestModSeq uint64
}

// ListOptions shapes a LIST.
type ListOptions struct {
	// Pattern is the mailbox pattern; "" means "*", every mailbox.
	Pattern string
	// SpecialUse asks for the special-use attributes (RFC 6154). It is
	// sent only when the server has SPECIAL-USE and LIST-EXTENDED; servers
	// with SPECIAL-USE commonly return the attributes anyway.
	SpecialUse bool
	// Status asks for STATUS items with each mailbox, such as MESSAGES,
	// UIDNEXT, UIDVALIDITY (RFC 5819). It is sent only when the server has
	// LIST-STATUS; otherwise Mailbox.Status stays nil and the caller asks
	// with Status.
	Status []string
}

// List lists the mailboxes matching opts.Pattern, asking for what the
// server can return with them.
func (c *Client) List(ctx context.Context, opts ListOptions) ([]Mailbox, error) {
	pattern := opts.Pattern
	if pattern == "" {
		pattern = "*"
	}
	qp, err := quote(pattern)
	if err != nil {
		return nil, &Error{Kind: KindRefused, Op: "LIST", Err: err}
	}
	if err := checkAtoms(opts.Status); err != nil {
		return nil, &Error{Kind: KindRefused, Op: "LIST", Err: err}
	}
	cmd := `LIST "" ` + qp
	var ret []string
	if opts.SpecialUse && c.caps.Has("SPECIAL-USE") && (c.caps.Has("LIST-EXTENDED") || c.caps.Has("LIST-STATUS")) {
		ret = append(ret, "SPECIAL-USE")
	}
	if len(opts.Status) > 0 && c.caps.Has("LIST-STATUS") {
		ret = append(ret, "STATUS ("+strings.Join(opts.Status, " ")+")")
	}
	if len(ret) > 0 {
		cmd += " RETURN (" + strings.Join(ret, " ") + ")"
	}

	var boxes []Mailbox
	status := map[string]*MailboxStatus{}
	_, err = c.exec(ctx, "LIST", cmd, false, smallLiteral, func(r *response) error {
		switch {
		case r.hasNum:
		case r.name == "LIST":
			m, err := parseList(r.fields)
			if err != nil {
				return err
			}
			boxes = append(boxes, m)
		case r.name == "STATUS":
			name, s, err := parseStatus(r.fields)
			if err != nil {
				return err
			}
			status[name] = s
		}
		return nil
	}, nil)
	if err != nil {
		return nil, err
	}
	for i := range boxes {
		boxes[i].Status = status[boxes[i].Name]
	}
	return boxes, nil
}

// parseList reads mailbox-list: "(" attrs ")" SP (quoted-char / NIL) SP
// mailbox [SP extended items], the extended items ignored.
func parseList(f []Value) (Mailbox, error) {
	if len(f) < 3 || f[0].Type != ListValue {
		return Mailbox{}, syntaxf("LIST response")
	}
	var m Mailbox
	for _, a := range f[0].List {
		t, ok := a.Text()
		if !ok {
			return Mailbox{}, syntaxf("LIST attribute")
		}
		m.Attrs = append(m.Attrs, t)
	}
	if f[1].Type != NilValue {
		d, ok := f[1].Text()
		if !ok {
			return Mailbox{}, syntaxf("LIST delimiter")
		}
		m.Delim = d
	}
	name, ok := f[2].Text()
	if !ok {
		return Mailbox{}, syntaxf("LIST mailbox name")
	}
	m.Name = name
	return m, nil
}

// parseStatus reads mailbox SP "(" item SP number ... ")".
func parseStatus(f []Value) (string, *MailboxStatus, error) {
	if len(f) != 2 || f[1].Type != ListValue || len(f[1].List)%2 != 0 {
		return "", nil, syntaxf("STATUS response")
	}
	name, ok := f[0].Text()
	if !ok {
		return "", nil, syntaxf("STATUS mailbox name")
	}
	s := &MailboxStatus{}
	l := f[1].List
	for i := 0; i < len(l); i += 2 {
		item, _ := l[i].Text()
		n, ok := l[i+1].Number()
		if !ok {
			return "", nil, syntaxf("STATUS %s value", item)
		}
		n32 := uint32(n)
		if n > 1<<32-1 && asciiUpper(item) != "HIGHESTMODSEQ" {
			return "", nil, syntaxf("STATUS %s value", item)
		}
		switch asciiUpper(item) {
		case "MESSAGES":
			s.Messages = n32
		case "UIDNEXT":
			s.UIDNext = n32
		case "UIDVALIDITY":
			s.UIDValidity = n32
		case "UNSEEN":
			s.Unseen = n32
		case "HIGHESTMODSEQ":
			s.HighestModSeq = n
		}
	}
	return name, s, nil
}

// Status asks for STATUS items of one mailbox without opening it.
func (c *Client) Status(ctx context.Context, mailbox string, items ...string) (*MailboxStatus, error) {
	qm, err := quote(mailbox)
	if err == nil {
		err = checkAtoms(items)
	}
	if err == nil && len(items) == 0 {
		err = errors.New("no STATUS items")
	}
	if err != nil {
		return nil, &Error{Kind: KindRefused, Op: "STATUS", Err: err}
	}
	var st *MailboxStatus
	_, err = c.exec(ctx, "STATUS", "STATUS "+qm+" ("+strings.Join(items, " ")+")", false, smallLiteral, func(r *response) error {
		if r.name != "STATUS" || r.hasNum {
			return nil
		}
		name, s, err := parseStatus(r.fields)
		if err != nil {
			return err
		}
		if name == mailbox || st == nil {
			st = s
		}
		return nil
	}, nil)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, &Error{Kind: KindRefused, Op: "STATUS", Err: errors.New("OK without a STATUS response")}
	}
	return st, nil
}

// Selected describes a mailbox opened with Examine.
type Selected struct {
	Name string
	// UIDValidity and UIDNext are zero when the server did not send them;
	// without UIDVALIDITY the UIDs cannot be kept across sessions.
	UIDValidity uint32
	UIDNext     uint32
	Exists      uint32
	// HighestModSeq is set when the server has CONDSTORE and reported it;
	// NoModSeq when it said the mailbox has no mod-sequences.
	HighestModSeq uint64
	NoModSeq      bool
	// ReadOnly is the server's [READ-ONLY] confirmation.
	ReadOnly bool
}

// Examine opens a mailbox read-only (RFC 3501 §6.3.2). Fetching from it
// never sets \Seen, whatever the fetch items. A failed Examine leaves no
// mailbox open.
func (c *Client) Examine(ctx context.Context, mailbox string) (*Selected, error) {
	qm, err := quote(mailbox)
	if err != nil {
		return nil, &Error{Kind: KindRefused, Op: "EXAMINE", Err: err}
	}
	sel := &Selected{Name: mailbox}
	s, err := c.exec(ctx, "EXAMINE", "EXAMINE "+qm, false, smallLiteral, func(r *response) error {
		if r.hasNum && r.name == "EXISTS" {
			sel.Exists = r.num
			return nil
		}
		if r.status == nil || r.status.Type != "OK" {
			return nil
		}
		return selectCode(sel, r.status)
	}, nil)
	if err != nil {
		return nil, err
	}
	if s.Code == "READ-ONLY" {
		sel.ReadOnly = true
	}
	return sel, nil
}

func selectCode(sel *Selected, s *StatusResponse) error {
	if s.Code == "NOMODSEQ" {
		sel.NoModSeq = true
		return nil
	}
	if s.Code != "UIDVALIDITY" && s.Code != "UIDNEXT" && s.Code != "HIGHESTMODSEQ" {
		return nil
	}
	v := Value{Type: AtomValue, Atom: s.Arg}
	n, ok := v.Number()
	n32, ok32 := v.number32()
	switch {
	case s.Code == "HIGHESTMODSEQ" && ok:
		sel.HighestModSeq = n
	case s.Code == "UIDVALIDITY" && ok32:
		sel.UIDValidity = n32
	case s.Code == "UIDNEXT" && ok32:
		sel.UIDNext = n32
	default:
		return syntaxf("[%s %s]", s.Code, s.Arg)
	}
	return nil
}

// UIDSearchAll returns the UIDs of every message in the open mailbox.
// Some servers return an empty or partial list; compare its length with
// Selected.Exists before trusting it (§10.1.2).
func (c *Client) UIDSearchAll(ctx context.Context) ([]uint32, error) {
	var uids []uint32
	_, err := c.exec(ctx, "UID SEARCH", "UID SEARCH ALL", false, smallLiteral, func(r *response) error {
		if r.name == "SEARCH" {
			uids = append(uids, r.uids...)
		}
		return nil
	}, nil)
	if err != nil {
		return nil, err
	}
	return uids, nil
}

// Noop does nothing; it keeps an idle connection alive and lets the
// server send pending updates.
func (c *Client) Noop(ctx context.Context) error {
	_, err := c.exec(ctx, "NOOP", "NOOP", false, smallLiteral, nil, nil)
	return err
}

// Unselect closes the open mailbox without expunging (RFC 3691) when the
// server has UNSELECT. Otherwise it does nothing: the next Examine
// replaces the open mailbox, and CLOSE is never used because on a
// mailbox opened read-write it expunges (§10.1.2).
func (c *Client) Unselect(ctx context.Context) error {
	if !c.caps.Has("UNSELECT") {
		return nil
	}
	_, err := c.exec(ctx, "UNSELECT", "UNSELECT", false, smallLiteral, nil, nil)
	return err
}

// Logout ends the session and closes the connection. A server that says
// BYE and hangs up without completing the command has still logged out.
func (c *Client) Logout(ctx context.Context) error {
	defer func() { _ = c.Close() }()
	_, err := c.exec(ctx, "LOGOUT", "LOGOUT", false, smallLiteral, nil, nil)
	var e *Error
	if errors.As(err, &e) && e.Resp != nil && e.Resp.Type == "BYE" {
		return nil
	}
	return err
}

// quote makes s a quoted string. Only 7-bit text without NUL or line
// breaks can be quoted (RFC 3501 §9 TEXT-CHAR); the client never sends
// literals.
func quote(s string) (string, error) {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == 0 || c == '\r' || c == '\n' || c >= 0x80:
			return "", errors.New("string cannot be sent quoted")
		case c == '"' || c == '\\':
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	b.WriteByte('"')
	return b.String(), nil
}

// checkAtoms checks names sent bare, such as STATUS items.
func checkAtoms(items []string) error {
	for _, it := range items {
		if it == "" {
			return errors.New("empty item")
		}
		if strings.TrimLeft(it, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-.") != "" {
			return fmt.Errorf("item %q", it)
		}
	}
	return nil
}
