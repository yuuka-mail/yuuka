package extract

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// One-time codes. A candidate is a run of ASCII letters and digits that
// looks like a code (4-8 digits, or 4-10 capitals and digits with both),
// and it scores by the words around it:
//
//	95  a code word right before it ("verification code: 482913",
//	    "验证码是 739201"), or right after ("482913 is your code")
//	90  a code word earlier on the same line, ending in ":" or "is"
//	80  a code word within the window before it, alone on its line
//	65  a code word within the window before it
//	60  alone on its line in a message that names a code somewhere
//
// A weak word ("code", "PIN") scores 10 less: it also names promo codes
// and confirmation numbers. Capitals-and-digits codes, which booking
// references look like, are taken only after a strong word in the first
// two tiers. A
// number that belongs to a longer number, a date, a time, a sum or an
// amount of some unit is never a candidate; one with an order, phone or
// account word between it and the code word is dropped.

// strongCue names a one-time code in the languages mail to yuuka comes
// in. \b is ASCII-only in RE2, so the non-Latin alternatives go without.
var strongCue = regexp.MustCompile(`(?i)` +
	`\b(?:verification|verify|security|log-?in|log in|sign-?in|sign in|sign-?up|sign up|one-?time|one time|single-use|` +
	`authentication|auth|2fa|two-factor|mfa|otp|temporary|passcode)[ -]?(?:code|pin|passcode)s?\b` +
	`|\bpasscodes?\b|\bone[- ]time pass(?:word|code)s?\b|\botps?\b` +
	`|验证码|校验码|驗證碼|动态码|動態碼|动态密码|确认码|安全码|登录码|登陆码|认证码|一次性密码` +
	`|認証コード|確認コード|認証番号|確認番号|ワンタイムパスワード|セキュリティコード` +
	`|인증\s?번호|인증\s?코드` +
	`|code de (?:vérification|confirmation|sécurité)|bestätigungscode|sicherheitscode|verifizierungscode` +
	`|código de (?:verificación|seguridad|verificação)|codice di verifica`)

// weakCue is a bare "code" or "PIN"; the word before it may make it a
// different kind of code (weakNot).
var (
	weakCue = regexp.MustCompile(`(?i)\b(?:code|pin)s?\b`)
	weakNot = regexp.MustCompile(`(?i)\b(?:promo|promotional|discount|coupon|zip|postal|post|error|status|source|country|area|` +
		`tracking|referral|invite|invitation|gift|voucher|product|qr|bar|html|sample|reference|booking|tax|sort|swift|bank|` +
		`colou?r|reason|response|exit|dress|redeem|redemption|activation|licen[cs]e|serial|door|gate|locker|pickup|pick-up|` +
		`airline|reservation|orders?|invoice|account|item|sku)[ -]?$`)
)

// negCue says that a nearby number is something other than a code.
var negCue = regexp.MustCompile(`(?i)\b(?:orders?|invoices?|receipts?|accounts?|acct|ref|reference|tickets?|case|tracking|` +
	`transactions?|txn|booking|reservation|members?|membership|customer|serial|model|phone|tel|telephone|fax|mobile|cell|` +
	`call|hotline|zip|postal|amount|total|price|balance|subtotal|payments?|card|ending|ext|extension|room|flight|seat|` +
	`suite|apt|number|id|version|build|issue|commit)\b|\bno\.|#` +
	`|订单|单号|运单|快递|物流|工单|账号|帳號|帐号|账户|卡号|尾号|电话|電話|手机|热线|客服|致电|拨打|联系|邮编|优惠|邀请|兑换|取件|提取码|` +
	`金额|余额|合计|总计|价格|实付|编号|地址|房间|航班|二维码`)

// urlish masks the spans whose digits belong to an address, never a code.
var urlish = regexp.MustCompile(`(?i)\b(?:https?://|www\.)[^\s<>"']+|[^\s<>"'@]+@[^\s<>"'@]+\.[a-z]{2,}`)

