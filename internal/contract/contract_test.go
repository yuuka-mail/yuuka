package contract_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/contract"
)

// The vectors are shared with the Worker; expected values live only there.
var vectorDir = filepath.Join("..", "..", "testdata", "contract", "v1")

func load(t *testing.T, name string, v any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(vectorDir, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// vectorMarker is the "marker" object of markers.json, kept in its JSON
// shape so the comparison does not depend on contract's own decoding.
type vectorMarker struct {
	V            int    `json:"v"`
	ID           string `json:"id"`
	ReceivedAt   string `json:"received_at"`
	EnvelopeFrom string `json:"envelope_from"`
	EnvelopeTo   string `json:"envelope_to"`
	RawKey       string `json:"raw_key"`
	RawSize      int64  `json:"raw_size"`
}

func TestMarkers(t *testing.T) {
	var vec struct {
		Valid []struct {
			Name   string       `json:"name"`
			JSON   string       `json:"json"`
			Marker vectorMarker `json:"marker"`
		} `json:"valid"`
		Invalid []struct {
			Name   string `json:"name"`
			JSON   string `json:"json"`
			Reason string `json:"reason"`
		} `json:"invalid"`
	}
	load(t, "markers.json", &vec)
	if len(vec.Valid) == 0 || len(vec.Invalid) == 0 {
		t.Fatal("empty vector file")
	}

	for _, tc := range vec.Valid {
		t.Run("valid/"+tc.Name, func(t *testing.T) {
			m, err := contract.Parse([]byte(tc.JSON))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			got := vectorMarker{
				V:            contract.Version,
				ID:           m.ID,
				ReceivedAt:   contract.FormatTime(m.ReceivedAt),
				EnvelopeFrom: m.EnvelopeFrom,
				EnvelopeTo:   m.EnvelopeTo,
				RawKey:       m.RawKey,
				RawSize:      m.RawSize,
			}
			if got != tc.Marker {
				t.Fatalf("Parse = %+v, want %+v", got, tc.Marker)
			}

			// Serializing must reproduce the wire bytes whenever the vector
			// carries no fields beyond the v1 set.
			out := contract.Marshal(m)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.JSON), &fields); err != nil {
				t.Fatal(err)
			}
			if len(fields) == 7 && string(out) != tc.JSON {
				t.Fatalf("Marshal =\n%s\nwant\n%s", out, tc.JSON)
			}
			again, err := contract.Parse(out)
			if err != nil || again != m {
				t.Fatalf("round trip: %+v, %v", again, err)
			}
		})
	}
	for _, tc := range vec.Invalid {
		t.Run("invalid/"+tc.Name, func(t *testing.T) {
			if m, err := contract.Parse([]byte(tc.JSON)); err == nil {
				t.Fatalf("Parse accepted %+v; want rejection (%s)", m, tc.Reason)
			}
		})
	}
}

func TestKeys(t *testing.T) {
	var vec []struct {
		Name       string `json:"name"`
		ID         string `json:"id"`
		ReceivedAt string `json:"received_at"`
		RawKey     string `json:"raw_key"`
		PendingKey string `json:"pending_key"`
	}
	load(t, "keys.json", &vec)
	for _, tc := range vec {
		t.Run(tc.Name, func(t *testing.T) {
			at, err := contract.ParseTime(tc.ReceivedAt)
			if err != nil {
				t.Fatal(err)
			}
			if got := contract.RawKey(at, tc.ID); got != tc.RawKey {
				t.Errorf("RawKey = %s, want %s", got, tc.RawKey)
			}
			if got := contract.PendingKey(tc.ID); got != tc.PendingKey {
				t.Errorf("PendingKey = %s, want %s", got, tc.PendingKey)
			}
			if id, ok := contract.IDFromPendingKey(tc.PendingKey); !ok || id != tc.ID {
				t.Errorf("IDFromPendingKey = %s, %v", id, ok)
			}
		})
	}
}

