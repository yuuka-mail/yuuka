Golden cases for the v1 rule semantics. **Append-only**: never edit or
delete a case, add new ones. A failing case means the meaning of stored
rules changed, which v1 does not allow; that needs v2 (see the package
documentation of `internal/rules`).

- `eval/*.json`: `{"cases": [{"name", "match", "facts", "want"}]}`.
  The match is loaded as a stored rule would be (no save-time
  validation); `want` is `match`, `no_match` or `unsupported`.
- `combine/*.json`: `{"cases": [{"name", "rules", "facts", "want"}]}`.
  `rules` are `{"id", "priority", "match", "actions"}`; `want` lists the
  effects (`{"rule", "action"}`) and hits (`{"rule", "error"}`, error
  `""`, `unsupported` or `deadline`).

`facts` uses the rule field names as keys. `body` is a string, or an array
of strings and `{"repeat": s, "count": n}` pieces, concatenated, so large
bodies stay readable.