// The connectors between a code word and its code.
var (
	adjacentGap = regexp.MustCompile(`(?i)^(?:[\s:：=>»\-–—"'“”‘’「」『』【】\[\]()（）*]|is|was|are|will|be|below|here|as|follows|following|est|ist|lautet|es|é|为|是|如下|：)*$`)
	sameLineGap = regexp.MustCompile(`(?i)(?:[:：=]|\bis|\bwas|\best|\bist|为|是)[\s"'“「【\[(（*]*$`)
	afterGap    = regexp.MustCompile(`(?i)^(?:(?:is|was)\b|是|为|就是|[(（【\[]|$)`)
)

// currencyBefore and unitAfter mark sums, dates and quantities.
var (
	currencyBefore = regexp.MustCompile(`(?i)(?:\b(?:rmb|usd|cny|eur|gbp|jpy|hkd|aud|cad|chf|sgd)|us\$|hk\$|[$€£¥￥₩₹])\s*$`)
	unitAfter      = regexp.MustCompile(`(?i)^\s?(?:(?:minutes?|mins?|seconds?|secs?|hours?|hrs?|days?|weeks?|months?|years?|` +
		`kb|mb|gb|tb|km|kg|ms|usd|cny|rmb|eur|gbp|jpy|pcs|items?|points?|pts)\b|分钟|分鐘|分|秒|小时|小時|時間|天|日|月|年|号|號|元|块|圆|円|位|个|個|次|条|件|%|％)`)
)

const (
	window      = 120 // characters before a candidate that a cue may sit in
	afterWindow = 40  // after it, for "482913 is your code"
	// maxTokens bounds the cues, candidates and masks read from one
	// text, so hostile mail costs at most maxTokens² comparisons.
	maxTokens = 256
)

type span struct{ start, end int }

type cue struct {
	span
	strong bool
}

type candidate struct {
	span
	value   string
	letters bool
}

func otps(texts []text) []Extraction {
	var out []Extraction
	named := false // a strong cue anywhere in the message
	cues := make([][]cue, len(texts))
	negs := make([][]span, len(texts))
	masks := make([][]span, len(texts))
	for i, t := range texts {
		masks[i] = spans(urlish.FindAllStringIndex(t.s, maxTokens))
		cues[i], negs[i] = findCues(t.s, masks[i])
		for _, c := range cues[i] {
			named = named || c.strong
		}
	}
	for i, t := range texts {
		cands := candidates(t.s, masks[i])
		// Scoring compares every candidate with every cue; mail with
		// more of either than this is not a code mail.
		for _, c := range cands[:min(len(cands), maxTokens)] {
			if n := score(t.s, c, cues[i], negs[i], named); n > 0 {
				out = append(out, Extraction{Kind: OTP, Value: c.value, Confidence: n, Source: t.src})
			}
		}
	}
	return out
}

func spans(idx [][]int) []span {
	out := make([]span, len(idx))
	for i, m := range idx {
		out[i] = span{m[0], m[1]}
	}
	return out
}

func inside(masks []span, s span) bool {
	for _, m := range masks {
		if s.start < m.end && m.start < s.end {
			return true
		}
	}
	return false
}

// findCues returns the code words and the not-a-code words of s, in
// order. A weak cue made specific by the word before it ("promo code")
// is a negative one.
func findCues(s string, masks []span) ([]cue, []span) {
	var cues []cue
	negs := spans(negCue.FindAllStringIndex(s, maxTokens))
	strong := spans(strongCue.FindAllStringIndex(s, maxTokens))
	for _, m := range strong {
		if !inside(masks, m) {
			cues = append(cues, cue{m, true})
		}
	}
	for _, m := range spans(weakCue.FindAllStringIndex(s, maxTokens)) {
		if inside(masks, m) || inside(strong, m) {
			continue
		}
		if weakNot.MatchString(s[max(0, m.start-24):m.start]) {
			negs = append(negs, m)
			continue
		}
		cues = append(cues, cue{m, false})
	}
	return cues, slices.DeleteFunc(negs, func(n span) bool { return inside(masks, n) })
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }
func isAlnum(b byte) bool { return isDigit(b) || b|0x20 >= 'a' && b|0x20 <= 'z' }

