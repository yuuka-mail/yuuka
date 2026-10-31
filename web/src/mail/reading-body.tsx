import { DownloadIcon, ExternalLinkIcon, PaperclipIcon, QuoteIcon } from "lucide-react";
import { useEffect, useRef } from "react";

import type { Attachment, Block, Span } from "@/api/types";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";

import { claimedHost, linkTarget } from "./links";
import { attachmentHref, type Reading } from "./reading-data";

/**
 * The message text as the structured view (§8.1, §12.1) gives it: blocks
 * with stable anchors, which are their DOM ids, so a cite
 * yuuka://message/<id>#part/1/para/3 opens on its paragraph.
 *
 * Mail is untrusted input (§8.3). Everything here is a React text node;
 * nothing the sender wrote reaches innerHTML. Links keep their real
 * target, open without opener or referrer, and show the host they go to.
 */
export function ReadingBody({
  id,
  uri,
  reading,
  anchor,
  onAnchor,
  loadMore,
  isFetchingMore,
}: {
  /** The message id, for attachment downloads. */
  id: string;
  uri: string;
  reading: Reading;
  anchor?: string;
  onAnchor: (anchor: string) => void;
  loadMore: () => void;
  isFetchingMore: boolean;
}) {
  const { parts, more } = reading;
  const found = !anchor || parts.some((p) => p.anchor === anchor || p.blocks.some((b) => b.anchor === anchor));

  // A deep link to text past the first stretch loads on until it is in.
  useEffect(() => {
    if (!found && more && !isFetchingMore) loadMore();
  }, [found, more, isFetchingMore, loadMore]);

  // Scroll to the anchor once it is rendered, once per anchor: clicking
  // another paragraph's mark should not jump the page.
  const scrolled = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (!anchor || !found || scrolled.current === anchor) return;
    scrolled.current = anchor;
    document.getElementById(anchor)?.scrollIntoView({ block: "center" });
  }, [anchor, found]);

  return (
    <div className="flex max-w-[78ch] flex-col gap-3 text-body">
      {parts.length === 0 && <p className="text-muted-foreground italic">{m.read_no_body()}</p>}
      {parts.map((p) => (
        <section key={p.anchor} id={p.anchor} aria-label={parts.length > 1 ? m.read_part({ section: p.anchor.slice(5) }) : undefined} className="flex flex-col gap-2">
          {parts.length > 1 && (
            <p className="border-b pb-0.5 font-mono text-meta text-muted-foreground">
              {p.anchor} · {p.media_type}
            </p>
          )}
          {p.blocks.map((b) => (
            <BlockView key={b.anchor} block={b} uri={uri} highlight={b.anchor === anchor} onAnchor={onAnchor} />
          ))}
        </section>
      ))}
      {more && (
        <Button variant="outline" size="sm" className="self-start" disabled={isFetchingMore} onClick={loadMore}>
          {isFetchingMore ? m.read_loading() : m.read_load_rest()}
        </Button>
      )}
      {!found && !more && <p className="text-small text-muted-foreground">{m.read_no_anchor({ anchor: anchor ?? "" })}</p>}
      {reading.content.attachments && <Attachments id={id} list={reading.content.attachments} />}
    </div>
  );
}

function BlockView({ block: b, uri, highlight, onAnchor }: { block: Block; uri: string; highlight: boolean; onAnchor: (a: string) => void }) {
  return (
    <div id={b.anchor} lang="" className={cn("group relative -mx-1.5 rounded-sm px-1.5 scroll-mt-4", highlight && "bg-ring/10 ring-1 ring-ring/40")}>
      <button
        type="button"
        aria-label={m.read_link_to({ anchor: b.anchor })}
        title={`${uri}#${b.anchor}`}
        onClick={() => {
          onAnchor(b.anchor);
          void navigator.clipboard?.writeText(`${uri}#${b.anchor}`).catch(() => undefined);
        }}
        className="absolute top-0 -left-4 hidden font-mono text-small text-muted-foreground group-hover:block hover:text-foreground focus-visible:block"
      >
        ¶
      </button>
      <BlockContent b={b} />
    </div>
  );
}

