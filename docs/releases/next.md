## Panel and Xray status

- Add a GET /panel/api/server/status request within the existing API source, parallel with configuration discovery.
- Export xui_panel_info{version}, xui_xray_info{version}, and xui_xray_state{state="running|stop|error"}.
- Preserve independent successful fields and sources on failure; omit unknown/missing state and versions without caching or source fallback.
- API duration now spans both parallel API requests; runtime collection starts immediately after configuration discovery, without waiting for status.
- Add version/process-state cards to both dashboard formats and display overlapping durations without stacking.

The panel supplies its latest background status snapshot, not a fresh process probe on each exporter request. Missing fields in older panels are omitted. Update the dashboard with the exporter to use the revised timing model.
