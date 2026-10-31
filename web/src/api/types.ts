// Names for the schemas of openapi.json (schema.gen.ts, generated from
// the Go types; see docs/design/architecture.md §5.1). Only aliases: a shape
// that is not in the contract does not belong here.

import type { components, paths } from "./schema.gen";

type S = components["schemas"];

export type Problem = S["Problem"];

export type Class = S["Class"];
export type MessageSummary = S["MessageSummary"];
export type Extracted = S["Extracted"];
export type Extraction = S["Extraction"];
export type MessagePage = S["MessagePage"];
export type Message = S["Message"];
export type Delivery = S["Delivery"];
export type Thread = S["Thread"];
export type Prefix = S["Prefix"];
export type PrefixList = S["PrefixList"];
export type PrefixDetail = S["PrefixDetail"];
export type ObservedDomain = S["ObservedDomain"];
export type Offense = S["Offense"];
export type LeakCheck = S["LeakCheck"];
export type LeakVerdict = S["LeakVerdict"];
export type SearchResult = S["SearchResult"];

/** The query of GET /api/v1/messages. */
export type MessageListQuery = NonNullable<paths["/api/v1/messages"]["get"]["parameters"]["query"]>;

// The structured view of §8.1, the tree get_message gives agents.
export type View = S["View"];
export type Level = S["Level"];
export type Facts = S["Facts"];
export type Auth = S["Auth"];
export type Stripped = S["Stripped"];
export type Content = S["Content"];
export type Address = S["Address"];
export type Attachment = S["Attachment"];
export type Section = S["Section"];
export type Part = S["Part"];
export type Kind = S["Kind"];
export type Block = S["Block"];
export type Span = S["Span"];

// Sending (M2, §8.9, §12.10).
export type Code = S["Code"];
export type Actor = S["Actor"];
export type Draft = S["Draft"];
export type DraftBody = S["DraftBody"];
export type DraftState = S["DraftState"];
export type Cite = S["Cite"];
export type SubmitResult = S["SubmitResult"];
export type HoldReason = S["HoldReason"];
export type Tier = S["Tier"];
export type SendingIdentity = S["SendingIdentity"];
export type ReplyIdentity = S["ReplyIdentity"];
export type Outgoing = S["Outgoing"];
export type OutboxState = S["OutboxState"];
export type OutboxEntry = S["OutboxEntry"];
export type Recipient = S["Recipient"];
export type RecipientState = S["RecipientState"];
export type Resolution = S["Resolution"];
export type Sent = S["Sent"];
export type RecipientScope = S["RecipientScope"];
export type Limits = S["Limits"];

// The review tier and its recall window (§8.9).
export type Review = S["Review"];
export type ReviewCheck = S["ReviewCheck"];
export type CheckName = S["CheckName"];
export type Withdrawal = S["Withdrawal"];
export type Suspension = S["Suspension"];

// Notes (§8.10).
export type Note = S["Note"];
export type NoteBody = S["NoteBody"];

// Management (§13.10): decisions prompted by mail, and who may do what.
export type PrefixDecision = S["PrefixDecision"];
export type ExpectResult = S["ExpectResult"];
export type CountedAs = S["CountedAs"];
export type ManagedCredential = S["ManagedCredential"];
export type ManagedIdentity = S["ManagedIdentity"];
export type ManagedGrant = S["ManagedGrant"];
export type AppliedGrant = S["AppliedGrant"];
export type TightenBody = S["TightenBody"];
export type IssueBody = S["IssueBody"];
export type IssuedCredential = S["IssuedCredential"];
export type IdentityBody = S["IdentityBody"];
export type IdentityAdminList = S["IdentityAdminList"];
export type GrantBody = S["GrantBody"];
export type StoredGrant = S["StoredGrant"];
export type Scope = S["Scope"];

// Annotations, rules and proposals (M3, §8.5). A rule's match is typed by
// hand in rules/match.ts: the contract says only "object" because the
// tree is recursive.
export type Annotation = S["Annotation"];
export type AnnotateKind = S["AnnotateKind"];
export type AnnotateOutcome = S["AnnotateOutcome"];
export type AnnotationOutcome = S["AnnotationOutcome"];
export type AnnotateResult = S["AnnotateResult"];
export type MessageOutcomes = S["MessageOutcomes"];
export type AnnotationEntry = S["AnnotationEntry"];
export type WriterRank = S["WriterRank"];
export type UndoResult = S["UndoResult"];
export type LabelCount = S["LabelCount"];
export type RuleAction = S["RuleAction"];
export type RuleBody = S["RuleBody"];
export type Rule = S["Rule"];
export type SavedRule = S["SavedRule"];
export type RuleWarning = S["RuleWarning"];
export type RuleDraft = S["RuleDraft"];
export type RuleCandidate = S["RuleCandidate"];
export type RulePreview = S["RulePreview"];
export type RetroStats = S["RetroStats"];
export type RevertStats = S["RevertStats"];
export type Proposal = S["Proposal"];
export type ProposalState = S["ProposalState"];
export type AcceptResult = S["AcceptResult"];

// Triage (§15): a scorer's estimates, which act on nothing.
export type TriageScore = S["TriageScore"];
export type TriageAnswer = S["TriageAnswer"];
export type TriageInput = S["TriageInput"];
export type TriageReport = S["TriageReport"];

// /auth/
export type SessionInfo = S["SessionInfo"];
export type RegisterBegin = S["RegisterBegin"];
export type LoginResult = S["LoginResult"];
export type RegisterResult = S["RegisterResult"];
export type ElevateResult = S["ElevateResult"];
export type RecoveryGrant = S["RecoveryGrant"];
