import { SettingsTable } from "../components/SettingsTable";
import { useDocumentTitle } from "../hooks";

export function SettingsPage() {
  useDocumentTitle("Settings");
  return (
    <div className="page">
      <div className="page-header">
        <div>
          <h1>Settings</h1>
          <p className="muted">Global secrets and variables, available to workflows in every repository.</p>
        </div>
      </div>
      <div className="stack">
        <SettingsTable kind="secrets" />
        <SettingsTable kind="variables" />
      </div>
    </div>
  );
}
