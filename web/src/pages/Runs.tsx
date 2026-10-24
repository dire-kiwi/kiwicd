import { RunList } from "../components/RunList";
import { useDocumentTitle } from "../hooks";

export function RunsPage() {
  useDocumentTitle("Runs");
  return (
    <div className="page">
      <div className="page-header">
        <div>
          <h1>All workflow runs</h1>
          <p className="muted">Showing the most recent runs across every repository.</p>
        </div>
      </div>
      <RunList />
    </div>
  );
}