function BlockContent({ b }: { b: Block }) {
  const spans = <Spans spans={b.spans ?? []} />;
  switch (b.kind) {
    case "heading": {
      const level = Math.min(Math.max(b.level ?? 1, 1), 5);
      const H = `h${level + 1}` as "h2" | "h3" | "h4" | "h5" | "h6";
      return <H className={cn("font-semibold break-words", level === 1 ? "pt-2" : "pt-1")}>{spans}</H>;
    }
    case "item":
      return (
        <div className="flex gap-2 break-words" style={{ paddingLeft: `${Math.max((b.level ?? 1) - 1, 0)}rem` }}>
          <span aria-hidden className="text-muted-foreground select-none">
            {(b.level ?? 1) > 1 ? "◦" : "•"}
          </span>
          <span className="min-w-0 whitespace-pre-wrap">{spans}</span>
        </div>
      );
    case "pre":
      return <pre className="overflow-x-auto rounded-sm bg-muted px-2 py-1 font-mono text-small whitespace-pre">{spans}</pre>;
    case "quote":
      return (
        <div className="flex items-baseline gap-1.5 border-l-2 pl-2 text-small text-muted-foreground">
          <QuoteIcon aria-hidden className="size-3 shrink-0 self-center" />
          <span className="min-w-0 break-words">
            {b.spans?.length ? <span className="italic">{spans}</span> : m.read_quoted()}
            {b.lines ? ` · ${m.read_lines_collapsed({ count: b.lines })}` : ""}
          </span>
        </div>
      );
    default:
      return <p className="break-words whitespace-pre-wrap">{spans}</p>;
  }
}

function Spans({ spans }: { spans: Span[] }) {
  return spans.map((s, i) => {
    const t = s.href ? linkTarget(s.href, s.text) : undefined;
    if (!t) return <span key={i}>{s.text}</span>;
    // A link whose text already shows its host, or that has no text and
    // so shows the host as its text, needs no second copy.
    const echoed = !s.text || claimedHost(s.text) === t.shown || s.text.includes(t.shown);
    return (
      <span key={i}>
        <a
          href={t.href}
          title={t.href}
          target="_blank"
          rel="noopener noreferrer"
          referrerPolicy="no-referrer"
          className="text-primary underline decoration-muted-foreground/50 underline-offset-2 hover:decoration-current"
        >
          {s.text || t.shown}
        </a>
        {(t.mismatch || !echoed) && (
          <span
            title={t.mismatch ? m.read_link_mismatch({ host: t.shown }) : m.read_link_goes({ host: t.shown })}
            className={cn("ml-1 inline-flex items-baseline gap-0.5 font-mono text-meta", t.mismatch ? "font-semibold text-destructive" : "text-muted-foreground")}
          >
            <ExternalLinkIcon aria-hidden className="size-3 self-center" />
            {t.shown}
          </span>
        )}
      </span>
    );
  });
}

/**
 * Attachments are plain downloads: no preview, nothing from the file is
 * shown but its declared name and type (§8.3). The server sends every one
 * as Content-Disposition: attachment.
 */
function Attachments({ id, list }: { id: string; list: Attachment[] }) {
  return (
    <section className="mt-1 border-t pt-2 text-small">
      <h2 className="mb-1 font-medium text-muted-foreground">{m.read_attachments({ count: list.length })}</h2>
      <ul className="flex flex-col gap-0.5">
        {list.map((a) => (
          <li key={a.section} className="flex items-baseline gap-2">
            <PaperclipIcon aria-hidden className="size-3 shrink-0 self-center text-muted-foreground" />
            <a
              href={attachmentHref(id, a.section)}
              download
              title={a.filename ? m.read_download({ name: a.filename }) : m.read_download_part({ section: a.section })}
              className="group inline-flex min-w-0 items-baseline gap-1 text-primary underline decoration-muted-foreground/50 underline-offset-2 hover:decoration-current"
            >
              <span className="min-w-0 truncate">{a.filename || m.read_no_filename({ section: a.section })}</span>
              <DownloadIcon aria-hidden className="size-3 shrink-0 self-center text-muted-foreground group-hover:text-foreground" />
            </a>
            <span className="shrink-0 font-mono text-meta text-muted-foreground">{a.media_type}</span>
          </li>
        ))}
      </ul>
    </section>
  );
}
