"""Migrate the user's Grafana v2 dashboard without changing the original."""
import copy
import json
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parent
SOURCE = Path('/Users/akhnin/.codex/attachments/e22fe5e0-e06e-4346-b78d-4495dcb3c5c5/Вставленный текст.txt')
d = json.loads(SOURCE.read_text())
d['metadata'] = {'name': '3x-ui-exporter', 'namespace': 'default'}
s = d['spec']
s['title'] = '3x-ui'
s['tags'] = ['3x-ui', 'xui-exporter']
s['timeSettings'].update({'from': 'now-30m', 'to': 'now', 'autoRefresh': '10s'})
elements = s['elements']
rows = s['layout']['spec']['rows']
FILTER = 'job="xui-exporter",instance="${instance}"'
mapping = {
    'xray_lb_up': 'xui_balancer_outbound_available',
    'xray_lb_outbound_selected': 'xui_balancer_outbound_selected',
    'xray_observatory_alive': 'xui_observatory_outbound_alive',
    'xray_observatory_delay_milliseconds': 'xui_observatory_outbound_delay_seconds',
    'xray_observatory_ping_average_seconds': 'xui_observatory_outbound_rtt_mean_seconds',
    'xray_observatory_ping_min_seconds': 'xui_observatory_outbound_rtt_min_seconds',
    'xray_observatory_ping_max_seconds': 'xui_observatory_outbound_rtt_max_seconds',
    'xray_observatory_ping_deviation_seconds': 'xui_observatory_outbound_rtt_deviation_seconds',
    'xray_observatory_ping_failed_samples': 'xui_observatory_outbound_probe_failed_samples',
    'xray_observatory_ping_samples': 'xui_observatory_outbound_probe_samples',
}

def migrate(value):
    if isinstance(value, dict):
        return {k: migrate(v) for k, v in value.items()}
    if isinstance(value, list):
        return [migrate(v) for v in value]
    if isinstance(value, str):
        for before, after in mapping.items():
            value = value.replace(before, after)
        value = value.replace('job="xray-balancers",', '').replace('job="xray-expvars",', '')
        value = value.replace('$outbound', '${outbound}').replace('$balancer', '${balancer}')
        value = value.replace(":raw}", "}").replace(":regex}", "}")
        def selector(match):
            body = match[2].strip().strip(',')
            return match[1] + '{' + FILTER + (',' + body if body else '') + '}'
        value = re.sub(r'(xui_[a-z_]+)\{((?:[^{}]|\$\{[^}]+\})*)\}', selector, value)
        return value
    return value

# Variables are row scoped in the user's v2 dashboard.
for row in rows:
    spec = row['spec']
    if spec['title'] == 'Burst observatory':
        spec['title'] = 'Observatory'
    for variable in spec.get('variables', []):
        v = variable['spec']
        metric = 'xui_observatory_outbound_alive' if v['name'] == 'outbound' else 'xui_balancer_outbound_available'
        expr = f'label_values({metric}{{{FILTER}}},{v["name"]})'
        v['query']['spec']['query'] = expr
        v['definition'] = expr
        v['includeAll'] = True
        v['allValue'] = '.*'
        v['sort'] = 'alphabeticalAsc'
        v['current'] = {'text': 'All', 'value': ['$__all']}
        v['options'] = []
    spec['collapse'] = spec['title'] not in ('Observatory', 'Balancers')

s['variables'] = [{
    'kind': 'QueryVariable', 'spec': {
        'name': 'instance', 'label': 'Instance', 'hide': 'dontHide',
        'current': {'text': '192.168.10.16:9109', 'value': '192.168.10.16:9109'},
        'refresh': 'onDashboardLoad', 'skipUrlSync': False,
        'query': {'kind': 'DataQuery', 'group': 'prometheus', 'version': 'v0',
                  'datasource': {'name': 'ffis3jx2q5yioa'},
                  'spec': {'qryType': 1, 'query': 'label_values(xui_exporter_build_info{job="xui-exporter"},instance)',
                           'refId': 'PrometheusVariableQueryEditor-VariableQuery'}},
        'definition': 'label_values(xui_exporter_build_info{job="xui-exporter"},instance)',
        'regex': '', 'regexApplyTo': 'value', 'sort': 'alphabeticalAsc',
        'options': [], 'multi': False, 'includeAll': False, 'allowCustomValue': False,
    }
}]

