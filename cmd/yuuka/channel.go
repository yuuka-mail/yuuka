package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/yuuka-mail/yuuka/internal/manage"
	"github.com/yuuka-mail/yuuka/internal/notify"
	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
)

const channelUsage = "usage: yuuka channel add NAME --token-file PATH|- | yuuka channel ls [--all] | yuuka channel rm NAME | " +
	"yuuka channel test NAME [--binding ID] | yuuka channel bind NAME [--principal human:<name>] [--events E,...] [--label L] | " +
	"yuuka channel unbind BINDING | yuuka channel subscribe|unsubscribe BINDING EVENT [EVENT]"

// bindPoll is how often `channel bind` looks whether its code was sent to
// the bot. serve's poller receives the /start (one poller per bot,
// §17.4), so the CLI waits on the database.
var bindPoll = time.Second

// channelCmd is `yuuka channel`: notification channels (a Telegram bot
// each), the chats bound to people on them, and what each binding
// receives (§17, §8.4, §13.7). Adding a channel and binding a chat are
// widenings: recorded, and announced as security alerts when the key is
// here to open a bot with.
func (h *host) channelCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(channelUsage)
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("channel add", flag.ContinueOnError)
		tokenFile := fs.String("token-file", "", "file holding the bot token from @BotFather, or - for standard input. "+
			"Never the token itself: arguments end up in shell history and ps")
		if err := parseInterspersed(fs, args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 || *tokenFile == "" {
			return errors.New(channelUsage)
		}
		return h.channelAdd(ctx, fs.Arg(0), *tokenFile)
	case "ls":
		fs := flag.NewFlagSet("channel ls", flag.ContinueOnError)
		all := fs.Bool("all", false, "include removed channels and revoked bindings")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New(channelUsage)
		}
		return h.channelList(ctx, *all)
	case "rm":
		if len(args) != 2 {
			return errors.New(channelUsage)
		}
		return h.channelRemove(ctx, args[1])
	case "test":
		fs := flag.NewFlagSet("channel test", flag.ContinueOnError)
		binding := fs.String("binding", "", "send to this binding only (default: every binding on the channel)")
		if err := parseInterspersed(fs, args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New(channelUsage)
		}
		return h.channelTest(ctx, fs.Arg(0), *binding)
	case "bind":
		fs := flag.NewFlagSet("channel bind", flag.ContinueOnError)
		principal := fs.String("principal", "", "human:<name> the chat is for (default: the only human)")
		events := fs.String("events", strings.Join(store.Events, ","), "events the binding receives, comma-separated")
		label := fs.String("label", "", "a name for the binding (default: who Telegram says sent /start)")
		if err := parseInterspersed(fs, args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New(channelUsage)
		}
		return h.channelBind(ctx, fs.Arg(0), *principal, splitList(*events), *label)
	case "unbind":
		if len(args) != 2 {
			return errors.New(channelUsage)
		}
		if err := h.db.RevokeBinding(ctx, args[1], h.action("channel.unbind", map[string]any{"binding_id": args[1]})); err != nil {
			return err
		}
		return h.say("unbound %s", args[1])
	case "subscribe", "unsubscribe":
		if len(args) < 3 {
			return errors.New(channelUsage)
		}
		on := args[0] == "subscribe"
		err := h.db.Subscribe(ctx, args[1], args[2:], on,
			h.action("channel."+args[0], map[string]any{"binding_id": args[1], "events": args[2:]}))
		if err != nil {
			return err
		}
		b, err := h.db.BindingByID(ctx, args[1])
		if err != nil {
			return err
		}
		return h.say("%s receives: %s", b.ID, eventList(b.Events))
	}
	return errors.New(channelUsage)
}

