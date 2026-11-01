// Package imap is a minimal, read-only IMAP4rev1 client (RFC 3501) for
// pulling external mailboxes (§9, §10.1). It speaks just enough of the
// protocol for one sync round: CAPABILITY, ID, LOGIN or AUTHENTICATE
// (PLAIN, XOAUTH2), LIST with the LIST-STATUS and SPECIAL-USE returns,
// STATUS, EXAMINE, UID SEARCH ALL, UID FETCH, NOOP, UNSELECT and LOGOUT.
// Nothing that changes the remote mailbox exists here: no SELECT, STORE,
// COPY, MOVE, APPEND or EXPUNGE, and no CLOSE, which expunges.
//
// It is hand-written rather than built on go-imap/v2 (docs/design/
// architecture.md §5). That client's FETCH takes a fixed options struct
// and fails the whole response on a data item it does not know, so it
// cannot fetch Gmail's X-GM-MSGID and X-GM-LABELS or RFC 8474's EMAILID,
// and upstream declined the Gmail extensions. Those ids are what a pulled
// delivery's source_key is built from (§10.1); without them a Gmail
// mailbox would have only UID-based keys, and every UIDVALIDITY change
// would mean downloading it all again. The subset needed here is
// read-only and parses no MIME, so it is far narrower than a general
// client. UID FETCH therefore takes any list of data items and returns
// every item as a generic Value, with typed accessors for the ones the
// sync engine uses.
//
// Everything the server sends is untrusted: the client parses protocol
// syntax and nothing else, and hands message bytes over without looking
// inside them. Memory is bounded per response. A literal is checked
// against the caller's limit when it is announced, and one that is too
// large closes the connection without a byte of it being read. The
// non-literal part of a response, the nesting depth and the size of a
// SEARCH result have fixed bounds.
//
// The connection is always implicit TLS on port 993 (RFC 8314); STARTTLS
// is not supported. Every command runs under its context: the context's
// deadline bounds the command and cancelling it abandons the connection,
// and each read or write must also make progress within an idle timeout.
//
// Failures are *Error values whose Kind tells the caller what to do: back
// off (KindConnection), stop until a person fixes the credentials
// (KindAuth), wait out a server limit (KindLimit), skip what the server
// refused (KindRefused), or treat the server as broken (KindProtocol,
// KindTooLarge). A Client is not safe for concurrent use.
package imap