for key in list(elements):
    elements[key] = migrate(elements[key])

def panel(n):
    return elements[f'panel-{n}']['spec']

def query(n, expression, ref='A'):
    for q in panel(n)['data']['spec']['queries']:
        if q['spec']['refId'] == ref:
            q['spec']['query']['spec']['expr'] = expression

def metric(name, extra=''):
    return name + '{' + FILTER + (',' + extra if extra else '') + '}'

alive = metric('xui_observatory_outbound_alive', 'outbound=~"${outbound}"')
failed = metric('xui_observatory_outbound_probe_failed_samples', 'outbound=~"${outbound}"')
health = f'({alive} * on(instance,outbound) (1 + ({failed} == bool 0)))\nor on(instance,outbound) ({alive} * 2)'
for n in (1, 4):
    query(n, health)
    panel(n)['description'] = 'DOWN: Xray alive=0. DEGRADED: burst window contains failed probes. UP: alive with no failed probes; ordinary observatory exposes only alive. Missing data stays missing.'
panel(1)['vizConfig']['spec']['fieldConfig']['defaults']['color'] = {'mode': 'fixed'}
common_delay = metric('xui_observatory_outbound_rtt_mean_seconds', 'outbound=~"${outbound}"') + ' or on(instance,outbound) ' + metric('xui_observatory_outbound_delay_seconds', 'outbound=~"${outbound}"')
query(4, common_delay, 'B')
query(2, common_delay)
panel(2)['description'] = 'Burst: mean RTT of successful samples in the observation window. Ordinary observatory: last successful delay. Seconds; no fabricated values on failure.'
panel(5)['description'] = 'P50/P95 are percentiles of observed window means, not individual HTTP requests. Probe success is a sample-weighted estimate over overlapping observation windows, not a count of unique probes.'
panel(6)['description'] = 'Dashboard heuristic, not the score used by Xray leastLoad. Burst-only reliability inputs are absent for ordinary observatory. No missing values are filled with zero.'
for n in (1, 2, 3, 17):
    panel(n)['data']['spec']['queryOptions'].update({'interval': '5s', 'maxDataPoints': 10000})
    for q in panel(n)['data']['spec']['queries']:
        q['spec']['query']['spec']['interval'] = '5s'

# New traffic metrics encode direction as a label, so aggregate directions rather
# than adding vectors with incompatible direction labels.
out = metric('xui_outbound_traffic_bytes_total', 'outbound!~"direct|blocked|_bl_.*"')
inbound = metric('xui_inbound_traffic_bytes_total', 'inbound!~"api|node_egress_.*"')
query(9, f'8 * sum by (instance,outbound) (rate({out}[$__rate_interval]))')
query(10, f'sort_desc(sum by (instance,outbound) (increase({out}[$__range])))')
query(11, f'sum(increase({out}[$__range]))')
direct = metric('xui_outbound_traffic_bytes_total', 'outbound="direct"')
query(11, f'sum(increase({direct}[$__range]))', 'B')
query(12, f'8 * sum by (instance,inbound) (rate({inbound}[$__rate_interval]))')
query(13, f'sort_desc(sum by (instance,inbound) (increase({inbound}[$__range])))')
for ref, direction in [('A', 'uplink'), ('B', 'downlink')]:
    m = metric('xui_inbound_traffic_bytes_total', f'inbound!~"api|node_egress_.*",direction="{direction}"')
    query(14, f'sum(increase({m}[$__range]))', ref)
panel(9)['title'] = 'Outbound throughput'
panel(15)['description'] = 'Observed target replacements in a centered 60-second window. Division by two assumes a constant selected-set size and stable inventory; additions/removals can produce half values. The latest centered bucket is incomplete.'
panel(16)['description'] = 'Fraction of observed samples in which a target was selected, not its share of traffic. Multiple selected targets can sum above 100%. Missing scrapes do not count as zero.'

def make_query(expr, legend, ref='A', instant=False):
    return {'kind': 'PanelQuery', 'spec': {'refId': ref, 'hidden': False, 'query': {
        'kind': 'DataQuery', 'group': 'prometheus', 'version': 'v0',
        'datasource': {'name': 'ffis3jx2q5yioa'},
        'spec': {'editorMode': 'code', 'expr': expr, 'legendFormat': legend,
                 'instant': instant, 'range': not instant}}}}

