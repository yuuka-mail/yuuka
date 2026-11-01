package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/yuuka-mail/yuuka/internal/auth"
)

// AccountsInput is the input of list_accounts: none.
type AccountsInput struct{}

// AccountsOutput is the structured result of list_accounts.
type AccountsOutput struct {
	Accounts []Account `json:"accounts"`
}

// Account is an account the caller reads (§9.4). Its name is what
// account: in search_messages takes. Where a pull account's mail comes
// from (server, user name, how far it synced) is a person's
// configuration and never here: no tool reads or changes it.
type Account struct {
	Name string `json:"name"`
	// Kind is cf_catchall (mail pushed to the deployment's own domains)
	// or imap (a mailbox yuuka pulls).
	Kind string `json:"kind"`
	// Address is a pull account's mailbox; absent for a catch-all.
	Address string `json:"address,omitempty"`
}

func (s *Server) listAccounts(ctx context.Context, v *auth.Viewer, _ AccountsInput) (result, error) {
	as, err := s.db.VisibleAccounts(ctx, v)
	if err != nil {
		return result{}, err
	}
	out := AccountsOutput{Accounts: make([]Account, len(as))}
	var b strings.Builder
	fmt.Fprintf(&b, "%d accounts you may read; search_messages takes account:<name> to search one\n", len(as))
	for i, a := range as {
		out.Accounts[i] = Account{Name: a.Name, Kind: a.Kind, Address: a.Address}
		fmt.Fprintf(&b, "%s · %s", a.Name, a.Kind)
		if a.Address != "" {
			fmt.Fprintf(&b, " · %s", a.Address)
		}
		b.WriteByte('\n')
	}
	return result{structured: out, text: b.String(), audit: map[string]any{"returned": len(as)}}, nil
}
