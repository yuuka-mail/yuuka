package extract

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// How long a code or link lasts, from a relative phrase: "expires in 10
// minutes", "valid for 24 hours", "10分钟内有效", "有效期为30分钟",
// "10分以内". Absolute times ("valid until 15:30") are left alone: the
// time zone they mean is a guess.
//
// The phrase counts from the earlier of the Date header and the time the
// message reached yuuka. Date is the sender's and may be forged; taking
// the earlier one can only shorten what is shown as still valid, never
// stretch it.

var (
	expiryEN = regexp.MustCompile(`(?i)\b(?:expires?|expiring|expiry|expiration|valid|good|active|usable|works?)\b` +
		`[^.\n。]{0,40}?\b(?:in|for|within|after)\s*:?\s*(?:the\s+next\s+|only\s+|up\s+to\s+)?` +
		`(\d{1,3}|an?|one|two|three|four|five|six|ten|fifteen|twenty|thirty|sixty|twenty[- ]four|forty[- ]eight|seventy[- ]two)` +
		`\s*-?\s*(seconds?|secs?|minutes?|mins?|hours?|hrs?|days?)\b`)
	expiryZH = regexp.MustCompile(`(\d{1,3}|[一二两兩三四五六七八九十]{1,3})\s*(秒钟|秒鐘|秒|分钟|分鐘|小时|小時|天)\s*` +
		`(?:之内|之內|以内|以內|内|內|后|後)?\s*(?:有效|失效|过期|過期)`)
	expiryZH2 = regexp.MustCompile(`有效期\s*(?:为|為|是)?\s*[:：]?\s*(\d{1,3}|[一二两兩三四五六七八九十]{1,3})\s*(秒钟|秒鐘|秒|分钟|分鐘|小时|小時|天)`)
	expiryJA  = regexp.MustCompile(`(\d{1,3})\s*(分|時間|日)\s*(?:間)?\s*(?:以内|有効)`)
	// expiryAbout: the phrase is about a code or a link, not a trial.
	expiryAbout = regexp.MustCompile(`(?i)\b(?:code|pin|link|otp|passcode|password)s?\b|码|碼|链接|鏈接|リンク|コード|パスワード|번호`)
)

const (
	minExpiry = 30 * time.Second
	maxExpiry = 30 * 24 * time.Hour
)

func expiry(texts []text, date, receivedAt time.Time) (Extraction, bool) {
	base := receivedAt
	if !date.IsZero() && date.Before(base) {
		base = date
	}
	for _, t := range texts {
		type hit struct {
			at int
			d  time.Duration
		}
		var first *hit
		for _, re := range []*regexp.Regexp{expiryEN, expiryZH, expiryZH2, expiryJA} {
			for _, m := range re.FindAllStringSubmatchIndex(t.s, -1) {
				d, ok := duration(t.s[m[2]:m[3]], t.s[m[4]:m[5]])
				if !ok || d < minExpiry || d > maxExpiry {
					continue
				}
				if first == nil || m[0] < first.at {
					first = &hit{m[0], d}
				}
				break
			}
		}
		if first == nil {
			continue
		}
		n := 70
		// A cut inside a rune at either end reads as U+FFFD; harmless.
		if expiryAbout.MatchString(t.s[max(0, first.at-3*120):min(len(t.s), first.at+3*120)]) {
			n = 85
		}
		return Extraction{Kind: Expiry, Value: iso(first.d), Confidence: n, Source: t.src, Expires: base.Add(first.d)}, true
	}
	return Extraction{}, false
}

var numberWords = map[string]int{
	"a": 1, "an": 1, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "ten": 10, "fifteen": 15,
	"twenty": 20, "thirty": 30, "sixty": 60, "twenty-four": 24, "twenty four": 24, "forty-eight": 48, "forty eight": 48,
	"seventy-two": 72, "seventy two": 72,
}

func duration(num, unit string) (time.Duration, bool) {
	n, err := strconv.Atoi(num)
	if err != nil {
		var ok bool
		if n, ok = numberWords[strings.ToLower(num)]; !ok {
			if n, ok = chineseNumber(num); !ok {
				return 0, false
			}
		}
	}
	u := strings.ToLower(unit)
	var per time.Duration
	switch {
	case strings.HasPrefix(u, "sec"), strings.HasPrefix(u, "秒"):
		per = time.Second
	case strings.HasPrefix(u, "min"), strings.HasPrefix(u, "分"):
		per = time.Minute
	case strings.HasPrefix(u, "h"), u == "小时", u == "小時", u == "時間":
		per = time.Hour
	case strings.HasPrefix(u, "day"), u == "天", u == "日":
		per = 24 * time.Hour
	default:
		return 0, false
	}
	return time.Duration(n) * per, true
}

// chineseNumber reads 一 to 九十九.
func chineseNumber(s string) (int, bool) {
	digit := map[rune]int{'一': 1, '二': 2, '两': 2, '兩': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
	r := []rune(s)
	switch {
	case len(r) == 1 && r[0] == '十':
		return 10, true
	case len(r) == 1:
		d, ok := digit[r[0]]
		return d, ok
	case len(r) == 2 && r[0] == '十':
		d, ok := digit[r[1]]
		return 10 + d, ok
	case len(r) == 2 && r[1] == '十':
		d, ok := digit[r[0]]
		return d * 10, ok
	case len(r) == 3 && r[1] == '十':
		a, ok1 := digit[r[0]]
		b, ok2 := digit[r[2]]
		return a*10 + b, ok1 && ok2
	}
	return 0, false
}

// iso writes d as an ISO 8601 duration in its largest whole unit, days
// from two on: "24 hours" stays PT24H.
func iso(d time.Duration) string {
	switch {
	case d >= 48*time.Hour && d%(24*time.Hour) == 0:
		return fmt.Sprintf("P%dD", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("PT%dH", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("PT%dM", d/time.Minute)
	}
	return fmt.Sprintf("PT%dS", d/time.Second)
}
