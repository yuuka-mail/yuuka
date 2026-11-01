package api

import (
	"reflect"
	"slices"
	"testing"

	"github.com/yuuka-mail/yuuka/internal/auth"
)

// Who may reach a Store method that takes no Viewer (§13.8). Such a
// method reads or writes whatever the caller could see or not, so it is
// a person's: a route that reaches it is served to a person only
// (TestACL checks every route it calls one from), and its handler
// decides what the person may do. The few that are not are a decision
// the route's scope stands for, made in the caller's name.
const (
	people = "people"
	scoped = "scoped"
)

var unviewed = map[string]string{
	// The audit record of an attachment download, in the reader's name.
	"RecordAction": scoped,
	// Claiming a prefix and what it expects (§3.3): prefix:manage.
	"SetPrefixState":    scoped,
	"SetPrefixExpected": scoped,

	// Deciding on mail whose delivery is unknown (§4.5).
	"ResolveOutbox": people,
	// Rules (§8.5).
	"Rules": people, "RuleByID": people, "CreateRule": people, "UpdateRule": people, "DeleteRule": people,
	"ApplyRule": people, "RevertRule": people,
	// Who may do what (§13.10).
	"Tokens": people, "Principal": people, "PrincipalByName": people, "Grantee": people, "CreateCredential": people,
	"CredentialForAuth": people, "RevokeCredential": people, "Accounts": people, "AddIdentity": people,
	"DisableIdentity": people, "DisableIdentityIfTighter": people, "Identities": people, "IdentityByID": people,
	"Grants": people, "GrantFor": people, "SetGrant": people, "RemoveGrant": people, "TightenGrant": people,
	"RemoveGrantIfTighter": people,
	// Transports, channels, bindings and their secrets (§17.6).
	"Transports": people, "TransportByName": people, "ConfigureTransport": people, "DisableTransport": people,
	"ReplaceIdentityTransports": people, "Secret": people, "Secrets": people, "Channels": people,
	"ChannelByName": people, "ConfigureChannel": people, "DisableChannel": people, "Bindings": people,
	"BindingByID": people, "RevokeBinding": people, "Subscribe": people, "NewBindCode": people,
	"BindCodeByID": people, "ConfirmBindCode": people, "DropBindCode": people,
	// Pull accounts' sources and who reads each account (§9.5, §13.8).
	"Sources": people, "SourceByName": people, "SourceMailboxes": people, "PullFailures": people,
	"AddPullAccount": people, "ConfigureSource": people, "DisableSource": people, "PauseSource": people,
	"ResumeSource": people, "RequestSync": people, "AccountByName": people, "AccountAccess": people,
	"SetAccountAccess": people, "SetAccountDefaultRead": people,
}

// TestUnviewed: every Store method without a Viewer says who may reach
// it, and the table names nothing else. A new one is a decision written
// down here.
func TestUnviewed(t *testing.T) {
	viewer := reflect.TypeFor[*auth.Viewer]()
	st := reflect.TypeFor[Store]()
	for i := range st.NumMethod() {
		m := st.Method(i)
		takes := false
		for j := range m.Type.NumIn() {
			takes = takes || m.Type.In(j) == viewer
		}
		_, listed := unviewed[m.Name]
		switch {
		case takes && listed:
			t.Errorf("%s takes a Viewer: leave it out of unviewed", m.Name)
		case !takes && !listed:
			t.Errorf("%s takes no Viewer: say in unviewed who may reach it", m.Name)
		}
	}
	for name, who := range unviewed {
		if _, ok := st.MethodByName(name); !ok {
			t.Errorf("unviewed lists %s, which Store does not have", name)
		}
		if !slices.Contains([]string{people, scoped}, who) {
			t.Errorf("%s: %q", name, who)
		}
	}
}
