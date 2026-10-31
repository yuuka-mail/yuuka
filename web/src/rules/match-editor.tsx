import { PlusIcon, XIcon } from "lucide-react";

import { m } from "@/paraglide/messages.js";

import {
  defaultValue,
  fieldNames,
  fields,
  newID,
  newLeaf,
  opsOf,
  splitValues,
  type EditNode,
  type Field,
  type FieldSpec,
} from "./match";
import { fieldName, opName } from "./words";

// The condition editor for the closed field set (§8.5): groups of all or
// any, conditions of field, operator and value, either negated. A tree
// it cannot hold is edited as JSON (rule-editor.tsx); the server
// validates whatever is saved and says where it is wrong.

const control =
  "h-6 rounded-md border border-input bg-background px-1 text-small outline-none focus-visible:border-ring";

export function MatchEditor({
  value,
  onChange,
}: {
  value: EditNode;
  onChange: (n: EditNode) => void;
}) {
  return <NodeEditor node={value} onChange={onChange} depth={0} />;
}

function NodeEditor({
  node,
  onChange,
  onRemove,
  depth,
}: {
  node: EditNode;
  onChange: (n: EditNode) => void;
  onRemove?: () => void;
  depth: number;
}) {
  const notBox = (
    <label className="flex shrink-0 items-center gap-1 text-muted-foreground">
      <input
        type="checkbox"
        className="accent-current"
        checked={node.not}
        onChange={(e) => onChange({ ...node, not: e.target.checked })}
      />
      {m.rule_not()}
    </label>
  );
  const remove = onRemove && (
    <button
      type="button"
      aria-label={m.rule_remove_condition()}
      className="shrink-0 text-muted-foreground hover:text-foreground"
      onClick={onRemove}
    >
      <XIcon className="size-3.5" />
    </button>
  );
  if (node.kind === "leaf") {
    return (
      <div className="flex flex-wrap items-center gap-1.5">
        {notBox}
        <select
          aria-label={m.rule_field()}
          className={control}
          value={node.field}
          onChange={(e) => {
            const f = e.target.value as Field;
            onChange({ ...newLeaf(f), id: node.id, not: node.not });
          }}
        >
          {fieldNames.map((f) => (
            <option key={f} value={f}>
              {fieldName(f)}
            </option>
          ))}
        </select>
        <select
          aria-label={m.rule_op()}
          className={control}
          value={node.op}
          onChange={(e) =>
            onChange({
              ...node,
              op: e.target.value,
              value: retype(fields[node.field], e.target.value, node.value),
            })
          }
        >
          {opsOf[fields[node.field].type].map((o) => (
            <option key={o} value={o}>
              {opName(o)}
            </option>
          ))}
        </select>
        <ValueEditor
          key={`${node.field} ${node.op}`}
          spec={fields[node.field]}
          op={node.op}
          value={node.value}
          onChange={(value) => onChange({ ...node, value })}
        />
        {remove}
      </div>
    );
  }
  const set = (i: number, n: EditNode) =>
    onChange({ ...node, children: node.children.map((c, j) => (j === i ? n : c)) });
  return (
    <div className="flex flex-col gap-1">
      <div className="flex items-center gap-1.5">
        {depth > 0 && notBox}
        <select
          aria-label={m.rule_group()}
          className={control}
          value={node.kind}
          onChange={(e) => onChange({ ...node, kind: e.target.value as "all" | "any" })}
        >
          <option value="all">{m.rule_all_of()}</option>
          <option value="any">{m.rule_any_of()}</option>
        </select>
        {remove}
      </div>
      <div className="ml-1 flex flex-col gap-1 border-l pl-2.5">
        {node.children.map((c, i) => (
          <NodeEditor
            key={c.id}
            node={c}
            depth={depth + 1}
            onChange={(n) => set(i, n)}
            onRemove={
              node.children.length > 1
                ? () => onChange({ ...node, children: node.children.filter((_, j) => j !== i) })
                : undefined
            }
          />
        ))}
        <div className="flex gap-3 text-muted-foreground">
          <button
            type="button"
            className="inline-flex items-center gap-1 hover:text-foreground"
            onClick={() =>
              onChange({ ...node, children: [...node.children, newLeaf("from.addr")] })
            }
          >
            <PlusIcon className="size-3" />
            {m.rule_add_condition()}
          </button>
          {/* Depth is capped at 4 by the server; groups stop a level short of it. */}
          {depth < 2 && (
            <button
              type="button"
              className="inline-flex items-center gap-1 hover:text-foreground"
              onClick={() =>
                onChange({
                  ...node,
                  children: [
                    ...node.children,
                    {
                      id: newID(),
                      kind: node.kind === "all" ? "any" : "all",
                      not: false,
                      children: [newLeaf("from.addr")],
                    },
                  ],
                })
              }
            >
              <PlusIcon className="size-3" />
              {m.rule_add_group()}
            </button>
          )}
        </div>
      </div>
    </div>
  );
}

/** A value kept where it still fits the new operator, else the default. */
function retype(spec: FieldSpec, op: string, v: unknown): unknown {
  if (op === "in")
    return Array.isArray(v) ? v : typeof v === "string" && v ? [v] : defaultValue(spec, op);
  if (Array.isArray(v)) return v[0] ?? defaultValue(spec, op);
  return v;
}

function ValueEditor({
  spec,
  op,
  value,
  onChange,
}: {
  spec: FieldSpec;
  op: string;
  value: unknown;
  onChange: (v: unknown) => void;
}) {
  if (spec.type === "bool") {
    return (
      <select
        aria-label={m.rule_value()}
        className={control}
        value={String(value === true)}
        onChange={(e) => onChange(e.target.value === "true")}
      >
        <option value="true">{m.rule_yes()}</option>
        <option value="false">{m.rule_no()}</option>
      </select>
    );
  }
  if (spec.values && op === "in") {
    const on = new Set(Array.isArray(value) ? value.map(String) : []);
    return (
      <span className="flex flex-wrap gap-2">
        {spec.values.map((v) => (
          <label key={v} className="flex items-center gap-1 font-mono">
            <input
              type="checkbox"
              className="accent-current"
              checked={on.has(v)}
              onChange={(e) =>
                onChange(spec.values?.filter((x) => (x === v ? e.target.checked : on.has(x))))
              }
            />
            {v}
          </label>
        ))}
      </span>
    );
  }
  if (spec.values) {
    return (
      <select
        aria-label={m.rule_value()}
        className={`${control} font-mono`}
        value={String(value)}
        onChange={(e) => onChange(e.target.value)}
      >
        {spec.values.map((v) => (
          <option key={v} value={v}>
            {v}
          </option>
        ))}
      </select>
    );
  }
  return (
    <input
      aria-label={m.rule_value()}
      className={`${control} min-w-40 flex-1 font-mono`}
      spellCheck={false}
      placeholder={op === "in" ? m.rule_value_list() : op === "regex" ? "(?i)…" : ""}
      // An in list is typed with commas; it is stored as the list.
      defaultValue={Array.isArray(value) ? value.join(", ") : String(value ?? "")}
      onChange={(e) => onChange(op === "in" ? splitValues(e.target.value) : e.target.value)}
    />
  );
}