func splitList(s string) []string {
	var out []string
	for x := range strings.SplitSeq(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func eventList(es []string) string {
	if len(es) == 0 {
		return "-"
	}
	return strings.Join(es, ",")
}

// channels opens what sending needs: the key and a dispatcher. Without
// YUUKA_KEK_FILE the dispatcher is locked and sends nothing.
func (h *host) channels() (*notify.Dispatcher, *secret.Sealer, error) {
	sealer, err := h.sealer()
	if err != nil {
		return nil, nil, err
	}
	return notify.NewDispatcher(notify.DispatcherConfig{
		DB: h.db, Sealer: sealer, BaseURL: telegramAPI, Log: slog.New(slog.NewTextHandler(h.msg, nil)),
	}), sealer, nil
}

func (h *host) stdin() io.Reader {
	if h.in == nil {
		return os.Stdin
	}
	return h.in
}

// alert sends a security alert, best effort: the change is made either
// way, and the person at the terminal is told when nobody else was.
func (h *host) alert(ctx context.Context, d *notify.Dispatcher, a notify.Alert) {
	if d.Locked() {
		h.note("no YUUKA_KEK_FILE here: no security alert was sent for this")
		return
	}
	if err := d.Security(ctx, a); err != nil {
		h.note("security alert not sent everywhere: %v", err)
	}
}

// alertWidening sends a security alert for a widening made by a command
// that has no dispatcher of its own (§17.6), best effort like alert.
func (h *host) alertWidening(ctx context.Context, a notify.Alert) {
	d, _, err := h.channels()
	if err != nil {
		h.note("no security alert was sent for this: %v", err)
		return
	}
	h.alert(ctx, d, a)
}

// by names who did something on the host, for alerts.
func (h *host) by() string {
	if h.user == "" {
		return "on the host"
	}
	return "on the host by " + h.user
}

func (h *host) channelAdd(ctx context.Context, name, tokenFile string) error {
	d, sealer, err := h.channels()
	if err != nil {
		return err
	}
	if d.Locked() {
		return errors.New("channel add seals the bot token: set YUUKA_KEK_FILE")
	}
	token, err := h.readBotToken(tokenFile)
	if err != nil {
		return err
	}
	revision := int64(0)
	cur, err := h.db.ChannelByName(ctx, name)
	switch {
	case err == nil:
		revision = cur.Revision
	case !errors.Is(err, store.ErrNotFound):
		return err
	}
	c, bot, err := manage.AddChannel(ctx, h.db, sealer, getMe, name, token, revision, h.action, h.now())
	if err != nil {
		return err
	}
	what := "added"
	if revision != 0 {
		what = "given a new token"
	}
	h.alert(ctx, d, notify.Alert{Text: fmt.Sprintf("yuuka: notification channel %s (bot @%s) was %s %s. "+
		"If this was not you, remove it with `yuuka channel rm %s`.", name, bot.Username, what, h.by(), name)})
	h.note("serve polls it within 15 s; bind a chat with `yuuka channel bind %s`", name)
	return h.say("channel %s: bot @%s (id %d), revision %d", c.Name, bot.Username, bot.ID, c.Revision)
}

// getMe asks the Bot API who token belongs to: how a channel learns its
// bot, here and in the WebUI.
func getMe(ctx context.Context, token string) (notify.Bot, error) {
	return (&notify.Telegram{Token: token, BaseURL: telegramAPI, Client: &http.Client{Timeout: 15 * time.Second}}).GetMe(ctx)
}

// readBotToken reads a bot token from path, or from standard input for "-",
// without its trailing newline.
func (h *host) readBotToken(path string) (string, error) {
	var b []byte
	var err error
	if path == "-" {
		b, err = io.ReadAll(io.LimitReader(h.stdin(), 4096))
	} else {
		b, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("read the token: %w", err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", errors.New("the token file is empty")
	}
	return s, nil
}

func (h *host) channelList(ctx context.Context, all bool) error {
	cs, err := h.db.Channels(ctx, all)
	if err != nil {
		return err
	}
	bs, err := h.db.Bindings(ctx, all)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(h.out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "CHANNEL\tKIND\tBOT\tSTATE\tREVISION\tCREATED")
	for _, c := range cs {
		state := "enabled"
		if c.DisabledAt != nil {
			state = "removed " + stamp(c.DisabledAt)
		}
		bot := c.ExternalID
		if u := c.Settings["username"]; u != "" {
			bot = "@" + u + " (" + c.ExternalID + ")"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", c.Name, c.Kind, bot, state, c.Revision, stamp(&c.CreatedAt))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(bs) == 0 {
		return nil
	}
	_, _ = fmt.Fprintln(h.out)
	w = tabwriter.NewWriter(h.out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "BINDING\tCHANNEL\tPRINCIPAL\tCHAT\tBUTTONS\tEVENTS\tLABEL\tLAST USED\tREVOKED")
	for _, b := range bs {
		buttons := "-"
		if b.ActorID != "" {
			buttons = "user " + b.ActorID
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s:%s\t%s\t%s\t%s\t%s\t%s\t%s\n", b.ID, b.ChannelName, b.PrincipalKind, b.PrincipalName,
			b.Address, buttons, eventList(b.Events), shown(b.Label), stamp(b.LastUsedAt), stamp(b.RevokedAt))
	}
	return w.Flush()
}

func (h *host) channelRemove(ctx context.Context, name string) error {
	c, err := h.db.ChannelByName(ctx, name)
	if err != nil {
		return err
	}
	if err := manage.DisableChannel(ctx, h.db, c, h.action); err != nil {
		return err
	}
	return h.say("removed channel %s: its token is erased and its bindings revoked", name)
}

func (h *host) channelTest(ctx context.Context, name, binding string) error {
	d, _, err := h.channels()
	if err != nil {
		return err
	}
	if d.Locked() {
		return errors.New("channel test opens the bot token: set YUUKA_KEK_FILE")
	}
	n, err := d.Test(ctx, name, binding)
	if n == 0 && err == nil {
		return fmt.Errorf("channel %s has no binding to send to", name)
	}
	if err != nil {
		return fmt.Errorf("sent to %d binding(s); %w", n, err)
	}
	return h.say("sent a test message to %d binding(s)", n)
}

// bindPrincipal is who a binding is for: --principal, else the only
// human.
func (h *host) bindPrincipal(ctx context.Context, s string) (store.Principal, error) {
	if s != "" {
		kind, name, err := parsePrincipal(s)
		if err != nil {
			return store.Principal{}, err
		}
		return h.db.PrincipalByName(ctx, kind, name)
	}
	hs, err := h.db.PrincipalsOfKind(ctx, "human")
	if err != nil {
		return store.Principal{}, err
	}
	if len(hs) != 1 {
		return store.Principal{}, fmt.Errorf("%d humans: say which with --principal human:<name>", len(hs))
	}
	return hs[0], nil
}

// channelBind is the host's side of §13.7: a code, a link to send it to
// the bot with, a wait for serve's poller to record who sent it, and the
// question whether that is the right Telegram identity.
func (h *host) channelBind(ctx context.Context, name, principal string, events []string, label string) error {
	c, err := h.db.ChannelByName(ctx, name)
	if err != nil {
		return err
	}
	if c.DisabledAt != nil {
		return fmt.Errorf("channel %s is removed", name)
	}
	p, err := h.bindPrincipal(ctx, principal)
	if err != nil {
		return err
	}
	code, err := manage.NewBindCode()
	if err != nil {
		return err
	}
	bc, err := h.db.NewBindCode(ctx, c.ID, p.ID, "", code, h.action("channel.bind.begin", manage.BindBeginArgs(name, p)))
	if err != nil {
		return err
	}
	// Ending here any other way than a confirmation drops the code: a
	// /start that comes later then finds nothing.
	bound := false
	defer func() {
		if !bound {
			_ = h.db.DropBindCode(context.WithoutCancel(ctx), bc.ID)
		}
	}()

	if private, group := manage.BindLinks(c.Settings["username"], code); private != "" {
		h.note("Open this link in the Telegram account that should get the notifications:\n\n  %s\n", private)
		h.note("For a group: %s", group)
	} else {
		h.note("Send this to the bot from the Telegram account that should get the notifications:\n\n  /start %s\n", code)
	}
	h.note("Waiting until %s for it (serve must be running, with YUUKA_KEK_FILE)...", bc.ExpiresAt.Local().Format("15:04:05"))

	tick := time.NewTicker(bindPoll)
	defer tick.Stop()
	for bc.RedeemedAt == nil {
		if !h.now().Before(bc.ExpiresAt) {
			return errors.New("the code expired before anyone sent it to the bot")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		if bc, err = h.db.BindCodeByID(ctx, bc.ID); err != nil {
			return err
		}
	}

	kind := bc.SeenChatKind
	if kind == "" {
		kind = "unknown"
	}
	h.note("\nTelegram says the code was sent by:\n\n  %s\n  user id %s, in chat %s (%s)\n", shown(bc.SeenLabel), bc.SeenActorID, bc.SeenAddress, kind)
	h.note("Bind this chat to %s:%s, with that user's button presses counting as theirs? [y/N] ", p.Kind, p.Name)
	answer, _ := bufio.NewReader(h.stdin()).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		return h.say("not bound; the code is void")
	}
	b, err := h.db.ConfirmBindCode(ctx, bc.ID, label, events, h.action("channel.bind.confirm", manage.BindConfirmArgs(name, p, bc, events)))
	if err != nil {
		return err
	}
	bound = true

	d, _, err := h.channels()
	if err != nil {
		h.note("%v", err)
	} else {
		if !d.Locked() {
			if _, err := d.Test(ctx, name, b.ID); err != nil {
				h.note("could not tell the chat: %v", err)
			}
		}
		// Not on the new binding, which may be the attacker's (§13.7).
		h.alert(ctx, d, notify.Alert{Subject: p.ID, Except: b.ID, Text: fmt.Sprintf(
			"yuuka: a Telegram chat (%s, chat %s) was bound to %s:%s on channel %s %s. "+
				"If this was not you, run `yuuka channel unbind %s`.", bc.SeenLabel, bc.SeenAddress, p.Kind, p.Name, name, h.by(), b.ID)})
	}
	return h.say("bound %s: %s:%s on %s, chat %s, receives %s", b.ID, p.Kind, p.Name, name, b.Address, eventList(b.Events))
}
