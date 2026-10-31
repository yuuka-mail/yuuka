package openapi

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

type color string

func (color) Enum() []string { return []string{"red", "blue"} }

type Thing struct {
	Colors []color     `json:"colors"`
	At     *time.Time  `json:"at,omitempty"`
	Parts  []ThingPart `json:"parts"`
	Size   int         `json:"size" maximum:"9"`
}

type ThingPart struct {
	Name string `json:"name"`
}

func TestDocument(t *testing.T) {
	t.Parallel()
	doc, err := Document("t", "1", []Op{{Method: "GET", Path: "/thing", Response: reflect.TypeFor[Thing]()}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	for _, want := range []string{
		`"$ref": "#/components/schemas/ThingPart"`, `"color": {`, `"enum": [`, `"format": "date-time"`,
		`"maximum": 9`, `"required": [`, `"Problem": {`, `"application/problem+json"`, `"bearer": []`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("document lacks %s", want)
		}
	}
	// No null anywhere: arrays are arrays, absent values are omitted.
	if strings.Contains(s, "null") {
		t.Errorf("null in %s", s)
	}

	// Component names are Go type names; two types cannot share one.
	type ThingPart struct{ Other int }
	_, err = Document("t", "1", []Op{
		{Method: "GET", Path: "/a", Response: reflect.TypeFor[Thing]()},
		{Method: "POST", Path: "/b", Body: reflect.TypeFor[ThingPart]()},
	})
	if err == nil || !strings.Contains(err.Error(), "both named ThingPart") {
		t.Errorf("duplicate name: %v", err)
	}
}
