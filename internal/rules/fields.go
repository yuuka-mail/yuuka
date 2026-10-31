package rules

// Field names of v1. The set is closed: anything else is unsupported.
const (
	FieldFromAddr      = "from.addr"
	FieldFromDomain    = "from.domain"
	FieldFromOrg       = "from.org"
	FieldFromVerified  = "from.verified"
	FieldToAddr        = "to.addr"
	FieldToPrefix      = "to.prefix"
	FieldToZone        = "to.zone"
	FieldPrefixClaimed = "prefix.claimed"
	FieldVerdict       = "verdict"
	FieldSubject       = "subject"
	FieldBody          = "body"
	FieldListID        = "list.id"
	FieldClass         = "class"
	FieldExtraction    = "extraction"
	FieldAttachment    = "attachment"
)

// Operators of v1.
const (
	OpEq       = "eq"
	OpIn       = "in"
	OpContains = "contains"
	OpRegex    = "regex"
	OpHas      = "has"
)

// Enumerated values of v1. They are stored in rules, so they are frozen
// strings rather than borrowed from other packages' String methods.
const (
	ClassMachine = "machine"
	ClassHuman   = "human"
	ClassUnknown = "unknown"

	VerdictLeak  = "leak"
	VerdictSpoof = "spoof"

	ExtractOTP    = "otp"
	ExtractLink   = "link"
	ExtractExpiry = "expiry"
	ExtractAmount = "amount"
)

type fieldType int

const (
	typeText fieldType = iota
	typeBool
	typeEnum // a single value out of fieldSpec.values
	typeSet  // any number of values out of fieldSpec.values
)

type fieldSpec struct {
	typ fieldType
	// addr marks fields whose facts are addresses or address parts. They
	// are folded with FoldAddr on both sides, so values are stored folded.
	addr   bool
	values []string // typeEnum, typeSet
}

var fields = map[string]fieldSpec{
	FieldFromAddr:      {typ: typeText, addr: true},
	FieldFromDomain:    {typ: typeText, addr: true},
	FieldFromOrg:       {typ: typeText, addr: true},
	FieldFromVerified:  {typ: typeBool},
	FieldToAddr:        {typ: typeText, addr: true},
	FieldToPrefix:      {typ: typeText, addr: true},
	FieldToZone:        {typ: typeText, addr: true},
	FieldPrefixClaimed: {typ: typeBool},
	FieldVerdict:       {typ: typeSet, values: []string{VerdictLeak, VerdictSpoof}},
	FieldSubject:       {typ: typeText},
	FieldBody:          {typ: typeText},
	FieldListID:        {typ: typeText},
	FieldClass:         {typ: typeEnum, values: []string{ClassMachine, ClassHuman, ClassUnknown}},
	FieldExtraction:    {typ: typeSet, values: []string{ExtractOTP, ExtractLink, ExtractExpiry, ExtractAmount}},
	FieldAttachment:    {typ: typeBool},
}

// ops lists the operators each field type accepts.
var ops = map[fieldType][]string{
	typeText: {OpEq, OpIn, OpContains, OpRegex},
	typeBool: {OpEq},
	typeEnum: {OpEq, OpIn},
	typeSet:  {OpHas},
}

func (s fieldSpec) allows(op string) bool {
	for _, o := range ops[s.typ] {
		if o == op {
			return true
		}
	}
	return false
}

func (s fieldSpec) known(v string) bool {
	for _, k := range s.values {
		if k == v {
			return true
		}
	}
	return false
}

// Action kinds and their parameters in v1. No kind sends, forwards,
// follows a link or deletes, and none ever will (§8.5): a rule is a
// standing capability nobody re-approves.
const (
	KindLabel    = "label"
	KindClassify = "classify"
	KindArchive  = "archive"
	KindNotify   = "notify"

	NotifyOff    = "off"
	NotifySilent = "silent"
	NotifyNormal = "normal"
)

// actionKeys is the exact member set of each action kind.
var actionKeys = map[string][]string{
	KindLabel:    {"kind", "label"},
	KindClassify: {"kind", "class"},
	KindArchive:  {"kind"},
	KindNotify:   {"kind", "level"},
}
