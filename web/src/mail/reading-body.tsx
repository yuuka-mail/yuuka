import type { Message } from "@/api/types.gen";

/**
 * The message body. M1 renders body_text as plain text: mail is untrusted
 * input (§8.3), and text in a text node can do nothing. The structured
 * view and, in M2, the sandboxed iframe replace this component.
 */
export function ReadingBody({ message }: { message: Message }) {
  if (!message.body_text.trim()) {
    return <p className="text-sm text-muted-foreground italic">No text body.</p>;
  }
  return <div className="max-w-[78ch] text-sm leading-relaxed break-words whitespace-pre-wrap">{message.body_text}</div>;
}
