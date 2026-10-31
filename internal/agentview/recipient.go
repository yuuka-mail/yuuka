package agentview

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/yuuka-mail/yuuka/internal/store"
)

// Recipient is one recipient of a draft as a caller writes it: a bare
// address, or {name, addr} for one with a display name. Drafts read
// back with Address, the shape a message's To and Cc have, so the
// recipients of a message can be copied into a draft as they are.
//
// An object rather than "Name <addr>": a decoded name such as
// `Smith, John` would need RFC 5322 quoting to survive the string, and a
// string like `"ceo@example.com" <x@example.net>` reads as one address
// and parses as another. Here the address is a field of its own, the
// only one any decision reads (§8.9); the name is shown and written into
// the header.
type Recipient struct {
	Name string `json:"name,omitempty"`
	Addr string `json:"addr"`
}

// UnmarshalJSON takes a bare address string or an object.
func (r *Recipient) UnmarshalJSON(b []byte) error {
	if b = bytes.TrimSpace(b); len(b) > 0 && b[0] == '"' {
		*r = Recipient{}
		return json.Unmarshal(b, &r.Addr)
	}
	type plain Recipient
	var p plain
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return err
	}
	if p.Addr == "" {
		return errors.New("a recipient needs addr")
	}
	*r = Recipient(p)
	return nil
}

// JSONSchema is the schema of a Recipient: either form.
func (Recipient) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{AnyOf: []*jsonschema.Schema{
		{Type: "string", Description: "a bare address"},
		{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"name": {Type: "string", Description: "the display name, shown and written into To / Cc; it decides nothing"},
				"addr": {Type: "string", Description: "a bare address"},
			},
			Required:             []string{"addr"},
			AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
		},
	}}
}

// DraftRecipients are rs as the store keeps them; nil writes none.
func DraftRecipients(rs []Recipient) []store.Recipient {
	out := make([]store.Recipient, len(rs))
	for i, r := range rs {
		out[i] = store.Recipient{Name: r.Name, Addr: r.Addr}
	}
	return out
}

// AddressesOf are a draft's recipients as they are read back: never nil.
func AddressesOf(rs []store.Recipient) []Address {
	out := make([]Address, len(rs))
	for i, r := range rs {
		out[i] = Address{Name: r.Name, Addr: r.Addr}
	}
	return out
}

// String is a as people write it, `Name <addr>`, the name quoted when it
// holds a character that would end it (RFC 5322 specials). Text to read,
// not a header: the name stays UTF-8.
func (a Address) String() string {
	if a.Name == "" {
		return a.Addr
	}
	name := a.Name
	if strings.ContainsAny(name, `()<>[]:;@\,."`) {
		name = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name) + `"`
	}
	return name + " <" + a.Addr + ">"
}
