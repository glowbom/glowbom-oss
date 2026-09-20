import type { DiscoveredStack } from '../lib/preview';

export function ExistingStacks({ apps, loading, error, limited, disabled, onRefresh, onAdd }: {
  apps: DiscoveredStack[]; loading: boolean; error: string; limited: boolean; disabled: boolean;
  onRefresh: () => void; onAdd: (app: DiscoveredStack) => void;
}) {
  return <div className="stack-discovery" aria-busy={loading}>
    {loading ? <p className="meta" role="status">Checking for other apps…</p> : null}
    {error ? <p className="error-inline" role="alert">{error}</p> : null}
    {!loading && !error && apps.length > 0 ? <details className="found-stacks">
      <summary>Found {apps.length} other {apps.length === 1 ? 'app' : 'apps'} in this project</summary>
      <p className="meta">Add an existing folder to your stacks. Review its settings before saving.</p>
      <ul className="found-stack-list">{apps.map((app) => <li className="found-stack" key={app.directory}>
        <div><strong>{app.name}</strong><span className="meta">{app.stack} · <code>{app.directory}</code></span></div>
        <button className="button secondary tiny" type="button" disabled={disabled} onClick={() => onAdd(app)}>Add to project</button>
      </li>)}</ul>
    </details> : null}
    {limited && !loading && !error ? <p className="meta">This scan reached its size limit. Connect another folder with Describe your own.</p> : null}
    <button className="preview-text-button" type="button" disabled={loading || disabled} onClick={onRefresh}>Scan for existing apps</button>
  </div>;
}
