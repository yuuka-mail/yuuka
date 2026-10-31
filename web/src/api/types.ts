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
export type RecipientState = S["RecipientState"];
export type Resolution = S["Resolution"];
export type Sent = S["Sent"];

// /auth/
export type SessionInfo = S["SessionInfo"];
export type RegisterBegin = S["RegisterBegin"];
export type LoginResult = S["LoginResult"];
export type RegisterResult = S["RegisterResult"];
export type RecoveryGrant = S["RecoveryGrant"];
