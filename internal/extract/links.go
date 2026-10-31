package extract

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/yuuka-mail/yuuka/internal/mimeparse"
)

// Sign-in, verification, confirmation and reset links: the <a href> of
// the HTML parts and the URLs written out in the text. A link is taken
// for what its own words say (85, 90 when its path agrees), for what the
// sentence before it says when that sentence points at it (80, 85), or
// for its path alone in a message whose subject is about signing in (65).
// Unsubscribe, preference, help and "view in browser" links never are,
// and neither is a link whose text names another host than it goes to:
// that is the shape of phishing, and the reading pane flags it (§12.7).

var (
	linkWords = regexp.MustCompile(`(?i)\b(?:verify|verification|confirm|activate|activation|sign[ -]?in|log[ -]?in|reset|` +
		`magic[ -]link|validate|authenticate|unlock|approve)\b` +
		`|验证|驗證|确认|確認|激活|登录|登錄|登入|重置|認証|ログイン|有効化|인증|로그인`)
	linkHere = regexp.MustCompile(`(?i)\b(?:click|tap|follow|use|open|visit|press|go to|copy|paste)\b[^.\n。]{0,60}\b(?:link|button|below|here|url)\b` +
		`|点击|點擊|访问|訪問|打开|打開|下方|以下链接|下面的链接|下面的鏈接|リンク`)
	linkPath = regexp.MustCompile(`(?i)verif|confirm|activat|magic|log-?in|sign-?in|sign_in|signin|reset|auth|token=|otp|passwordless|validate|unlock`)
	linkNot  = regexp.MustCompile(`(?i)unsubscribe|opt-?out|preferences|privacy|\bterms\b|policy|\bhelp\b|support|faq|contact|` +
		`view (?:it |this )?(?:email |message )?(?:in|on) (?:your |a )?(?:browser|web)|web ?version|online version|` +
		`退订|退訂|取消订阅|隐私|隱私|条款|條款|帮助|幫助|客服|配信停止`)
	plainURL  = regexp.MustCompile(`(?i)https?://[^\s<>"'）)\]}，。、]+`)
	imagePath = regexp.MustCompile(`(?i)\.(?:png|jpe?g|gif|svg|webp|ico)$`)
	// claimedHost is the host a link's text names when it reads as a
	// URL or a bare domain (web/src/mail/links.ts does the same).
	claimedHost = regexp.MustCompile(`(?i)^\s*(?:https?://)?((?:[a-z0-9-]+\.)+[a-z]{2,})\.?(?:[:/?#]\S*)?\s*$`)
)

type linkCandidate struct {
	href, text, context string
	src                 Source
}

func links(subject, body string, html []mimeparse.Link) []Extraction {
	var cands []linkCandidate
	shown := map[string]bool{} // URLs that are the text of an <a>
	for _, l := range html {
		cands = append(cands, linkCandidate{l.Href, l.Text, l.Context, HTML})
		shown[strings.TrimSpace(l.Text)] = true
	}
	prev := 0
	for _, m := range plainURL.FindAllStringIndex(body, -1) {
		u := strings.TrimRight(body[m[0]:m[1]], ".,;:!?")
		// Like mimeparse.Link.Context: the text since the previous URL.
		before := body[max(prev, m[0]-4*160):m[0]]
		prev = m[1]
		if shown[u] {
			// The text of a link, which may go elsewhere: the HTML
			// candidate stands for it.
			continue
		}
		cands = append(cands, linkCandidate{u, "", before, Body})
	}
	aboutLinks := linkWords.MatchString(subject)
	var out []Extraction
	for _, c := range cands {
		if n := linkScore(c, aboutLinks); n > 0 {
			out = append(out, Extraction{Kind: Link, Value: c.href, Confidence: n, Source: c.src})
		}
	}
	return out
}

func linkScore(c linkCandidate, aboutLinks bool) int {
	u, err := url.Parse(c.href)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return 0
	}
	where := u.EscapedPath() + "?" + u.RawQuery
	if linkNot.MatchString(c.text) || linkNot.MatchString(where) || imagePath.MatchString(u.Path) {
		return 0
	}
	if m := claimedHost.FindStringSubmatch(c.text); m != nil {
		claimed, host := strings.ToLower(m[1]), strings.ToLower(u.Hostname())
		if claimed != host && !strings.HasSuffix(host, "."+claimed) && !strings.HasSuffix(claimed, "."+host) {
			return 0
		}
	}
	path := linkPath.MatchString(where)
	plus := func(n int) int {
		if path {
			return n + 5
		}
		return n
	}
	ctx := c.context
	if r := []rune(ctx); len(r) > 160 {
		ctx = string(r[len(r)-160:])
	}
	// The link's text says nothing of its own: "here", the URL itself.
	bare := c.text == "" || claimedHost.MatchString(c.text) || len([]rune(c.text)) <= 12
	switch {
	case linkWords.MatchString(c.text):
		return plus(85)
	case linkWords.MatchString(ctx) && bare && (linkHere.MatchString(ctx) || path):
		return plus(80)
	case path && aboutLinks:
		return 65
	}
	return 0
}

// Host is the host a link goes to, lowercased, for showing next to it;
// "" if raw is not an http or https URL. International names stay as
// written: the WebUI shows the punycode the browser resolves.
func Host(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
