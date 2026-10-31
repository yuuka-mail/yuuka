// Package rules is the pure core of the rule engine (docs/design/agents.md
// §8.5): the stored rule format v1, its validation, the one evaluator, the
// resolution of several matching rules into one intent, and the
// generalization of an example message into candidate conditions.
//
// It does no I/O. The store loads facts and rules, calls in here, and writes
// the resulting intent through the annotation core; previews and retroactive
// runs use the same evaluator, so there is exactly one implementation of the
// semantics and it is not compiled to SQL.
//
// # The format is a one-way door
//
// A rule is a match tree and a list of actions:
//
//	{"v": 1,
//	 "match": {"all": [
//	   {"field": "from.addr",     "op": "eq", "value": "billing@example.com"},
//	   {"field": "from.verified", "op": "eq", "value": true},
//	   {"any": [{"field": "subject",    "op": "regex", "value": "(?i)invoice|receipt"},
//	            {"field": "extraction", "op": "has",   "value": "amount"}]}]},
//	 "actions": [{"kind": "label", "label": "billing"}, {"kind": "archive"}]}
//
// Once a rule is stored, what it matches must not change under it. So:
//
//   - Adding a field, an operator or an action kind does not bump the
//     version. A binary that does not know one treats the whole rule as
//     unsupported: it never matches and says so (fail closed, also under
//     "not").
//   - Changing what an existing field, operator or action means requires
//     v2, and the v1 evaluator stays forever.
//   - The golden cases under testdata/v1 pin the semantics. They are
//     append-only: never edit or delete a case, add new ones.
//   - Limits (depth, leaf count, value sizes, regex budget) are checked on
//     save only. They may be raised later without a version bump, never
//     lowered, since stored rules must stay valid.
//
// # Mail is untrusted input
//
// Anyone can deliver to a catch-all address, and rules run on every
// delivery. Regexes are RE2 (linear time), size-capped at save and run on a
// bounded window of text; evaluation takes a context whose deadline makes
// the remaining rules fail closed. Actions are a closed set of annotations:
// nothing a rule does can send, forward, follow a link or delete (§8.5).
// Generalization never draws conditions from the subject or body, which
// the sender controls.
package rules
