#!/usr/bin/env python3
"""
Dashboard Config Anonymizer for 3x-ui-exporter

Transforms a standard dashboard JSON (v2 Scenes or Classic) into an obfuscated version
where all metrics, legends, tables, and tabs are natively anonymized:
- UID, name, and title set to 3x-ui-exporter-demo.
- Inbounds (Throughput, Traffic, Admin state) -> Inbound-1, Inbound-2 (no ports leaked).
- Outbounds -> direct, outbound-1 .. outbound-6, others.
  Uses a two-phase label_replace -> sum/max by (outbound_anon) -> outbound pipeline
  so Prometheus NEVER fails with 'vector cannot contain metrics with the same labelset'
  and NO raw tag can ever leak.
- Connected nodes -> node-1, node-2, node-3 (no country codes or host provider names).
- Balancers -> single clean tab 'balancer-1' with zero warnings or duplicate series.

Usage:
    python3 scripts/anonymize-dashboard.py | pbcopy
    python3 scripts/anonymize-dashboard.py grafana-3x-ui.v2.json | pbcopy
    python3 scripts/anonymize-dashboard.py grafana-3x-ui.classic.json | pbcopy
"""

import sys
import json
from pathlib import Path

# Rules mapping raw outbound tags to outbound-1 .. outbound-9 via intermediate label outbound_anon.
# Even if multiple raw outbounds match the same bucket, sum/max by (outbound_anon) cleanly
# aggregates them without ever causing duplicate labelset errors in Prometheus.
OUTBOUND_ANON_RULES = [
    ("(?i).*(?:[-_]|^)ch$", "1"),
    ("(?i).*(?:[-_]|^)(?:cz|panel)$", "2"),
    ("(?i).*(?:[-_]|^)(?:ee|awg)$", "3"),
    ("(?i).*(?:[-_]|^)(?:fi|fl|1)$", "4"),
    ("(?i).*(?:[-_]|^)(?:lu|nike)$", "5"),
    ("(?i).*(?:[-_]|^)ru$", "6"),
    ("(?i).*(?:[-_]|^)de$", "7"),
    ("(?i).*(?:[-_]|^)nl$", "8"),
    ("(?i).*(?:[-_]|^)us$", "9"),
]

COUNTRY_NODE_MAP = [
    ("ru", "1"), ("ch", "2"), ("ee", "3"), ("de", "4"),
    ("nl", "5"), ("fi", "6"), ("us", "7"), ("cz", "8"),
    ("lu", "9"), ("se", "10"), ("fr", "11"), ("gb", "12"),
    ("kz", "13"), ("tr", "14"), ("sg", "15"), ("jp", "16"),
]


def anonymize_inbound_query(expr: str) -> str:
    """Replaces the 'remark' label with clean numbered identifiers Inbound-1, Inbound-2, etc."""
    if "label_replace" in expr and '"remark"' in expr:
        return expr
    return (
        "label_replace(\n"
        "  label_replace(\n"
        "    label_replace(\n"
        f"      ({expr}),\n"
        '      "remark", "Inbound-1", "inbound", ".*"\n'
        "    ),\n"
        '    "remark", "Inbound-2", "inbound", "(?i).*(?:[0-9]{3,}|19458).*"\n'
        "  ),\n"
        '  "remark", "Inbound-1", "inbound", "(?i).*(?:[-_]1[-_]|[-_]1$|in-1-).*"\n'
        ")"
    )


def anonymize_outbound_query(expr: str, agg: str = "max", sort_desc_wrap: bool = False) -> str:
    """
    Anonymizes outbound series using a collision-proof 4-step pipeline:
    1. Write anonymized name to a new label 'outbound_anon' (keeping original 'outbound' intact
       so labelsets remain 100% unique during all label_replace calls; includes catch-all default).
    2. Aggregate with sum/max by (outbound_anon) to collapse any series sharing a bucket.
    3. Copy 'outbound_anon' back to 'outbound' (safe because 'outbound_anon' is now unique).
    4. Aggregate with sum/max by (outbound) to drop the temporary 'outbound_anon' label.
    """
    if "outbound_anon" in expr:
        return expr

    # Step 1: catch-all default so no raw outbound tag can ever leak
    cur = (
        "label_replace(\n"
        f"  ({expr}),\n"
        '  "outbound_anon", "outbound-6", "outbound", ".*"\n'
        ")"
    )
    for regex, num in OUTBOUND_ANON_RULES:
        cur = (
            "label_replace(\n"
            f"  {cur},\n"
            f'  "outbound_anon", "outbound-{num}", "outbound", "{regex}"\n'
            ")"
        )

    # Steps 2-4: aggregate by outbound_anon -> copy to outbound -> aggregate by outbound
    pipeline = (
        f"{agg} by (outbound) (\n"
        "  label_replace(\n"
        f"    {agg} by (outbound_anon) (\n"
        f"      {cur}\n"
        "    ),\n"
        '    "outbound", "$1", "outbound_anon", "(.+)"\n'
        "  )\n"
        ")"
    )
    if sort_desc_wrap:
        return f"sort_desc(\n  {pipeline}\n)"
    return pipeline


