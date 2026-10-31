import { useQueryClient } from "@tanstack/react-query";
import { ArrowLeftIcon, PlusIcon } from "lucide-react";
import { useState } from "react";

import { api, data } from "@/api/client";
import { invalidateAnnotated, useRule, useRules } from "@/api/queries";
import type { Rule, RuleWarning } from "@/api/types";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Signature, system } from "@/components/voice";
import { fullTime, listTime } from "@/lib/format";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";
import { Warnings } from "@/rules/preview";
import { RetroActions } from "@/rules/retro";
import { RuleEditor } from "@/rules/rule-editor";
import { ActionsWords } from "@/rules/words";
import { ProblemLine } from "@/send/marks";
import { problem, type Abilities } from "@/send/outgoing";
import { settingsSections, type SettingsSection, type View } from "@/mail/view";

import { CredentialsSection } from "./credentials";
import { GrantsSection } from "./grants";
import { IdentitiesSection } from "./identities";

// Settings (§12.12): the one page that does not follow a message,
// entered from the account menu, never from the sidebar. It holds what
// has no message of its own: rules, then who may do what (§13.10) —
// credentials, identities and grants, which only a person's own session
// with admin sees, to look at and tighten. Each section is listed only
// when the session may see it.

const sectionTitle: Record<SettingsSection, () => string> = {
  rules: m.settings_rules,
  credentials: m.settings_credentials,
  identities: m.settings_identities,
  grants: m.settings_grants,
};

/** The settings sections the session may see, in the page's order. */
export function settingsFor(can: Abilities): SettingsSection[] {
  return settingsSections.filter((s) => (s === "rules" ? can.rules : can.authority));
}

export function SettingsPage({ view, can, go, onOpenMessage }: { view: View; can: Abilities; go: (next: Partial<View>, replace?: boolean) => void; onOpenMessage: (id: string) => void }) {
  const sections = settingsFor(can);
  const section = view.settings && sections.includes(view.settings) ? view.settings : sections[0];
  return (
    <div className={cn("grid min-h-0 flex-1 grid-cols-1 md:grid-cols-[13.5rem_1fr] md:grid-rows-1", view.rule ? "grid-rows-1" : "grid-rows-[auto_1fr]")}>
      <nav aria-label={m.settings_title()} className={cn("min-h-0 flex-col border-b bg-sidebar py-2 text-ui md:border-r md:border-b-0", view.rule ? "hidden md:flex" : "flex")}>
        <div className="flex items-center gap-1 px-1.5 pb-1">
          <Button variant="ghost" size="sm" onClick={() => go({ settings: undefined, rule: undefined })}>
            <ArrowLeftIcon data-icon="inline-start" />
            {m.settings_back()}
          </Button>
        </div>
        <h1 className="mt-1 mb-1 px-3 text-meta font-medium tracking-wide text-muted-foreground uppercase">{m.settings_title()}</h1>
        {sections.map((s) => (
          <button
            key={s}
            type="button"
            aria-current={section === s || undefined}
            onClick={() => go({ settings: s, rule: undefined })}
            className={cn("mx-1.5 flex h-7 items-center rounded-md px-2 text-left hover:bg-muted", section === s && "bg-accent text-accent-foreground hover:bg-accent")}
          >
            {sectionTitle[s]()}
          </button>
        ))}
        {sections.length === 0 && <p className="px-3 text-small text-muted-foreground">{m.settings_nothing()}</p>}
      </nav>
      <main className="min-h-0 min-w-0 overflow-y-auto">
        {section === "rules" && <RulesSection view={view} go={go} onOpenMessage={onOpenMessage} />}
        {section === "credentials" && <CredentialsSection />}
        {section === "identities" && <IdentitiesSection />}
        {section === "grants" && <GrantsSection />}
      </main>
    </div>
  );
}

