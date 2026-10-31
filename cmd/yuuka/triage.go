package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/triage"
)

const triageUsage = "usage: yuuka triage status | yuuka triage rescore --since T | yuuka triage report [--since T]"

// probeState is what `triage status` sends to see that the scorer
// answers: no mail, so nothing leaves the deployment for it.
const probeState = "A status check from the mail system yuuka. There is no email here; answer anything."

// triageCmd is `yuuka triage` (§15): whether the scorer answers and how
// far it got, scoring mail again, and how the scores fall.
func triageCmd(ctx context.Context, log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return errors.New(triageUsage)
	}
	tc, err := triage.LoadConfig(os.Getenv, os.ReadFile)
	if err != nil {
		return err
	}
	if !tc.On() {
		return errors.New("YUUKA_TRIAGE_URL is not set: no scorer is configured (docs/operate/config.md)")
	}
	switch args[0] {
	case "status":
		fs := flag.NewFlagSet("triage status", flag.ContinueOnError)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New(triageUsage)
		}
		return withHost(ctx, func(h *host) error { return h.triageStatus(ctx, tc) })
	case "rescore":
		fs := flag.NewFlagSet("triage rescore", flag.ContinueOnError)
		since := fs.String("since", "", "score again mail received at or after this time (RFC 3339 or YYYY-MM-DD); required")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 || *since == "" {
			return errors.New(triageUsage)
		}
		t, err := parseSince(*since)
		if err != nil {
			return err
		}
		return rescore(ctx, log, tc, t)
	case "report":
		fs := flag.NewFlagSet("triage report", flag.ContinueOnError)
		since := fs.String("since", "", "only mail received at or after this time (RFC 3339 or YYYY-MM-DD; default all)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New(triageUsage)
		}
		t, err := parseSince(*since)
		if err != nil {
			return err
		}
		return withHost(ctx, func(h *host) error { return h.triageReport(ctx, tc, t) })
	}
	return errors.New(triageUsage)
}

func (h *host) triageStatus(ctx context.Context, tc triage.Config) error {
	p := tc.Profile()
	_ = h.say("scorer      %s (questions v%d)", p.Scorer, p.Questions)
	_ = h.say("endpoint    %s (%s)", tc.Endpoint, tc.EndpointKind())
	if tc.Model != "" {
		_ = h.say("model asked %s", tc.Model)
	}
	since := h.now().Add(-tc.Lookback)
	c, err := h.db.ScoreCounts(ctx, h.viewer(auth.MessageRead), p, since)
	if err != nil {
		return err
	}
	_ = h.say("lookback    %s: %d scored, %d to score", tc.Lookback, c.Scored, c.Unscored)
	start := time.Now()
	res, err := tc.Client().Score(ctx, triage.Input{State: probeState})
	if err != nil {
		_ = h.say("probe       failed: %v", err)
		return errors.New("the scorer did not answer the probe")
	}
	return h.say("probe       ok in %s, model %s", time.Since(start).Round(time.Millisecond), res.Model)
}

func (h *host) triageReport(ctx context.Context, tc triage.Config, since time.Time) error {
	c, err := h.db.ScoreCounts(ctx, h.viewer(auth.MessageRead), tc.Profile(), since)
	if err != nil {
		return err
	}
	return triage.WriteReport(h.out, tc.Profile(), since, c)
}

// rescore scores the mail since then again with the configured scorer,
// writing new scores beside the old. It reads raw mail, so it needs
// serve's configuration, not just the database.
func rescore(ctx context.Context, log *slog.Logger, tc triage.Config, since time.Time) error {
	a, err := setup(ctx, log, false)
	if err != nil {
		return err
	}
	defer a.close()
	if tc.Hosted {
		log.Warn("triage: the scorer is hosted; the content of every message rescored is sent to a third party")
	}
	r := triage.NewRunner(triage.RunnerConfig{
		Profile: tc.Profile(), Endpoint: tc.EndpointKind(), Scorer: tc.Client(),
		Store: a.db, Raw: a.in, Log: log,
	})
	st, err := r.Rescore(ctx, since)
	log.Info("triage rescore", "scored", st.Scored, "skipped", st.Skipped, "failed", st.Failed)
	if err != nil {
		return err
	}
	if st.Failed > 0 {
		return fmt.Errorf("%d message(s) failed; see log", st.Failed)
	}
	return nil
}