def anonymize_node_query(expr: str) -> str:
    """Replaces node names with clean numbered identifiers node-1, node-2, etc. (no country codes)."""
    if 'node", "node-1"' in expr or 'node", "node-2"' in expr:
        return expr
    cur = f"({expr})"
    for code, num in COUNTRY_NODE_MAP:
        cur = (
            "label_replace(\n"
            f"  {cur},\n"
            f'  "node", "node-{num}", "node", "(?i)(?:.*[-_])?{code}$"\n'
            ")"
        )
    cur = (
        "label_replace(\n"
        f"  {cur},\n"
        '  "node", "node-$1", "node", "^(?:.*[-_])?([0-9]+)$"\n'
        ")"
    )
    return cur


def patch_dashboard(data: dict) -> dict:
    demo_id = "3x-ui-exporter-demo"

    # 0. Set demo title, name, and distinct UID
    if "metadata" in data and isinstance(data["metadata"], dict):
        data["metadata"]["name"] = demo_id
        data["metadata"]["uid"] = demo_id

    if "spec" in data and isinstance(data["spec"], dict):
        # v2 Scenes
        data["spec"]["title"] = demo_id
        data["spec"]["name"] = demo_id
        data["spec"]["uid"] = demo_id

    if "title" in data:
        # Classic
        data["title"] = demo_id
        data["name"] = demo_id
        data["uid"] = demo_id

    # 1. Variables: make balancer single-select to prevent duplicate series when tab repetition is removed
    def patch_variables(obj):
        if isinstance(obj, dict):
            if obj.get("name") == "balancer":
                obj["multi"] = False
                obj["includeAll"] = False
            for v in obj.values():
                patch_variables(v)
        elif isinstance(obj, list):
            for it in obj:
                patch_variables(it)

    patch_variables(data)

    # 2. Patch Balancers tab titles in v2 layout & rows in Classic
    def patch_layout_titles(obj):
        if isinstance(obj, dict):
            # v2 TabsLayoutTab
            if obj.get("kind") == "TabsLayoutTab":
                spec = obj.get("spec", {})
                if spec.get("title") == "$balancer":
                    spec["title"] = "balancer-1"
                if "repeat" in spec:
                    del spec["repeat"]

            # Classic row titles
            if obj.get("type") == "row":
                title = obj.get("title", "")
                if "$balancer" in title:
                    obj["title"] = title.replace("$balancer", "balancer-1")
                if obj.get("repeat") == "balancer":
                    del obj["repeat"]

            for v in obj.values():
                patch_layout_titles(v)
        elif isinstance(obj, list):
            for it in obj:
                patch_layout_titles(it)

    patch_layout_titles(data)

    # 3. Patch Panel Queries and Legends
    def walk_panels(obj, current_panel_title=""):
        if isinstance(obj, dict):
            title = (
                obj.get("title")
                or (obj.get("spec", {}).get("title") if isinstance(obj.get("spec"), dict) else None)
                or current_panel_title
            )
            expr = obj.get("expr")
            legend = obj.get("legendFormat")

            if expr and isinstance(expr, str):
                # A. Inbounds section queries: wrap and rewrite 'remark' label to Inbound-1, Inbound-2
                if (
                    "xui_inbound_traffic_bytes_total" in expr
                    and "group_left(remark" in expr
                ) or (
                    "xui_inbound_enabled" in expr
                    and 'node="local"' in expr
                ):
                    obj["expr"] = anonymize_inbound_query(expr)

                # B. Outbound throughput (panel-9):
                elif "Outbound throughput" in title and "xui_outbound_traffic_bytes_total" in expr:
                    if legend == "{{outbound}}" and 'outbound="direct"' not in expr:
                        obj["expr"] = anonymize_outbound_query(expr, agg="sum")

                # C. Outbound traffic distribution (panel-10):
                elif "Traffic distribution" in title and "xui_outbound_traffic_bytes_total" in expr:
                    if legend == "{{outbound}}" and 'outbound="direct"' not in expr:
                        obj["expr"] = anonymize_outbound_query(expr, agg="sum", sort_desc_wrap=True)

                # D. Balancer availability: panel-25 (aggregate BEFORE label_replace so it never collides)
                elif ("xui_balancer_outbound_available" in expr or "xui_balancer_state" in expr) and (
                    legend == "{{balancer}}" or "balancer" in (legend or "")
                ):
                    obj["legendFormat"] = "balancer-1"
                    if "label_replace" not in expr:
                        obj["expr"] = (
                            "label_replace(\n"
                            f"  max by (instance) ({expr}),\n"
                            '  "balancer", "balancer-1", "instance", ".*"\n'
                            ")"
                        )

                # E. Outbound selected time (in Balancers):
                elif (
                    "xui_balancer_outbound_selected" in expr
                    and "role!~" in expr
                    and legend == "{{outbound}}"
                ):
                    obj["expr"] = anonymize_outbound_query(expr, agg="max", sort_desc_wrap=True)

                # Outbound status summary table:
                elif (
                    ("Status" in title or "Outbound status" in title)
                    and "xui_observatory_outbound_alive" in expr
                ):
                    obj["expr"] = anonymize_outbound_query(expr, agg="max")

                # F. Outbound health & selection:
                elif (
                    "xui_balancer_outbound_selected" in expr
                    and "xui_observatory_outbound" in expr
                ):
                    obj["expr"] = anonymize_outbound_query(expr, agg="max")

                # G. Outbound observatory metrics (Latency, Deviations, Probes):
                elif (
                    "xui_observatory_outbound" in expr
                    and legend == "{{outbound}}"
                ):
                    obj["expr"] = anonymize_outbound_query(expr, agg="max")

                # H. Outbound quality & health table queries (panel-73):
                elif (
                    "Outbound quality & health" in title
                    and "xui_observatory_outbound" in expr
                ):
                    obj["expr"] = anonymize_outbound_query(expr, agg="max")

                # I. Connected nodes - Availability history:
                elif "xui_node_online" in expr and legend == "{{node}}":
                    obj["expr"] = anonymize_node_query(expr)

                # J. Connected nodes - Latency & Resources:
                elif (
                    "xui_node_latency_seconds" in expr
                    or "xui_node_cpu_percent" in expr
                    or "xui_node_memory_percent" in expr
                ):
                    obj["expr"] = anonymize_node_query(expr)

                # K. Connected nodes - Inventory table queries:
                elif (
                    "xui_node_xray_state" in expr
                    or "xui_node_uptime_seconds" in expr
                    or ("count by (node)" in expr and "xui_inbound_enabled" in expr)
                    or "xui_node_info" in expr
                    or "xui_node_online_clients" in expr
                ):
                    obj["expr"] = anonymize_node_query(expr)

                # L. Connected nodes - Throughput & Traffic volume:
                elif "xui_inbound_bytes_total" in expr and 'node!="local"' in expr:
                    obj["expr"] = anonymize_node_query(expr)

            for k, v in obj.items():
                walk_panels(v, title)

        elif isinstance(obj, list):
            for item in obj:
                walk_panels(item, current_panel_title)

    walk_panels(data)
    return data


def main():
    src_arg = sys.argv[1] if len(sys.argv) > 1 else "grafana-3x-ui.v2.json"
    dst_arg = sys.argv[2] if len(sys.argv) > 2 else "-"

    # Read input
    if src_arg == "-":
        data = json.load(sys.stdin)
    else:
        src_path = Path(src_arg)
        if not src_path.exists():
            print(f"Error: input file not found: {src_path}", file=sys.stderr)
            sys.exit(1)
        with open(src_path, "r", encoding="utf-8") as f:
            data = json.load(f)

    # Anonymize
    data = patch_dashboard(data)

    # Output pure JSON
    if dst_arg == "-":
        json.dump(data, sys.stdout, indent=2, ensure_ascii=False)
        sys.stdout.write("\n")
    else:
        dst_path = Path(dst_arg)
        with open(dst_path, "w", encoding="utf-8") as f:
            json.dump(data, f, indent=2, ensure_ascii=False)
            f.write("\n")
        print(f"[SUCCESS] Obfuscated dashboard saved to {dst_path}", file=sys.stderr)


if __name__ == "__main__":
    main()