def add(n, title, group, queries, unit='short', description=''):
    base = copy.deepcopy(panel(2 if group == 'timeseries' else 17))
    base.update({'id': n, 'title': title, 'description': description})
    base['data']['spec'] = {'queries': queries, 'transformations': [], 'queryOptions': {'interval': '5s', 'maxDataPoints': 10000}}
    base['vizConfig']['group'] = group
    v = base['vizConfig']['spec']
    v['fieldConfig'] = {'defaults': {'unit': unit, 'min': 0, 'color': {'mode': 'palette-classic'}}, 'overrides': []}
    if group == 'timeseries':
        v['fieldConfig']['defaults']['custom'] = copy.deepcopy(panel(2)['vizConfig']['spec']['fieldConfig']['defaults']['custom'])
        v['fieldConfig']['defaults']['custom'].update({'fillOpacity': 10, 'thresholdsStyle': {'mode': 'off'}})
        v['options'] = {'legend': {'showLegend': True, 'displayMode': 'table', 'placement': 'bottom', 'calcs': ['lastNotNull', 'max']},
                        'tooltip': {'mode': 'multi', 'sort': 'desc', 'hideZeros': False}}
    elif group == 'stat':
        v['options'] = {'reduceOptions': {'calcs': ['lastNotNull'], 'fields': '', 'values': False},
                        'orientation': 'auto', 'textMode': 'auto', 'colorMode': 'value', 'graphMode': 'none', 'justifyMode': 'auto'}
    else:
        v['options'] = {'mergeValues': True, 'showValue': 'never', 'alignValue': 'left', 'rowHeight': 0.9,
                        'legend': {'showLegend': False, 'displayMode': 'list', 'placement': 'bottom'},
                        'tooltip': {'mode': 'multi', 'sort': 'none', 'hideZeros': False}}
        v['fieldConfig']['defaults']['color'] = {'mode': 'fixed'}
        v['fieldConfig']['defaults']['custom'] = {'lineWidth': 0, 'fillOpacity': 100, 'spanNulls': False, 'insertNulls': False}
    elements[f'panel-{n}'] = {'kind': 'Panel', 'spec': base}

def boolean(n, zero, one, zero_color='red'):
    panel(n)['vizConfig']['spec']['fieldConfig']['defaults']['mappings'] = [{'type': 'value', 'options': {
        '0': {'text': zero, 'color': zero_color, 'index': 0},
        '1': {'text': one, 'color': 'green', 'index': 1}}}]

def item(n, x, y, w, h):
    return {'kind': 'GridLayoutItem', 'spec': {'x': x, 'y': y, 'width': w, 'height': h,
            'element': {'kind': 'ElementReference', 'name': f'panel-{n}'}}}

def row(title, items, collapsed=False):
    return {'kind': 'RowsLayoutRow', 'spec': {'title': title, 'collapse': collapsed,
            'layout': {'kind': 'GridLayout', 'spec': {'items': items}}}}