function RulesSection({ view, go, onOpenMessage }: { view: View; go: (next: Partial<View>, replace?: boolean) => void; onOpenMessage: (id: string) => void }) {
  const [deleted, setDeleted] = useState(false);
  const q = useRules(deleted);
  if (view.rule) {
    return <RuleDetail key={view.rule} id={view.rule} onBack={() => go({ rule: undefined }, true)} onOpenMessage={onOpenMessage} onCreated={(id) => go({ rule: id }, true)} />;
  }
  return (
    <div className="flex max-w-[64rem] flex-col gap-3 px-4 py-4 md:px-6">
      <header className="flex flex-wrap items-center gap-2">
        <h2 className="text-title font-semibold">{m.settings_rules()}</h2>
        <Button size="sm" variant="outline" className="ml-auto" onClick={() => go({ rule: "new" })}>
          <PlusIcon data-icon="inline-start" />
          {m.rules_new()}
        </Button>
      </header>
      <p className="text-small text-muted-foreground">{m.rules_hint()}</p>
      <label className="flex items-center gap-1.5 self-start text-small text-muted-foreground">
        <input type="checkbox" className="accent-current" checked={deleted} onChange={(e) => setDeleted(e.target.checked)} />
        {m.rules_show_deleted()}
      </label>
      {q.error ? (
        <ProblemLine p={problem(q.error)} />
      ) : !q.data ? (
        <Skeleton className="h-24 w-full" />
      ) : q.data.rules.length === 0 ? (
        <p className="text-muted-foreground">{m.rules_none()}</p>
      ) : (
        <ul aria-label={m.settings_rules()} className="flex flex-col border-t">
          {q.data.rules.map((r) => (
            <RuleRow key={r.id} r={r} onOpen={() => go({ rule: r.id })} />
          ))}
        </ul>
      )}
    </div>
  );
}

function RuleRow({ r, onOpen }: { r: Rule; onOpen: () => void }) {
  const off = !r.enabled || !!r.deleted_at;
  return (
    <li>
      <button type="button" onClick={onOpen} className={cn("flex min-h-14 w-full flex-col justify-center gap-0.5 border-b px-2 py-1.5 text-left hover:bg-muted", off && "text-muted-foreground")}>
        <span className="flex min-w-0 items-center gap-2 text-ui">
          <span className="w-8 shrink-0 text-meta text-muted-foreground tabular-nums" title={m.rule_priority()}>
            {r.priority}
          </span>
          <span className="min-w-0 truncate font-medium">{r.name || m.rules_unnamed()}</span>
          <RuleState r={r} />
          {r.unsupported?.length ? <span className="shrink-0 text-meta font-medium text-warning">{m.rules_unsupported()}</span> : null}
          {r.proposal_id && <span className="shrink-0 text-meta text-agent-foreground">{m.rules_from_proposal()}</span>}
          <span className="ml-auto shrink-0 text-meta text-muted-foreground tabular-nums">{m.rules_revision({ n: String(r.revision) })}</span>
        </span>
        <span className="truncate pl-10 text-small">
          <ActionsWords actions={r.actions} />
        </span>
      </button>
    </li>
  );
}

function RuleState({ r }: { r: Rule }) {
  if (r.deleted_at) return <span className="shrink-0 text-meta">{m.rules_deleted({ when: listTime(r.deleted_at) })}</span>;
  return <span className={cn("shrink-0 text-meta", r.enabled ? "text-system-foreground" : "text-muted-foreground")}>{r.enabled ? m.rules_enabled() : m.rules_disabled()}</span>;
}

