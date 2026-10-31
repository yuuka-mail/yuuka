package agentview

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// The anchor scheme is an external identifier (§8.1, §11): once people
// and agents hold these URIs it cannot change. Its grammar:
//
//	yuuka://message/<ULID>
//	yuuka://message/<ULID>#part/<section>
//	yuuka://message/<ULID>#part/<section>/para/<n>
//
// <section> is the IMAP section number of the body part (RFC 3501
// §6.4.5: "1" for a single-part message, "2", "1.2" ... in multiparts), so
// it depends only on the MIME structure of the raw message. <n> counts the
// blocks of that part from 1 in document order, whatever their kind.

const messagePrefix = "yuuka://message/"

// MessageURI is the URI of a message.
func MessageURI(id string) string { return messagePrefix + id }

// Anchor is the fragment of a message URI. The zero Anchor is the whole
// message.
type Anchor struct {
	Part string // IMAP section number; "" for the whole message
	Para int    // 1-based block number; 0 for the whole part
}

// String is the fragment without "#".
func (a Anchor) String() string {
	switch {
	case a.Part == "":
		return ""
	case a.Para == 0:
		return "part/" + a.Part
	}
	return "part/" + a.Part + "/para/" + strconv.Itoa(a.Para)
}

var (
	sectionPattern = regexp.MustCompile(`^[1-9][0-9]{0,3}(\.[1-9][0-9]{0,3}){0,15}$`)
	paraPattern    = regexp.MustCompile(`^[1-9][0-9]{0,6}$`)
)

// ErrBadURI is returned for anything that is not a yuuka:// message URI.
var ErrBadURI = errors.New("not a yuuka://message/<id>[#part/<section>[/para/<n>]] URI")

// ParseURI splits a message URI into the message id and the anchor.
func ParseURI(s string) (id string, a Anchor, err error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), messagePrefix)
	if !ok {
		return "", a, ErrBadURI
	}
	id, frag, hasFrag := strings.Cut(rest, "#")
	if !ulid.Valid(id) {
		return "", a, ErrBadURI
	}
	if !hasFrag || frag == "" {
		return id, a, nil
	}
	a, err = ParseAnchor(frag)
	return id, a, err
}

// UnmarshalText is ParseAnchor, for a fragment that arrives as a
// parameter.
func (a *Anchor) UnmarshalText(b []byte) error {
	var err error
	if *a, err = ParseAnchor(string(b)); err != nil {
		return errors.New("want part/<section>[/para/<n>]")
	}
	return nil
}

// ParseAnchor reads a fragment such as "part/2/para/7".
func ParseAnchor(frag string) (Anchor, error) {
	seg := strings.Split(frag, "/")
	if len(seg) != 2 && len(seg) != 4 || seg[0] != "part" || !sectionPattern.MatchString(seg[1]) {
		return Anchor{}, ErrBadURI
	}
	a := Anchor{Part: seg[1]}
	if len(seg) == 4 {
		if seg[2] != "para" || !paraPattern.MatchString(seg[3]) {
			return Anchor{}, ErrBadURI
		}
		a.Para, _ = strconv.Atoi(seg[3])
	}
	return a, nil
}