add(18, 'Prometheus target', 'stat', [make_query('up{'+FILTER+'}', '{{instance}}', instant=True)], description='Prometheus can scrape the exporter. Source statuses separately report collection success.')
boolean(18, 'DOWN', 'UP')
add(19, 'Collection sources', 'stat', [make_query(metric('xui_exporter_scrape_success'), '{{source}}', instant=True)], description='API: configuration discovery. gRPC: runtime routing, traffic and process. Expvars: observatory. Disabled sources are absent, not zero.')
boolean(19, 'FAILED', 'OK')
add(22, 'Allocated memory', 'stat', [make_query(metric('xui_xray_memory_alloc_bytes'), 'Go heap', instant=True)], 'bytes', 'Go runtime allocation; not RSS.')
add(23, 'Collection history', 'state-timeline', [make_query(metric('xui_exporter_scrape_success'), '{{source}}')])
boolean(23, 'FAILED', 'OK')
add(24, 'Collection duration', 'timeseries', [make_query(metric('xui_exporter_scrape_duration_seconds'), '{{source}}')], 's', 'Active collection time per source. gRPC measures only runtime outbound/balancer reads, excluding API/expvars waits. Prometheus scrape_duration_seconds measures the complete scrape.')
add(25, 'Balancer outbound availability', 'state-timeline', [make_query(metric('xui_balancer_outbound_available', 'balancer=~"${balancer}"'), '{{balancer}}')], description='A selected target or fallback resolves to a runtime outbound. Structural availability, not a test of internet access.')
boolean(25, 'Unavailable', 'Available')
add(26, 'Selected target count', 'timeseries', [make_query('sum by(instance,balancer) ('+metric('xui_balancer_outbound_selected', 'balancer=~"${balancer}"')+')', '{{balancer}}')], description='Number of selected targets. No selection series means no value; it is not synthesized as zero.')
add(27, 'Burst RTT range', 'timeseries', [make_query(metric('xui_observatory_outbound_rtt_'+stat+'_seconds','outbound=~"${outbound}"'), '{{outbound}} · '+stat, chr(65+i)) for i,stat in enumerate(['min','max'])], 's', 'Minimum and maximum successful RTT in the current burst window. Not available for ordinary observatory.')
add(28, 'Burst failed probes', 'timeseries', [make_query(f'{failed} / on(instance,outbound) '+metric('xui_observatory_outbound_probe_samples','outbound=~"${outbound}"'), '{{outbound}}')], 'percentunit', 'Failed fraction in the current burst window, not a cumulative counter. Empty measurements remain absent.')
add(29, 'Go runtime memory', 'timeseries', [make_query(metric('xui_xray_memory_'+name+'_bytes'), legend, chr(65+i)) for i,(name,legend) in enumerate([('alloc','Allocated'),('sys','Reserved by Go')])], 'bytes', 'Go runtime memory values; neither is process RSS.')
add(30, 'Allocation throughput', 'timeseries', [make_query('rate('+metric('xui_xray_memory_alloc_bytes_total')+'[$__rate_interval])','Allocated bytes/s')], 'Bps')
add(31, 'Live objects', 'timeseries', [make_query(metric('xui_xray_memory_live_objects'), 'Objects')])
add(32, 'Object allocations / frees', 'timeseries', [make_query('rate('+metric('xui_xray_memory_'+name+'_total')+'[$__rate_interval])', legend, chr(65+i)) for i,(name,legend) in enumerate([('mallocs','Allocations/s'),('frees','Frees/s')])], 'ops')
add(33, 'GC cycles', 'timeseries', [make_query('rate('+metric('xui_xray_gc_cycles_total')+'[$__rate_interval])','Cycles/s')], 'ops')
add(34, 'GC pause time', 'timeseries', [make_query('rate('+metric('xui_xray_gc_pause_seconds_total')+'[$__rate_interval])','Pause fraction')], 'percentunit', 'GC pause seconds per elapsed second, displayed as a percentage.')
add(35, 'Exporter version', 'stat', [make_query(metric('xui_exporter_build_info'), '{{version}}', instant=True)], description='Version label of the running exporter.')
panel(35)['vizConfig']['spec']['options']['textMode'] = 'name'

rows.insert(0, row('Overview', [item(18,0,0,4,4),item(19,4,0,10,4),item(22,14,0,6,4),item(35,20,0,4,4),item(23,0,4,12,6),item(24,12,4,12,6)]))
obsrow = next(r['spec'] for r in rows if r['spec']['title']=='Observatory')
obsrow['layout']['spec']['items'].extend([item(27,0,29,12,8),item(28,12,29,12,8)])
lbrow = next(r['spec'] for r in rows if r['spec']['title']=='Balancers')
lbrow['layout']['spec']['items'].extend([item(25,0,15,12,6),item(26,12,15,12,6)])
rows.append(row('Xray process', [item(29,0,0,12,8),item(30,12,0,12,8),item(31,0,8,12,8),item(32,12,8,12,8),item(33,0,16,12,8),item(34,12,16,12,8)]))

# Reject old data dependencies and dangling layout references before upload.
payload = json.dumps(d, ensure_ascii=False, indent=2)
assert not re.search(r'xray_(?:observatory|inbound|outbound|lb)_', payload)
refs = []
for r in rows:
    refs.extend(i['spec']['element']['name'] for i in r['spec']['layout']['spec']['items'])
assert set(refs)==set(elements) and len(refs)==len(set(refs))
assert len({v['spec']['id'] for v in elements.values()})==len(elements)
(ROOT/'grafana-3x-ui.v2.json').write_text(payload+'\n')
print(f'Created {len(elements)} panels in {len(rows)} rows')