/** One rule: its editor, and what can be done to it and to the mail it touched. */
function RuleDetail({ id, onBack, onOpenMessage, onCreated }: { id: string; onBack: () => void; onOpenMessage: (id: string) => void; onCreated: (id: string) => void }) {
  const qc = useQueryClient();
  const isNew = id === "new";
  const q = useRule(isNew ? undefined : id);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [err, setErr] = useState<unknown>();
  // Kept here: saving remounts the editor on the new revision.
  const [warnings, setWarnings] = useState<RuleWarning[]>();
  const r = q.data;
  const refresh = () => {
    invalidateAnnotated(qc);
    void qc.invalidateQueries({ queryKey: ["get", "/api/v1/rules/{id}"] });
  };
  const header = (
    <header className="flex flex-wrap items-center gap-2">
      <Button variant="ghost" size="sm" onClick={onBack}>
        <ArrowLeftIcon data-icon="inline-start" />
        {m.settings_rules()}
      </Button>
      <h2 className="min-w-0 truncate text-title font-semibold">{isNew ? m.rules_new() : r ? r.name || m.rules_unnamed() : ""}</h2>
      {r && <RuleState r={r} />}
    </header>
  );
  if (isNew) {
    return (
      <div className="flex max-w-[64rem] flex-col gap-3 px-4 py-4 md:px-6">
        {header}
        <RuleEditor
          initial={{ name: "", priority: 0, enabled: true, actions: [] }}
          submitLabel={m.rules_create()}
          onOpen={onOpenMessage}
          onSubmit={async (body) => {
            const saved = await data(api.POST("/api/v1/rules", { body }));
            refresh();
            onCreated(saved.id);
            return saved.warnings;
          }}
        />
      </div>
    );
  }
  if (q.error) return <div className="p-6">{header}<ProblemLine p={problem(q.error)} /></div>;
  if (!r) return <Skeleton className="m-6 h-40" />;
  const put = async (patch: { disabled?: boolean }) => {
    setErr(undefined);
    try {
      await data(api.PUT("/api/v1/rules/{id}", { params: { path: { id } }, body: { name: r.name, priority: r.priority, disabled: !r.enabled, match: r.match, actions: r.actions, ...patch } }));
      refresh();
    } catch (e) {
      setErr(e);
    }
  };
  return (
    <div className="flex max-w-[64rem] flex-col gap-3 px-4 py-4 md:px-6">
      {header}
      <section className="flex flex-col gap-1 rounded-md border border-system-border bg-system-surface px-2 py-1.5 text-small">
        <p className="flex flex-wrap items-center gap-x-2">
          <Signature speaker={system} />
          <span>{m.rules_facts({ revision: String(r.revision), created: fullTime(r.created_at), updated: fullTime(r.updated_at) })}</span>
          {r.proposal_id && <span className="text-agent-foreground">{m.rules_from_proposal_id({ id: r.proposal_id })}</span>}
        </p>
        {r.unsupported?.length ? (
          <p className="text-warning">
            {m.rules_unsupported_why()} <span className="font-mono">{r.unsupported.join("; ")}</span>
          </p>
        ) : null}
        <p className="text-muted-foreground">{m.rules_arrival_only()}</p>
      </section>
      {!r.deleted_at && (
        <div className="flex flex-wrap items-center gap-2">
          <Button size="xs" variant="outline" onClick={() => void put({ disabled: r.enabled })}>
            {r.enabled ? m.rules_disable() : m.rules_enable()}
          </Button>
          {confirmDelete ? (
            <>
              <Button
                size="xs"
                variant="destructive"
                onClick={async () => {
                  try {
                    await api.DELETE("/api/v1/rules/{id}", { params: { path: { id } } });
                    refresh();
                    setConfirmDelete(false);
                  } catch (e) {
                    setErr(e);
                  }
                }}
              >
                {m.rules_delete_confirm()}
              </Button>
              <Button size="xs" variant="ghost" onClick={() => setConfirmDelete(false)}>
                {m.rule_cancel()}
              </Button>
              <span className="text-small text-muted-foreground">{m.rules_delete_cost()}</span>
            </>
          ) : (
            <Button size="xs" variant="ghost" onClick={() => setConfirmDelete(true)}>
              {m.rules_delete()}
            </Button>
          )}
        </div>
      )}
      <section className="flex flex-col gap-1.5 border-t pt-2">
        <h3 className="text-small font-medium">{m.rules_past_mail()}</h3>
        <RetroActions id={id} enabled={r.enabled && !r.deleted_at && !r.unsupported?.length} />
      </section>
      {err !== undefined && <ProblemLine p={problem(err)} />}
      {!r.deleted_at && (
        <section className="flex flex-col gap-1.5 border-t pt-2">
          <h3 className="text-small font-medium">{m.rules_edit()}</h3>
          {warnings && (
            <div className="text-small">
              <Warnings warnings={warnings} />
            </div>
          )}
          <RuleEditor
            key={r.revision}
            initial={{ name: r.name, priority: r.priority, enabled: r.enabled, match: r.match, actions: r.actions }}
            submitLabel={m.rules_save()}
            onOpen={onOpenMessage}
            onSubmit={async (body) => {
              const saved = await data(api.PUT("/api/v1/rules/{id}", { params: { path: { id } }, body }));
              setWarnings(saved.warnings);
              refresh();
            }}
          />
        </section>
      )}
    </div>
  );
}