func TestMetadata(t *testing.T) {
	var vec []struct {
		Name        string `json:"name"`
		MarkerJSON  string `json:"marker_json"`
		YuukaMarker string `json:"yuuka_marker"`
	}
	load(t, "metadata.json", &vec)
	for _, tc := range vec {
		t.Run(tc.Name, func(t *testing.T) {
			if got := contract.EncodeMetadata([]byte(tc.MarkerJSON)); got != tc.YuukaMarker {
				t.Errorf("EncodeMetadata = %s, want %s", got, tc.YuukaMarker)
			}
			dec, err := contract.DecodeMetadata(tc.YuukaMarker)
			if err != nil {
				t.Fatal(err)
			}
			if string(dec) != tc.MarkerJSON {
				t.Errorf("DecodeMetadata = %s, want %s", dec, tc.MarkerJSON)
			}
			if _, err := contract.Parse(dec); err != nil {
				t.Errorf("decoded metadata does not parse: %v", err)
			}
		})
	}
}

func TestHMAC(t *testing.T) {
	var vec struct {
		WindowSeconds int64 `json:"window_seconds"`
		Sign          []struct {
			Name      string `json:"name"`
			Secret    string `json:"secret"`
			Timestamp int64  `json:"timestamp"`
			Body      string `json:"body"`
			Signature string `json:"signature"`
		} `json:"sign"`
		Verify []struct {
			Name      string   `json:"name"`
			Secrets   []string `json:"secrets"`
			Now       int64    `json:"now"`
			Timestamp int64    `json:"timestamp"`
			Body      string   `json:"body"`
			Signature string   `json:"signature"`
			Valid     bool     `json:"valid"`
		} `json:"verify"`
	}
	load(t, "hmac.json", &vec)
	if got := int64(contract.SignatureWindow / time.Second); got != vec.WindowSeconds {
		t.Fatalf("SignatureWindow = %ds, vectors say %ds", got, vec.WindowSeconds)
	}
	for _, tc := range vec.Sign {
		t.Run("sign/"+tc.Name, func(t *testing.T) {
			if got := contract.Sign([]byte(tc.Secret), tc.Timestamp, []byte(tc.Body)); got != tc.Signature {
				t.Errorf("Sign = %s, want %s", got, tc.Signature)
			}
		})
	}
	for _, tc := range vec.Verify {
		t.Run("verify/"+tc.Name, func(t *testing.T) {
			secrets := make([][]byte, len(tc.Secrets))
			for i, s := range tc.Secrets {
				secrets[i] = []byte(s)
			}
			err := contract.Verify(secrets, time.Unix(tc.Now, 0), strconv.FormatInt(tc.Timestamp, 10), []byte(tc.Body), tc.Signature)
			if (err == nil) != tc.Valid {
				t.Errorf("Verify valid=%v (err=%v), want %v", err == nil, err, tc.Valid)
			}
		})
	}
}

func TestVerifyRejectsMalformedTimestamp(t *testing.T) {
	secret := []byte("test-secret-0123456789abcdef0123456789")
	body := []byte("{}")
	now := time.Unix(1793410898, 0)
	for _, ts := range []string{"", "+1793410898", " 1793410898", "1793410898.0", "-1"} {
		// Sign the malformed string itself, so only the timestamp check can
		// reject it.
		h := hmac.New(sha256.New, secret)
		h.Write([]byte("v1:" + ts + ":"))
		h.Write(body)
		sig := "v1=" + hex.EncodeToString(h.Sum(nil))
		if err := contract.Verify([][]byte{secret}, now, ts, body, sig); err == nil {
			t.Errorf("timestamp %q accepted", ts)
		}
	}
}

func TestMarshalEscapesLikeJSONStringify(t *testing.T) {
	m := contract.Marker{
		ID:           "01M67SX66B38NKRKAYDXR834N3",
		ReceivedAt:   time.Date(2026, 10, 31, 1, 41, 38, 123e6, time.UTC),
		EnvelopeFrom: "a&b<c>\"\\\x01 @example.net",
		EnvelopeTo:   "x@example.com",
		RawSize:      0,
	}
	m.RawKey = contract.RawKey(m.ReceivedAt, m.ID)
	got := string(contract.Marshal(m))
	want := `{"v":1,"id":"01M67SX66B38NKRKAYDXR834N3","received_at":"2026-10-31T01:41:38.123Z",` +
		`"envelope_from":"a&b<c>\"\\\u0001` + " " + `@example.net","envelope_to":"x@example.com",` +
		`"raw_key":"raw/2026/10/01M67SX66B38NKRKAYDXR834N3.eml","raw_size":0}`
	if got != want {
		t.Fatalf("Marshal =\n%s\nwant\n%s", got, want)
	}
	back, err := contract.Parse([]byte(got))
	if err != nil || back != m {
		t.Fatalf("round trip: %+v, %v", back, err)
	}
}
