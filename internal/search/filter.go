// Package search reads the text form of a search query (§16) into a
// Filter, the JMAP-shaped tree (RFC 8621 §4.4.1) that the store compiles
// to SQL under a Viewer, and writes the tree back as the normalized echo
// the caller shows. It is pure data: nothing here touches the database or
// decides what anyone may see.
package search

import (
	"fmt"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/classify"
)

// Operator is a JMAP FilterOperator.
type Operator string

const (
	And Operator = "AND"
	Or  Operator = "OR"
	// Not is JMAP's NOT: none of its conditions holds.
	Not Operator = "NOT"
)

// Field names a condition by its JSON (JMAP) name: RFC 8621's where JMAP
// defines one, yuuka's extensions in the same style. The text form uses
// other, shorter names (§16.3).
type Field string

// The conditions of §16.3. Fields JMAP defines but this version does not
// support have no Field: the parser reports them (Error.Unsupported).
const (
	// Text matches the subject, the sender's name and address and the
	// body. To / Cc names and Bcc join it with §16.7 step 2.
	Text    Field = "text"
	From    Field = "from"
	To      Field = "to"  // the To addresses (names: step 2)
	Cc      Field = "cc"  // the Cc addresses (names: step 2)
	Bcc     Field = "bcc" // the envelope Bcc of our own mail
	Subject Field = "subject"
	Body    Field = "body"
	// After is received_at >= Time, Before received_at < Time.
	After         Field = "after"
	Before        Field = "before"
	HasAttachment Field = "hasAttachment"

	DeliveredTo Field = "deliveredTo"
	Label       Field = "label"
	Archived    Field = "archived"
	Downgraded  Field = "downgraded"
	Sent        Field = "sent"
	Class       Field = "class"
	ListID      Field = "listId"
	Triage      Field = "triage"
)

// Filter is a node of the tree: an operator over Args, or, when Op is
// empty, the single condition Cond.
//
// A boolean condition (hasAttachment, archived, ...) holds when the fact
// is true; JMAP's hasAttachment: false is NOT over it. A JMAP condition
// with several properties is an AND of one-property conditions, and a
// string value of several terms an AND of one-term conditions, so every
// leaf here tests one thing.
type Filter struct {
	Op   Operator
	Args []*Filter
	Cond Condition
}

// Condition is one leaf. Which fields are set depends on Field.
type Condition struct {
	Field Field
	// Term is the value of a string condition (Text, From, To, Cc, Bcc,
	// Subject, Body, ListID): it must occur, case-insensitively, as a
	// substring. A quoted phrase is one term.
	Term string
	// Time is the bound of After and Before. Date is the day it was
	// written as, if it was (YYYY-MM-DD, kept for the echo).
	Time time.Time
	Date string
	// Prefix and Zone of DeliveredTo, folded; an empty Zone is any.
	Prefix, Zone string
	// Label is matched exactly, case-sensitively (§8.5).
	Label string
	Class classify.Class
	// Question and Option of Triage, checked against the question set.
	Question, Option string
	// At is where the text form wrote the condition (1-based
	// character), for errors found later; 0 when it came from elsewhere.
	At int
}

// Uses reports whether field occurs anywhere in f, negated or not.
func (f *Filter) Uses(field Field) bool {
	if f == nil {
		return false
	}
	if f.Op == "" {
		return f.Cond.Field == field
	}
	for _, a := range f.Args {
		if a.Uses(field) {
			return true
		}
	}
	return false
}

// Error is a query that cannot run as written. It says where, so a person
// or an agent can fix the query rather than guess.
type Error struct {
	// At is the 1-based character position in the query; 0 when the
	// error is about the query as a whole.
	At  int
	Msg string
	// Unsupported: JMAP defines the condition and yuuka reserves its name
	// but does not support it yet (JMAP's unsupportedFilter). It is
	// refused, never ignored, so supporting it later changes no query.
	Unsupported bool
	// Scope is set when a condition needs a scope the caller lacks.
	Scope auth.Scope
}

func (e *Error) Error() string {
	if e.At == 0 {
		return e.Msg
	}
	return fmt.Sprintf("at character %d: %s", e.At, e.Msg)
}

// Hint says that an item that looks like an operator was searched as
// text, and what was probably meant.
type Hint struct {
	Term    string // as written
	Suggest string // such as from: or has:attachment
}

func (h Hint) String() string {
	return fmt.Sprintf("%s was searched as text; did you mean %s?", h.Term, h.Suggest)
}