// candidates returns the runs of s that could be codes and are not part
// of something else. Two runs of three or four digits joined by one
// space or hyphen ("482 913", "4829-1374") are one code.
func candidates(s string, masks []span) []candidate {
	var runs []span
	for i := 0; i < len(s); {
		if !isAlnum(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && isAlnum(s[j]) {
			j++
		}
		runs = append(runs, span{i, j})
		i = j
	}
	digits := func(r span) bool {
		for k := r.start; k < r.end; k++ {
			if !isDigit(s[k]) {
				return false
			}
		}
		return true
	}
	// joined: run a and b are digit runs one space or hyphen apart.
	joined := func(a, b int) bool {
		if a < 0 || b >= len(runs) || !digits(runs[a]) || !digits(runs[b]) || runs[b].start-runs[a].end != 1 {
			return false
		}
		c := s[runs[a].end]
		return c == ' ' || c == '-'
	}
	var out []candidate
	for i := 0; i < len(runs); i++ {
		r := runs[i]
		value := s[r.start:r.end]
		// A grouped code, unless the groups go on (a phone number).
		if joined(i, i+1) && !joined(i-1, i) && !joined(i+1, i+2) {
			a, b := r.end-r.start, runs[i+1].end-runs[i+1].start
			if a == b && (a == 3 || a == 4) {
				out = appendCandidate(out, s, span{r.start, runs[i+1].end}, value+s[runs[i+1].start:runs[i+1].end], masks)
				i++
				continue
			}
		}
		// Digits in a chain of digit groups: a phone number, a date.
		if digits(r) && (joined(i-1, i) || joined(i, i+1)) {
			continue
		}
		out = appendCandidate(out, s, r, value, masks)
	}
	return out
}

func appendCandidate(out []candidate, s string, r span, value string, masks []span) []candidate {
	c := candidate{span: r, value: value}
	var lower, upper, digit int
	for i := 0; i < len(value); i++ {
		switch b := value[i]; {
		case isDigit(b):
			digit++
		case b >= 'a':
			lower++
		default:
			upper++
		}
	}
	switch {
	case lower == 0 && upper == 0 && len(value) >= 4 && len(value) <= 8:
	case lower == 0 && upper > 0 && digit > 0 && len(value) >= 4 && len(value) <= 10:
		c.letters = true
	default:
		return out
	}
	if inside(masks, r) || partOfSomething(s, r) {
		return out
	}
	return append(out, c)
}

// partOfSomething reports whether the run r of s is a piece of a longer
// token: a decimal, a date, a time, a hyphenated identifier, a path, a
// sum, a quantity.
func partOfSomething(s string, r span) bool {
	p1, n1 := lastRune(s[:r.start])
	p2, _ := lastRune(s[:r.start-n1])
	q1, m1 := firstRune(s[r.end:])
	q2, _ := firstRune(s[r.end+m1:])
	joiner := func(c rune) bool { return strings.ContainsRune(".,:/-–", c) }
	alnum := func(c rune) bool { return c < utf8.RuneSelf && isAlnum(byte(c)) }
	switch {
	case joiner(p1) && alnum(p2) && (p1 != '-' || !singleLetter(s, r.start-1)):
		return true // 2026-11-01, 3.14, 10:30, ABCD-1234; but G-482913
	case joiner(q1) && alnum(q2):
		return true
	case strings.ContainsRune("#$€£¥￥+@%&=?_/\\~", p1), strings.ContainsRune("%％@_/\\&=", q1):
		return true
	}
	if currencyBefore.MatchString(s[max(0, r.start-8):r.start]) {
		return true
	}
	return unitAfter.MatchString(s[r.end:min(len(s), r.end+16)])
}

// singleLetter: the byte before i is one capital letter standing alone,
// the "G-" in front of some providers' codes.
func singleLetter(s string, i int) bool {
	if i < 1 || s[i-1] < 'A' || s[i-1] > 'Z' {
		return false
	}
	return i < 2 || !isAlnum(s[i-2])
}

func lastRune(s string) (rune, int) {
	if s == "" {
		return 0, 0
	}
	return utf8.DecodeLastRuneInString(s)
}

func firstRune(s string) (rune, int) {
	if s == "" {
		return 0, 0
	}
	return utf8.DecodeRuneInString(s)
}

// score rates candidate c of s against the cues around it; 0 drops it.
func score(s string, c candidate, cues []cue, negs []span, named bool) int {
	year := false
	if !c.letters && len(c.value) == 4 {
		n, _ := strconv.Atoi(c.value)
		year = n >= 1900 && n <= 2099
	}
	weak := func(cu cue, n int) int {
		if cu.strong {
			return n
		}
		return n - 10
	}
	best := 0
	for _, cu := range cues {
		var n int
		switch {
		case cu.end <= c.start:
			gap := s[cu.end:c.start]
			runes := utf8.RuneCountInString(gap)
			switch {
			case runes <= 30 && strings.Count(gap, "\n") <= 3 && adjacentGap.MatchString(gap):
				n = weak(cu, 95)
			case runes <= 50 && !strings.ContainsAny(gap, "\n.。!！?？;；") && sameLineGap.MatchString(gap):
				n = weak(cu, 90)
			case runes <= window && !c.letters && !year && !negBetween(negs, cu.end, c.start):
				n = weak(cu, 65)
				if alone(s, c.span) {
					n = weak(cu, 80)
				}
				if negAfter(s, negs, c.end) {
					n = 0
				}
			}
		case cu.start >= c.end:
			gap := s[c.end:cu.start]
			if utf8.RuneCountInString(gap) <= afterWindow && !strings.Contains(gap, "\n") &&
				afterGap.MatchString(strings.TrimLeft(gap, " \t\"'”」】])）*,，:：")) {
				n = weak(cu, 95)
			}
		}
		if c.letters && !cu.strong {
			n = 0 // "Confirmation code: K7QX2P" is a booking
		}
		best = max(best, n)
	}
	if best == 0 && named && !c.letters && !year && alone(s, c.span) && !negBetween(negs, max(0, c.start-60), c.start) {
		best = 60
	}
	if len(c.value) == 4 && best < 90 {
		best -= 5 // four digits are a year, a PIN pad, a room...
	}
	return best
}

// negBetween reports a not-a-code word inside [from, to).
func negBetween(negs []span, from, to int) bool {
	for _, n := range negs {
		if n.start >= from && n.end <= to {
			return true
		}
	}
	return false
}

// negAfter reports a not-a-code word right after position i on its line:
// "12345678 is your order number".
func negAfter(s string, negs []span, i int) bool {
	for _, n := range negs {
		if n.start >= i && n.start-i <= 30 && !strings.Contains(s[i:n.start], "\n") {
			return true
		}
	}
	return false
}

// alone reports whether r is all there is on its line, give or take
// punctuation and spaces: the big code in the middle of an HTML mail.
func alone(s string, r span) bool {
	start := strings.LastIndexByte(s[:r.start], '\n') + 1
	end := strings.IndexByte(s[r.end:], '\n')
	if end < 0 {
		end = len(s)
	} else {
		end += r.end
	}
	trim := func(x string) string {
		return strings.TrimFunc(x, func(c rune) bool { return unicode.IsSpace(c) || unicode.IsPunct(c) })
	}
	return trim(s[start:r.start]) == "" && trim(s[r.end:end]) == ""
}
